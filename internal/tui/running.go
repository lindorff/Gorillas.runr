package tui

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/supervisor"
)

// Detacher is a session the view can leave running (d): one in another
// process, attached over its socket.
type Detacher interface {
	Detach() error
}

// ErrDetached is returned by Run when the user detached from the session.
var ErrDetached = errors.New("detached from the session")

// lostSession reports why a session ended without the view stopping it, or
// nil; its Events are closed by then.
type lostSession interface {
	Lost() error
}

// Session is the part of the supervisor the running view uses.
type Session interface {
	Processes() []supervisor.ProcessInfo
	Events() <-chan supervisor.Event
	Logs() *supervisor.Logs
	Start(names ...string) error
	Stop(name string) error
	Restart(name string) error
	SetArgs(name, args string) error // extra arguments from the next start on
	StopAll() <-chan struct{}
	KillAll()
	Note(name string, lines ...string) error
	Pids() map[string][]int // process trees of the live processes, by project
	RememberPorts(ports map[string][]int)
	Resize(cols, rows int) error       // of the pseudo-terminals (pty = true); 0 keeps one
	Input(name string, b []byte) error // typed into a project's pseudo-terminal
}

const (
	frameInterval  = time.Second / 30 // minimum time between event batches
	maxBatch       = 1 << 16          // events per batch, so up to ~2M lines/s
	drainIdle      = time.Millisecond // a batch ends when no event arrives for this long
	maxPrefixWidth = 16               // widest label in front of combined log lines
	minWidth       = 30
	minHeight      = 5
	paneMinWidth   = 50 // below this, or paneMinHeight, the view has no panes
	paneMinHeight  = 10
)

type (
	eventsMsg struct {
		events []supervisor.Event
		closed bool
	}
	stoppedMsg   struct{}          // StopAll finished
	interruptMsg struct{}          // a signal, treated like Ctrl+C
	hangupMsg    struct{}          // the terminal went away (SIGHUP, SIGTERM)
	uiTickMsg    struct{ gen int } // time to redraw spinners and uptimes
)

const (
	animateEvery = 100 * time.Millisecond // tick while a spinner turns
	uptimeEvery  = time.Second            // tick while uptimes count
	statusWidth  = 6                      // minimum width of the uptime column
)

// sidebarEntry is a project shown in the sidebar.
type sidebarEntry struct {
	info    supervisor.ProcessInfo
	label   string
	color   string         // the project's color, configured or a default
	style   lipgloss.Style // label color, from color
	rate    outputRate     // the project's output lines per second
	oneShot bool           // ready = "exit": shows "done" once it exited
}

