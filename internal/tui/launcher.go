package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Skaanning/runr/internal/config"
	runrstate "github.com/Skaanning/runr/internal/state" // "state" is a test helper in this package
)

const (
	maxDirWidth         = 60 // widest directory column; longer paths lose directories
	minDirWidth         = 16 // the directory column shrinks to this on narrow screens
	maxLauncherGitWidth = 40 // widest git column; longer branches are truncated
	minCmdWidth         = 20 // room left for the command before columns shrink
)

// launcherItem is a group or project in the launcher list.
type launcherItem struct {
	name  string
	group bool
}

// startMsg asks the app to start the given groups and projects, with extra
// arguments by project (blank: none). notice, if set, is shown in the
// running view.
type startMsg struct {
	names  []string
	args   map[string]string
	notice string
}

// launcherModel lists groups and projects, lets the user pick what to start,
// and edits the registry through forms.
type launcherModel struct {
	cfg      *config.Config
	st       *styles
	save     SaveFunc       // nil: changes are only kept in memory
	items    []launcherItem // groups first, then projects, each sorted
	selected map[string]bool
	missing  map[string]bool        // projects whose dir does not exist
	cursor   int                    // index into items
	cardCol  int                    // column of the card the cursor left the cards from, downwards
	top      int                    // first row shown, see rows
	notice   string                 // shown instead of the help line until the next key
	form     *editForm              // open project or group form, or nil
	confirm  *confirmation          // open delete confirmation, or nil
	picker   *picker                // open branch or theme picker, or nil
	ask      *question              // open question in the help line (pull?), or nil
	output   []string               // output of a failed git action, shown until the next key
	skipped  bool                   // output is about skipped repositories only (P), not failures
	jump     bool                   // jump mode (/): every item shows its letter, see jumpLabel
	lastArgs map[string]string      // extra arguments typed last, by project (R)
	last     *runrstate.LastSession // the last session, which l starts; nil if none

	bg           *backgroundTracker // sessions running elsewhere; nil: not shown
	sessCursor   int                // index into the running sessions; -1: the cursor is in items
	sessPid      int                // the session under the cursor, kept across refreshes
	stoppingSess map[int]bool       // sessions being stopped (x), by pid

	git      *gitTracker // nil: git is not installed
	gitOps   *gitActions
	gitStale bool // the config or a repository changed; refresh the git state
	cleaning bool // leftover processes are being stopped

	width, height int
	now           func() time.Time // for the clock
}

func newLauncherModel(cfg *config.Config, save SaveFunc, st *styles) *launcherModel {
	m := &launcherModel{cfg: cfg, st: st, save: save, selected: map[string]bool{}, now: time.Now, sessCursor: -1, stoppingSess: map[int]bool{}}
	m.gitOps = newGitActions(launcherGit{m}, st, cfg)
	m.reload()
	return m
}

// reload rebuilds the list from the config.
func (m *launcherModel) reload() {
	m.items = m.items[:0]
	for _, g := range slices.Sorted(maps.Keys(m.cfg.Groups)) {
		m.items = append(m.items, launcherItem{name: g, group: true})
	}
	for _, p := range slices.Sorted(maps.Keys(m.cfg.Projects)) {
		m.items = append(m.items, launcherItem{name: p})
	}
	m.missing = map[string]bool{}
	warnings, _ := m.cfg.Validate()
	for _, w := range warnings {
		if w.Field == "dir" {
			m.missing[w.Name] = true
		}
	}
	for p := range m.selected {
		if m.cfg.Projects[p] == nil {
			delete(m.selected, p)
		}
	}
	m.cursor = max(min(m.cursor, len(m.items)-1), 0)
}

func (m *launcherModel) Init() tea.Cmd {
	return tea.Batch(m.git.refresh(m.gitDirs()), m.bg.refresh(), tea.RequestBackgroundColor, clockTick(m, m.now()))
}

func (m *launcherModel) gitDirs() map[string]string {
	return projectDirs(m.cfg, slices.Collect(maps.Keys(m.cfg.Projects)))
}

