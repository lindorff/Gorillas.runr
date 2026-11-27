package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Skaanning/runr/internal/supervisor"
)

// Drawing the running view: the layout of the sidebar and the log pane,
// and how processes, log lines, the header, the status bar and the help
// line are rendered.

// panes reports whether the view is drawn as two panes with a status bar;
// small terminals get a header, rules and a divider instead.
func (m *runningModel) panes() bool {
	return m.width >= paneMinWidth && m.height >= paneMinHeight
}

// sidebarPrefix is the width of the selection marker and the state icon in
// front of a sidebar label: " ▌● " in a pane, " ▌ ● " without.
func (m *runningModel) sidebarPrefix() int {
	if m.panes() {
		return 4
	}
	return 5
}

// logWidth is the width of the log column.
func (m *runningModel) logWidth() int {
	sw := m.sidebarLayout().width
	if m.panes() {
		return m.width - sw - 2 - 3 // the sidebar's borders, "│ " and "│"
	}
	return m.width - sw - 2 // "│ "
}

// sidebarLayout returns the width of the sidebar's content (without borders
// or the divider) and its columns. The sidebar takes at most two fifths of
// the screen. In panes, each process takes two lines (see twoLineLayout);
// in the compact layout one, and when narrow the sparkline is dropped first,
// then the status column, then the git column is narrowed and dropped,
// before names are truncated.
func (m *runningModel) sidebarLayout() sidebarCols {
	base := m.sidebarPrefix() + m.labelWidth + 1 // marker and icon + label + " "
	limit := max(m.width*2/5, 12)
	want, details := 0, 0 // widest git label, widest second line
	for _, e := range m.entries {
		if g, ok := m.git.get(e.info.Name); ok {
			want = max(want, gitWidth(g))
		}
		details = max(details, m.detailsWidth(e))
	}
	status := 0 // widest status
	for _, e := range m.entries {
		if text, _ := statusText(e.info, e.oneShot, m.now()); text != "" {
			status = max(status, len(text), statusWidth)
		}
	}
	if m.panes() {
		c := twoLineLayout(m.sidebarPrefix(), base, limit, details, status)
		c.width = max(c.width, min(m.idleRowWidth(), limit))
		return c
	}
	// Column sets from most to least complete.
	for _, o := range []struct{ spark, status, git bool }{
		{true, true, true}, {false, true, true}, {false, false, true},
		{true, true, false}, {false, true, false},
	} {
		c, names := sidebarCols{}, base
		if o.spark {
			c.spark = sparkCells
			names += sparkCells + 1
		}
		if o.status && status > 0 {
			c.status = status
			names += status + 1
		}
		if o.git {
			c.git = min(want, maxGitWidth, limit-names-1)
			if c.git <= 0 || c.git < min(want, minGitWidth) {
				continue
			}
			c.width = names + c.git + 1
			return c
		}
		if names <= limit {
			c.width = names
			return c
		}
	}
	return sidebarCols{width: max(min(base, limit), 12)}
}

// twoLineLayout lays out a sidebar whose processes take two lines: the
// name, status column and sparkline, then the details (ports and git label)
// under the name. When narrow, line 1 drops the sparkline first, then the
// status column, before names are truncated; the details are truncated to
// fit. The sidebar grows to fit the widest details (see detailsWidth).
func twoLineLayout(prefix, base, limit, details, status int) sidebarCols {
	c := sidebarCols{twoLine: true}
	for _, o := range []struct{ spark, status bool }{{true, true}, {false, true}, {false, false}} {
		c.width, c.spark, c.status = base, 0, 0
		if o.spark {
			c.spark = sparkCells
			c.width += sparkCells + 1
		}
		if o.status && status > 0 {
			c.status = status
			c.width += status + 1
		}
		if c.width <= limit {
			break
		}
	}
	if details > 0 {
		c.width = max(c.width, prefix+details+1)
	}
	c.width = max(min(c.width, limit), 12)
	return c
}