// runningModel is the running view: a sidebar of processes next to either
// their combined log output or the output of the selected process.
type runningModel struct {
	sess   Session
	cfg    *config.Config
	st     *styles
	titles []string // the groups and projects started, for the header

	entries    []*sidebarEntry
	byName     map[string]*sidebarEntry
	labelWidth int
	selected   int // index into entries, then idle
	sideTop    int // first sidebar entry shown

	showAll   bool     // the sidebar lists the projects not started too
	idle      []string // registered projects not in the session, sorted; empty unless showAll
	idleWidth int      // widest label in idle
	idleTop   int      // first of idle shown

	combined  *logView // always kept up to date, so switching back is instant
	single    *logView // the selected process's output; nil in the combined view
	singleFor string   // project shown in single
	snapSeq   uint64   // newest line in the snapshot single was built from
	lastBatch time.Time
	notice    string  // result of the last action, shown until the next key
	toasts    []toast // crashes and blocks, oldest first

	search     search // applies to both views
	prevSearch search // restored when editing is cancelled
	editing    bool   // the search input is open
	argsFor    string // project whose extra arguments the input edits (R), or ""

	// pending is the unfinished current line of each project on a
	// pseudo-terminal, shown after its lines (see logView.tail).
	pending map[string]string
	// attached is the project keys are typed into (i), or ""; ctrl+q
	// detaches, back to the combined view if attaching left it.
	attached     string
	attachedFrom bool // the combined view was shown before attaching
	input        textinput.Model
	matchInfo    string // position of the current match, e.g. "3/17"
	errorSeq     uint64 // Seq of the error line jumped to (e), 0 if none
	errorInfo    string // its position, e.g. "2/7"

	picker  *picker   // open picker (start another, branches), or nil
	confirm *question // open question in the help line, or nil

	git     *gitTracker // nil: git is not installed
	gitOps  *gitActions
	ports   *portTracker
	saveDir string // where w saves logs; "" for the current directory

	width, height int
	now           func() time.Time // for the clock, uptimes and spinners
	tickEvery     time.Duration    // interval of the scheduled uiTickMsg, 0 if none
	tickGen       int              // generation of the scheduled uiTickMsg
	termCols      int              // width last given to sess.Resize

	stopping bool
	forced   bool
	detached bool // the user left the session running (d, D)
	finished bool // the session stopped (q, Q, Ctrl+C)
	// toLauncher: after detaching or stopping, show the launcher instead
	// of exiting runr (D, Q). Only with hasLauncher (the app).
	toLauncher  bool
	hasLauncher bool
	lost        error // the session ended without the view stopping it
}

func newRunningModel(sess Session, cfg *config.Config, names []string, st *styles) *runningModel {
	m := &runningModel{sess: sess, cfg: cfg, st: st, titles: names, byName: map[string]*sidebarEntry{}, pending: map[string]string{}, now: time.Now}
	m.ports = newPortTracker(sess.Pids)
	m.gitOps = newGitActions(runningGit{m}, st, cfg)
	m.showAll = cfg.Settings.ShowAll()
	for _, info := range sess.Processes() {
		m.entry(info.Name).info = info
	}
	m.refreshIdle()
	limit := cfg.Settings.ScrollbackLines() * max(len(m.entries), 1)
	m.combined = newLogView(limit, m.formatCombined)

	m.input = textinput.New()
	m.input.Prompt = ""
	styles := textinput.DefaultStyles(true)
	styles.Cursor.Blink = false
	m.input.SetStyles(styles)
	return m
}

// entry returns the sidebar entry for a project, adding it if needed.
func (m *runningModel) entry(name string) *sidebarEntry {
	if e := m.byName[name]; e != nil {
		return e
	}
	e := &sidebarEntry{info: supervisor.ProcessInfo{Name: name}, label: name}
	if p := m.cfg.Projects[name]; p != nil {
		e.label = p.Label(name)
		e.color = m.cfg.ColorOf(name)
		e.style = lipgloss.NewStyle().Foreground(m.st.color(e.color))
		e.oneShot = p.OneShot()
	}
	m.entries = append(m.entries, e)
	m.byName[name] = e
	m.labelWidth = max(m.labelWidth, ansi.StringWidth(e.label))
	if m.combined != nil { // added after startup
		m.combined.limit += m.cfg.Settings.ScrollbackLines()
		m.combined.invalidate()
		m.refreshIdle()
		m.layout()
	}
	return e
}

// restyle resolves the projects' colors again after the palette changed.
func (m *runningModel) restyle() {
	for _, e := range m.entries {
		if e.color != "" {
			e.style = lipgloss.NewStyle().Foreground(m.st.color(e.color))
		}
	}
	m.combined.invalidate()
	if m.single != nil {
		m.single.invalidate()
	}
}

// view returns the log view on screen.
func (m *runningModel) view() *logView {
	if m.single != nil {
		return m.single
	}
	return m.combined
}

// showSingle switches to the output of one project. The view starts from
// the supervisor's buffer; events for lines already in it are skipped.
func (m *runningModel) showSingle(name string) {
	lines := m.sess.Logs().Lines(name)
	m.single = newLogView(m.cfg.Settings.ScrollbackLines(), m.formatSingle)
	m.single.append(lines...)
	m.singleFor, m.snapSeq = name, 0
	for _, l := range lines {
		m.snapSeq = max(m.snapSeq, l.Seq)
	}
	m.updateTails()
	m.layout()
}

