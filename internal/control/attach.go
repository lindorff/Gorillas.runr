package control

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/proc"
	"github.com/Skaanning/runr/internal/supervisor"
)

// OpAttach connects a view: the session sends a Snapshot, then every
// event, and takes Commands on the same connection until the view leaves.
const OpAttach = "attach"

// AttachTarget is a session a view can attach to: a supervisor, or what
// wraps one.
type AttachTarget interface {
	Target
	Subscribe() (*supervisor.Subscription, map[string]string)
	SetArgs(name, args string) error
	StopAll() <-chan struct{}
	KillAll()
	Note(name string, lines ...string) error
	RememberPorts(ports map[string][]int)
	Resize(cols, rows int) error
	Input(name string, b []byte) error
}

// Snapshot is what an attaching view gets first: everything it shows.
type Snapshot struct {
	Pid       int                  `json:"pid"`
	Started   time.Time            `json:"started"`
	Titles    []string             `json:"titles"`
	Config    *config.Config       `json:"config"` // as the session was started
	Processes []ProcInfo           `json:"processes"`
	Lines     []supervisor.LogLine `json:"lines"`   // every buffered line, in order
	Pending   map[string]string    `json:"pending"` // unfinished lines by project
}

// ProcInfo is a supervisor.ProcessInfo on the wire.
type ProcInfo struct {
	Name      string           `json:"name"`
	State     supervisor.State `json:"state"`
	Pid       int              `json:"pid,omitempty"`
	StartedAt time.Time        `json:"started_at,omitzero"`
	Exit      ExitStatus       `json:"exit"`
	Restarts  int              `json:"restarts,omitempty"`
	Args      string           `json:"args,omitempty"`
	Dir       string           `json:"dir,omitempty"`
	Errors    int              `json:"errors,omitempty"`
}

// ExitStatus is a proc.ExitStatus on the wire.
type ExitStatus struct {
	Code      int    `json:"code,omitempty"`
	Signal    string `json:"signal,omitempty"`
	Err       string `json:"err,omitempty"`
	Leftovers bool   `json:"leftovers,omitempty"`
}

func procInfo(in supervisor.ProcessInfo) ProcInfo {
	e := ExitStatus{Code: in.Exit.Code, Signal: in.Exit.Signal, Leftovers: in.Exit.Leftovers}
	if in.Exit.Err != nil {
		e.Err = in.Exit.Err.Error()
	}
	return ProcInfo{Name: in.Name, State: in.State, Pid: in.Pid, StartedAt: in.StartedAt, Exit: e, Restarts: in.Restarts, Args: in.Args, Dir: in.Dir, Errors: in.Errors}
}

func (p ProcInfo) info() supervisor.ProcessInfo {
	e := proc.ExitStatus{Code: p.Exit.Code, Signal: p.Exit.Signal, Leftovers: p.Exit.Leftovers}
	if p.Exit.Err != "" {
		e.Err = errors.New(p.Exit.Err)
	}
	return supervisor.ProcessInfo{Name: p.Name, State: p.State, Pid: p.Pid, StartedAt: p.StartedAt, Exit: e, Restarts: p.Restarts, Args: p.Args, Dir: p.Dir, Errors: p.Errors}
}

// Event is a supervisor.Event on the wire: one of its fields is set.
type Event struct {
	State   *ProcInfo           `json:"state,omitempty"`
	Line    *supervisor.LogLine `json:"line,omitempty"`
	Pending *Text               `json:"pending,omitempty"`
	Warning *Text               `json:"warning,omitempty"`
}

// Text is a project's text: a pending line or a warning.
type Text struct {
	Project string `json:"project"`
	Text    string `json:"text,omitempty"`
}

func wireEvent(e supervisor.Event) Event {
	switch e := e.(type) {
	case supervisor.StateEvent:
		p := procInfo(e.Info)
		return Event{State: &p}
	case supervisor.LineEvent:
		return Event{Line: &e.Line}
	case supervisor.PendingEvent:
		return Event{Pending: &Text{e.Project, e.Text}}
	case supervisor.WarningEvent:
		return Event{Warning: &Text{e.Project, e.Text}}
	}
	return Event{}
}