func (m *launcherModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case gitMsg, gitTickMsg:
		return m, m.git.update(msg, m.gitDirs)
	}
	if cmd, ok := m.bg.update(msg); ok {
		m.setSessions()
		return m, cmd
	}
	_, cmd := m.update(msg)
	if m.gitStale {
		m.gitStale = false
		cmd = tea.Batch(cmd, m.git.refresh(m.gitDirs()))
	}
	return m, cmd
}

func (m *launcherModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := m.gitOps.update(msg); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.ColorProfileMsg:
		m.st.setProfile(msg.Profile)
	case tea.BackgroundColorMsg:
		m.st.setDark(msg.IsDark())
	case clockMsg:
		if msg.owner == m {
			return m, clockTick(m, m.now())
		}
		return m, nil
	case interruptMsg:
		return m, tea.Quit
	case leftoversMsg:
		if msg.owner == m {
			m.notice, m.cleaning = msg.report, false
		}
		return m, nil
	case stoppedSessionMsg:
		if msg.owner == m {
			return m, m.stoppedSession(msg)
		}
		return m, nil
	}
	switch {
	case m.form != nil && m.picker != nil: // the directory browser
		switch msg.(type) {
		case tea.KeyPressMsg, tea.PasteMsg:
			return m, m.updatePicker(msg)
		}
		return m, m.updateForm(msg)
	case m.form != nil:
		return m, m.updateForm(msg)
	case m.confirm != nil:
		return m, m.updateConfirm(msg)
	case m.picker != nil:
		return m, m.updatePicker(msg)
	case m.ask != nil:
		if msg, ok := msg.(tea.KeyPressMsg); ok {
			m.notice = ""
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
			done, cmd := m.ask.answer(msg.String())
			if done {
				m.ask = nil
			}
			return m, cmd
		}
		return m, nil
	case m.output != nil:
		if msg, ok := msg.(tea.KeyPressMsg); ok {
			m.output, m.notice = nil, ""
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
		}
		return m, nil
	case m.jump:
		if msg, ok := msg.(tea.KeyPressMsg); ok {
			return m, m.updateJump(msg.String())
		}
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		m.notice = ""
		if s, ok := m.currentSession(); ok {
			switch msg.String() {
			case "enter":
				return m, m.attachSession(s)
			case "x":
				m.confirmStopSession()
				return m, nil
			case "space", "R", "shift+enter", "c":
				return m, nil // about groups and projects
			}
		}
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "down", "left", "right":
			m.move(msg.String())
		case "home":
			m.cursor = 0
			if len(m.bg.sessions()) > 0 {
				m.selectSession(0)
			}
		case "end":
			m.cursor = max(len(m.items)-1, 0)
			m.selectSession(-1)
		case "space":
			m.toggle()
		case "enter":
			return m, m.startSelection()
		case "R", "shift+enter":
			return m, m.openArgsForm()
		case "l":
			return m, m.startLast()
		case "/":
			m.jump = len(m.items) > 0 || len(m.bg.sessions()) > 0
		case "n":
			return m, m.openForm(newProjectForm(m.st, m.cfg, "", m.gitReader()))
		case "e":
			if it, ok := m.current(); ok && it.group {
				return m, m.openForm(newGroupForm(m.st, m.cfg, it.name, nil))
			} else if ok {
				return m, m.openForm(newProjectForm(m.st, m.cfg, it.name, m.gitReader()))
			}
		case "g":
			if it, ok := m.current(); ok && it.group {
				return m, m.openForm(newGroupForm(m.st, m.cfg, it.name, nil))
			}
			return m, m.openForm(newGroupForm(m.st, m.cfg, "", slices.Sorted(maps.Keys(m.selected))))
		case "d":
			if it, ok := m.current(); ok {
				m.confirm = newDeleteConfirmation(m.cfg, it)
			}
		case "b":
			return m, m.openBranches()
		case "F":
			return m, m.fetchAllRepos()
		case "P":
			m.confirmPullAll()
		case "p":
			return m, m.openPull()
		case "t":
			return m, m.openThemes()
		case "c":
			if it, ok := m.current(); ok && !it.group {
				var cmd tea.Cmd
				m.notice, cmd = copyCommand(m.cfg, it.name, "", "")
				return m, cmd
			}
		}
	}
	return m, nil
}