// updateTails shows the unfinished lines in the views: every project's in
// the combined view, in session order, and the one of the single view's.
// The attached project's ends in a cursor.
func (m *runningModel) updateTails() {
	tail := func(name string) []supervisor.LogLine {
		text, ok := m.pending[name]
		if name == m.attached {
			text += "\x1b[7m \x1b[27m"
		} else if !ok {
			return nil
		}
		return []supervisor.LogLine{{Project: name, Stream: supervisor.Stdout, Text: text}}
	}
	var all []supervisor.LogLine
	for _, e := range m.entries {
		all = append(all, tail(e.info.Name)...)
	}
	if m.combined != nil {
		m.combined.setTail(all)
	}
	if m.single != nil {
		m.single.setTail(tail(m.singleFor))
	}
}

// attach starts typing into the selected project, in its own view: keys go
// to the program until ctrl+q.
func (m *runningModel) attach() {
	name := m.selectedName()
	if name == "" || m.stopping {
		return
	}
	label := m.labelOf(name)
	e := m.byName[name]
	switch {
	case m.cfg.Projects[name] == nil || !m.cfg.Projects[name].PTY:
		m.notice = label + " doesn't take input: set pty = true"
		return
	case m.selectedIdle() != "" || e == nil || !e.info.State.Alive():
		m.notice = label + " isn't running"
		return
	}
	m.attachedFrom = m.single == nil
	if m.single == nil || m.singleFor != name {
		m.showSingle(name)
	}
	m.attached = name
	m.single.scrollBottom()
	m.updateTails()
	m.layout()
}

// detach stops typing into the attached project, with notice (if any) in
// the help line.
func (m *runningModel) detach(notice string) {
	if m.attached == "" {
		return
	}
	m.attached = ""
	if m.attachedFrom {
		m.single = nil
	}
	m.notice = notice
	m.updateTails()
	m.layout()
}

// handleAttachedKey types a key into the attached project; ctrl+q detaches.
func (m *runningModel) handleAttachedKey(msg tea.KeyPressMsg) tea.Cmd {
	if msg.String() == "ctrl+q" {
		m.detach("")
		return nil
	}
	m.typeInto(keyBytes(msg))
	return nil
}

// typeInto sends b to the attached project, and shows the newest output.
func (m *runningModel) typeInto(b []byte) {
	if len(b) == 0 {
		return
	}
	if err := m.sess.Input(m.attached, b); err != nil {
		m.detach(err.Error())
		return
	}
	if m.single != nil && !m.single.following() {
		m.single.scrollBottom()
	}
}

func (m *runningModel) Init() tea.Cmd {
	return tea.Batch(m.waitEvents(), m.git.refresh(m.gitDirs()), m.ports.refresh(), tea.RequestBackgroundColor, clockTick(m, m.now()))
}

// gitDirs returns the directories of the projects in the sidebar.
func (m *runningModel) gitDirs() map[string]string {
	names := slices.Clone(m.idle)
	for _, e := range m.entries {
		names = append(names, e.info.Name)
	}
	dirs := projectDirs(m.cfg, names)
	for _, e := range m.entries {
		if e.info.Dir != "" { // e.g. a git worktree: its branch is shown
			dirs[e.info.Name] = e.info.Dir
		}
	}
	return dirs
}

