//go:build !windows

package main

import (
	"bufio"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Skaanning/runr/internal/config"
)

// A write to a stdout nobody reads ends runr logs quietly, rather than
// SIGPIPE killing it: a busy project writes before the follow's check for
// a closed reader.
func TestLogsEndsQuietlyOnBrokenPipe(t *testing.T) {
	sess := startSessionWith(t, map[string]*config.Project{
		"spam": {Cmd: "while :; do echo tick; sleep 0.01; done"},
	}, "spam")
	waitOutput(t, sess.out, "tick")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errOut := &syncBuffer{}
	cmd := exec.Command(os.Args[0], "logs", "-f", "spam")
	cmd.Env = append(os.Environ(), mainEnv+"=1")
	cmd.Stdout = w
	cmd.Stderr = errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	// Like head -1: read a line, then go away while lines keep coming.
	if _, err := bufio.NewReader(r).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	r.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil || errOut.String() != "" {
			t.Errorf("err = %v, stderr %q", err, errOut)
		}
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		t.Fatal("logs -f went on after its output closed")
	}
}
