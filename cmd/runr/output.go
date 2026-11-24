package main

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// errOutputGone ends a command whose output nobody reads any more.
var errOutputGone = errors.New("the output was closed")

// goneEvery is how often a follow checks that its output is still read. A
// variable so tests can shorten it.
var goneEvery = 200 * time.Millisecond

// watchOutput calls gone, once, if nobody reads w any more before ctx is
// done: w is a pipe whose reader has exited, as head and grep -m1 do after
// what they wanted. A follow notices that without waiting for a line to
// write. Nothing is watched if w isn't a file.
func watchOutput(ctx context.Context, w io.Writer, gone func()) {
	f, ok := w.(*os.File)
	if !ok {
		return
	}
	go func() {
		t := time.NewTicker(goneEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if readerGone(f) {
				gone()
				return
			}
		}
	}()
}

// outputError returns errOutputGone if writing to w failed with err because
// nobody reads w any more, and err otherwise (e.g. a full disk).
func outputError(w io.Writer, err error) error {
	if f, ok := w.(*os.File); ok && readerGone(f) {
		return errOutputGone
	}
	return err
}