// waitEvents returns a command that delivers the next batch of supervisor
// events. Under heavy output it waits so that batches arrive at most once
// per frame; the view is redrawn once per batch rather than per line.
func (m *runningModel) waitEvents() tea.Cmd {
	events, last := m.sess.Events(), m.lastBatch
	return func() tea.Msg {
		e, ok := <-events
		if !ok {
			return eventsMsg{closed: true}
		}
		if wait := frameInterval - time.Since(last); wait > 0 {
			time.Sleep(wait)
		}
		batch := []supervisor.Event{e}
		var idle *time.Timer
		for len(batch) < maxBatch {
			select {
			case e, ok := <-events:
				if !ok {
					return eventsMsg{events: batch, closed: true}
				}
				batch = append(batch, e)
				continue
			default:
			}
			// The channel is refilled from the supervisor's queue as it is
			// drained, so it is often momentarily empty during a burst.
			if idle == nil {
				idle = time.NewTimer(drainIdle)
				defer idle.Stop()
			} else {
				idle.Reset(drainIdle)
			}
			select {
			case e, ok := <-events:
				if !ok {
					return eventsMsg{events: batch, closed: true}
				}
				batch = append(batch, e)
			case <-idle.C:
				return eventsMsg{events: batch}
			}
		}
		return eventsMsg{events: batch}
	}
}

func (m *runningModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	n := len(m.entries)
	m.pruneToasts()
	cmd := m.update(msg)
	if len(m.entries) > n && m.combined != nil {
		// Projects were added (e.g. with the picker): show their git state too.
		cmd = tea.Batch(cmd, m.git.refresh(m.gitDirs()))
	}
	return m, tea.Batch(cmd, m.scheduleTick())
}

// wantTick returns how often the view needs redrawing without other
// events: often while a spinner turns, every second while uptimes count or
// sparklines move, never when nothing changes.
func (m *runningModel) wantTick() time.Duration {
	every := time.Duration(0)
	now := m.now().Unix()
	for _, e := range m.entries {
		switch e.info.State {
		case supervisor.Starting, supervisor.Waiting, supervisor.Stopping:
			return animateEvery
		case supervisor.Running:
			every = uptimeEvery
		}
		if e.rate.recent(now) {
			every = uptimeEvery
		}
	}
	return every
}

// scheduleTick starts a tick of the wanted speed when it changed. Ticks of
// an older generation are ignored, so they never stack.
func (m *runningModel) scheduleTick() tea.Cmd {
	want := m.wantTick()
	if want == m.tickEvery {
		return nil
	}
	m.tickGen++
	m.tickEvery = want
	return m.tick()
}

func (m *runningModel) tick() tea.Cmd {
	if m.tickEvery == 0 {
		return nil
	}
	gen := m.tickGen
	return tea.Tick(m.tickEvery, func(time.Time) tea.Msg { return uiTickMsg{gen: gen} })
}