// idleRowWidth is the width the rows of the projects not started want in
// the two-line layout: their names, then their git labels aligned after
// them. 0 when there are none.
func (m *runningModel) idleRowWidth() int {
	if len(m.idle) == 0 {
		return 0
	}
	git := 0
	for _, n := range m.idle {
		if g, ok := m.git.get(n); ok {
			git = max(git, gitWidth(g))
		}
	}
	w := m.sidebarPrefix() + m.idleWidth + 1
	if git > 0 {
		w += min(git, maxGitWidth) + 1
	}
	return w
}

// sidebarCols is the sidebar's layout: its width without borders or the
// divider, and the widths of its status, sparkline and git columns, 0 if
// hidden. With twoLine, each process takes two lines and git is unused: the
// git label goes on the second line.
type sidebarCols struct {
	width, status, spark, git int
	twoLine                   bool
}

// entryLines is the number of sidebar lines each of the session's
// processes takes: two in panes (see twoLineLayout), one in the compact
// layout.
func (m *runningModel) entryLines() int {
	if m.panes() {
		return 2
	}
	return 1
}

// statusText returns what the status column shows for a process: its
// uptime while it runs, why it failed (failed is set), "done" for a one-shot
// task that finished, or nothing.
func statusText(info supervisor.ProcessInfo, oneShot bool, now time.Time) (text string, failed bool) {
	switch info.State {
	case supervisor.Exited:
		if oneShot {
			return "done", false
		}
	case supervisor.Crashed:
		if info.Exit.Code > 0 {
			return "exit " + strconv.Itoa(info.Exit.Code), true
		}
		return "crashed", true
	case supervisor.Blocked:
		return "blocked", true
	case supervisor.Starting, supervisor.Running, supervisor.Stopping:
		if !info.StartedAt.IsZero() {
			return Uptime(now.Sub(info.StartedAt)), false
		}
	}
	return "", false
}

// Uptime formats a duration in at most two units: 45s, 4m02s, 1h12m, 3d04h.
func Uptime(d time.Duration) string {
	s := int(max(d, 0) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	default:
		return fmt.Sprintf("%dd%02dh", s/86400, s%86400/3600)
	}
}

// bodyHeight is the number of log rows: all but the header (or top border),
// the rule (bottom border), the status bar (rule) and the help line.
func (m *runningModel) bodyHeight() int {
	return max(m.height-4, 0)
}

// layout updates sizes and scroll positions after a change.
func (m *runningModel) layout() {
	h := m.bodyHeight()
	// Programs on a pseudo-terminal (pty = true) get the logs pane's width.
	if w := m.logWidth(); w > 0 && w != m.termCols {
		m.termCols = w
		_ = m.sess.Resize(w, 0)
	}
	m.combined.setSize(m.logWidth(), h)
	if m.single != nil {
		m.single.setSize(m.logWidth(), h)
	}
	// The session's projects scroll in the rows the ones not started leave.
	idle := m.idleRows(h)
	sh, ih := (h-idle)/m.entryLines(), max(idle-1, 0)
	switch sel := m.selected; {
	case sel >= len(m.entries):
	case sel < m.sideTop:
		m.sideTop = sel
	case sh > 0 && sel >= m.sideTop+sh:
		m.sideTop = sel - sh + 1
	}
	m.sideTop = max(min(m.sideTop, len(m.entries)-sh), 0)
	switch sel := m.selected - len(m.entries); {
	case sel < 0:
	case sel < m.idleTop:
		m.idleTop = sel
	case ih > 0 && sel >= m.idleTop+ih:
		m.idleTop = sel - ih + 1
	}
	m.idleTop = max(min(m.idleTop, len(m.idle)-ih), 0)
}

