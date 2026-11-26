package control

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Skaanning/runr/internal/state"
)

// ErrNoSession is returned by Pick when no session is running.
var ErrNoSession = errors.New("no runr session is running")

// Timing, variables so tests can shorten them.
var (
	dialTimeout  = time.Second      // a session that doesn't accept by then is busy, not gone
	replyTimeout = 10 * time.Second // for a whole answer, unless it follows
	staleAfter   = 2 * time.Second  // a socket nobody listens on is removed once this old
)

// NoMatchError is returned by Pick when no session has all the names.
type NoMatchError struct{ msg string }

func (e *NoMatchError) Error() string { return e.msg }

// Session is a running session found by Sessions.
type Session struct {
	Path   string
	Status Status
}

// Sessions returns the sessions listening in dir, oldest first, and the
// ones that didn't answer properly. Sockets nobody listens on any more
// (their runr was killed) are removed.
func Sessions(dir string) ([]Session, []error) {
	if len(filepath.Join(dir, "4294967295.sock")) > maxPath {
		return nil, []error{fmt.Errorf("%w for sessions to listen on: %s; set %s to a shorter directory", ErrPathTooLong, dir, state.EnvVar)}
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	var found []Session
	var problems []error
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sock") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		var st *Status
		err := Do(path, Request{Op: OpStatus}, func(r Reply) error {
			st = r.Status
			return nil
		})
		switch {
		case err == nil && st != nil:
			found = append(found, Session{Path: path, Status: *st})
		case isStale(path, err):
			os.Remove(path)
		default:
			if err == nil {
				err = errors.New("no status in the answer")
			}
			problems = append(problems, fmt.Errorf("session %s: %w", strings.TrimSuffix(e.Name(), ".sock"), err))
		}
	}
	slices.SortFunc(found, func(a, b Session) int {
		return cmp.Or(a.Status.Started.Compare(b.Status.Started), cmp.Compare(a.Status.Pid, b.Status.Pid))
	})
	return found, problems
}

// isStale reports whether the socket at path failed to connect because
// nobody listens on it, and isn't new: a session binds its socket a moment
// before it listens.
func isStale(path string, err error) bool {
	var dial *dialError
	if !errors.As(err, &dial) {
		return false
	}
	var ne net.Error
	if errors.As(dial.err, &ne) && ne.Timeout() {
		return false // busy
	}
	fi, statErr := os.Stat(path)
	return statErr == nil && time.Since(fi.ModTime()) > staleAfter
}

type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// Do sends req (with the current Version) to the session at path and calls
// each with every reply, until the session closes the connection, each
// returns an error, or a reply is an error. Except for following logs and
// stopping the session (which takes as long as the shutdown), the whole
// answer must arrive within a few seconds.
func Do(path string, req Request, each func(Reply) error) error {
	return DoContext(context.Background(), path, req, each)
}

// DoContext is Do, ended early once ctx is done: it then returns ctx's
// cause (see context.Cause), e.g. to stop following logs.
func DoContext(ctx context.Context, path string, req Request, each func(Reply) error) error {
	c, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return &dialError{err}
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	if !req.Follow && req.Op != OpDown {
		c.SetDeadline(time.Now().Add(replyTimeout))
	}
	req.V = Version
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return cmp.Or(context.Cause(ctx), err)
	}
	dec := json.NewDecoder(c)
	for {
		var r Reply
		if err := dec.Decode(&r); ctx.Err() != nil {
			return context.Cause(ctx)
		} else if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if r.Error != "" {
			return errors.New(r.Error)
		}
		if err := each(r); err != nil {
			return err
		}
	}
}

// Pick chooses the session a command is for: the one with runr's pid if
// pid isn't 0; otherwise the only session that has all of names (any
// session when there are none).
func Pick(sessions []Session, names []string, pid int) (Session, error) {
	if pid != 0 {
		for _, s := range sessions {
			if s.Status.Pid == pid {
				return s, nil
			}
		}
		return Session{}, fmt.Errorf("no runr session with pid %d is running", pid)
	}
	if len(sessions) == 0 {
		return Session{}, ErrNoSession
	}
	var match []Session
	for _, s := range sessions {
		if hasAll(s.Status, names) {
			match = append(match, s)
		}
	}
	switch {
	case len(match) == 1:
		return match[0], nil
	case len(match) > 1:
		pids := make([]string, len(match))
		for i, s := range match {
			pids[i] = strconv.Itoa(s.Status.Pid)
		}
		return Session{}, fmt.Errorf("%d runr sessions are running (pids %s); choose one with --session", len(match), strings.Join(pids, ", "))
	case len(sessions) == 1:
		return Session{}, &NoMatchError{"not in the running session: " + strings.Join(missing(sessions[0].Status, names), ", ")}
	default:
		return Session{}, &NoMatchError{"no running session has " + strings.Join(names, ", ")}
	}
}

func hasAll(st Status, names []string) bool {
	return len(missing(st, names)) == 0
}

func missing(st Status, names []string) []string {
	var out []string
	for _, n := range names {
		if !st.Has(n) {
			out = append(out, n)
		}
	}
	return out
}
