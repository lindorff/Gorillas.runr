package control

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/ports"
	"github.com/Skaanning/runr/internal/supervisor"
)

// Target is the session a Server answers for (a supervisor). Its methods
// are called from the server's goroutines.
type Target interface {
	Processes() []supervisor.ProcessInfo
	Logs() *supervisor.Logs
	Pids() map[string][]int
	Start(names ...string) error
	Stop(name string) error
	Restart(name string) error
	SetDir(name, dir string) error
}

// Info is what a Server says about its session besides the processes.
type Info struct {
	Pid     int
	Started time.Time
	Titles  []string
	Labels  map[string]string // display names by project; others show their name
	Quit    func(force bool)  // stops the session, like q (force: like a second q); nil: can't
	Config  *config.Config    // as the session was started, for attached views; never changed

	// Host marks a session that runs in the background (runr host), shown
	// only by attached views. Dropped, if set, is called when the last
	// attached view went away without detaching: its terminal was closed,
	// or it died. A view that fell too far behind counts as detached.
	Host    bool
	Dropped func()
}

// Timing, variables so tests can shorten them.
var (
	readTimeout = 5 * time.Second        // for a client to send its request
	followEvery = 100 * time.Millisecond // new lines are sent at most this often
	closeGrace  = time.Second            // for handlers to finish once the server closes
	listenPorts = ports.Listeners
)

// Server answers requests for one session until Close.
type Server struct {
	ln        *net.UnixListener
	path      string
	target    Target
	info      Info
	listeners func([]int) ([]ports.Listener, error)
	done      chan struct{} // closed by Close

	mu     sync.Mutex
	conns  map[net.Conn]bool
	closed bool
	views  int  // attached now
	viewed bool // a view has attached
	wg     sync.WaitGroup
}

// Listen starts serving target on <dir>/<info.Pid>.sock, creating dir.
func Listen(dir string, target Target, info Info) (*Server, error) {
	path, err := SocketPath(dir, info.Pid)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	os.Remove(path) // left by an earlier process with this pid that was killed
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	os.Chmod(path, 0o600) // the directory already keeps others out
	s := &Server{
		ln:        ln,
		path:      path,
		target:    target,
		info:      info,
		listeners: listenPorts,
		done:      make(chan struct{}),
		conns:     map[net.Conn]bool{},
	}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Path is the socket's path.
func (s *Server) Path() string { return s.path }

// Viewed reports whether a view has attached to the session.
func (s *Server) Viewed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewed
}

// viewJoined counts an attached view.
func (s *Server) viewJoined() {
	s.mu.Lock()
	s.views++
	s.viewed = true
	s.mu.Unlock()
}

// viewLeft counts a view that is gone, and calls Info.Dropped if it was
// the last one and didn't detach, unless the server is closing.
func (s *Server) viewLeft(detached bool) {
	s.mu.Lock()
	s.views--
	last := s.views == 0 && !detached && !s.closed
	s.mu.Unlock()
	if last && s.info.Dropped != nil {
		s.info.Dropped()
	}
}

// Close stops listening and ends every connection: followers send the lines
// that arrived since their last batch, within a second. It waits for the
// handlers, and removes the socket.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	s.ln.Close()
	for c := range s.conns {
		c.SetDeadline(time.Now().Add(closeGrace))
	}
	s.mu.Unlock()
	s.wg.Wait()
	os.Remove(s.path)
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return // closed
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			return
		}
		s.conns[c] = true
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.handle(c)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			c.Close()
		}()
	}
}

// handle answers the one request on c.
func (s *Server) handle(c net.Conn) {
	c.SetReadDeadline(time.Now().Add(readTimeout))
	var req Request
	dec := json.NewDecoder(c)
	if err := dec.Decode(&req); err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	enc := json.NewEncoder(c)
	if req.V != Version {
		enc.Encode(Reply{Error: fmt.Sprintf("this runr session uses version %d of the control protocol, not %d: use the runr it was started with", Version, req.V)})
		return
	}
	switch req.Op {
	case OpStatus:
		st := s.status()
		enc.Encode(Reply{Status: &st})
	case OpLogs:
		if err := s.checkNames(req.Names); err != nil {
			enc.Encode(Reply{Error: err.Error()})
			return
		}
		// Anything the client sends after its request, or its closing the
		// connection, ends a follow.
		gone := make(chan struct{})
		go func() {
			io.Copy(io.Discard, io.MultiReader(dec.Buffered(), c))
			close(gone)
		}()
		s.logs(enc, req, gone)
	case OpStart, OpStop, OpRestart:
		if err := s.act(req); err != nil {
			enc.Encode(Reply{Error: err.Error()})
			return
		}
		// The states already show the change: the supervisor makes it
		// before it returns.
		st := s.status()
		enc.Encode(Reply{Status: &st})
	case OpAttach:
		s.attach(c, dec)
	case OpDown:
		if s.info.Quit == nil {
			enc.Encode(Reply{Error: "this session can't be stopped from outside"})
			return
		}
		s.info.Quit(req.Force)
		enc.Encode(Reply{})
		<-s.done // the connection ends with the session
	default:
		enc.Encode(Reply{Error: "unknown request " + strconv.Quote(req.Op)})
	}
}

