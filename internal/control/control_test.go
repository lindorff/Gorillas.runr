package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Skaanning/runr/internal/ports"
	"github.com/Skaanning/runr/internal/proc"
	"github.com/Skaanning/runr/internal/supervisor"
)

// fakeTarget is a session with fixed processes and a real log store. It
// records starts, stops and restarts, and fails those of "broken".
type fakeTarget struct {
	mu    sync.Mutex
	procs []supervisor.ProcessInfo
	pids  map[string][]int
	logs  *supervisor.Logs
	calls []string
}

func (f *fakeTarget) record(call, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call+" "+name)
	if strings.Contains(name, "broken") {
		return errors.New("it broke")
	}
	return nil
}

func (f *fakeTarget) Start(names ...string) error { return f.record("start", strings.Join(names, ",")) }
func (f *fakeTarget) Stop(name string) error      { return f.record("stop", name) }
func (f *fakeTarget) Restart(name string) error   { return f.record("restart", name) }

func (f *fakeTarget) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeTarget) Processes() []supervisor.ProcessInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]supervisor.ProcessInfo(nil), f.procs...)
}
func (f *fakeTarget) Logs() *supervisor.Logs { return f.logs }
func (f *fakeTarget) Pids() map[string][]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pids
}

func newTarget() *fakeTarget {
	started := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	return &fakeTarget{
		procs: []supervisor.ProcessInfo{
			{Name: "db", State: supervisor.Running, Pid: 100, StartedAt: started},
			{Name: "api", State: supervisor.Crashed, Pid: 200, StartedAt: started, Exit: proc.ExitStatus{Code: 1}, Restarts: 2, Errors: 1, Args: "--verbose"},
		},
		pids: map[string][]int{"db": {100, 101}},
		logs: supervisor.NewLogs(100),
	}
}

// shortDir returns a new directory with a short path: socket paths are
// limited to about 100 bytes, and t.TempDir's can be longer.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "control")
}