func (e Event) event() (supervisor.Event, bool) {
	switch {
	case e.State != nil:
		return supervisor.StateEvent{Info: e.State.info()}, true
	case e.Line != nil:
		return supervisor.LineEvent{Line: *e.Line}, true
	case e.Pending != nil:
		return supervisor.PendingEvent{Project: e.Pending.Project, Text: e.Pending.Text}, true
	case e.Warning != nil:
		return supervisor.WarningEvent{Project: e.Warning.Project, Text: e.Warning.Text}, true
	}
	return nil, false
}

// Commands an attached view sends.
const (
	CmdStart         = "start"
	CmdStop          = "stop"
	CmdRestart       = "restart"
	CmdSetArgs       = "set-args"
	CmdStopAll       = "stop-all" // stop the session, as q does
	CmdKillAll       = "kill-all" // force it, as a second q does
	CmdNote          = "note"
	CmdRememberPorts = "remember-ports"
	CmdResize        = "resize"
	CmdInput         = "input"
	CmdPids          = "pids"
	CmdDetach        = "detach" // leave, the session running on
)

// Command is a request of an attached view; the session answers with a
// Reply of the same ID.
type Command struct {
	ID    int              `json:"id"`
	Op    string           `json:"op"`
	Names []string         `json:"names,omitempty"`
	Name  string           `json:"name,omitempty"`
	Args  string           `json:"args,omitempty"`
	Lines []string         `json:"lines,omitempty"`
	Ports map[string][]int `json:"ports,omitempty"`
	Cols  int              `json:"cols,omitempty"`
	Rows  int              `json:"rows,omitempty"`
	Data  []byte           `json:"data,omitempty"`
}

// CommandReply answers a Command.
type CommandReply struct {
	ID    int              `json:"id"`
	Error string           `json:"error,omitempty"`
	Pids  map[string][]int `json:"pids,omitempty"`
}

// Frame is one line the session sends to an attached view: one field is
// set. Error ends the connection (e.g. the view fell too far behind).
type Frame struct {
	Snapshot *Snapshot     `json:"snapshot,omitempty"`
	Event    *Event        `json:"event,omitempty"`
	Reply    *CommandReply `json:"reply,omitempty"`
	Error    string        `json:"error,omitempty"`
}

// ErrLagged ends a view that fell too far behind the session's output.
var ErrLagged = errors.New("the view fell too far behind the session's output; runr attach to see it again")

// frameWriter writes frames, buffered, from the event loop and the command
// handler.
type frameWriter struct {
	mu  sync.Mutex
	bw  *bufio.Writer
	enc *json.Encoder
}

func newFrameWriter(w io.Writer) *frameWriter {
	bw := bufio.NewWriterSize(w, 64<<10)
	return &frameWriter{bw: bw, enc: json.NewEncoder(bw)}
}

func (w *frameWriter) write(f Frame, flush bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(f); err != nil {
		return err
	}
	if flush {
		return w.bw.Flush()
	}
	return nil
}

func (w *frameWriter) flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bw.Flush()
}

// attach serves a view on c until it leaves, the session ends or the
// server closes.
func (s *Server) attach(c net.Conn, dec *json.Decoder) {
	w := newFrameWriter(c)
	t, ok := s.target.(AttachTarget)
	if !ok {
		w.write(Frame{Error: "this session can't be attached to"}, true)
		return
	}
	// Subscribed first: what happens while the snapshot is taken is in
	// both, and the view skips the lines it has.
	sub, pending := t.Subscribe()
	defer sub.Close()
	snap := Snapshot{
		Pid: s.info.Pid, Started: s.info.Started, Titles: s.info.Titles, Config: s.info.Config,
		Lines: t.Logs().Combined(), Pending: pending,
	}
	for _, in := range t.Processes() {
		snap.Processes = append(snap.Processes, procInfo(in))
	}
	if w.write(Frame{Snapshot: &snap}, true) != nil {
		return
	}
	// A view that leaves without detaching (its terminal was closed, or
	// it died) may stop the session (Info.Dropped).
	var detached atomic.Bool
	s.viewJoined()
	defer func() { s.viewLeft(detached.Load()) }()

	gone := make(chan struct{}) // the view left (or detached)
	go func() {
		defer close(gone)
		for {
			var cmd Command
			if err := dec.Decode(&cmd); err != nil {
				return
			}
			if cmd.Op == CmdDetach {
				detached.Store(true)
				w.write(Frame{Reply: &CommandReply{ID: cmd.ID}}, true)
				return
			}
			reply := s.command(t, cmd)
			if w.write(Frame{Reply: &reply}, true) != nil {
				return
			}
		}
	}()

	events := sub.Events()
	for {
		select {
		case e, ok := <-events:
			if !ok {
				if sub.Lagged() {
					detached.Store(true) // it can attach again
					w.write(Frame{Error: ErrLagged.Error()}, true)
				}
				return
			}
			if !sendEvents(w, e, events) {
				return
			}
		case <-gone:
			return
		case <-s.done:
			// The session has ended: its last events (e.g. the final
			// states) are still on their way, until the subscription
			// closes with the supervisor.
			timeout := time.After(closeGrace)
			for {
				select {
				case e, ok := <-events:
					if !ok || !sendEvents(w, e, events) {
						return
					}
				case <-gone:
					return
				case <-timeout:
					return
				}
			}
		}
	}
}