// startSelection starts the selection, or the item under the cursor when
// nothing is selected.
func (m *launcherModel) startSelection() tea.Cmd {
	return m.start(m.toStart(), nil)
}

// toStart returns what enter starts: the selection, or the item under the
// cursor when nothing is selected.
func (m *launcherModel) toStart() []string {
	if m.sessCursor >= 0 {
		return nil
	}
	names := m.selection()
	if len(names) == 0 && m.cursor < len(m.items) {
		names = []string{m.items[m.cursor].name}
	}
	return names
}

// setLast sets the last session: l starts it again, and the arguments form
// starts with its arguments.
func (m *launcherModel) setLast(last *runrstate.LastSession) {
	m.last = last
	if last != nil {
		m.lastArgs = maps.Clone(last.Args)
	}
}

// lastNames returns the groups and projects of the last session that are
// still registered, or nil.
func (m *launcherModel) lastNames() []string {
	if m.last == nil {
		return nil
	}
	names, _, _ := m.last.Registered(m.cfg)
	return names
}

// startLast starts the last session again, with its extra arguments,
// leaving out what isn't registered any more.
func (m *launcherModel) startLast() tea.Cmd {
	if m.last == nil {
		m.notice = "no last session yet"
		return nil
	}
	names, args, skipped := m.last.Registered(m.cfg)
	if len(names) == 0 {
		m.notice = "nothing from the last session is registered any more"
		return nil
	}
	cmd := m.start(names, args)
	if cmd == nil || len(skipped) == 0 {
		return cmd
	}
	notice := "skipped from the last session, not registered any more: " + strings.Join(skipped, ", ")
	return func() tea.Msg {
		msg := cmd().(startMsg)
		msg.notice = notice
		return msg
	}
}

// openArgsForm opens the form asking for extra arguments for what enter
// would start, pre-filled with the ones typed last.
func (m *launcherModel) openArgsForm() tea.Cmd {
	names := m.toStart()
	if len(names) == 0 {
		return nil
	}
	f, err := newArgsForm(m.st, m.cfg, names, m.lastArgs, func(args map[string]string) tea.Cmd {
		if m.lastArgs == nil {
			m.lastArgs = map[string]string{}
		}
		for p, a := range args {
			if a == "" {
				delete(m.lastArgs, p)
			} else {
				m.lastArgs[p] = a
			}
		}
		return m.start(names, args)
	})
	if err != nil {
		m.notice = oneLine(err.Error())
		return nil
	}
	return m.openForm(f)
}

// start asks the app to start names with extra arguments by project (nil
// for none), unless a git action or the cleanup of leftover processes is
// still running; then a notice says so.
func (m *launcherModel) start(names []string, args map[string]string) tea.Cmd {
	switch {
	case len(names) == 0:
		return nil
	case m.gitOps.action != "":
		m.notice = m.gitOps.action + " is still running"
		return nil
	case m.cleaning:
		m.notice = "the leftover processes are still being stopped"
		return nil
	}
	return func() tea.Msg { return startMsg{names: names, args: args} }
}

// updateJump handles a key in jump mode: a group's letter starts the group
// (the selection is left out), a project's letter selects or unselects the
// project and moves the cursor to it, staying in jump mode. enter starts
// what it starts outside jump mode.
func (m *launcherModel) updateJump(key string) tea.Cmd {
	m.notice = ""
	switch key {
	case "ctrl+c":
		return tea.Quit
	case "esc", "/":
		m.jump = false
		return nil
	case "enter":
		m.jump = false
		return m.startSelection()
	}
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		if i := int(key[0] - '1'); i < len(m.bg.sessions()) {
			m.jump = false
			return m.attachSession(m.bg.sessions()[i])
		}
		return nil
	}
	i := jumpIndex(key)
	if i < 0 || i >= len(m.items) {
		return nil
	}
	if it := m.items[i]; it.group {
		m.jump = false
		return m.start([]string{it.name}, nil)
	}
	m.cursor = i
	m.selectSession(-1)
	m.toggle()
	return nil
}

// jumpLabel returns the letter of the item at index i in jump mode: a–z,
// then A–Z, and "" for the items after those.
func jumpLabel(i int) string {
	switch {
	case i < 0:
		return ""
	case i < 26:
		return string(rune('a' + i))
	case i < 52:
		return string(rune('A' + i - 26))
	}
	return ""
}