func (m *runningModel) update(msg tea.Msg) tea.Cmd {
	if cmd, ok := m.gitOps.update(msg); ok {
		return cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
	case tea.ColorProfileMsg:
		m.st.setProfile(msg.Profile)
		m.restyle()
	case tea.BackgroundColorMsg:
		m.st.setDark(msg.IsDark())
		m.restyle()
	case clockMsg:
		if msg.owner == m {
			return clockTick(m, m.now())
		}
	case uiTickMsg:
		if msg.gen == m.tickGen {
			m.layout() // uptimes can widen their column
			return m.tick()
		}
	case eventsMsg:
		m.lastBatch = time.Now()
		cmd := m.handleEvents(msg.events)
		if !msg.closed {
			return tea.Batch(cmd, m.waitEvents())
		}
		if l, ok := m.sess.(lostSession); ok && !m.stopping && !m.detached {
			if m.lost = l.Lost(); m.lost != nil {
				return tea.Quit // the session is gone: nothing left to show
			}
		}
		return cmd
	case toastMsg: // expired toasts are pruned in Update
	case gitMsg:
		cmd := m.git.update(msg, m.gitDirs)
		m.layout() // the git column may have changed width
		return cmd
	case gitTickMsg:
		if !m.stopping {
			return m.git.update(msg, m.gitDirs)
		}
	case portsMsg:
		cmd := m.ports.update(msg, m.alive() > 0 && !m.stopping)
		m.layout() // the second lines may have changed width
		if msg.owner == m.ports && len(msg.ports) > 0 {
			// So that a later start warns when another process took one.
			sess, seen := m.sess, msg.ports
			cmd = tea.Batch(cmd, func() tea.Msg { sess.RememberPorts(seen); return nil })
		}
		return cmd
	case leftoversMsg:
		if msg.owner == m {
			m.notice = msg.report
			// The session starts once the leftovers are dealt with.
			if err := m.sess.Start(m.titles...); err != nil {
				m.notice = "can't start: " + oneLine(err.Error())
			}
		}
	case savedLogMsg:
		if msg.owner == m {
			m.notice = savedNotice(msg)
		}
	case openedMsg:
		if msg.owner == m {
			m.notice = "opened " + msg.url
			if msg.err != nil {
				m.notice = "can't open " + msg.url + ": " + msg.err.Error()
			}
		}
	case portTickMsg:
		if !m.stopping {
			return m.ports.update(msg, true)
		}
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.MouseWheelMsg:
		m.wheel(msg)
	case interruptMsg:
		return m.interrupt()
	case stoppedMsg:
		m.finished = true
		return tea.Quit
	default:
		var cmd tea.Cmd
		switch {
		case m.attached != "":
			if p, ok := msg.(tea.PasteMsg); ok {
				m.typeInto([]byte(strings.NewReplacer("\r\n", "\r", "\n", "\r").Replace(p.Content)))
			}
		case m.picker != nil:
			cmd = m.picker.update(msg) // a paste
		case m.editing:
			cmd = m.updateSearchInput(msg)
		case m.argsFor != "":
			m.input, cmd = m.input.Update(msg)
		}
		return cmd
	}
	return nil
}

// handleEvents applies a batch of supervisor events. It returns a command
// to expire the toasts it showed.
func (m *runningModel) handleEvents(events []supervisor.Event) tea.Cmd {
	var lines, single []supervisor.LogLine
	states, changed, toasts, pending := false, false, false, false
	sec := m.now().Unix()
	for _, e := range events {
		switch e := e.(type) {
		case supervisor.StateEvent:
			entry := m.entry(e.Info.Name)
			prev := entry.info.State
			entry.info = e.Info
			states, changed = true, changed || prev != e.Info.State
			if t, ok := m.newToast(prev, e.Info); ok {
				m.addToast(t)
				toasts = true
			}
		case supervisor.PendingEvent:
			m.entry(e.Project)
			if e.Text == "" {
				delete(m.pending, e.Project)
			} else {
				m.pending[e.Project] = e.Text
			}
			pending = true
		case supervisor.WarningEvent:
			m.entry(e.Project)
			if !m.stopping {
				m.addToast(toast{name: e.Project, warn: true, what: e.Text, until: m.now().Add(toastFor)})
				toasts = true
			}
		case supervisor.LineEvent:
			if entry := m.entry(e.Line.Project); !e.Line.Stream.Runr() {
				entry.rate.add(sec, 1)
			}
			lines = append(lines, e.Line) // sanitized when shown
			// Events can arrive slightly out of Seq order, so only lines
			// covered by the snapshot are skipped.
			if m.single != nil && e.Line.Project == m.singleFor && e.Line.Seq > m.snapSeq {
				single = append(single, e.Line)
			}
		}
	}
	if len(lines) > 0 {
		m.combined.append(lines...)
	}
	if len(single) > 0 {
		m.single.append(single...)
	}
	if pending {
		m.updateTails()
	}
	if e := m.byName[m.attached]; e != nil && !e.info.State.Alive() {
		m.detach(e.label + " isn't running any more")
	}
	var cmd tea.Cmd
	if states {
		m.layout() // exit codes and error counts change the sidebar's width
	}
	if changed && !m.stopping {
		cmd = m.ports.refresh() // started processes may listen soon, ended ones don't
	}
	if toasts {
		return tea.Batch(cmd, tea.Tick(toastFor, func(time.Time) tea.Msg { return toastMsg{} }))
	}
	return cmd
}

