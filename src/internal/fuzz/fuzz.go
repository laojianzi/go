// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package fuzz provides common fuzzing functionality for tests built with
// "go test" and for programs that use fuzzing functionality in the testing
// package.
package fuzz

import (
	"crypto/sha256"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// CoordinateFuzzing creates several worker processes and communicates with
// them to test random inputs that could trigger crashes and expose bugs.
// The worker processes run the same binary in the same directory with the
// same environment variables as the coordinator process. Workers also run
// with the same arguments as the coordinator, except with the -test.fuzzworker
// flag prepended to the argument list.
//
// parallel is the number of worker processes to run in parallel. If parallel
// is 0, CoordinateFuzzing will run GOMAXPROCS workers.
//
// seed is a list of seed values added by the fuzz target with testing.F.Add and
// in testdata.
// Seed values from GOFUZZCACHE should not be included in this list; this
// function loads them separately.
//
// If a crash occurs, the function will return an error containing information
// about the crash, which can be reported to the user.
func CoordinateFuzzing(parallel int, seed [][]byte, crashDir string) error {
	if parallel == 0 {
		parallel = runtime.GOMAXPROCS(0)
	}
	// TODO(jayconrod): support fuzzing indefinitely or with a given duration.
	// The value below is just a placeholder until we figure out how to handle
	// interrupts.
	duration := 5 * time.Second

	var corpus corpus
	var maxSeedLen int
	if len(seed) == 0 {
		corpus.entries = []corpusEntry{{b: []byte{}}}
		maxSeedLen = 0
	} else {
		corpus.entries = make([]corpusEntry, len(seed))
		for i, v := range seed {
			corpus.entries[i].b = v
			if len(v) > maxSeedLen {
				maxSeedLen = len(v)
			}
		}
	}
	// TODO(jayconrod,katiehockman): read corpus from GOFUZZCACHE.

	// TODO(jayconrod): do we want to support fuzzing different binaries?
	dir := "" // same as self
	binPath := os.Args[0]
	args := append([]string{"-test.fuzzworker"}, os.Args[1:]...)
	env := os.Environ() // same as self

	c := &coordinator{
		doneC:        make(chan struct{}),
		inputC:       make(chan corpusEntry),
		interestingC: make(chan fuzzResponse),
	}

	newWorker := func() (*worker, error) {
		mem, err := sharedMemTempFile(maxSeedLen)
		if err != nil {
			return nil, err
		}
		return &worker{
			dir:         dir,
			binPath:     binPath,
			args:        args,
			env:         env,
			coordinator: c,
			mem:         mem,
		}, nil
	}

	// Start workers.
	workers := make([]*worker, parallel)
	for i := range workers {
		var err error
		workers[i], err = newWorker()
		if err != nil {
			return err
		}
	}

	workerErrs := make([]error, len(workers))
	var wg sync.WaitGroup
	wg.Add(len(workers))
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			workerErrs[i] = workers[i].runFuzzing()
			if cleanErr := workers[i].cleanup(); workerErrs[i] == nil {
				workerErrs[i] = cleanErr
			}
		}(i)
	}

	// Main event loop.
	stopC := time.After(duration)
	i := 0
	for {
		select {
		// TODO(jayconrod): handle interruptions like SIGINT.
		// TODO(jayconrod,katiehockman): receive crashers and new corpus values
		// from workers.

		case <-stopC:
			// Time's up.
			close(c.doneC)

		case <-c.doneC:
			// Wait for workers to stop and return.
			wg.Wait()
			for _, err := range workerErrs {
				if err != nil {
					return err
				}
			}
			return nil

		case resp := <-c.interestingC:
			// Some interesting input arrived from a worker.
			if resp.Err != "" {
				// This is a crasher, which should be written to testdata and
				// reported to the user.
				fileName, err := writeToCorpus(resp.Value, crashDir)
				if err == nil {
					err = fmt.Errorf("    Crash written to: %s\n%s", fileName, resp.Err)
				}
				// TODO(jayconrod,katiehockman): if -keepfuzzing, don't stop all
				// of the workers, but still report to the user.

				// Stop the rest of the workers and wait until they have
				// stopped before returning this error.
				close(c.doneC)
				wg.Wait()
				return err
			} else if len(resp.Value) > 0 {
				// This is not a crasher, but something interesting that should
				// be added to the on disk corpus and prioritized for future
				// workers to fuzz.

				corpus.entries = append(corpus.entries, corpusEntry{b: resp.Value})
				// TODO(jayconrod, katiehockman): Add this to the on disk corpus
				// TODO(jayconrod, katiehockman): Prioritize fuzzing these values which expanded coverage
			}

		case c.inputC <- corpus.entries[i]:
			// Sent the next input to any worker.
			// TODO(jayconrod,katiehockman): need a scheduling algorithm that chooses
			// which corpus value to send next (or generates something new).
			i = (i + 1) % len(corpus.entries)
		}
	}

	// TODO(jayconrod,katiehockman): write crashers to testdata and other inputs
	// to GOFUZZCACHE. If the testdata directory is outside the current module,
	// always write to GOFUZZCACHE, since the testdata is likely read-only.
}

type corpus struct {
	entries []corpusEntry
}

// TODO(jayconrod,katiehockman): decide whether and how to unify this type
// with the equivalent in testing.
type corpusEntry struct {
	b []byte
}

// coordinator holds channels that workers can use to communicate with
// the coordinator.
type coordinator struct {
	// doneC is closed to indicate fuzzing is done and workers should stop.
	// doneC may be closed due to a time limit expiring or a fatal error in
	// a worker.
	doneC chan struct{}

	// inputC is sent values to fuzz by the coordinator. Any worker may receive
	// values from this channel.
	inputC chan corpusEntry

	// interestingC is sent interesting values by the worker, which is received
	// by the coordinator. The interesting value could be a crash or some
	// value that increased coverage.
	interestingC chan fuzzResponse
}

// ReadCorpus reads the corpus from the testdata directory in this target's
// package.
func ReadCorpus(dir string) ([][]byte, error) {
	files, err := ioutil.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil // No corpus to read
	} else if err != nil {
		return nil, fmt.Errorf("testing: reading seed corpus from testdata: %v", err)
	}
	var corpus [][]byte
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		bytes, err := ioutil.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			return nil, fmt.Errorf("testing: failed to read corpus file: %v", err)
		}
		corpus = append(corpus, bytes)
	}
	return corpus, nil
}

// writeToCorpus writes the given bytes to a new file in testdata. If the
// directory does not exist, it will create one. It returns the filename that
// was written, or an error if it failed.
func writeToCorpus(b []byte, crashDir string) (string, error) {
	// TODO: Consider not writing a new file if one with those contents already
	// exists. Perhaps the filename can be compared to those that already exist
	// if all of the filenames are normalized, or by checking the contents of
	// all other files.
	if _, err := ioutil.ReadDir(crashDir); os.IsNotExist(err) {
		// Make the seed corpus directory since it doesn't exist.
		err = os.MkdirAll(crashDir, 0777)
		if err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(b))
	name := filepath.Join(crashDir, sum)
	err := ioutil.WriteFile(name, b, 0666)
	if err != nil {
		return "", err
	}
	return name, nil
}