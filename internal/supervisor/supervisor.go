package supervisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/logfile"
	"github.com/Skaanning/runr/internal/proc"
)

var (
	// ErrClosed is returned by commands sent after Close.
	ErrClosed = errors.New("supervisor is closed")
	// ErrShuttingDown is returned by Start and Restart while StopAll is in progress.
	ErrShuttingDown = errors.New("shutting down")
)

// Options configures a Supervisor.
type Options struct {
	// BaseEnv is the environment every process starts from; defaults to os.Environ().
	BaseEnv []string
	// LogDir, if set, is an existing directory where each project's lines
	// are appended to <project>.log (see logfile.Writer).
	LogDir string
	// Tracker, if set, is told about every process started and ended
	// (sessions.Recorder), on the loop goroutine.
	Tracker Tracker
}

// Tracker records the session's live processes, so that a later runr can
// find those left running if this one dies (see sessions.Recorder).
type Tracker interface {
	// Spawned records a new process, started in dir; stopCmd is the
	// project's stop_cmd, for cleaning up after it if it is left over.
	Spawned(project string, pid int, command, dir, stopCmd string)
	Exited(project string)
}

// Supervisor runs projects from a config, tracks their state and collects
// their output.
//
// All process state is owned by a single loop goroutine; the public methods
// send it messages. Output lines bypass the loop and go straight to Logs and
// Events, so heavy output never delays commands.
type Supervisor struct {
	cfg     *config.Config
	logs    *Logs
	errorRe *regexp.Regexp // settings.error_pattern; nil if invalid
	baseEnv []string
	timeout time.Duration

	msgs      chan func()
	closed    chan struct{}
	ctx       context.Context // cancelled by Close; ends readiness probes
	files     *logfile.Writer // persistent logs (Options.LogDir); nil if off
	tracker   Tracker         // Options.Tracker; nil if none
	cancel    context.CancelFunc
	closeOnce sync.Once
	events    *eventHub
	running   sync.WaitGroup // live processes, for Close

	// Owned by the loop goroutine.
	procs    map[string]*managed
	args     map[string]string // extra arguments by project, see SetArgs
	dirs     map[string]string // working directories by project, see SetDir
	cols     int               // pseudo-terminal size, see Resize
	rows     int
	order    []string // session order: the order projects were first added
	closing  bool     // set by Close; no new processes are started
	shutdown *shutdown
}

type managed struct {
	name    string
	project *config.Project
	readyRe *regexp.Regexp // ready_log; nil if not set
	probe   *probe         // ready_port or ready_url; nil if not set
	info    ProcessInfo

	seenPorts map[int]bool // ports it listened on earlier in the session

	// The run's error lines (ProcessInfo.Errors is their sum): the
	// program's, as counted by its lineHandler, and runr's failure lines.
	outputErrors, failures int

	proc           *proc.Process
	stopProc       *proc.Process // the stop_cmd running after the run was killed; nil if none
	gen            int           // incremented on every start; stale messages are ignored
	stopRequested  bool
	killed         bool // runr killed the run's process (see config.Project.StopCmd)
	restartPending bool
	killTimer      *time.Timer
	cancelProbe    context.CancelFunc // stops the current run's probe; nil if none
}

// waitsForReady reports whether a run of m becomes ready later than its
// spawn: on a ready_log line, a successful probe, or a clean exit.
func (m *managed) waitsForReady() bool {
	return m.readyRe != nil || m.probe != nil || m.project.OneShot()
}