func (m *runningModel) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	m.notice = ""
	toasts := m.toasts // any key dismisses them
	m.toasts = nil
	switch {
	case m.attached != "":
		return m.handleAttachedKey(msg)
	case m.confirm != nil:
		return m.handleConfirmKey(msg)
	case m.picker != nil:
		return m.handlePickerKey(msg)
	case m.editing:
		return m.handleSearchKey(msg)
	case m.argsFor != "":
		return m.handleArgsKey(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		return m.interrupt()
	case "q", "Q":
		if msg.String() == "Q" && !m.hasLauncher {
			return nil
		}
		m.toLauncher = msg.String() == "Q" // the last key decides, also while stopping
		if !m.stopping {
			return m.interrupt()
		}
	case "d", "D":
		if msg.String() == "D" && !m.hasLauncher {
			return nil
		}
		if d, ok := m.sess.(Detacher); ok && !m.stopping {
			if err := d.Detach(); err != nil {
				m.notice = "can't detach: " + err.Error()
				return nil
			}
			m.detached, m.toLauncher = true, msg.String() == "D"
			return tea.Quit
		}
	case "!": // select the project of the newest toast
		if len(toasts) > 0 {
			name := toasts[len(toasts)-1].name
			if i := slices.IndexFunc(m.entries, func(e *sidebarEntry) bool { return e.info.Name == name }); i >= 0 {
				m.selectEntry(i)
			}
		}
	case "up":
		m.selectEntry(m.selected - 1)
	case "down":
		m.selectEntry(m.selected + 1)
	case "tab":
		if m.single != nil {
			m.single = nil
			m.layout()
		} else if name := m.selectedName(); name != "" {
			m.showSingle(name)
		}
	case "a":
		m.toggleAll()
		if m.showAll {
			return m.git.refresh(m.gitDirs())
		}
	case "enter":
		if name := m.selectedIdle(); name != "" {
			m.startIdle(name)
		}
	case "+":
		if m.stopping {
			return nil
		}
		empty := "everything is running"
		if m.showAll {
			empty = "no groups to start (projects start from the sidebar)"
		}
		var cmd tea.Cmd
		m.picker, cmd = newPicker(m.st, &picker{
			title: "start", action: "start", empty: empty, noun: "item", items: m.pickerItems(),
			pick: func(it pickerItem) tea.Cmd { m.startPicked(it); return nil },
		})
		return cmd
	case "/":
		m.editing, m.prevSearch = true, m.search
		m.input.Reset()
		m.setSearch(search{})
		return m.input.Focus()
	case "n":
		m.jump(1)
	case "N":
		m.jump(-1)
	case "e":
		m.jumpError(1)
	case "E":
		m.jumpError(-1)
	case "esc":
		if m.search.active() {
			m.setSearch(search{})
		}
		m.clearErrorJump()
	case "i":
		m.attach()
	case "b":
		return m.openBranches()
	case "p":
		return m.openPull()
	case "o":
		return m.openPorts()
	case "F":
		return m.fetchAllRepos()
	case "P":
		m.confirmPullAll()
	case "w":
		return m.saveLog()
	case "c":
		if e := m.selectedEntry(); e != nil {
			var cmd tea.Cmd
			m.notice, cmd = copyCommand(m.cfg, e.info.Name, e.info.Args, e.info.Dir)
			return cmd
		}
	case "r":
		m.restart()
	case "R":
		return m.openArgs()
	case "s":
		m.stopOrStart()
	case "pgup":
		m.view().pageUp()
	case "pgdown":
		m.view().pageDown()
	case "g", "home":
		m.view().scrollTop()
	case "G", "end":
		m.view().scrollBottom()
	case "f":
		m.view().toggleFollow()
	}
	return nil
}