// sendEvents writes e and everything queued after it in one write,
// reporting false once writing fails.
func sendEvents(w *frameWriter, e supervisor.Event, events <-chan supervisor.Event) bool {
	for ok := true; ok; {
		ev := wireEvent(e)
		if w.write(Frame{Event: &ev}, false) != nil {
			return false
		}
		select {
		case e, ok = <-events:
		default:
			ok = false
		}
	}
	return w.flush() == nil
}

// command carries out a view's command.
func (s *Server) command(t AttachTarget, cmd Command) CommandReply {
	reply := CommandReply{ID: cmd.ID}
	var err error
	switch cmd.Op {
	case CmdStart:
		err = t.Start(cmd.Names...)
	case CmdStop:
		err = t.Stop(cmd.Name)
	case CmdRestart:
		err = t.Restart(cmd.Name)
	case CmdSetArgs:
		err = t.SetArgs(cmd.Name, cmd.Args)
	case CmdStopAll, CmdKillAll:
		// The session's own way of stopping, so that it ends as with q in
		// its own terminal; without one, just its processes.
		force := cmd.Op == CmdKillAll
		switch {
		case s.info.Quit != nil:
			s.info.Quit(force)
		case force:
			t.KillAll()
		default:
			t.StopAll()
		}
	case CmdNote:
		err = t.Note(cmd.Name, cmd.Lines...)
	case CmdRememberPorts:
		t.RememberPorts(cmd.Ports)
	case CmdResize:
		err = t.Resize(cmd.Cols, cmd.Rows)
	case CmdInput:
		err = t.Input(cmd.Name, cmd.Data)
	case CmdPids:
		reply.Pids = t.Pids()
	default:
		err = fmt.Errorf("unknown command %q", cmd.Op)
	}
	if err != nil {
		reply.Error = err.Error()
	}
	return reply
}

// ErrSessionEnded is reported by Remote.Lost when the session ended (or
// its connection broke) without the view stopping or leaving it.
var ErrSessionEnded = errors.New("the session ended")

// commandTimeout is how long a Remote waits for a command's reply. A
// variable so tests can shorten it.
var commandTimeout = 10 * time.Second

// Remote is a session in another process, attached over its socket: it
// has the methods of tui.Session, so the running view can show it.
type Remote struct {
	conn   net.Conn
	sendMu sync.Mutex
	enc    *json.Encoder // commands, unbuffered
	snap   Snapshot
	logs   *supervisor.Logs
	q      *queue
	ended  chan struct{} // closed when the connection is gone

	mu      sync.Mutex
	procs   map[string]supervisor.ProcessInfo
	order   []string
	nextID  int
	waiting map[int]chan CommandReply
	err     error // why the connection ended, if not by the view
	leaving bool  // the view detached, or stopped the session
	snapSeq uint64
}