// formatCombined renders a line with its project's label in front.
func (m *runningModel) formatCombined(line supervisor.LogLine, width int) []string {
	e := m.entry(line.Project)
	lw := min(m.labelWidth, maxPrefixWidth)
	rows := wrap(m.display(line, m.combined.current), width-lw-1)
	label := e.style.Render(pad(ansi.Truncate(e.label, lw, "…"), lw)) + " "
	indent := strings.Repeat(" ", lw+1)
	for i, row := range rows {
		if i == 0 {
			rows[i] = label + row
		} else {
			rows[i] = indent + row
		}
	}
	return rows
}

// display returns a line's text sanitized, with runr's own lines faint and
// search matches highlighted, ready to wrap.
func (m *runningModel) display(line supervisor.LogLine, current uint64) string {
	text := sanitize(line.Text)
	switch line.Stream {
	case supervisor.System:
		text = m.st.system + text
	case supervisor.SystemError:
		text = m.st.systemErr + text
	}
	if m.search.active() {
		if ranges := m.search.find(plain(text)); len(ranges) > 0 {
			on := m.st.match
			if current != 0 && line.Seq == current {
				on = m.st.current
			}
			text = highlight(text, ranges, on)
		}
	}
	return text
}

// formatSingle renders a line of the single-process view.
func (m *runningModel) formatSingle(line supervisor.LogLine, width int) []string {
	return wrap(m.display(line, m.single.current), width)
}

func (m *runningModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion // the wheel scrolls the log (see wheel)
	v.WindowTitle = "runr · " + strings.Join(m.titles, " + ")
	v.ForegroundColor, v.BackgroundColor = m.st.screen()
	return v
}

func (m *runningModel) render() string {
	if m.width == 0 {
		return ""
	}
	if m.width < minWidth || m.height < minHeight {
		return "terminal too small"
	}
	cols := m.sidebarLayout()
	var logRows []string
	var marks []rowMark // none for the picker
	if m.picker != nil {
		logRows = m.picker.view(m.logWidth()-1, m.bodyHeight()) // a space before the border
	} else {
		logRows = m.drawToasts(m.view().view(), m.logWidth())
		marks = m.view().rowMarks()
	}
	if m.panes() {
		return m.renderPanes(cols, logRows, marks)
	}

	sw := cols.width
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteByte('\n')
	b.WriteString(m.st.rule.Render(strings.Repeat("─", sw) + "┬" + strings.Repeat("─", m.width-sw-1)))
	b.WriteByte('\n')
	divider := m.st.rule.Render("│")
	for r, side := range m.sidebarRows(m.bodyHeight(), cols) {
		b.WriteString(side)
		b.WriteString(divider)
		b.WriteString(m.gutter(marks, r))
		b.WriteString(logRows[r])
		b.WriteByte('\n')
	}
	b.WriteString(m.st.rule.Render(strings.Repeat("─", sw) + "┴" + strings.Repeat("─", m.width-sw-1)))
	b.WriteByte('\n')
	b.WriteString(m.help())
	return b.String()
}

// gutter renders the column left of log row r: a red bar on the rows of
// error lines, a yellow one on the error line jumped to (e).
func (m *runningModel) gutter(marks []rowMark, r int) string {
	if r < len(marks) {
		switch marks[r] {
		case errorMark:
			return m.st.err.Render("▌")
		case currentError:
			return m.st.warn.Render("▌")
		}
	}
	return " "
}

func (m *runningModel) header() string {
	left := " " + m.st.badge + "  " + m.st.bold.Render(strings.Join(m.titles, " + "))
	name := "combined"
	if m.single != nil {
		name = m.styleOf(m.singleFor).Render(m.labelOf(m.singleFor))
	}
	var right string
	switch v := m.view(); {
	case m.forced:
		right = m.st.warn.Render("killing…")
	case m.stopping:
		right = m.st.warn.Render(fmt.Sprintf("stopping… %d left · ctrl+c to force", m.alive()))
	case v.following():
		right = name + m.st.faint.Render(" · following")
	default:
		right = name + " · " + m.st.warn.Render("paused")
		if n := v.below(); n > 0 {
			right += m.st.faint.Render(fmt.Sprintf(" · %d more below", n))
		}
	}
	right += " "
	states := make([]supervisor.State, len(m.entries))
	for i, e := range m.entries {
		states[i] = e.info.State
	}
	// The counts go first when narrow, then the right side.
	if counts := m.st.stateCounts(states, "  ", spinner(m.now())); counts != "" {
		if line, ok := spread(left, counts+"   "+right, m.width); ok {
			return line
		}
	}
	if line, ok := spread(left, right, m.width); ok {
		return line
	}
	return ansi.Truncate(left, m.width, "…")
}

