package supervisor

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/plaintext"
	"github.com/Skaanning/runr/internal/proc"
)

// The methods in this file run on the loop goroutine.

// spawn starts the process for m. Failures to start are reported as Crashed.
func (s *Supervisor) spawn(m *managed) {
	if !m.info.StartedAt.IsZero() {
		m.info.Restarts++
	}
	m.gen++
	m.stopRequested, m.killed = false, false
	m.restartPending = false
	m.info.StartedAt = time.Now()
	m.info.Exit = proc.ExitStatus{}
	m.info.Pid = 0
	m.info.Errors, m.outputErrors, m.failures = 0, 0, 0
	m.info.Args = s.args[m.name]
	m.info.Dir = s.dirs[m.name]
	command := m.project.CommandWith(m.info.Args)

	// Logged before spawning so it precedes the process's first output.
	if m.info.Dir != "" {
		s.system(m, "starting: %s (in %s)", command, m.info.Dir)
	} else {
		s.system(m, "starting: %s", command)
	}
	spec, err := s.spec(m.project, command, m.info.Dir)
	if err == nil {
		m.proc, err = proc.Start(spec, s.lineHandler(m))
	}
	if err != nil {
		m.proc = nil
		m.info.Exit = proc.ExitStatus{Code: -1, Err: err}
		s.failure(m, "failed to start: %v", err)
		s.setState(m, Crashed)
		s.schedule()
		return
	}

	m.info.Pid = m.proc.Pid()
	if s.tracker != nil {
		s.tracker.Spawned(m.name, m.info.Pid, command, spec.Dir, m.project.StopCmd)
	}
	gen, p := m.gen, m.proc
	if ps := knownPorts(m); len(ps) > 0 {
		s.checkPorts(m, gen, p, ps)
	}
	s.watch(p, func() { s.exited(m, gen, p.Status()) })

	s.unblockDependents(m)
	// With a ready_log, the run becomes ready on a matching line, with
	// ready_port or ready_url once the probe succeeds; a one-shot task
	// becomes ready by exiting with code 0 (Exited).
	if m.waitsForReady() {
		s.setState(m, Starting)
		if m.probe != nil {
			s.startProbe(m)
		}
		return
	}
	s.setState(m, Running)
	s.schedule()
}

// lineHandler returns the output callback for the current run of m. It
// stores each line, marking and counting those that match error_pattern, and,
// while the run is not ready yet, checks it against ready_log. It runs on the
// process's reader goroutines (one per stream), not the loop.
func (s *Supervisor) lineHandler(m *managed) func(proc.Line) {
	name, gen, re, errorRe := m.name, m.gen, m.readyRe, s.errorRe
	var ready atomic.Bool
	var errCount atomic.Int64
	var counting atomic.Bool // a count is on its way to the loop
	pending := false         // a PendingEvent with text was sent last; calls are never concurrent
	return func(l proc.Line) {
		if l.Partial {
			pending = l.Text != ""
			s.events.push(PendingEvent{Project: name, Text: l.Text})
			return
		}
		if pending {
			pending = false
			s.events.push(PendingEvent{Project: name})
		}
		stream := Stdout
		if l.Stream == proc.Stderr {
			stream = Stderr
		}
		text := plaintext.StripANSI(l.Text)
		isError := errorRe != nil && errorRe.MatchString(text)
		s.events.push(LineEvent{Line: s.logs.AppendLine(LogLine{Project: name, Stream: stream, Text: l.Text, Error: isError})})

		// The loop takes the latest count, so a burst of errors sends one
		// update rather than one per line.
		if isError {
			errCount.Add(1)
			if counting.CompareAndSwap(false, true) {
				s.post(func() {
					counting.Store(false)
					s.countErrors(m, gen, int(errCount.Load()))
				})
			}
		}
		if re != nil && !ready.Load() && re.MatchString(text) {
			ready.Store(true)
			s.post(func() { s.becameReady(m, gen) })
		}
	}
}

// countErrors updates the count of the program's error lines in run gen of m.
func (s *Supervisor) countErrors(m *managed, gen, n int) {
	if gen != m.gen || n == m.outputErrors {
		return
	}
	m.outputErrors = n
	m.info.Errors = m.outputErrors + m.failures
	s.events.push(StateEvent{Info: m.info})
}

