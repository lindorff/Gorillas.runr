//go:build !windows

package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Skaanning/runr/internal/config"
)

const testTimeout = 300 * time.Millisecond

// Tests that check something happens before the shutdown timeout use
// longTimeout, and allow quickLimit: far apart, so a slow CI runner can't
// reach the limit, while waiting for the timeout still would.
const (
	longTimeout = 10 * time.Second
	quickLimit  = 2 * time.Second
)

// recorder consumes a supervisor's events and records state history.
type recorder struct {
	mu       sync.Mutex
	history  map[string][]State
	latest   map[string]ProcessInfo
	warnings map[string][]string
	pending  map[string][]string // PendingEvent texts
	closed   chan struct{}
}

func record(s *Supervisor) *recorder {
	r := &recorder{history: map[string][]State{}, latest: map[string]ProcessInfo{}, warnings: map[string][]string{}, pending: map[string][]string{}, closed: make(chan struct{})}
	go func() {
		defer close(r.closed)
		for e := range s.Events() {
			r.mu.Lock()
			switch e := e.(type) {
			case StateEvent:
				r.history[e.Info.Name] = append(r.history[e.Info.Name], e.Info.State)
				r.latest[e.Info.Name] = e.Info
			case WarningEvent:
				r.warnings[e.Project] = append(r.warnings[e.Project], e.Text)
			case PendingEvent:
				r.pending[e.Project] = append(r.pending[e.Project], e.Text)
			}
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *recorder) states(name string) []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]State(nil), r.history[name]...)
}

// waitInfo waits until the latest info of a project satisfies ok.
func (r *recorder) waitInfo(t *testing.T, name string, ok func(ProcessInfo) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		info := r.latest[name]
		r.mu.Unlock()
		if ok(info) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: unexpected info %+v", name, info)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *recorder) pendingOf(name string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.pending[name]...)
}

func (r *recorder) warningsOf(name string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.warnings[name]...)
}

// waitState waits until the project's latest state is want and returns its info.
func (r *recorder) waitState(t *testing.T, name string, want State) ProcessInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		info, ok := r.latest[name]
		r.mu.Unlock()
		if ok && info.State == want {
			return info
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not reach %v; history %v", name, want, r.states(name))
	return ProcessInfo{}
}

// newTestSupervisor creates a supervisor for projects given as name → command.
func newTestSupervisor(t *testing.T, projects map[string]string) (*Supervisor, *recorder) {
	t.Helper()
	return newTestSupervisorTimeout(t, projects, testTimeout)
}

// newTestSupervisorTimeout is newTestSupervisor with the given shutdown timeout.
func newTestSupervisorTimeout(t *testing.T, projects map[string]string, timeout time.Duration) (*Supervisor, *recorder) {
	t.Helper()
	cfg := config.New()
	cfg.Settings.ShutdownTimeoutValue.Duration = timeout
	dir := t.TempDir()
	for name, cmd := range projects {
		cfg.Projects[name] = &config.Project{Dir: dir, Cmd: cmd}
	}
	return newTestSupervisorWithConfig(t, cfg)
}

func newTestSupervisorWithConfig(t *testing.T, cfg *config.Config) (*Supervisor, *recorder) {
	t.Helper()
	s := New(cfg, Options{BaseEnv: []string{"PATH=" + os.Getenv("PATH")}})
	r := record(s)
	t.Cleanup(s.Close)
	return s, r
}

// logLines returns a project's log as "stream:text" strings.
func logLines(s *Supervisor, name string) []string {
	var out []string
	for _, l := range s.Logs().Lines(name) {
		out = append(out, []string{"out", "err", "sys", "fail"}[l.Stream]+":"+l.Text)
	}
	return out
}

func TestStartAndExit(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "echo hello; echo oops >&2"})
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	info := r.waitState(t, "api", Exited)

	if !info.Exit.Success() || info.Pid != 0 {
		t.Errorf("info = %+v", info)
	}
	if got := r.states("api"); !reflect.DeepEqual(got, []State{Running, Exited}) {
		t.Errorf("states = %v", got)
	}
	want := []string{
		"sys:starting: echo hello; echo oops >&2",
		"out:hello",
		"err:oops",
		"sys:exited (exit code 0)",
	}
	got := logLines(s, "api")
	// stdout and stderr are read concurrently, so their relative order may vary.
	if len(got) != 4 || got[0] != want[0] || got[3] != want[3] ||
		!(reflect.DeepEqual(got[1:3], want[1:3]) || reflect.DeepEqual(got[1:3], []string{want[2], want[1]})) {
		t.Errorf("logs = %q", got)
	}
}