// spread puts left and right at the edges of a line of width cells, if both
// fit with a gap between them.
func spread(left, right string, width int) (string, bool) {
	gap := width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 1 {
		return "", false
	}
	return left + strings.Repeat(" ", gap) + right, true
}

// renderPanes draws the processes and logs panes, the status bar and the
// help line.
func (m *runningModel) renderPanes(cols sidebarCols, logRows []string, marks []rowMark) string {
	sw := cols.width
	side, sideTitle := m.st.accent, m.st.accent.Bold(true).Render("processes")
	title, note, border := m.logPane()
	logW := m.width - sw - 2
	sideRows := m.sidebarRows(m.bodyHeight(), cols)
	if m.picker != nil {
		// The picker has the focus: the processes pane fades.
		side, sideTitle = m.st.faint, m.st.faint.Render("processes")
		for i, row := range sideRows {
			sideRows[i] = m.st.faint.Render(ansi.Strip(row))
		}
	}

	var b strings.Builder
	b.WriteString(paneTop(sw+2, sideTitle, "", side))
	b.WriteString(paneTop(logW, title, note, border))
	b.WriteByte('\n')
	for r, row := range sideRows {
		b.WriteString(side.Render("│"))
		b.WriteString(row)
		b.WriteString(side.Render("│"))
		b.WriteString(border.Render("│"))
		b.WriteString(m.gutter(marks, r))
		b.WriteString(pad(logRows[r], logW-3))
		b.WriteString(border.Render("│"))
		b.WriteByte('\n')
	}
	b.WriteString(paneBottom(sw+2, side))
	b.WriteString(paneBottom(logW, border))
	b.WriteByte('\n')
	b.WriteString(m.statusBar())
	b.WriteByte('\n')
	b.WriteString(m.help())
	return b.String()
}

// logPane returns the log pane's title, the note on the right of its top
// border, and its border style: dim while following, yellow while paused,
// accent while searching or picking.
func (m *runningModel) logPane() (title, note string, border lipgloss.Style) {
	v := m.view()
	border = m.st.faint
	switch {
	case m.picker != nil || m.editing || m.argsFor != "" || m.search.active() || m.attached != "":
		border = m.st.accent
	case !v.following():
		border = m.st.warn
	}
	if m.picker != nil {
		return m.picker.titleText(), "", border
	}

	title = m.st.bold.Render("logs") + m.st.faint.Render(" · combined")
	if m.single != nil {
		title = m.st.bold.Render("logs") + m.st.faint.Render(" · ") + m.styleOf(m.singleFor).Render(m.labelOf(m.singleFor))
		if e := m.byName[m.singleFor]; e != nil && e.info.Args != "" {
			title += m.st.faint.Render(" " + e.info.Args)
		}
	}
	switch {
	case m.forced:
		note = m.st.warn.Render("killing…")
	case m.stopping:
		note = m.st.warn.Render(fmt.Sprintf("stopping… %d left · ctrl+c to force", m.alive()))
	case v.following():
		note = m.st.faint.Render("following")
	default:
		note = m.st.warn.Render("paused")
		if n := v.below(); n > 0 {
			note += m.st.faint.Render(fmt.Sprintf(" · %d more below", n))
		}
	}
	return title, note, border
}