// jumpIndex returns the index of the item whose letter is key, or -1.
func jumpIndex(key string) int {
	if len(key) != 1 {
		return -1
	}
	switch c := key[0]; {
	case c >= 'a' && c <= 'z':
		return int(c - 'a')
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 26
	}
	return -1
}

// jumpLabelAt returns the letter shown for the item at index i: its
// jumpLabel in jump mode, "" otherwise.
func (m *launcherModel) jumpLabelAt(i int) string {
	if !m.jump {
		return ""
	}
	return jumpLabel(i)
}

// openThemes opens the theme picker, on the current theme. The launcher is
// drawn in the theme under the cursor until one is picked or esc restores
// the saved one.
func (m *launcherModel) openThemes() tea.Cmd {
	current := m.cfg.Settings.Theme()
	items := make([]pickerItem, len(config.ThemeNames))
	for i, name := range config.ThemeNames {
		t := themeNamed(name)
		items[i] = pickerItem{name: name, label: name, info: t.about}
		if name == current {
			items[i].pill = "current"
		}
		if m.st.tints() {
			items[i].swatch = m.st.swatch(t)
		}
	}
	p := &picker{
		title: "theme", action: "use", mode: "theme", noun: "theme", items: items,
		pick:    func(it pickerItem) tea.Cmd { m.saveTheme(it.name); return nil },
		preview: func(it pickerItem) { m.st.setTheme(themeNamed(it.name)) },
		cancel:  m.restoreTheme,
	}
	if !m.st.tints() {
		p.note = "this terminal shows 16 colors, so runr uses the terminal theme here"
	}
	var cmd tea.Cmd
	m.picker, cmd = newPicker(m.st, p)
	m.picker.cursor = max(slices.Index(config.ThemeNames, current), 0)
	return cmd
}

// saveTheme makes the named theme the configured one.
func (m *launcherModel) saveTheme(name string) {
	c := *m.cfg // save only reads it
	c.Settings.ThemeValue = name
	if m.save != nil {
		if err := m.save(&c); err != nil {
			m.notice = "can't save the theme: " + oneLine(err.Error())
			m.restoreTheme()
			return
		}
	}
	m.cfg.Settings.ThemeValue = name
	m.st.setTheme(themeNamed(name))
	m.notice = "theme set to " + name
}

// restoreTheme draws the launcher in the configured theme again.
func (m *launcherModel) restoreTheme() {
	m.st.setTheme(themeNamed(m.cfg.Settings.Theme()))
}

// busy reports whether something else has the screen.
func (m *launcherModel) busy() bool {
	return m.form != nil || m.confirm != nil || m.picker != nil || m.ask != nil || m.output != nil || m.jump
}

func (m *launcherModel) updatePicker(msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m.picker.update(msg) // a paste
	}
	m.notice = ""
	if m.picker.keys != nil {
		if done, cmd := m.picker.keys(key.String()); done {
			return cmd
		}
	}
	switch key.String() {
	case "ctrl+c":
		return tea.Quit
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
		return m.picker.update(key)
	}
	return nil
}

// current returns the item under the cursor; none while it's on a running
// session.
func (m *launcherModel) current() (launcherItem, bool) {
	if m.sessCursor < 0 && m.cursor < len(m.items) {
		return m.items[m.cursor], true
	}
	return launcherItem{}, false
}

func (m *launcherModel) openForm(f *editForm) tea.Cmd {
	m.form = f
	return f.init()
}

// gitReader returns what reads git state, or nil if git isn't installed.
func (m *launcherModel) gitReader() GitReader {
	if m.git == nil {
		return nil
	}
	return m.git.reader
}

func (m *launcherModel) updateForm(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		m.notice = ""
		switch k.String() {
		case "ctrl+c":
			return tea.Quit
		case "esc":
			m.form = nil
			return nil
		case "ctrl+o":
			if fd := m.form.form.focused(); fd != nil && fd.key == "dir" {
				return m.browseDirs(fd)
			}
		}
	}
	cmd := m.form.update(msg)
	if f := m.form.form; f.done {
		if !f.saved {
			m.form = nil
			return cmd
		}
		if submit := m.form.submit; submit != nil {
			m.form = nil
			return tea.Batch(cmd, submit())
		}
		m.submitForm()
	}
	return cmd
}