func TestCrash(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "exit 3"})
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	info := r.waitState(t, "api", Crashed)
	if info.Exit.Code != 3 {
		t.Errorf("exit = %+v", info.Exit)
	}
	if logs := logLines(s, "api"); logs[len(logs)-1] != "fail:crashed (exit code 3)" {
		t.Errorf("logs = %q", logs)
	}
}

func TestStartErrors(t *testing.T) {
	cfg := config.New()
	cfg.Projects["sleepy"] = &config.Project{Dir: t.TempDir(), Cmd: "sleep 30"}
	missing := filepath.Join(t.TempDir(), "gone")
	cfg.Projects["nodir"] = &config.Project{Dir: missing, Cmd: "true"}
	s, r := newTestSupervisorWithConfig(t, cfg)

	if err := s.Start("nope"); err == nil || err.Error() != `unknown project or group "nope"` {
		t.Errorf("unknown: %v", err)
	}

	if err := s.Start("sleepy"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "sleepy", Running)
	// Starting a live project again is a no-op.
	pid := r.waitState(t, "sleepy", Running).Pid
	if err := s.Start("sleepy"); err != nil {
		t.Errorf("double start: %v", err)
	}
	if got := s.Processes()[0]; got.Pid != pid || got.Restarts != 0 {
		t.Errorf("double start changed the process: %+v", got)
	}

	// A start failure is not an API error; it shows up as Crashed.
	if err := s.Start("nodir"); err != nil {
		t.Fatal(err)
	}
	info := r.waitState(t, "nodir", Crashed)
	if info.Exit.Err == nil {
		t.Errorf("exit = %+v", info.Exit)
	}
	if logs := logLines(s, "nodir"); logs[len(logs)-1] != "fail:failed to start: dir "+missing+" does not exist" {
		t.Errorf("logs = %q", logs)
	}
}

func TestStopGraceful(t *testing.T) {
	s, r := newTestSupervisorTimeout(t, map[string]string{"api": "sleep 30"}, longTimeout)
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "api", Running)

	start := time.Now()
	if err := s.Stop("api"); err != nil {
		t.Fatal(err)
	}
	info := r.waitState(t, "api", Stopped)
	if elapsed := time.Since(start); elapsed >= quickLimit {
		t.Errorf("graceful stop took %v (kill timeout is %v)", elapsed, longTimeout)
	}
	if info.Exit.Signal != "terminated" {
		t.Errorf("exit = %+v", info.Exit)
	}
	if got := r.states("api"); !reflect.DeepEqual(got, []State{Running, Stopping, Stopped}) {
		t.Errorf("states = %v", got)
	}
}

func TestStopKillsAfterTimeout(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "trap '' TERM; echo ready; sleep 30"})
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	waitLog(t, s, "api", "out:ready")

	start := time.Now()
	if err := s.Stop("api"); err != nil {
		t.Fatal(err)
	}
	info := r.waitState(t, "api", Stopped)
	if elapsed := time.Since(start); elapsed < testTimeout {
		t.Errorf("stopped after %v, before the %v timeout", elapsed, testTimeout)
	}
	if info.Exit.Signal != "killed" {
		t.Errorf("exit = %+v", info.Exit)
	}
	if !containsLine(logLines(s, "api"), "fail:still running after 300ms, killing") {
		t.Errorf("logs = %q", logLines(s, "api"))
	}
}

func TestLeftoversStoppedAfterExit(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "sleep 30 & echo pid $!"})
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "api", Exited)
	logs := logLines(s, "api")
	if !containsLine(logs, "sys:stopped processes the command left running") {
		t.Errorf("logs = %q", logs)
	}
	for _, l := range logs {
		if pid, ok := strings.CutPrefix(l, "out:pid "); ok {
			if out, err := exec.Command("ps", "-p", pid).Output(); err == nil {
				t.Errorf("leftover still running:\n%s", out)
			}
		}
	}
}

func TestStopWaitsForLeftovers(t *testing.T) {
	// The shell exits on SIGTERM, its background child ignores it.
	s, r := newTestSupervisor(t, map[string]string{"api": "(trap '' TERM; echo ready; sleep 30) & wait"})
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	waitLog(t, s, "api", "out:ready")

	start := time.Now()
	if err := s.Stop("api"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "api", Stopped)
	if elapsed := time.Since(start); elapsed < testTimeout {
		t.Errorf("stopped after %v, before the %v timeout", elapsed, testTimeout)
	}
	logs := logLines(s, "api")
	if !containsLine(logs, "fail:still running after 300ms, killing") ||
		containsLine(logs, "sys:stopped processes the command left running") {
		t.Errorf("logs = %q", logs)
	}
}