// statusBar renders the mode, the session with its state counts, and the
// selected project with its git state.
func (m *runningModel) statusBar() string {
	b := statusBar{mode: "FOLLOW", color: m.st.pal.accent, title: strings.Join(m.titles, " + "), clock: m.now().Format("15:04")}
	switch {
	case m.stopping:
		b.mode, b.color = "STOPPING", m.st.pal.ansi[ansiRed]
	case m.attached != "":
		b.mode, b.color = "ATTACHED", m.st.pal.ansi[ansiBlue]
	case m.picker != nil:
		b.mode, b.color = m.picker.modeName(), m.st.pal.ansi[ansiBlue]
	case m.confirm != nil:
		b.mode, b.color = strings.ToUpper(m.confirm.action), m.st.pal.ansi[ansiBlue]
	case m.editing || m.search.active():
		b.mode, b.color = "SEARCH", m.st.pal.ansi[ansiMagenta]
	case !m.view().following():
		b.mode, b.color = "PAUSED", m.st.pal.ansi[ansiYellow]
	}
	states := make([]supervisor.State, len(m.entries))
	for i, e := range m.entries {
		states[i] = e.info.State
	}
	b.info = m.st.stateCounts(states, " ", spinner(m.now()))
	if name := m.selectedName(); name != "" {
		b.project = m.styleOf(name).Bold(true).Render(m.labelOf(name))
		if g, ok := m.git.get(name); ok {
			b.project += m.st.faint.Render(" · ") + gitLabel(m.st, g, gitWidth(g))
		}
	}
	return m.st.statusBar(b, m.width)
}

// sidebarRow renders a process of the session: one line in the compact
// layout, two in panes (see twoLineLayout).
func (m *runningModel) sidebarRow(i int, cols sidebarCols) []string {
	full, width := cols.width, cols.width
	if i >= len(m.entries) {
		return []string{strings.Repeat(" ", width)}
	}
	if cols.git > 0 {
		width -= cols.git + 1 // the git column and the margin after it
	}
	e := m.entries[i]
	selected := i == m.selected
	marker := " "
	style := e.style
	if selected {
		marker = e.style.Render("▌")
		style = style.Bold(true)
		if m.st.selBar == "" {
			style = style.Reverse(true) // no background tint: highlight the name
		}
	}
	labelWidth := width - m.sidebarPrefix() - 1 // marker and icon, and the right margin
	if cols.status > 0 {
		labelWidth -= cols.status + 1
	}
	if cols.spark > 0 {
		labelWidth -= cols.spark + 1
	}
	label := ansi.Truncate(e.label, labelWidth, "…")
	icon := m.st.stateIcon(e.info.State, spinner(m.now()))
	row := " " + marker + " " + icon + " " + style.Render(label)
	if m.panes() {
		row = " " + marker + icon + " " + style.Render(label)
	}
	row = pad(row, m.sidebarPrefix()+labelWidth)
	if cols.status > 0 {
		text, failed := statusText(e.info, e.oneShot, m.now())
		row += " " + strings.Repeat(" ", cols.status-len(text))
		if failed {
			row += m.st.err.Render(text)
		} else {
			row += m.st.faint.Render(text)
		}
	}
	if cols.spark > 0 {
		style := e.style
		if e.info.State == supervisor.Crashed {
			style = m.st.err
		}
		row += " " + m.st.sparkline(&e.rate, m.now().Unix(), style)
	}
	if cols.git > 0 {
		row = pad(row, width)
		if g, ok := m.git.get(e.info.Name); ok {
			row += gitLabel(m.st, g, cols.git)
		}
	}
	rows := []string{pad(row, full)}
	if cols.twoLine {
		rows = append(rows, pad(" "+marker+"  "+m.details(e, full-m.sidebarPrefix()-1), full))
	}
	if selected && m.st.selBar != "" {
		for i, r := range rows {
			rows[i] = onBackground(r, m.st.selBar)
		}
	}
	return rows
}