func (s *Supervisor) becameReady(m *managed, gen int) {
	if gen != m.gen || m.info.State != Starting {
		return
	}
	m.stopProbe()
	s.system(m, "ready")
	s.setState(m, Running)
	s.schedule()
}

// stop begins a graceful stop of m, or cancels it if it has not started yet.
func (s *Supervisor) stop(m *managed) {
	switch m.info.State {
	case Pending, Waiting, Blocked:
		if m.info.State != Pending {
			s.system(m, "cancelled")
		}
		s.setState(m, Stopped)
		s.schedule() // dependents waiting on m become blocked
		return
	case Starting, Running:
	default:
		return // Stopping, or not alive
	}

	m.stopRequested = true
	s.setState(m, Stopping)
	if err := m.proc.Terminate(); err != nil {
		s.systemError(m, "terminate failed (%v), killing", err)
		m.killed = true
		_ = m.proc.Kill()
		return
	}
	gen := m.gen
	m.killTimer = time.AfterFunc(s.timeout, func() {
		s.post(func() {
			if m.gen == gen && m.info.State == Stopping && m.proc != nil {
				s.systemError(m, "still running after %v, killing", s.timeout)
				m.killed = true
				_ = m.proc.Kill()
			}
		})
	})
}

// watch calls exited on the loop once p has ended. Close waits for it: the
// goroutine counts as running until exited has run, so that whatever exited
// starts (a stop_cmd) is counted before it isn't.
func (s *Supervisor) watch(p *proc.Process, exited func()) {
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		<-p.Done()
		done := make(chan struct{})
		s.post(func() {
			exited()
			close(done)
		})
		select {
		case <-done:
		case <-s.closed:
		}
	}()
}

// kill force-kills m if it is alive, or cancels it if it is waiting. Blocked
// projects keep their state: there is nothing to kill, and Blocked explains
// why they never ran.
func (s *Supervisor) kill(m *managed) {
	m.restartPending = false
	if !m.info.State.Alive() {
		if m.info.State == Waiting {
			s.stop(m)
		}
		return
	}
	m.stopRequested = true
	if m.info.State != Stopping {
		s.setState(m, Stopping)
	}
	switch {
	case m.proc != nil:
		m.killed = true
		_ = m.proc.Kill()
	case m.stopProc != nil: // forcing: its stop_cmd too
		_ = m.stopProc.Kill()
	}
}

// exited handles the end of a run.
func (s *Supervisor) exited(m *managed, gen int, status proc.ExitStatus) {
	if gen != m.gen {
		return
	}
	if s.tracker != nil {
		s.tracker.Exited(m.name)
	}
	if m.killTimer != nil {
		m.killTimer.Stop()
		m.killTimer = nil
	}
	m.stopProbe()
	m.proc = nil
	m.info.Pid = 0
	m.info.Exit = status
	if m.project.PTY {
		s.events.push(PendingEvent{Project: m.name}) // in case reading the output was cut short
	}

	if status.Leftovers && !m.stopRequested {
		s.system(m, "stopped processes the command left running")
	}
	if m.killed && m.project.StopCmd != "" {
		s.runStopCmd(m, status)
		return
	}
	s.ended(m, status)
}

// ended sets the state of m after its run ended with status, and moves on:
// a pending restart, waiting dependents, the shutdown.
func (s *Supervisor) ended(m *managed, status proc.ExitStatus) {
	switch {
	case m.stopRequested:
		s.system(m, "stopped (%s)", status)
		s.setState(m, Stopped)
	case status.Success():
		s.system(m, "exited (%s)", status)
		s.setState(m, Exited)
	default:
		s.failure(m, "crashed (%s)", status)
		s.setState(m, Crashed)
	}

	if m.restartPending && !s.closing && s.shutdown == nil {
		s.request(m)
		return
	}
	s.schedule()
	s.advanceShutdown()
}

// stopCmdTimeout is how long a stop_cmd may run before it is killed. A
// variable so tests can shorten it.
var stopCmdTimeout = time.Minute