// wheelRows is how many rows a step of the mouse wheel scrolls the log.
const wheelRows = 3

// wheel scrolls the log with the mouse wheel, unless a question, picker or
// input is open. (Without mouse reporting, terminals send the wheel as the
// arrow keys, which select projects.)
func (m *runningModel) wheel(msg tea.MouseWheelMsg) {
	v := m.view()
	if v == nil || m.confirm != nil || m.picker != nil || m.editing || m.argsFor != "" {
		return
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		v.scrollUp(wheelRows)
	case tea.MouseWheelDown:
		v.scrollDown(wheelRows)
	}
}

// handlePickerKey handles keys while a picker is open.
func (m *runningModel) handlePickerKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		m.picker = nil
		return m.interrupt()
	case "esc":
		if m.picker.cancel != nil {
			m.picker.cancel()
		}
		m.picker = nil
	case "enter":
		if it, ok := m.picker.selected(); ok {
			pick := m.picker.pick
			m.picker = nil
			return pick(it)
		}
	default:
		return m.picker.update(msg)
	}
	return nil
}

// handleConfirmKey handles keys while a question is open: enter or y
// answers yes; esc, n or q cancel.
func (m *runningModel) handleConfirmKey(msg tea.KeyPressMsg) tea.Cmd {
	if msg.String() == "ctrl+c" {
		m.confirm = nil
		return m.interrupt()
	}
	done, cmd := m.confirm.answer(msg.String())
	if done {
		m.confirm = nil
	}
	return cmd
}

// pickerItems lists the groups with something left to start and, unless
// the sidebar lists every project, the projects that are not running or
// waiting to run.
func (m *runningModel) pickerItems() []pickerItem {
	running := func(name string) bool {
		e := m.byName[name]
		return e != nil && canStop(e.info.State)
	}
	var items []pickerItem
	for _, g := range slices.Sorted(maps.Keys(m.cfg.Groups)) {
		resolved, err := m.cfg.Resolve(g)
		if err != nil || !slices.ContainsFunc(resolved, func(n string) bool { return !running(n) }) {
			continue
		}
		items = append(items, pickerItem{
			name: g, label: g, group: true, section: "Groups", info: strings.Join(m.cfg.Groups[g], ", "),
		})
	}
	for _, name := range slices.Sorted(maps.Keys(m.cfg.Projects)) {
		if running(name) || m.showAll {
			continue
		}
		p := m.cfg.Projects[name]
		it := pickerItem{name: name, label: p.Label(name), section: "Projects", mark: m.dot(name)}
		if e := m.byName[name]; e != nil && e.info.State != supervisor.Pending {
			it.mark += " " + m.st.stateIcon(e.info.State, spinner(m.now()))
			it.right = e.info.State.String()
		}
		if len(p.DependsOn) > 0 {
			it.info = "needs " + strings.Join(p.DependsOn, ", ")
		}
		items = append(items, it)
	}
	return items
}

// dot renders a dot in the project's color.
func (m *runningModel) dot(name string) string {
	return lipgloss.NewStyle().Foreground(m.st.color(m.cfg.ColorOf(name))).Render("●")
}

// startPicked starts a group or project chosen in the picker, with its
// dependencies, and adds it to the sidebar and the title.
func (m *runningModel) startPicked(it pickerItem) {
	resolved, err := m.cfg.Resolve(it.name)
	if err == nil {
		err = m.checkDirs(resolved)
	}
	if err == nil {
		err = m.sess.Start(it.name)
	}
	if err != nil {
		m.report(err)
		return
	}
	known := m.byName[it.name] != nil
	for _, name := range resolved { // in the supervisor's order: dependencies first
		m.entry(name)
	}
	if !slices.Contains(m.titles, it.name) && (it.group || !known) {
		m.titles = append(m.titles, it.name)
	}
	if !it.group {
		m.selectEntry(m.indexOf(it.name))
	}
}