// browseDirs opens the directory browser over the project form, starting
// at the directory in fd. ctrl+o in it puts the directory shown into fd.
func (m *launcherModel) browseDirs(fd *formField) tea.Cmd {
	var cmd tea.Cmd
	m.picker, cmd = newDirBrowser(m.st, browseStart(*fd.str), func(dir string) tea.Cmd {
		m.picker = nil
		fd.setValue(config.DisplayPath(dir))
		fd.touched = true
		m.form.form.validate()
		return m.form.handle(nil) // checks the directory
	})
	return cmd
}

// submitForm saves a form closed with Save, or keeps it open with the error
// shown.
func (m *launcherModel) submitForm() {
	f := m.form
	c, focus, err := f.result()
	if err == nil {
		_, err = c.Validate()
	}
	if err == nil && m.save != nil {
		err = m.save(c)
	}
	if err != nil {
		m.notice = oneLine(err.Error())
		f.form.done = false
		return
	}
	m.form = nil
	renamed := f.original.name != "" && f.original.name != focus.name
	if renamed && m.selected[f.original.name] {
		delete(m.selected, f.original.name)
		m.selected[focus.name] = true
	}
	m.apply(c)
	if i := slices.Index(m.items, focus); i >= 0 {
		m.cursor = i
	}
	kind := "project"
	if focus.group {
		kind = "group"
	}
	m.notice = fmt.Sprintf("saved %s %q", kind, focus.name)
	if renamed {
		m.notice += fmt.Sprintf(" (renamed from %q)", f.original.name)
	}
}

func (m *launcherModel) updateConfirm(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch k.String() {
	case "ctrl+c":
		return tea.Quit
	case "y", "enter":
		c := m.confirm.result()
		m.confirm = nil
		if m.save != nil {
			if err := m.save(c); err != nil {
				m.notice = oneLine(err.Error())
				return nil
			}
		}
		m.apply(c)
	case "n", "esc":
		m.confirm = nil
	}
	return nil
}

// apply replaces the config in place, so everything sharing it sees the
// change, and rebuilds the list.
func (m *launcherModel) apply(c *config.Config) {
	*m.cfg = *c
	m.reload()
	m.gitStale = true // dirs may have changed
}

// oneLine joins a multi-line message, e.g. a list of validation errors.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// members returns the projects an item stands for.
func (m *launcherModel) members(it launcherItem) []string {
	if it.group {
		return m.cfg.Groups[it.name]
	}
	return []string{it.name}
}

// toggle selects the item under the cursor, or unselects it if all of its
// projects are already selected.
func (m *launcherModel) toggle() {
	if m.sessCursor >= 0 || m.cursor >= len(m.items) {
		return
	}
	members := m.members(m.items[m.cursor])
	all := m.countSelected(members) == len(members)
	for _, p := range members {
		if all {
			delete(m.selected, p)
		} else {
			m.selected[p] = true
		}
	}
}

func (m *launcherModel) countSelected(projects []string) int {
	n := 0
	for _, p := range projects {
		if m.selected[p] {
			n++
		}
	}
	return n
}

// selection names what to start: groups whose members are all selected,
// then the other selected projects.
func (m *launcherModel) selection() []string {
	var names []string
	covered := map[string]bool{}
	for _, it := range m.items {
		members := m.members(it)
		switch {
		case it.group && len(members) > 0 && m.countSelected(members) == len(members):
			names = append(names, it.name)
			for _, p := range members {
				covered[p] = true
			}
		case !it.group && m.selected[it.name] && !covered[it.name]:
			names = append(names, it.name)
		}
	}
	return names
}

// count returns "1 project", "2 projects", "2 branches".
func count(n int, noun string) string {
	switch {
	case n == 1:
		return "1 " + noun
	case strings.HasSuffix(noun, "ch"), strings.HasSuffix(noun, "s"):
		return fmt.Sprintf("%d %ses", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// isCursor reports whether the cursor is on the item at index i.
func (m *launcherModel) isCursor(i int) bool {
	return m.sessCursor < 0 && i == m.cursor
}