// New creates a supervisor for the projects in cfg, which should be valid
// (see config.Validate). Nothing is started until Start is called. The caller
// must keep reading Events until it is closed.
func New(cfg *config.Config, opts Options) *Supervisor {
	baseEnv := opts.BaseEnv
	if baseEnv == nil {
		baseEnv = os.Environ()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{
		ctx:     ctx,
		cancel:  cancel,
		cfg:     cfg,
		logs:    NewLogs(cfg.Settings.ScrollbackLines()),
		baseEnv: baseEnv,
		timeout: cfg.Settings.ShutdownTimeout(),
		msgs:    make(chan func(), 64),
		closed:  make(chan struct{}),
		events:  newEventHub(cfg.Settings.ScrollbackLines()),
		procs:   map[string]*managed{},
		args:    map[string]string{},
		dirs:    map[string]string{},
		cols:    proc.DefaultCols,
		rows:    proc.DefaultRows,
		tracker: opts.Tracker,
	}
	if re, err := regexp.Compile(cfg.Settings.ErrorPattern()); err == nil {
		s.errorRe = re
	}
	if opts.LogDir != "" {
		s.files = logfile.NewWriter(opts.LogDir, s.logFileFailed)
		s.logs.onAppend = func(l LogLine) { s.files.Write(l.Project, l.Time, l.Stream.Runr(), l.Text) }
	}
	go s.loop()
	return s
}

// logFileFailed reports in a project's log that its log file can't be
// written any more. It runs with the Writer's lock held, maybe on the loop.
func (s *Supervisor) logFileFailed(project, path string, err error) {
	go s.post(func() {
		if m := s.procs[project]; m != nil {
			s.systemError(m, "can't write the log file, no more output is saved there: %v", err)
		}
	})
}

// Events delivers state changes and log lines in order. It is closed after Close.
func (s *Supervisor) Events() <-chan Event {
	return s.events.main.out
}

// Subscribe returns a stream of every event from now on, for another
// consumer than the one of Events (a view attached from another terminal),
// and the pending lines now (by project, see PendingEvent): together with
// Processes and Logs, what a new view needs. Lines stored just before the
// subscription may be both in Logs and in its events. The subscriber must
// keep reading, or Close it.
func (s *Supervisor) Subscribe() (*Subscription, map[string]string) {
	h := s.events
	h.mu.Lock()
	defer h.mu.Unlock()
	sub := &Subscription{q: newEventQueue(), hub: h}
	pending := maps.Clone(h.pending)
	if h.closed {
		sub.q.close()
	} else {
		h.subs[sub] = true
	}
	return sub, pending
}

// Logs returns the store holding every project's recent output.
func (s *Supervisor) Logs() *Logs {
	return s.logs
}

func (s *Supervisor) loop() {
	for {
		select {
		case fn := <-s.msgs:
			fn()
		case <-s.closed:
			return
		}
	}
}

// post runs fn on the loop goroutine without waiting. It must not be called
// from the loop goroutine itself.
func (s *Supervisor) post(fn func()) {
	select {
	case s.msgs <- fn:
	case <-s.closed:
	}
}

// call runs fn on the loop goroutine and returns its result.
func (s *Supervisor) call(fn func() error) error {
	reply := make(chan error, 1)
	select {
	case s.msgs <- func() { reply <- fn() }:
	case <-s.closed:
		return ErrClosed
	}
	select {
	case err := <-reply:
		return err
	case <-s.closed:
		return ErrClosed
	}
}

// Processes returns a snapshot of every project in the session, in the order
// they were added (dependencies before the projects that need them).
func (s *Supervisor) Processes() []ProcessInfo {
	var out []ProcessInfo
	_ = s.call(func() error {
		for _, name := range s.order {
			out = append(out, s.procs[name].info)
		}
		return nil
	})
	return out
}

// Pids returns the process IDs in the process tree of every project with a
// live process, by name (see proc.Pids). The trees are read outside the
// loop.
func (s *Supervisor) Pids() map[string][]int {
	var names []string
	var procs []*proc.Process
	_ = s.call(func() error {
		for _, name := range s.order {
			if p := s.procs[name].proc; p != nil {
				names = append(names, name)
				procs = append(procs, p)
			}
		}
		return nil
	})
	out := make(map[string][]int, len(names))
	for i, pids := range proc.Pids(procs) {
		if len(pids) > 0 {
			out[names[i]] = pids
		}
	}
	return out
}

// RememberPorts records ports the projects were seen listening on (by
// name), so that later starts warn when another process has taken one.
func (s *Supervisor) RememberPorts(ports map[string][]int) {
	_ = s.call(func() error {
		for name, ps := range ports {
			m := s.procs[name]
			if m == nil {
				continue
			}
			if m.seenPorts == nil {
				m.seenPorts = map[int]bool{}
			}
			for _, p := range ps {
				m.seenPorts[p] = true
			}
		}
		return nil
	})
}

// Start starts the given projects and groups, plus all their dependencies.
// Projects that are already alive or waiting are left alone. Each project
// starts as soon as its dependencies are ready; until then it is Waiting.
func (s *Supervisor) Start(names ...string) error {
	return s.call(func() error {
		if err := s.acceptingStarts(); err != nil {
			return err
		}
		resolved, err := s.cfg.Resolve(names...)
		if err != nil {
			return err
		}
		ms := make([]*managed, len(resolved))
		for i, name := range resolved {
			if ms[i], err = s.managed(name); err != nil {
				return err
			}
		}
		// Resolve orders dependencies first, so they are requested first.
		for _, m := range ms {
			if !m.info.State.Alive() && m.info.State != Waiting {
				s.request(m)
			}
		}
		return nil
	})
}

// SetArgs sets extra arguments for a project, appended to its command from
// its next start or restart on (see config.Project.CommandWith); blank
// args remove them. Restarts keep them. The project doesn't have to be in
// the session yet.
func (s *Supervisor) SetArgs(name, args string) error {
	return s.call(func() error {
		if s.cfg.Projects[name] == nil {
			return fmt.Errorf("unknown project %q", name)
		}
		if args = strings.TrimSpace(args); args == "" {
			delete(s.args, name)
		} else {
			s.args[name] = args
		}
		return nil
	})
}

// SetDir sets the directory a project runs in, instead of its configured
// dir, from its next start or restart on (e.g. a git worktree of its
// repository); a blank dir goes back to the configured one. Restarts keep
// it. The project doesn't have to be in the session yet.
func (s *Supervisor) SetDir(name, dir string) error {
	return s.call(func() error {
		if s.cfg.Projects[name] == nil {
			return fmt.Errorf("unknown project %q", name)
		}
		if dir = strings.TrimSpace(dir); dir == "" {
			delete(s.dirs, name)
		} else {
			s.dirs[name] = dir
		}
		return nil
	})
}

// Resize sets the size of the pseudo-terminals of the projects with
// pty = true: of those running now, and of those started later. Zero or
// negative values keep the current one.
func (s *Supervisor) Resize(cols, rows int) error {
	return s.call(func() error {
		if cols > 0 {
			s.cols = cols
		}
		if rows > 0 {
			s.rows = rows
		}
		for _, m := range s.procs {
			if m.proc != nil && m.project.PTY {
				_ = m.proc.Resize(s.cols, s.rows)
			}
		}
		return nil
	})
}

// Stop asks a project to stop: it is terminated gracefully and killed if it
// is still alive after the shutdown timeout. A project that is waiting to
// start is cancelled. Stop returns immediately; the outcome is reported
// through Events.
func (s *Supervisor) Stop(name string) error {
	return s.call(func() error {
		m, err := s.existing(name)
		if m == nil {
			return err
		}
		m.restartPending = false
		s.stop(m)
		return nil
	})
}

// Restart stops a live project and starts it again once it has exited. A
// project that is not alive is started like Start, dependencies included.
func (s *Supervisor) Restart(name string) error {
	return s.call(func() error {
		if err := s.acceptingStarts(); err != nil {
			return err
		}
		m, err := s.managed(name)
		if err != nil {
			return err
		}
		if !m.info.State.Alive() {
			resolved, err := s.cfg.Resolve(name)
			if err != nil {
				return err
			}
			for _, dep := range resolved {
				dm, err := s.managed(dep)
				if err != nil {
					return err
				}
				if !dm.info.State.Alive() && dm.info.State != Waiting {
					s.request(dm)
				}
			}
			return nil
		}
		m.restartPending = true
		s.stop(m)
		return nil
	})
}

// Note appends lines to a project's log as runr's own (System) lines, e.g.
// the output of a git command run for it.
func (s *Supervisor) Note(name string, lines ...string) error {
	return s.call(func() error {
		m := s.procs[name]
		if m == nil {
			return fmt.Errorf("project %q is not in the session", name)
		}
		for _, l := range lines {
			s.system(m, "%s", l)
		}
		return nil
	})
}

// Input types b into a running project's pseudo-terminal (pty = true), as
// keys pressed in a terminal: "\r" is Enter. It doesn't wait for the
// program to read it.
func (s *Supervisor) Input(name string, b []byte) error {
	return s.call(func() error {
		m, err := s.existing(name)
		if err != nil {
			return err
		}
		p := s.cfg.Projects[name]
		if m != nil {
			p = m.project // as started
		}
		label := p.Label(name)
		switch {
		case !p.PTY:
			return fmt.Errorf("%s doesn't take input: set pty = true", label)
		case m == nil || m.proc == nil:
			return fmt.Errorf("%s isn't running", label)
		}
		if err := m.proc.Type(b); err != nil {
			if errors.Is(err, proc.ErrNoTerminal) {
				return fmt.Errorf("%s isn't running", label)
			}
			return fmt.Errorf("%s: %w", label, err)
		}
		return nil
	})
}

// Kill force-kills a project immediately.
func (s *Supervisor) Kill(name string) error {
	return s.call(func() error {
		m, err := s.existing(name)
		if m == nil {
			return err
		}
		s.kill(m)
		return nil
	})
}

// StopAll gracefully stops every project in reverse dependency order: a
// project is only asked to stop once everything depending on it has exited.
// Projects in the same dependency level stop concurrently, each with its own
// shutdown timeout. Waiting projects are cancelled. The returned channel is
// closed once no process is alive.
func (s *Supervisor) StopAll() <-chan struct{} {
	done := make(chan struct{})
	if err := s.call(func() error {
		s.beginShutdown(done)
		return nil
	}); err != nil {
		close(done)
	}
	return done
}

// KillAll force-kills every live process immediately, e.g. on a second
// Ctrl+C during StopAll.
func (s *Supervisor) KillAll() {
	_ = s.call(func() error {
		for _, name := range s.order {
			s.kill(s.procs[name])
		}
		return nil
	})
}

// Close force-kills every live process, waits for them to exit, and shuts
// the supervisor down. Events is closed once all remaining events are delivered.
func (s *Supervisor) Close() {
	_ = s.call(func() error {
		s.closing = true
		for _, name := range s.order {
			s.kill(s.procs[name])
		}
		return nil
	})
	s.running.Wait()
	s.closeOnce.Do(func() {
		s.cancel()
		close(s.closed)
		s.events.close()
		if s.files != nil {
			s.files.Close()
		}
	})
}

func (s *Supervisor) acceptingStarts() error {
	switch {
	case s.closing:
		return ErrClosed
	case s.shutdown != nil:
		return ErrShuttingDown
	}
	return nil
}

// existing returns the session entry for a project. It returns nil and no
// error for a known project that is not in the session yet.
func (s *Supervisor) existing(name string) (*managed, error) {
	if m := s.procs[name]; m != nil {
		return m, nil
	}
	if s.cfg.Projects[name] == nil {
		return nil, fmt.Errorf("unknown project %q", name)
	}
	return nil, nil
}

// managed returns the session entry for a project, adding it if needed.
func (s *Supervisor) managed(name string) (*managed, error) {
	if m := s.procs[name]; m != nil {
		return m, nil
	}
	p := s.cfg.Projects[name]
	if p == nil {
		return nil, fmt.Errorf("unknown project %q", name)
	}
	m := &managed{name: name, project: p, info: ProcessInfo{Name: name, State: Pending}}
	if p.ReadyLog != "" {
		re, err := regexp.Compile(p.ReadyLog)
		if err != nil {
			return nil, fmt.Errorf("project %q: invalid ready_log: %w", name, err)
		}
		m.readyRe = re
	}
	m.probe = newProbe(p)
	s.procs[name] = m
	s.order = append(s.order, name)
	return m, nil
}

func (s *Supervisor) setState(m *managed, state State) {
	m.info.State = state
	s.events.push(StateEvent{Info: m.info})
}

// system appends a runr-generated line to a project's log.
func (s *Supervisor) system(m *managed, format string, args ...any) {
	line := s.logs.Append(m.name, System, fmt.Sprintf(format, args...))
	s.events.push(LineEvent{Line: line})
}

// warn appends a runr-generated warning and reports it with a WarningEvent.
func (s *Supervisor) warn(m *managed, text string) {
	s.system(m, "%s", text)
	s.events.push(WarningEvent{Project: m.name, Text: text})
}

// failure appends a runr-generated line saying the run failed ("crashed",
// "failed to start"). It is an error line of the run, counted in Errors; the
// StateEvent that follows reports the count.
func (s *Supervisor) failure(m *managed, format string, args ...any) {
	line := s.logs.AppendLine(LogLine{Project: m.name, Stream: SystemError, Text: fmt.Sprintf(format, args...), Error: true})
	s.events.push(LineEvent{Line: line})
	m.failures++
	m.info.Errors = m.outputErrors + m.failures
}

// systemError appends a runr-generated line about a failure.
func (s *Supervisor) systemError(m *managed, format string, args ...any) {
	line := s.logs.Append(m.name, SystemError, fmt.Sprintf(format, args...))
	s.events.push(LineEvent{Line: line})
}