func TestNote(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "true", "web": "true"})
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "api", Exited)
	if err := s.Note("api", "$ git switch main", "Switched to branch 'main'"); err != nil {
		t.Fatal(err)
	}
	logs := logLines(s, "api")
	if got := logs[len(logs)-2:]; !reflect.DeepEqual(got, []string{"sys:$ git switch main", "sys:Switched to branch 'main'"}) {
		t.Errorf("logs = %q", logs)
	}
	if err := s.Note("web", "x"); err == nil {
		t.Error("noted on a project outside the session")
	}
}

func TestKill(t *testing.T) {
	s, r := newTestSupervisorTimeout(t, map[string]string{"api": "trap '' TERM; echo ready; sleep 30"}, longTimeout)
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	waitLog(t, s, "api", "out:ready")

	start := time.Now()
	if err := s.Kill("api"); err != nil {
		t.Fatal(err)
	}
	info := r.waitState(t, "api", Stopped)
	if elapsed := time.Since(start); elapsed >= quickLimit {
		t.Errorf("kill took %v (shutdown timeout is %v)", elapsed, longTimeout)
	}
	if info.Exit.Signal != "killed" {
		t.Errorf("exit = %+v", info.Exit)
	}
}

func TestStopWhenNotAliveIsNoop(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "true"})
	if err := s.Stop("api"); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "api", Exited)
	if err := s.Stop("api"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := r.states("api"); !reflect.DeepEqual(got, []State{Running, Exited}) {
		t.Errorf("states = %v", got)
	}
}

func TestSetArgs(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "echo run"})
	if err := s.SetArgs("nope", "x"); err == nil {
		t.Error("SetArgs accepted an unknown project")
	}
	if err := s.SetArgs("api", " with args "); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	if info := r.waitState(t, "api", Exited); info.Args != "with args" {
		t.Errorf("Args = %q", info.Args)
	}
	waitLog(t, s, "api", "sys:starting: echo run with args")
	waitLog(t, s, "api", "out:run with args")

	// A restart keeps the args; a change applies from the next start.
	if err := s.Restart("api"); err != nil {
		t.Fatal(err)
	}
	waitRuns := func(line string, n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for countLines(logLines(s, "api"), line) < n {
			if time.Now().After(deadline) {
				t.Fatalf("no %q (%d); logs %q", line, n, logLines(s, "api"))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitRuns("out:run with args", 2)
	r.waitState(t, "api", Exited)

	if err := s.SetArgs("api", "  "); err != nil { // blank: removed
		t.Fatal(err)
	}
	if err := s.Restart("api"); err != nil {
		t.Fatal(err)
	}
	waitRuns("out:run", 1)
	// The recorder may still have the last run's Exited, with its args:
	// states come through events, behind the logs.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		info := r.latest["api"]
		r.mu.Unlock()
		if info.State == Exited && info.Args == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Exited without args; last %v with Args %q", info.State, info.Args)
		}
	}
}

func TestSetDir(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "cat marker"})
	if err := s.SetDir("nope", t.TempDir()); err == nil {
		t.Error("SetDir accepted an unknown project")
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "marker"), []byte("from other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDir("api", " "+other+" "); err != nil {
		t.Fatal(err)
	}
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	if info := r.waitState(t, "api", Exited); info.Dir != other {
		t.Errorf("Dir = %q, want %q", info.Dir, other)
	}
	waitLog(t, s, "api", "sys:starting: cat marker (in "+other+")")
	waitLog(t, s, "api", "out:from other")

	// A restart keeps the dir.
	if err := s.Restart("api"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); countLines(logLines(s, "api"), "out:from other") < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no second run in %s; logs %q", other, logLines(s, "api"))
		}
	}
	r.waitState(t, "api", Exited)

	// Blank: back to the configured dir, which has no marker.
	if err := s.SetDir("api", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Restart("api"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); countLines(logLines(s, "api"), "sys:starting: cat marker") < 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no start in the configured dir; logs %q", logLines(s, "api"))
		}
	}
}