// details renders the second line of a process in the sidebar, in at most
// width cells: its count of error lines in red, then its ports and git label,
// joined by " · ", faint except for the git markers. The git label is
// truncated first.
func (m *runningModel) details(e *sidebarEntry, width int) string {
	if width <= 0 || e.info.Errors == 0 {
		return m.portsAndGit(e, width)
	}
	errs := ansi.Truncate(count(e.info.Errors, "error"), width, "…")
	out := m.st.err.Render(errs)
	if rest := m.portsAndGit(e, width-ansi.StringWidth(errs)-3); ansi.StringWidth(rest) > 0 {
		out += m.st.faint.Render(" · ") + rest
	}
	return out
}

// portsAndGit renders the ports and git label of details.
func (m *runningModel) portsAndGit(e *sidebarEntry, width int) string {
	if width <= 0 {
		return ""
	}
	out := ""
	if e.info.State.Alive() {
		out = ansi.Truncate(portsLabel(m.ports.get(e.info.Name)), width, "…")
	}
	g, ok := m.git.get(e.info.Name)
	if !ok {
		return m.st.faint.Render(out)
	}
	if out == "" {
		return gitLabelStyled(m.st, g, width, m.st.faint)
	}
	if rest := width - ansi.StringWidth(out) - 3; rest >= 2 { // room for at least "x…"
		return m.st.faint.Render(out+" · ") + gitLabelStyled(m.st, g, rest, m.st.faint)
	}
	return m.st.faint.Render(out)
}

// detailsWidth is the width the second line of a process wants (see
// details), with the git label at most maxGitWidth wide.
func (m *runningModel) detailsWidth(e *sidebarEntry) int {
	w := 0
	if e.info.State.Alive() {
		w = ansi.StringWidth(portsLabel(m.ports.get(e.info.Name)))
	}
	if g, ok := m.git.get(e.info.Name); ok {
		if w > 0 {
			w += 3 // " · "
		}
		w += min(gitWidth(g), maxGitWidth)
	}
	if e.info.Errors > 0 {
		if w > 0 {
			w += 3
		}
		w += len(count(e.info.Errors, "error"))
	}
	return w
}