// act starts, stops or restarts the projects of req.
func (s *Server) act(req Request) error {
	if len(req.Names) == 0 {
		return fmt.Errorf("no projects to %s", req.Op)
	}
	if req.Op != OpStop {
		for _, name := range slices.Sorted(maps.Keys(req.Dirs)) {
			if err := s.target.SetDir(name, req.Dirs[name]); err != nil {
				return err
			}
		}
	}
	if req.Op == OpStart {
		return s.target.Start(req.Names...) // also groups, and projects not in the session yet
	}
	if err := s.checkNames(req.Names); err != nil {
		return err
	}
	for _, n := range req.Names {
		var err error
		if req.Op == OpStop {
			err = s.target.Stop(n)
		} else {
			err = s.target.Restart(n)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	return nil
}

// status describes the session now.
func (s *Server) status() Status {
	infos := s.target.Processes()
	listening := s.ports(s.target.Pids())
	s.mu.Lock()
	views := s.views
	s.mu.Unlock()
	st := Status{Pid: s.info.Pid, Started: s.info.Started, Titles: s.info.Titles, Host: s.info.Host, Attached: views, Processes: []Process{}}
	for _, in := range infos {
		p := Process{
			Name:      in.Name,
			Label:     in.Name,
			State:     in.State.String(),
			StartedAt: in.StartedAt,
			Restarts:  in.Restarts,
			Errors:    in.Errors,
			Args:      in.Args,
			Dir:       in.Dir,
			Ports:     listening[in.Name],
		}
		if l := s.info.Labels[in.Name]; l != "" {
			p.Label = l
		}
		switch in.State {
		case supervisor.Starting, supervisor.Running, supervisor.Stopping:
			p.Pid = in.Pid
		case supervisor.Stopped, supervisor.Exited, supervisor.Crashed:
			p.Exit = in.Exit.String()
		}
		st.Processes = append(st.Processes, p)
	}
	return st
}

// ports returns the listening ports of the process trees in pids, by
// project, or nil when they can't be read.
func (s *Server) ports(pids map[string][]int) map[string][]int {
	owner := map[int]string{}
	var all []int
	for name, tree := range pids {
		for _, pid := range tree {
			owner[pid] = name
			all = append(all, pid)
		}
	}
	if len(all) == 0 {
		return nil
	}
	ls, err := s.listeners(all)
	if err != nil {
		return nil
	}
	out := map[string][]int{}
	for _, l := range ls {
		if name, ok := owner[l.Pid]; ok && !slices.Contains(out[name], l.Port) {
			out[name] = append(out[name], l.Port)
		}
	}
	for _, ps := range out {
		slices.Sort(ps)
	}
	return out
}

// checkNames reports names that aren't projects of the session.
func (s *Server) checkNames(names []string) error {
	known := map[string]bool{}
	for _, in := range s.target.Processes() {
		known[in.Name] = true
	}
	var unknown []string
	for _, n := range names {
		if !known[n] {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("not in this session: %s", strings.Join(unknown, ", "))
	}
	return nil
}

// logs sends the buffered lines req asks for and, with req.Follow, new ones
// until the client is gone or the server closes.
func (s *Server) logs(enc *json.Encoder, req Request, gone <-chan struct{}) {
	logs := s.target.Logs()
	var changed <-chan struct{}
	if req.Follow {
		changed = logs.Changed() // before the snapshot, so no line is missed
	}
	lines := logs.Combined(req.Names...)
	var last uint64
	if n := len(lines); n > 0 {
		last = lines[n-1].Seq
	}
	if req.Errors {
		lines = errorLines(lines)
	}
	if req.Tail > 0 && len(lines) > req.Tail {
		lines = lines[len(lines)-req.Tail:]
	}
	if !send(enc, lines) || !req.Follow {
		return
	}
	// next sends the lines that arrived since the last batch.
	next := func() bool {
		lines := logs.Since(last, req.Names...)
		if len(lines) == 0 {
			return true
		}
		last = lines[len(lines)-1].Seq
		if req.Errors {
			lines = errorLines(lines)
		}
		return send(enc, lines)
	}
	for {
		select {
		case <-changed:
		case <-gone:
			return
		case <-s.done:
			next()
			return
		}
		// Let a burst of lines collect.
		select {
		case <-time.After(followEvery):
		case <-gone:
			return
		case <-s.done:
			next()
			return
		}
		changed = logs.Changed()
		if !next() {
			return
		}
	}
}

// send writes lines, reporting false once writing fails.
func send(enc *json.Encoder, lines []supervisor.LogLine) bool {
	for _, l := range lines {
		line := Line{Seq: l.Seq, Project: l.Project, Time: l.Time, Runr: l.Stream.Runr(), Error: l.Error, Text: l.Text}
		if enc.Encode(Reply{Line: &line}) != nil {
			return false
		}
	}
	return true
}

func errorLines(lines []supervisor.LogLine) []supervisor.LogLine {
	var out []supervisor.LogLine
	for _, l := range lines {
		if l.Error {
			out = append(out, l)
		}
	}
	return out
}