func listen(t *testing.T, dir string, target Target, pid int) *Server {
	t.Helper()
	s, err := Listen(dir, target, Info{
		Pid:     pid,
		Started: time.Date(2026, 11, 18, 9, 59, 0, 0, time.UTC),
		Titles:  []string{"backend"},
		Labels:  map[string]string{"api": "API"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// collect sends req and returns the replies.
func collect(t *testing.T, path string, req Request) ([]Reply, error) {
	t.Helper()
	var out []Reply
	err := Do(path, req, func(r Reply) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

func lineTexts(replies []Reply) []string {
	var out []string
	for _, r := range replies {
		if r.Line != nil {
			out = append(out, r.Line.Text)
		}
	}
	return out
}

func TestStatus(t *testing.T) {
	old := listenPorts
	listenPorts = func(pids []int) ([]ports.Listener, error) {
		return []ports.Listener{{Pid: 100, Port: 5432}, {Pid: 101, Port: 80}, {Pid: 101, Port: 5432}, {Pid: 999, Port: 1}}, nil
	}
	t.Cleanup(func() { listenPorts = old })
	dir := shortDir(t)
	s := listen(t, dir, newTarget(), 4123)
	if s.Path() != filepath.Join(dir, "4123.sock") {
		t.Errorf("Path = %s", s.Path())
	}
	if fi, err := os.Stat(dir); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Errorf("dir: %v %v", fi.Mode(), err)
	}

	replies, err := collect(t, s.Path(), Request{Op: OpStatus})
	if err != nil || len(replies) != 1 || replies[0].Status == nil {
		t.Fatalf("replies = %+v, err = %v", replies, err)
	}
	st := *replies[0].Status
	if st.Pid != 4123 || !reflect.DeepEqual(st.Titles, []string{"backend"}) || st.Started.IsZero() {
		t.Errorf("status = %+v", st)
	}
	started := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	want := []Process{
		{Name: "db", Label: "db", State: "running", Pid: 100, StartedAt: started, Ports: []int{80, 5432}},
		{Name: "api", Label: "API", State: "crashed", StartedAt: started, Exit: "exit code 1", Restarts: 2, Errors: 1, Args: "--verbose"},
	}
	for i := range st.Processes {
		st.Processes[i].StartedAt = st.Processes[i].StartedAt.UTC()
	}
	if !reflect.DeepEqual(st.Processes, want) {
		t.Errorf("processes =\n%+v\nwant\n%+v", st.Processes, want)
	}
	if !st.Has("api") || st.Has("web") {
		t.Error("Has is wrong")
	}
}

func TestStatusWithoutPorts(t *testing.T) {
	old := listenPorts
	listenPorts = func([]int) ([]ports.Listener, error) { return nil, ports.ErrUnsupported }
	t.Cleanup(func() { listenPorts = old })
	s := listen(t, shortDir(t), newTarget(), 1)
	replies, err := collect(t, s.Path(), Request{Op: OpStatus})
	if err != nil || replies[0].Status.Processes[0].Ports != nil {
		t.Errorf("replies = %+v, err = %v", replies, err)
	}
}

func TestLogs(t *testing.T) {
	target := newTarget()
	target.logs.Append("db", supervisor.Stdout, "d1")
	target.logs.Append("api", supervisor.Stdout, "a1")
	target.logs.AppendLine(supervisor.LogLine{Project: "api", Stream: supervisor.Stderr, Text: "a2 \x1b[31mfailed\x1b[0m", Error: true})
	target.logs.Append("db", supervisor.System, "ready")
	target.logs.AppendLine(supervisor.LogLine{Project: "api", Stream: supervisor.SystemError, Text: "crashed (exit code 1)", Error: true})
	s := listen(t, shortDir(t), target, 1)

	cases := []struct {
		req  Request
		want []string
	}{
		{Request{Op: OpLogs}, []string{"d1", "a1", "a2 \x1b[31mfailed\x1b[0m", "ready", "crashed (exit code 1)"}},
		{Request{Op: OpLogs, Tail: 2}, []string{"ready", "crashed (exit code 1)"}},
		{Request{Op: OpLogs, Names: []string{"db"}}, []string{"d1", "ready"}},
		{Request{Op: OpLogs, Errors: true}, []string{"a2 \x1b[31mfailed\x1b[0m", "crashed (exit code 1)"}},
		{Request{Op: OpLogs, Errors: true, Tail: 1, Names: []string{"api"}}, []string{"crashed (exit code 1)"}},
	}
	for _, c := range cases {
		replies, err := collect(t, s.Path(), c.req)
		if err != nil {
			t.Fatalf("%+v: %v", c.req, err)
		}
		if got := lineTexts(replies); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.req, got, c.want)
		}
	}

	replies, _ := collect(t, s.Path(), Request{Op: OpLogs, Tail: 1})
	l := replies[0].Line
	if l.Project != "api" || !l.Runr || !l.Error || l.Seq != 5 || l.Time.IsZero() {
		t.Errorf("line = %+v", l)
	}

	_, err := collect(t, s.Path(), Request{Op: OpLogs, Names: []string{"db", "web", "x"}})
	if err == nil || err.Error() != "not in this session: web, x" {
		t.Errorf("unknown names: %v", err)
	}
}

func TestLogsFollow(t *testing.T) {
	old := followEvery
	followEvery = time.Millisecond
	t.Cleanup(func() { followEvery = old })
	target := newTarget()
	target.logs.Append("db", supervisor.Stdout, "old")
	s := listen(t, shortDir(t), target, 1)

	got := make(chan string, 100)
	result := make(chan error, 1)
	go func() {
		result <- Do(s.Path(), Request{Op: OpLogs, Names: []string{"api"}, Follow: true}, func(r Reply) error {
			got <- r.Line.Text
			return nil
		})
	}()
	next := func() string {
		t.Helper()
		select {
		case s := <-got:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("no line arrived")
			return ""
		}
	}
	// Lines keep arriving until one shows the follow started.
	appending := make(chan struct{})
	appended := make(chan struct{})
	go func() {
		defer close(appended)
		for {
			select {
			case <-appending:
				return
			default:
			}
			target.logs.Append("api", supervisor.Stdout, "first")
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if l := next(); l != "first" {
		t.Fatalf("got %q", l)
	}
	close(appending)
	<-appended
	target.logs.Append("db", supervisor.Stdout, "not followed")
	target.logs.Append("api", supervisor.Stdout, "last")
	s.Close() // ends the follow, after the lines since the last batch
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("Do: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not end when the server closed")
	}
	var rest []string
	for len(got) > 0 {
		rest = append(rest, <-got)
	}
	if len(rest) == 0 || rest[len(rest)-1] != "last" || slices.Contains(rest, "not followed") {
		t.Errorf("after the first line: %q", rest)
	}
}

func TestDoContextEndsFollow(t *testing.T) {
	target := newTarget()
	target.logs.Append("api", supervisor.Stdout, "old")
	s := listen(t, shortDir(t), target, 1)

	why := errors.New("nobody reads")
	ctx, cancel := context.WithCancelCause(t.Context())
	got := make(chan string, 10)
	result := make(chan error, 1)
	go func() {
		result <- DoContext(ctx, s.Path(), Request{Op: OpLogs, Follow: true}, func(r Reply) error {
			got <- r.Line.Text
			return nil
		})
	}()
	select {
	case l := <-got:
		if l != "old" {
			t.Fatalf("got %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no line arrived")
	}
	cancel(why) // while waiting for new lines
	select {
	case err := <-result:
		if err != why {
			t.Errorf("DoContext = %v, want the cause", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the follow did not end with the context")
	}
}

func TestLogsFollowSkipsNothing(t *testing.T) {
	old := followEvery
	followEvery = time.Millisecond
	t.Cleanup(func() { followEvery = old })
	const n = 500
	target := newTarget()
	target.logs = supervisor.NewLogs(n) // every line is kept, whenever the follow starts
	s := listen(t, shortDir(t), target, 1)

	var mu sync.Mutex
	var texts []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		Do(s.Path(), Request{Op: OpLogs, Follow: true}, func(r Reply) error {
			mu.Lock()
			defer mu.Unlock()
			texts = append(texts, r.Line.Text)
			if len(texts) == n {
				return errStop
			}
			return nil
		})
	}()
	for i := range n {
		target.logs.Append("db", supervisor.Stdout, strings.Repeat("x", i%7))
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		mu.Lock()
		t.Fatalf("got %d of %d lines", len(texts), n)
	}
	for i, s := range texts {
		if s != strings.Repeat("x", i%7) {
			t.Fatalf("line %d = %q", i, s)
		}
	}
}

var errStop = &stopError{}

type stopError struct{}

func (*stopError) Error() string { return "stop" }

func TestActions(t *testing.T) {
	target := newTarget()
	target.procs = append(target.procs, supervisor.ProcessInfo{Name: "broken"})
	s := listen(t, shortDir(t), target, 1)
	for _, req := range []Request{
		{Op: OpStart, Names: []string{"backend", "web"}}, // anything: the supervisor resolves names
		{Op: OpStop, Names: []string{"db"}},
		{Op: OpRestart, Names: []string{"api", "db"}},
	} {
		replies, err := collect(t, s.Path(), req)
		if err != nil || len(replies) != 1 || replies[0].Status == nil || len(replies[0].Status.Processes) != 3 {
			t.Errorf("%+v: %+v, %v", req, replies, err)
		}
	}
	want := []string{"start backend,web", "stop db", "restart api", "restart db"}
	if got := target.recorded(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q", got)
	}

	errs := []struct {
		req  Request
		want string
	}{
		{Request{Op: OpStop, Names: []string{"db", "web"}}, "not in this session: web"},
		{Request{Op: OpRestart}, "no projects to restart"},
		{Request{Op: OpRestart, Names: []string{"broken"}}, "broken: it broke"},
		{Request{Op: OpStart, Names: []string{"broken"}}, "it broke"},
		{Request{Op: OpDown}, "this session can't be stopped from outside"},
	}
	for _, c := range errs {
		if _, err := collect(t, s.Path(), c.req); err == nil || err.Error() != c.want {
			t.Errorf("%+v: err = %v, want %q", c.req, err, c.want)
		}
	}
}

func TestDown(t *testing.T) {
	old := replyTimeout
	replyTimeout = 10 * time.Millisecond // a shutdown may take longer: down waits anyway
	t.Cleanup(func() { replyTimeout = old })
	quits := make(chan bool, 2)
	s, err := Listen(shortDir(t), newTarget(), Info{Pid: 1, Quit: func(force bool) { quits <- force }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	acked := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- Do(s.Path(), Request{Op: OpDown, Force: true}, func(Reply) error {
			close(acked)
			return nil
		})
	}()
	select {
	case force := <-quits:
		if !force {
			t.Error("force not passed on")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Quit not called")
	}
	<-acked
	select {
	case err := <-result:
		t.Fatalf("down returned before the session ended: %v", err)
	case <-time.After(50 * time.Millisecond): // past replyTimeout
	}
	s.Close() // the session ends
	if err := <-result; err != nil {
		t.Errorf("Do: %v", err)
	}
}

func TestBadRequests(t *testing.T) {
	s := listen(t, shortDir(t), newTarget(), 1)
	if _, err := collect(t, s.Path(), Request{Op: "dance"}); err == nil || err.Error() != `unknown request "dance"` {
		t.Errorf("unknown op: %v", err)
	}

	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	json.NewEncoder(c).Encode(Request{V: 99, Op: OpStatus})
	var r Reply
	if err := json.NewDecoder(c).Decode(&r); err != nil || !strings.Contains(r.Error, "version 1 of the control protocol, not 99") {
		t.Errorf("reply = %+v, err = %v", r, err)
	}
}

func TestCloseRemovesSocket(t *testing.T) {
	s, err := Listen(shortDir(t), newTarget(), Info{Pid: 7})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close() // twice is fine
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Errorf("socket still there: %v", err)
	}
}

func TestListenPathTooLong(t *testing.T) {
	dir := filepath.Join(shortDir(t), strings.Repeat("d", 100))
	if _, err := Listen(dir, newTarget(), Info{Pid: 1}); err != ErrPathTooLong {
		t.Errorf("err = %v", err)
	}
}

// deadSocket leaves a socket file nobody listens on in dir, as a runr that
// was killed does. (Windows can't change a socket file's times.)
func deadSocket(t *testing.T, dir, name string) string {
	t.Helper()
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, name)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	return path
}

func TestSessions(t *testing.T) {
	dir := shortDir(t)
	if found, problems := Sessions(dir); found != nil || problems != nil {
		t.Errorf("no dir: %v %v", found, problems)
	}
	b := listen(t, dir, newTarget(), 200)
	a, err := Listen(dir, newTarget(), Info{Pid: 300, Started: time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	dead := deadSocket(t, dir, "100.sock")
	os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600)

	// A new socket nobody listens on may be a session about to listen: kept.
	found, problems := Sessions(dir)
	if len(found) != 2 || found[0].Path != a.Path() || found[1].Path != b.Path() || found[1].Status.Pid != 200 {
		t.Errorf("found = %+v", found)
	}
	if len(problems) != 1 || !strings.HasPrefix(problems[0].Error(), "session 100: ") {
		t.Errorf("problems = %v", problems)
	}
	if _, err := os.Stat(dead); err != nil {
		t.Error("new socket removed")
	}

	// Once it is old enough, it is stale: removed without a word.
	old := staleAfter
	staleAfter = -time.Hour
	t.Cleanup(func() { staleAfter = old })
	found, problems = Sessions(dir)
	if len(found) != 2 || len(problems) != 0 {
		t.Errorf("found = %+v, problems = %v", found, problems)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("stale socket kept")
	}
}

func TestSessionsPathTooLong(t *testing.T) {
	dir := filepath.Join(shortDir(t), strings.Repeat("d", 90))
	_, problems := Sessions(dir)
	if len(problems) != 1 || !errors.Is(problems[0], ErrPathTooLong) || !strings.Contains(problems[0].Error(), "set RUNR_STATE_DIR to a shorter directory") {
		t.Errorf("problems = %v", problems)
	}
}

func TestPick(t *testing.T) {
	session := func(pid int, names ...string) Session {
		st := Status{Pid: pid}
		for _, n := range names {
			st.Processes = append(st.Processes, Process{Name: n})
		}
		return Session{Status: st}
	}
	one := []Session{session(1, "db", "api")}
	two := []Session{session(1, "db", "api"), session(2, "db", "web")}
	cases := []struct {
		sessions []Session
		names    []string
		pid      int
		want     int
		err      string
	}{
		{nil, nil, 0, 0, "no runr session is running"},
		{one, nil, 0, 1, ""},
		{one, []string{"api"}, 0, 1, ""},
		{one, []string{"api", "web", "x"}, 0, 0, "not in the running session: web, x"},
		{two, nil, 0, 0, "2 runr sessions are running (pids 1, 2); choose one with --session"},
		{two, []string{"db"}, 0, 0, "2 runr sessions are running (pids 1, 2); choose one with --session"},
		{two, []string{"web"}, 0, 2, ""},
		{two, []string{"api", "web"}, 0, 0, "no running session has api, web"},
		{two, nil, 2, 2, ""},
		{two, []string{"api"}, 2, 2, ""}, // the server reports the name
		{two, nil, 3, 0, "no runr session with pid 3 is running"},
	}
	for _, c := range cases {
		s, err := Pick(c.sessions, c.names, c.pid)
		var noMatch *NoMatchError
		if errors.As(err, &noMatch) != (strings.HasPrefix(c.err, "not in") || strings.HasPrefix(c.err, "no running session has")) {
			t.Errorf("Pick(%v, %d): %T", c.names, c.pid, err)
		}
		switch {
		case c.err != "" && (err == nil || err.Error() != c.err):
			t.Errorf("Pick(%v, %d) err = %v, want %q", c.names, c.pid, err, c.err)
		case c.err == "" && (err != nil || s.Status.Pid != c.want):
			t.Errorf("Pick(%v, %d) = %d, %v, want %d", c.names, c.pid, s.Status.Pid, err, c.want)
		}
	}
}