func (m *runningModel) help() string {
	if m.confirm != nil {
		return questionLine(m.st, m.confirm, m.width)
	}
	if m.notice != "" {
		return ansi.Truncate(" "+m.st.warn.Render(m.notice), m.width, "…")
	}
	if m.picker != nil {
		return ansi.Truncate(" "+m.st.hintList([][2]string{
			{"type", "to filter"}, {"↑↓", "select"}, {"enter", m.picker.action}, {"esc", "cancel"},
		}), m.width, "…")
	}
	if m.attached != "" {
		label := m.styleOf(m.attached).Bold(true).Render(m.labelOf(m.attached))
		return ansi.Truncate(" "+label+m.st.faint.Render(" · keys go to the program  ")+
			m.st.hintList([][2]string{{"ctrl+q", "detach"}}), m.width, "…")
	}
	if m.argsFor != "" {
		hints := m.st.hintList([][2]string{{"enter", "restart"}, {"esc", "cancel"}})
		label := m.st.bold.Render(m.labelOf(m.argsFor) + " args ")
		m.input.SetWidth(max(m.width-ansi.StringWidth(label)-ansi.StringWidth(hints)-5, 10))
		return ansi.Truncate(" "+label+m.input.View()+"  "+hints, m.width, "…")
	}
	if m.editing {
		hints := m.st.hintList([][2]string{{"enter", "search"}, {"esc", "cancel"}})
		m.input.SetWidth(max(m.width-ansi.StringWidth(hints)-6, 10)) // " /", the cursor and the gap
		return ansi.Truncate(" "+m.st.bold.Render("/")+m.input.View()+"  "+hints, m.width, "…")
	}
	view, stop := "view", "start"
	if m.single != nil {
		view = "all"
	}
	if e := m.selectedEntry(); e != nil && canStop(e.info.State) {
		stop = "stop"
	}
	// Hints are dropped from the end of the middle when narrow.
	keys := [][2]string{{"↑↓", "select"}, {"tab", view}, {"r", "restart"}, {"s", stop}, {"+", "start"}}
	if m.selectedIdle() != "" {
		keys = [][2]string{{"↑↓", "select"}, {"tab", view}, {"s", "start"}, {"+", "start"}}
	}
	if m.showAll {
		keys[len(keys)-1][1] = "group" // projects start from the sidebar
	}
	if e := m.selectedEntry(); e != nil && m.selectedIdle() == "" && e.info.State.Alive() && len(m.ports.get(e.info.Name)) > 0 {
		keys = append(keys, [2]string{"o", "open"})
	}
	if e := m.selectedEntry(); e != nil && m.selectedIdle() == "" && e.info.State.Alive() &&
		m.cfg.Projects[e.info.Name] != nil && m.cfg.Projects[e.info.Name].PTY {
		keys = append(keys, [2]string{"i", "attach"})
	}
	if name := m.selectedName(); name != "" {
		if _, ok := m.git.get(name); ok {
			keys = append(keys, [2]string{"b", "branch"}, [2]string{"p", "pull"})
		}
	}
	if m.git != nil && len(m.git.repos.ByRoot) > 0 {
		keys = append(keys, [2]string{"F", "fetch all"}, [2]string{"P", "pull all"})
	}
	keys = append(keys, [2]string{"/", "search"})
	if m.hasErrors() {
		keys = append(keys, [2]string{"e", "errors"})
	}
	keys = append(keys, [][2]string{
		{"pgup/pgdn", "scroll"}, {"g/G", "top/bottom"}, {"f", "follow"}, {"w", "save"},
		{"c", "copy cmd"},
	}...)
	if len(m.idle) > 0 || !m.showAll && len(m.entries) < len(m.cfg.Projects) {
		all := "hide others"
		if !m.showAll {
			all = "show all"
		}
		keys = append(keys, [2]string{"a", all})
	}
	keys = append(keys, [2]string{"q", "quit"})
	var parts []string
	var front [][2]string // the keys of the search and the error jumps
	var where []string    // their positions
	if m.search.active() {
		front = append(front, [2]string{"n/N", "next/prev"})
		where = append(where, m.st.bold.Render("/"+m.search.query)+" "+m.st.faint.Render(m.matchInfo))
	}
	if m.errorJumpShown() {
		front = append(front, [2]string{"e/E", "next/prev error"})
		keys = slices.DeleteFunc(keys, func(k [2]string) bool { return k[0] == "e" })
		where = append(where, m.st.err.Bold(true).Render("error")+" "+m.st.faint.Render(m.errorInfo))
	}
	if len(front) > 0 {
		keys = append(append(front, [2]string{"esc", "clear"}), keys...)
		parts = append(parts, strings.Join(where, "  "))
	}
	if m.stopping {
		keys, parts = [][2]string{{"ctrl+c", "force quit"}}, nil
	}
	for _, k := range keys {
		parts = append(parts, m.st.hint(k[0], k[1]))
	}
	// Kept with q when hints are dropped for room.
	if _, ok := m.sess.(Detacher); ok && !m.stopping && len(parts) > 0 {
		parts[len(parts)-1] = m.st.hint("d", "detach") + m.st.hintGap + parts[len(parts)-1]
	}
	if m.hasLauncher && !m.stopping && len(parts) > 0 {
		keys := "Q"
		if _, ok := m.sess.(Detacher); ok {
			keys = "D/Q"
		}
		parts[len(parts)-1] += m.st.hintGap + m.st.hint(keys, "…to launcher")
	}
	return m.st.fitHints(parts, m.width)
}

// pad pads s with spaces to width cells.
func pad(s string, width int) string {
	if n := width - ansi.StringWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