func TestRestart(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "echo run; sleep 30"})
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	waitLog(t, s, "api", "out:run")
	firstPid := r.waitState(t, "api", Running).Pid

	if err := s.Restart("api"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for countLines(logLines(s, "api"), "out:run") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("no second run; logs %q", logLines(s, "api"))
		}
		time.Sleep(5 * time.Millisecond)
	}

	info := r.waitState(t, "api", Running)
	if info.Restarts != 1 || info.Pid == firstPid || info.Pid == 0 {
		t.Errorf("info = %+v (first pid %d)", info, firstPid)
	}
	if got := r.states("api"); !reflect.DeepEqual(got, []State{Running, Stopping, Stopped, Running}) {
		t.Errorf("states = %v", got)
	}
}

func TestRestartWhenNotAlive(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"api": "echo run"})
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "api", Exited)
	if err := s.Restart("api"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for countLines(logLines(s, "api"), "out:run") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("no second run; logs %q", logLines(s, "api"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEnvLayers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("B=file\nC=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.New()
	cfg.Projects["api"] = &config.Project{
		Dir:     dir,
		Cmd:     `echo "$A $B $C $FORCE_COLOR"`,
		EnvFile: ".env",
		Env:     map[string]string{"C": "env"},
	}
	s := New(cfg, Options{BaseEnv: []string{"PATH=" + os.Getenv("PATH"), "A=base", "B=base", "C=base"}})
	r := record(s)
	t.Cleanup(s.Close)

	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "api", Exited)
	if !containsLine(logLines(s, "api"), "out:base file env 1") {
		t.Errorf("logs = %q", logLines(s, "api"))
	}
}

func TestMissingEnvFileFailsStart(t *testing.T) {
	cfg := config.New()
	cfg.Projects["api"] = &config.Project{Dir: t.TempDir(), Cmd: "true", EnvFile: "missing.env"}
	s, r := newTestSupervisorWithConfig(t, cfg)
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	info := r.waitState(t, "api", Crashed)
	if info.Exit.Err == nil || !strings.Contains(info.Exit.Err.Error(), "env file") {
		t.Errorf("exit = %+v", info.Exit)
	}
}

func TestProcesses(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"b": "sleep 30", "a": "true", "c": "true"})
	for _, name := range []string{"b", "a"} {
		if err := s.Start(name); err != nil {
			t.Fatal(err)
		}
	}
	r.waitState(t, "a", Exited)
	r.waitState(t, "b", Running)

	got := s.Processes() // session order, not alphabetical
	if len(got) != 2 || got[0].Name != "b" || got[0].State != Running || got[1].Name != "a" || got[1].State != Exited {
		t.Errorf("Processes() = %+v", got)
	}
}

func TestPids(t *testing.T) {
	s, r := newTestSupervisor(t, map[string]string{"a": "sleep 30 & echo $!; wait", "b": "true"})
	if err := s.Start("a", "b"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "b", Exited)
	info := r.waitState(t, "a", Running)
	child := 0
	for deadline := time.Now().Add(5 * time.Second); child == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, l := range logLines(s, "a") {
			if pid, ok := strings.CutPrefix(l, "out:"); ok {
				child, _ = strconv.Atoi(pid)
			}
		}
	}
	if child == 0 {
		t.Fatalf("no pid in the logs %q", logLines(s, "a"))
	}

	got := s.Pids() // b has exited: no tree
	if len(got) != 1 || !slices.Contains(got["a"], info.Pid) || !slices.Contains(got["a"], child) {
		t.Errorf("Pids() = %v, want a: [%d %d]", got, info.Pid, child)
	}
	if err := s.Stop("a"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "a", Stopped)
	if got := s.Pids(); len(got) != 0 {
		t.Errorf("Pids() after stop = %v", got)
	}
}

func TestClose(t *testing.T) {
	cfg := config.New()
	cfg.Settings.ShutdownTimeoutValue.Duration = time.Hour // Close must not wait for this
	dir := t.TempDir()
	cfg.Projects["a"] = &config.Project{Dir: dir, Cmd: "trap '' TERM; sleep 30"}
	cfg.Projects["b"] = &config.Project{Dir: dir, Cmd: "sleep 30"}
	s := New(cfg, Options{BaseEnv: []string{"PATH=" + os.Getenv("PATH")}})
	r := record(s)

	for _, name := range []string{"a", "b"} {
		if err := s.Start(name); err != nil {
			t.Fatal(err)
		}
		r.waitState(t, name, Running)
	}

	done := make(chan struct{})
	go func() {
		s.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-r.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Events was not closed")
	}

	if err := s.Start("a"); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close: %v", err)
	}
	s.Close() // idempotent
}

