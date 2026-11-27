package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/state"
	"github.com/Skaanning/runr/internal/supervisor"
)

// recordingSession is a supervisor whose starts and extra arguments are
// recorded as the last session, for `runr up --last` and the launcher's l.
type recordingSession struct {
	*supervisor.Supervisor
	last *state.Recorder
}

func newRecordingSession(cfg *config.Config, opts supervisor.Options) recordingSession {
	return recordingSession{supervisor.New(cfg, opts), state.NewRecorder(stateDir())}
}

// Start starts names and, if that worked, records them.
func (s recordingSession) Start(names ...string) error {
	if err := s.Supervisor.Start(names...); err != nil {
		return err
	}
	s.last.Started(names...)
	return nil
}

// SetArgs sets a project's extra arguments and records them.
func (s recordingSession) SetArgs(name, args string) error {
	if err := s.Supervisor.SetArgs(name, args); err != nil {
		return err
	}
	s.last.SetArgs(name, args)
	return nil
}

// begin sets extra arguments and directories (see configure), then starts
// names.
func (s recordingSession) begin(names []string, args, dirs map[string]string) error {
	if err := s.configure(args, dirs); err != nil {
		return err
	}
	return s.Start(names...)
}

// configure sets extra arguments, which are recorded, and directories to
// run projects in instead of their configured ones (--worktree), which
// aren't: a worktree is usually gone before the session is started again.
func (s recordingSession) configure(args, dirs map[string]string) error {
	for name, a := range args {
		if err := s.SetArgs(name, a); err != nil {
			return err
		}
	}
	for name, d := range dirs {
		if err := s.SetDir(name, d); err != nil {
			return err
		}
	}
	return nil
}

// stateDir is runr's state directory, or "" if it can't be found (then
// nothing is remembered).
func stateDir() string {
	dir, err := state.Dir()
	if err != nil {
		return ""
	}
	return dir
}

// loadLast returns the last session, or nil if there is none.
func loadLast() *state.LastSession {
	if l, ok := state.LoadLast(stateDir()); ok {
		return &l
	}
	return nil
}

// lastSession returns what `runr up --last` starts: the last session's
// groups and projects that are still registered, with their extra
// arguments. Skipped names are reported on stderr.
func lastSession(cfg *config.Config, stderr io.Writer) ([]string, map[string]string, error) {
	l := loadLast()
	if l == nil {
		return nil, nil, errors.New("no last session yet: it is recorded when runr starts something")
	}
	names, args, skipped := l.Registered(cfg)
	if len(names) == 0 {
		return nil, nil, errors.New("nothing from the last session is registered any more")
	}
	if len(skipped) > 0 {
		fmt.Fprintf(stderr, "runr: skipped from the last session, not registered any more: %s\n", strings.Join(skipped, ", "))
	}
	return names, args, nil
}