// Attach connects to the session at path as a view.
func Attach(path string) (*Remote, error) {
	c, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return nil, &dialError{err}
	}
	if err := json.NewEncoder(c).Encode(Request{V: Version, Op: OpAttach}); err != nil {
		c.Close()
		return nil, err
	}
	dec := json.NewDecoder(bufio.NewReaderSize(c, 64<<10))
	c.SetReadDeadline(time.Now().Add(replyTimeout))
	// The snapshot, or an error (e.g. another protocol version).
	var frame Frame
	if err := dec.Decode(&frame); err != nil {
		c.Close()
		return nil, err
	}
	c.SetReadDeadline(time.Time{})
	switch {
	case frame.Error != "":
		c.Close()
		return nil, errors.New(frame.Error)
	case frame.Snapshot == nil:
		c.Close()
		return nil, errors.New("the session sent no snapshot")
	}
	snap := *frame.Snapshot
	if snap.Config == nil {
		snap.Config = config.New()
	}
	r := &Remote{
		conn:    c,
		enc:     json.NewEncoder(c),
		snap:    snap,
		logs:    supervisor.NewLogs(snap.Config.Settings.ScrollbackLines()),
		q:       newQueue(),
		ended:   make(chan struct{}),
		procs:   map[string]supervisor.ProcessInfo{},
		waiting: map[int]chan CommandReply{},
	}
	for _, p := range snap.Processes {
		r.procs[p.Name] = p.info()
		r.order = append(r.order, p.Name)
	}
	// The view builds its log from events: the buffered lines come first.
	for _, l := range snap.Lines {
		r.logs.Insert(l)
		r.q.push(supervisor.LineEvent{Line: l})
		r.snapSeq = max(r.snapSeq, l.Seq)
	}
	for project, text := range snap.Pending {
		r.q.push(supervisor.PendingEvent{Project: project, Text: text})
	}
	go r.read(dec)
	return r, nil
}

// Pid is the pid of the session's runr.
func (r *Remote) Pid() int { return r.snap.Pid }

// Snapshot is what the session sent when the view attached (its config,
// titles and pid).
func (r *Remote) Snapshot() Snapshot { return r.snap }

// read applies the session's frames until the connection ends.
func (r *Remote) read(dec *json.Decoder) {
	var err error
	for {
		var f Frame
		if err = dec.Decode(&f); err != nil {
			break
		}
		switch {
		case f.Error != "":
			err = errors.New(f.Error)
		case f.Reply != nil:
			r.mu.Lock()
			ch := r.waiting[f.Reply.ID]
			delete(r.waiting, f.Reply.ID)
			r.mu.Unlock()
			if ch != nil {
				ch <- *f.Reply
			}
			continue
		case f.Event != nil:
			r.apply(*f.Event)
			continue
		default:
			continue
		}
		break
	}
	r.mu.Lock()
	if !r.leaving {
		r.err = ErrSessionEnded
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			r.err = fmt.Errorf("%w: %v", ErrSessionEnded, err)
		}
	}
	for id, ch := range r.waiting {
		close(ch)
		delete(r.waiting, id)
	}
	r.mu.Unlock()
	r.conn.Close()
	close(r.ended)
	r.q.close()
}

// apply updates the copy of the session's state, and passes the event on.
func (r *Remote) apply(ev Event) {
	e, ok := ev.event()
	if !ok {
		return
	}
	switch e := e.(type) {
	case supervisor.StateEvent:
		r.mu.Lock()
		if _, known := r.procs[e.Info.Name]; !known {
			r.order = append(r.order, e.Info.Name)
		}
		r.procs[e.Info.Name] = e.Info
		r.mu.Unlock()
	case supervisor.LineEvent:
		if e.Line.Seq <= r.snapSeq {
			return // in the snapshot already
		}
		r.logs.Insert(e.Line)
	}
	r.q.push(e)
}

// call sends a command and waits for its reply.
func (r *Remote) call(cmd Command) (CommandReply, error) {
	r.mu.Lock()
	select {
	case <-r.ended:
		r.mu.Unlock()
		return CommandReply{}, ErrSessionEnded
	default:
	}
	r.nextID++
	cmd.ID = r.nextID
	ch := make(chan CommandReply, 1)
	r.waiting[cmd.ID] = ch
	r.mu.Unlock()
	if err := r.send(cmd); err != nil {
		return CommandReply{}, err
	}
	select {
	case reply, ok := <-ch:
		if !ok {
			return CommandReply{}, ErrSessionEnded
		}
		if reply.Error != "" {
			return reply, errors.New(reply.Error)
		}
		return reply, nil
	case <-time.After(commandTimeout):
		r.mu.Lock()
		delete(r.waiting, cmd.ID)
		r.mu.Unlock()
		return CommandReply{}, fmt.Errorf("the session didn't answer %s", cmd.Op)
	}
}