// checkDirs reports projects that would be started but whose working
// directory is missing, like runr up does before starting anything.
func (m *runningModel) checkDirs(projects []string) error {
	warnings, _ := m.cfg.Validate()
	var missing []string
	for _, w := range warnings {
		if e := m.byName[w.Name]; w.Field == "dir" && slices.Contains(projects, w.Name) && (e == nil || !canStop(e.info.State)) {
			missing = append(missing, w.String())
		}
	}
	if len(missing) > 0 {
		return errors.New("cannot start: " + strings.Join(missing, "; "))
	}
	return nil
}

// openArgs opens the input for the selected project's extra arguments,
// with the ones of its latest start.
func (m *runningModel) openArgs() tea.Cmd {
	name := m.selectedName()
	if name == "" || m.stopping {
		return nil
	}
	m.argsFor = name
	m.input.Reset()
	if e := m.byName[name]; e != nil {
		m.input.SetValue(e.info.Args)
		m.input.CursorEnd()
	}
	return m.input.Focus()
}

// handleArgsKey handles keys while the extra arguments input is open: enter
// sets them and restarts the project (or starts it), esc cancels.
func (m *runningModel) handleArgsKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		m.argsFor = ""
		m.input.Blur()
		return m.interrupt()
	case "esc":
		m.argsFor = ""
		m.input.Blur()
		return nil
	case "enter":
		name := m.argsFor
		m.argsFor = ""
		m.input.Blur()
		if err := m.sess.SetArgs(name, m.input.Value()); err != nil {
			m.report(err)
			return nil
		}
		if m.selectedName() == name {
			m.restart()
		}
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

func (m *runningModel) selectedEntry() *sidebarEntry {
	if m.selected < len(m.entries) {
		return m.entries[m.selected]
	}
	return nil
}

func (m *runningModel) restart() {
	if name := m.selectedIdle(); name != "" {
		m.startIdle(name) // restarting what isn't running starts it
		return
	}
	e := m.selectedEntry()
	if e == nil || m.stopping {
		return
	}
	m.report(m.sess.Restart(e.info.Name))
}

// stopOrStart stops the selected project if it is running or waiting to
// run, and starts it (with its dependencies) otherwise.
func (m *runningModel) stopOrStart() {
	if name := m.selectedIdle(); name != "" {
		m.startIdle(name)
		return
	}
	e := m.selectedEntry()
	if e == nil || m.stopping {
		return
	}
	switch {
	case e.info.State == supervisor.Stopping:
	case canStop(e.info.State):
		m.report(m.sess.Stop(e.info.Name))
	default:
		m.report(m.sess.Start(e.info.Name))
	}
}

// canStop reports whether s stops a project in this state (else it starts it).
func canStop(s supervisor.State) bool {
	return s.Alive() || s == supervisor.Waiting
}

func (m *runningModel) report(err error) {
	if err != nil {
		m.notice = err.Error()
	}
}

// interrupt starts a graceful shutdown, or forces it if one is running.
func (m *runningModel) interrupt() tea.Cmd {
	m.confirm, m.toasts = nil, nil // everything stops now
	m.detach("")
	if !m.stopping {
		m.stopping = true
		done := m.sess.StopAll()
		return func() tea.Msg {
			<-done
			return stoppedMsg{}
		}
	}
	if !m.forced {
		m.forced = true
		m.sess.KillAll()
	}
	return nil
}

func (m *runningModel) selectEntry(i int) {
	m.selected = max(min(i, m.rows()-1), 0)
	if name := m.selectedName(); m.single != nil && name != "" && name != m.singleFor {
		m.showSingle(name)
	}
	m.layout()
}

// alive counts the processes that still have to exit.
func (m *runningModel) alive() int {
	n := 0
	for _, e := range m.entries {
		if e.info.State.Alive() {
			n++
		}
	}
	return n
}