// runStopCmd runs the stop_cmd of m, whose killed run ended with status.
// m stays Stopping meanwhile (a shutdown waits for it), and its output goes
// to m's log; then the run's end is handled as usual.
func (s *Supervisor) runStopCmd(m *managed, status proc.ExitStatus) {
	if m.info.State != Stopping {
		s.setState(m, Stopping)
	}
	s.system(m, "running stop_cmd: %s", m.project.StopCmd)
	spec, err := StopSpec(m.project, m.info.Dir, s.baseEnv, s.timeout)
	var p *proc.Process
	if err == nil {
		p, err = proc.Start(spec, s.stopCmdLines(m))
	}
	if err != nil {
		s.failure(m, "stop_cmd failed to start: %v", err)
		s.ended(m, status)
		return
	}
	m.stopProc = p
	timer := time.AfterFunc(stopCmdTimeout, func() {
		s.post(func() {
			if m.stopProc == p {
				s.systemError(m, "stop_cmd still running after %v, killing it", stopCmdTimeout)
				_ = p.Kill()
			}
		})
	})
	s.watch(p, func() {
		timer.Stop()
		m.stopProc = nil
		if st := p.Status(); st.Success() {
			s.system(m, "stop_cmd done (%s)", st)
		} else {
			s.failure(m, "stop_cmd failed (%s)", st)
		}
		s.ended(m, status)
	})
}

// stopCmdLines returns the output callback of m's stop_cmd: its lines go
// to m's log like the program's.
func (s *Supervisor) stopCmdLines(m *managed) func(proc.Line) {
	name := m.name
	return func(l proc.Line) {
		if l.Partial {
			return
		}
		stream := Stdout
		if l.Stream == proc.Stderr {
			stream = Stderr
		}
		s.events.push(LineEvent{Line: s.logs.AppendLine(LogLine{Project: name, Stream: stream, Text: l.Text})})
	}
}

// spec builds the process spec for a project running command: in dir (the
// project's resolved dir if "") with the layered environment (base, runr
// defaults, env_file, env).
func (s *Supervisor) spec(p *config.Project, command, dir string) (proc.Spec, error) {
	spec, err := commandSpec(p, command, dir, s.baseEnv, p.PTY, s.timeout)
	spec.Cols, spec.Rows = s.cols, s.rows
	return spec, err
}

// StopSpec is how a project's stop_cmd runs: like its command, without a
// terminal, in dir (the project's if "") with the project's environment
// on top of base. Processes it leaves behind get timeout to stop.
func StopSpec(p *config.Project, dir string, base []string, timeout time.Duration) (proc.Spec, error) {
	return commandSpec(p, p.StopCmd, dir, base, false, timeout)
}

// commandSpec builds the spec for running command for p: in dir (p's
// resolved dir if ""), which must exist, with the layered environment
// (base, runr defaults, TERM on a terminal, env_file, env).
func commandSpec(p *config.Project, command, dir string, base []string, pty bool, timeout time.Duration) (proc.Spec, error) {
	if dir == "" {
		var err error
		if dir, err = p.ResolvedDir(); err != nil {
			return proc.Spec{}, err
		}
	}
	// The dir is checked before starting (config.Validate), but it can
	// disappear later, e.g. before a restart.
	if fi, err := os.Stat(dir); err != nil {
		return proc.Spec{}, fmt.Errorf("dir %s does not exist", dir)
	} else if !fi.IsDir() {
		return proc.Spec{}, fmt.Errorf("dir %s is not a directory", dir)
	}
	var fileVars map[string]string
	if p.EnvFile != "" {
		envFile, err := p.ResolvedEnvFile()
		if err != nil {
			return proc.Spec{}, err
		}
		if envFile != "" {
			if fileVars, err = proc.LoadEnvFile(envFile); err != nil {
				return proc.Spec{}, err
			}
		}
	}
	var termEnv map[string]string
	if pty {
		termEnv = map[string]string{"TERM": "xterm-256color"}
	}
	return proc.Spec{
		Command: command,
		Dir:     dir,
		Env:     proc.BuildEnv(base, proc.DefaultEnv, termEnv, fileVars, p.Env),
		PTY:     pty,
		// Processes left behind by the command get as long to stop as the
		// command itself does.
		LeftoverTimeout: timeout,
	}, nil
}