func waitLog(t *testing.T, s *Supervisor, name, line string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !containsLine(logLines(s, name), line) {
		if time.Now().After(deadline) {
			t.Fatalf("log line %q not seen; logs %q", line, logLines(s, name))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func containsLine(lines []string, line string) bool {
	return countLines(lines, line) > 0
}

func countLines(lines []string, line string) int {
	n := 0
	for _, l := range lines {
		if l == line {
			n++
		}
	}
	return n
}

// fakeTracker records Tracker calls.
type fakeTracker struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeTracker) Spawned(project string, pid int, command, dir, stopCmd string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := "spawned " + project + " " + command + " " + strconv.FormatBool(pid > 0)
	if stopCmd != "" {
		call += " stop_cmd " + stopCmd + " in " + filepath.Base(dir)
	}
	f.calls = append(f.calls, call)
}

func (f *fakeTracker) Exited(project string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "exited "+project)
}

func (f *fakeTracker) get() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func TestTracker(t *testing.T) {
	cfg := config.New()
	cfg.Settings.ShutdownTimeoutValue.Duration = testTimeout
	cfg.Projects["api"] = &config.Project{Dir: t.TempDir(), Cmd: "sleep 30"}
	cfg.Projects["bad"] = &config.Project{Dir: "/nonexistent/dir", Cmd: "true"}
	tr := &fakeTracker{}
	s := New(cfg, Options{BaseEnv: []string{"PATH=" + os.Getenv("PATH")}, Tracker: tr})
	r := record(s)
	t.Cleanup(s.Close)
	if err := s.Start("api", "bad"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "api", Running)
	r.waitState(t, "bad", Crashed) // never started: not tracked
	if err := s.Restart("api"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); len(tr.get()) < 3 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	r.waitState(t, "api", Running)
	if err := s.Stop("api"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "api", Stopped)
	want := []string{"spawned api sleep 30 true", "exited api", "spawned api sleep 30 true", "exited api"}
	if got := tr.get(); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestErrorLines(t *testing.T) {
	// The first run prints three error lines, colored or not, and two that
	// only look like it; the second run none.
	s, r := newTestSupervisor(t, map[string]string{"api": `if [ -f ran ]; then echo fine; exit 0; fi; touch ran
printf '\033[31mERROR\033[0m: boom\n'; echo 'no errors here'; echo 'terror'; echo 'panic: x' >&2; echo 'Traceback (most recent call last):'`})
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	r.waitInfo(t, "api", func(i ProcessInfo) bool { return i.State == Exited && i.Errors == 3 })
	var marked []string
	for _, l := range s.Logs().Lines("api") {
		if l.Error {
			marked = append(marked, l.Text)
		}
	}
	slices.Sort(marked) // stdout and stderr lines can come in either order
	if want := []string{"\x1b[31mERROR\x1b[0m: boom", "Traceback (most recent call last):", "panic: x"}; !slices.Equal(marked, want) {
		t.Errorf("marked %q", marked)
	}

	if err := s.Restart("api"); err != nil {
		t.Fatal(err)
	}
	r.waitInfo(t, "api", func(i ProcessInfo) bool { return i.State == Exited && i.Restarts == 1 && i.Errors == 0 })
}

func TestErrorPatternSetting(t *testing.T) {
	cfg := config.New()
	cfg.Settings.ErrorPatternValue = `^E\d+`
	cfg.Projects["api"] = &config.Project{Dir: t.TempDir(), Cmd: "echo E42 failed; echo error"}
	s, r := newTestSupervisorWithConfig(t, cfg)
	if err := s.Start("api"); err != nil {
		t.Fatal(err)
	}
	r.waitInfo(t, "api", func(i ProcessInfo) bool { return i.State == Exited && i.Errors == 1 })
	waitLog(t, s, "api", "out:error")
	time.Sleep(20 * time.Millisecond) // a second count would have arrived by now
	r.waitInfo(t, "api", func(i ProcessInfo) bool { return i.Errors == 1 })
}

func TestCrashIsAnError(t *testing.T) {
	cfg := config.New()
	dir := t.TempDir()
	cfg.Projects["api"] = &config.Project{Dir: dir, Cmd: "echo error one; exit 3"}
	cfg.Projects["gone"] = &config.Project{Dir: dir + "/missing", Cmd: "true"}
	s, r := newTestSupervisorWithConfig(t, cfg)
	if err := s.Start("api", "gone"); err != nil {
		t.Fatal(err)
	}
	r.waitInfo(t, "api", func(i ProcessInfo) bool { return i.State == Crashed && i.Errors == 2 })
	r.waitInfo(t, "gone", func(i ProcessInfo) bool { return i.State == Crashed && i.Errors == 1 })
	for name, want := range map[string]string{"api": "crashed (exit code 3)", "gone": "failed to start: "} {
		lines := s.Logs().Lines(name)
		if l := lines[len(lines)-1]; !l.Error || l.Stream != SystemError || !strings.HasPrefix(l.Text, want) {
			t.Errorf("%s: last line %+v", name, l)
		}
	}
}

func TestPTY(t *testing.T) {
	cfg := config.New()
	dir := t.TempDir()
	cfg.Projects["term"] = &config.Project{Dir: dir, PTY: true, ReadyLog: `^on a tty$`,
		Cmd: `[ -t 1 ] && echo on a tty; echo "TERM=$TERM"; trap 'stty size' WINCH; stty size; while :; do sleep 0.02; done`}
	cfg.Projects["pipes"] = &config.Project{Dir: dir, Cmd: `[ -t 1 ] || echo no tty`}
	s, r := newTestSupervisorWithConfig(t, cfg)
	if err := s.Resize(100, 0); err != nil { // rows stay at the default
		t.Fatal(err)
	}
	if err := s.Start("term", "pipes"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "term", Running) // ready_log matched the terminal's output
	waitLog(t, s, "term", "out:TERM=xterm-256color")
	waitLog(t, s, "term", "out:50 100")
	waitLog(t, s, "pipes", "out:no tty")

	if err := s.Resize(90, 30); err != nil {
		t.Fatal(err)
	}
	waitLog(t, s, "term", "out:30 90")

	if err := s.Stop("term"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "term", Stopped)
}

func TestInput(t *testing.T) {
	cfg := config.New()
	dir := t.TempDir()
	cfg.Projects["ask"] = &config.Project{Dir: dir, PTY: true, DisplayName: "Ask",
		Cmd: `printf 'branch: '; read b; echo "got $b"; sleep 30`}
	cfg.Projects["pipes"] = &config.Project{Dir: dir, Cmd: `sleep 30`}
	cfg.Projects["idle"] = &config.Project{Dir: dir, PTY: true, Cmd: `true`}
	s, r := newTestSupervisorWithConfig(t, cfg)
	if err := s.Start("ask", "pipes"); err != nil {
		t.Fatal(err)
	}
	waitPending := func(text string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !slices.Contains(r.pendingOf("ask"), text) {
			if time.Now().After(deadline) {
				t.Fatalf("no unfinished line %q; got %q", text, r.pendingOf("ask"))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitPending("branch: ")
	// Typed and echoed piece by piece, with a typo taken back.
	for _, typed := range []struct{ in, echo string }{{"feat", "branch: feat"}, {"ure/xx", "branch: feature/xx"}, {"\x7f", "branch: feature/x"}} {
		if err := s.Input("ask", []byte(typed.in)); err != nil {
			t.Fatal(err)
		}
		waitPending(typed.echo)
	}
	if err := s.Input("ask", []byte("\r")); err != nil {
		t.Fatal(err)
	}
	waitLog(t, s, "ask", "out:branch: feature/x")
	waitLog(t, s, "ask", "out:got feature/x")
	if p := r.pendingOf("ask"); p[len(p)-1] != "" {
		t.Errorf("pending lines %q: not cleared", p)
	}
	for _, l := range logLines(s, "ask") {
		if l == "out:branch: " || l == "out:branch: feat" {
			t.Errorf("an unfinished line in the log: %q", logLines(s, "ask"))
		}
	}

	for name, want := range map[string]string{
		"pipes": "pipes doesn't take input: set pty = true",
		"idle":  "idle isn't running",
		"nope":  `unknown project "nope"`,
	} {
		if err := s.Input(name, []byte("x")); err == nil || err.Error() != want {
			t.Errorf("Input(%s) = %v, want %q", name, err, want)
		}
	}
	if err := s.Stop("ask"); err != nil {
		t.Fatal(err)
	}
	r.waitState(t, "ask", Stopped)
	if err := s.Input("ask", []byte("x")); err == nil || err.Error() != "Ask isn't running" {
		t.Errorf("after stopping: %v", err)
	}
}
