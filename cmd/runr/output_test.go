package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReaderGone(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if readerGone(w) {
		t.Fatal("gone while the reader is open")
	}
	r.Close()
	if !readerGone(w) {
		t.Fatal("not gone after the reader closed")
	}

	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if readerGone(f) {
		t.Error("a file is never gone")
	}
}

func TestWatchOutput(t *testing.T) {
	old := goneEvery
	goneEvery = time.Millisecond
	t.Cleanup(func() { goneEvery = old })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	gone := make(chan struct{})
	watchOutput(t.Context(), w, func() { close(gone) })
	select {
	case <-gone:
		t.Fatal("gone while the reader is open")
	case <-time.After(20 * time.Millisecond):
	}
	r.Close()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("not told the reader closed")
	}

	// Not a file: nothing to watch.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	watchOutput(ctx, &bytes.Buffer{}, func() { t.Error("gone called for a buffer") })
}

func TestOutputError(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	full := errors.New("no space left on device")
	if got := outputError(w, full); got != full {
		t.Errorf("reader open: %v", got)
	}
	r.Close()
	if got := outputError(w, full); got != errOutputGone {
		t.Errorf("reader closed: %v", got)
	}
}