// send writes a command to the session.
func (r *Remote) send(cmd Command) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	return r.enc.Encode(cmd)
}

func (r *Remote) do(cmd Command) error {
	_, err := r.call(cmd)
	return err
}

// Processes returns the session's projects, in its order.
func (r *Remote) Processes() []supervisor.ProcessInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]supervisor.ProcessInfo, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.procs[n])
	}
	return out
}

// Events delivers the buffered lines, then the session's events, until
// the connection ends.
func (r *Remote) Events() <-chan supervisor.Event { return r.q.out }

// Logs is a copy of the session's log store, kept up to date.
func (r *Remote) Logs() *supervisor.Logs { return r.logs }

func (r *Remote) Start(names ...string) error {
	return r.do(Command{Op: CmdStart, Names: slices.Clone(names)})
}
func (r *Remote) Stop(name string) error    { return r.do(Command{Op: CmdStop, Name: name}) }
func (r *Remote) Restart(name string) error { return r.do(Command{Op: CmdRestart, Name: name}) }
func (r *Remote) SetArgs(name, args string) error {
	return r.do(Command{Op: CmdSetArgs, Name: name, Args: args})
}
func (r *Remote) Note(name string, lines ...string) error {
	return r.do(Command{Op: CmdNote, Name: name, Lines: lines})
}
func (r *Remote) RememberPorts(ports map[string][]int) {
	r.do(Command{Op: CmdRememberPorts, Ports: ports})
}
func (r *Remote) Resize(cols, rows int) error {
	return r.do(Command{Op: CmdResize, Cols: cols, Rows: rows})
}
func (r *Remote) Input(name string, b []byte) error {
	return r.do(Command{Op: CmdInput, Name: name, Data: b})
}

// Pids returns the process trees of the session's live processes.
func (r *Remote) Pids() map[string][]int {
	reply, err := r.call(Command{Op: CmdPids})
	if err != nil {
		return nil
	}
	return reply.Pids
}

// StopAll stops the session the way q does in its own terminal; the
// returned channel is closed once it has ended.
func (r *Remote) StopAll() <-chan struct{} {
	r.leave()
	go r.do(Command{Op: CmdStopAll})
	return r.ended
}

// KillAll forces the session to stop, as a second q does.
func (r *Remote) KillAll() {
	r.leave()
	go r.do(Command{Op: CmdKillAll})
}

// Detach leaves the session running and ends the connection.
func (r *Remote) Detach() error {
	r.leave()
	err := r.do(Command{Op: CmdDetach})
	r.conn.Close()
	<-r.ended
	return err
}

func (r *Remote) leave() {
	r.mu.Lock()
	r.leaving = true
	r.mu.Unlock()
}

// Lost reports why the session ended without the view stopping or leaving
// it, or nil.
func (r *Remote) Lost() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Drop ends the connection without detaching, as if the view's terminal
// was closed: the view ends with reason (see Lost).
func (r *Remote) Drop(reason error) {
	r.mu.Lock()
	if !r.leaving {
		r.leaving, r.err = true, reason
	}
	r.mu.Unlock()
	r.conn.Close()
	<-r.ended
}

// Close ends the connection, leaving the session as it is.
func (r *Remote) Close() {
	r.leave()
	r.conn.Close()
	<-r.ended
}

// queue is an unbounded, ordered event queue like the supervisor's: push
// never blocks.
type queue struct {
	mu     sync.Mutex
	items  []supervisor.Event
	closed bool
	notify chan struct{}
	out    chan supervisor.Event
}

func newQueue() *queue {
	q := &queue{notify: make(chan struct{}, 1), out: make(chan supervisor.Event, 256)}
	go q.pump()
	return q
}

func (q *queue) push(e supervisor.Event) {
	q.mu.Lock()
	q.items = append(q.items, e)
	q.mu.Unlock()
	q.wake()
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wake()
}

func (q *queue) wake() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *queue) pump() {
	for range q.notify {
		for {
			q.mu.Lock()
			items, closed := q.items, q.closed
			q.items = nil
			q.mu.Unlock()
			for _, e := range items {
				q.out <- e
			}
			if closed && len(items) == 0 {
				close(q.out)
				return
			}
			if len(items) == 0 {
				break
			}
		}
	}
}
