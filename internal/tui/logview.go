package tui

import (
	"math"

	"github.com/Skaanning/runr/internal/supervisor"
)

// formatFunc renders one log line as rows at most width cells wide.
type formatFunc func(line supervisor.LogLine, width int) []string

// logView is a scrollable, wrapping viewport over log lines.
//
// Only the visible lines are formatted. The scroll position is kept as a
// line index (plus a row within that line) anchored to the top or bottom edge
// of the viewport rather than as a row offset into the whole buffer, so
// wrapping never has to be computed for the whole buffer and resizing keeps
// the same lines in view.
type logView struct {
	lines  []supervisor.LogLine
	limit  int // most lines kept; older lines are dropped
	format formatFunc

	follow    bool // keep the newest line at the bottom
	anchor    int  // when not following: the line kept at an edge
	anchorRow int  // the row of the anchor line at that edge (clamped; lastRow = its last row)
	anchorTop bool // whether anchor is at the top edge (else the bottom)

	width, height int

	current uint64 // Seq of the line jumped to (search match or error); 0 if none

	// tail holds the unfinished lines of programs on a terminal (prompts,
	// what is typed), drawn after the lines while following. They aren't
	// lines yet: scrolling, search and below() leave them out.
	tail []supervisor.LogLine

	// Cached rendering, valid unless dirty.
	dirty             bool
	rows              []string
	marks             []rowMark // of each row
	first, last       int       // lines visible in rows, partially at the edges; -1 if none
	firstRow, lastRow int       // rows of first and last at the top and bottom edges
	atTop             bool      // the first row is the start of the first line
}

// lastRow as an anchor row means the anchor line's last row.
const lastRow = math.MaxInt

// rowMark is what the gutter left of a row shows.
type rowMark byte

const (
	noMark       rowMark = iota
	errorMark            // a row of an error line
	currentError         // a row of the error line jumped to
)

// mark returns the mark of the rows of line i.
func (v *logView) mark(i int) rowMark {
	switch l := v.lines[i]; {
	case !l.Error:
		return noMark
	case l.Seq == v.current:
		return currentError
	}
	return errorMark
}

// appendMarks appends n marks of line i.
func (v *logView) appendMarks(i, n int) {
	m := v.mark(i)
	for range n {
		v.marks = append(v.marks, m)
	}
}

func newLogView(limit int, format formatFunc) *logView {
	return &logView{limit: max(limit, 1), format: format, follow: true, dirty: true}
}

func (v *logView) append(lines ...supervisor.LogLine) {
	v.lines = append(v.lines, lines...)
	if over := len(v.lines) - v.limit; over > 0 {
		v.lines = v.lines[over:]
		if v.anchor -= over; v.anchor < 0 {
			v.anchor, v.anchorRow = 0, 0
		}
	}
	v.dirty = true
}

// setTail sets the unfinished lines shown after the lines while following.
func (v *logView) setTail(tail []supervisor.LogLine) {
	v.tail = tail
	v.dirty = true
}

func (v *logView) setSize(width, height int) {
	if width != v.width || height != v.height {
		v.width, v.height = width, height
		v.dirty = true
	}
}

// invalidate forces a new rendering, e.g. after the format changed.
func (v *logView) invalidate() {
	v.dirty = true
}

func (v *logView) pageUp() {
	v.render()
	if v.atTop || v.first < 0 {
		return
	}
	// The old top row moves to the bottom edge, so one row stays in view.
	line, row := v.first, v.firstRow
	if v.height == 1 {
		if row--; row < 0 {
			line, row = line-1, lastRow
		}
	}
	v.follow = false
	v.setAnchor(line, row, false)
}

func (v *logView) pageDown() {
	v.render()
	if v.follow || v.first < 0 {
		return
	}
	// The old bottom row moves to the top edge.
	line, row := v.last, v.lastRow
	if v.height == 1 {
		if row++; row >= len(v.format(v.lines[line], v.width)) {
			line, row = line+1, 0
		}
	}
	v.setAnchor(min(line, len(v.lines)-1), row, true)
}

// scrollUp moves the view n rows towards older lines.
func (v *logView) scrollUp(n int) {
	v.render()
	if v.atTop || v.first < 0 || n <= 0 {
		return
	}
	line := v.first
	row := clampRow(v.firstRow, v.format(v.lines[line], v.width))
	for ; n > 0; n-- {
		if row > 0 {
			row--
		} else if line > 0 {
			line--
			row = len(v.format(v.lines[line], v.width)) - 1
		} else {
			break
		}
	}
	v.follow = false
	v.setAnchor(line, max(row, 0), true)
}

// scrollDown moves the view n rows towards newer lines; reaching the end
// follows them again.
func (v *logView) scrollDown(n int) {
	v.render()
	if v.follow || v.first < 0 || n <= 0 {
		return
	}
	line := v.first
	rows := len(v.format(v.lines[line], v.width))
	row := max(min(v.firstRow, rows-1), 0)
	for ; n > 0; n-- {
		if row+1 < rows {
			row++
		} else if line+1 < len(v.lines) {
			line++
			row, rows = 0, len(v.format(v.lines[line], v.width))
		} else {
			break
		}
	}
	v.setAnchor(line, row, true)
	// render follows again if the lines run out before the bottom edge;
	// the newest row just reaching it does too.
	v.render()
	if n := len(v.lines) - 1; !v.follow && v.last == n && v.lastRow >= len(v.format(v.lines[n], v.width))-1 {
		v.scrollBottom()
	}
}

func (v *logView) scrollTop() {
	v.follow = false
	v.setAnchor(0, 0, true)
}

func (v *logView) scrollBottom() {
	v.follow = true
	v.dirty = true
}

// toggleFollow switches follow mode; turning it off keeps the current view.
func (v *logView) toggleFollow() {
	if !v.follow {
		v.scrollBottom()
		return
	}
	v.render()
	v.follow = false
	v.setAnchor(max(v.last, 0), v.lastRow, false)
}

func (v *logView) setAnchor(line, row int, top bool) {
	v.anchor, v.anchorRow, v.anchorTop = line, row, top
	v.dirty = true
}

// reveal scrolls line i into view, unless it already is, roughly centered.
func (v *logView) reveal(i int) {
	v.render()
	if (i > v.first || i == v.first && v.atTop) && i <= v.last {
		return
	}
	v.follow = false
	v.setAnchor(min(i+(v.height-1)/2, len(v.lines)-1), lastRow, false)
	v.render()
	if i < v.first || i == v.first && v.firstRow > 0 {
		// Wrapped lines below it took the space.
		v.setAnchor(i, lastRow, false)
	}
}

// matching returns the indexes of the lines that contain the search query.
func (v *logView) matching(s search) []int {
	var out []int
	var buf []byte
	for i, l := range v.lines {
		if s.matches(l.Text, &buf) {
			out = append(out, i)
		}
	}
	return out
}

// indexOf returns the index of the line with the given Seq, or -1.
func (v *logView) indexOf(seq uint64) int {
	for i := len(v.lines) - 1; i >= 0; i-- {
		if v.lines[i].Seq == seq {
			return i
		}
	}
	return -1
}

// following reports whether new lines scroll into view.
func (v *logView) following() bool {
	v.render()
	return v.follow
}

// below returns how many lines are after the last visible one.
func (v *logView) below() int {
	v.render()
	if v.last < 0 {
		return 0
	}
	return max(len(v.lines)-1-v.last, 0)
}

// view returns exactly height rows.
func (v *logView) view() []string {
	v.render()
	return v.rows
}

// rowMarks returns the marks of the rows view returns.
func (v *logView) rowMarks() []rowMark {
	v.render()
	return v.marks
}

func (v *logView) render() {
	if !v.dirty {
		return
	}
	v.dirty = false
	if v.follow && len(v.tail) > 0 {
		// Drawn as lines after the last one; indexes past the lines (first,
		// last) are clamped where they are used.
		lines := v.lines
		v.lines = append(lines[:len(lines):len(lines)], v.tail...)
		defer func() {
			v.lines = lines
			if n := len(lines) - 1; v.last > n {
				v.last, v.lastRow = n, lastRow
				if v.first > n {
					v.first, v.firstRow = n, lastRow
				}
			}
		}()
	}
	v.rows, v.marks = v.rows[:0], v.marks[:0]
	v.first, v.last, v.firstRow, v.lastRow, v.atTop = -1, -1, 0, 0, true
	if len(v.lines) > 0 && v.height > 0 {
		if !v.follow && v.anchorTop && !v.renderFromTop(min(v.anchor, len(v.lines)-1), v.anchorRow) {
			// Everything after the anchor fits: show the newest lines instead.
			v.follow = true
		}
		if v.follow || !v.anchorTop {
			bottom, row := len(v.lines)-1, lastRow
			if !v.follow && v.anchor <= bottom {
				bottom, row = v.anchor, v.anchorRow
			}
			v.renderFromBottom(bottom, row)
		}
	}
	for len(v.rows) < v.height {
		v.rows, v.marks = append(v.rows, ""), append(v.marks, noMark)
	}
}

// renderFromTop fills the viewport with lines starting at row of line top.
// It reports false, leaving rows empty, if the lines run out before the
// viewport is full.
func (v *logView) renderFromTop(top, row int) bool {
	for i := top; i < len(v.lines); i++ {
		rows := v.format(v.lines[i], v.width)
		offset := 0 // row of line i that rows starts at
		if i == top {
			row = clampRow(row, rows)
			offset, rows = row, rows[row:]
		}
		v.rows = append(v.rows, rows...)
		v.appendMarks(i, len(rows))
		if over := len(v.rows) - v.height; over >= 0 {
			v.rows, v.marks = v.rows[:v.height], v.marks[:v.height]
			v.first, v.last, v.firstRow, v.lastRow = top, i, row, offset+len(rows)-1-over
			v.atTop = top == 0 && row == 0
			return true
		}
	}
	v.rows, v.marks = v.rows[:0], v.marks[:0]
	return false
}

// renderFromBottom fills the viewport with lines ending at row of line
// bottom. If the lines before it don't fill the viewport, the first line goes
// at the top and the rows after the bottom one fill the rest.
func (v *logView) renderFromBottom(bottom, row int) {
	var chunks [][]string // lines bottom, bottom-1, …
	var after []string    // rows of bottom below row
	n := 0
	i := bottom
	for ; i >= 0 && n < v.height; i-- {
		rows := v.format(v.lines[i], v.width)
		if i == bottom {
			row = clampRow(row, rows)
			rows, after = rows[:row+1], rows[row+1:]
		}
		chunks = append(chunks, rows)
		n += len(rows)
	}
	v.first, v.last, v.lastRow, v.atTop = i+1, bottom, row, i < 0
	for j := len(chunks) - 1; j >= 0; j-- {
		v.rows = append(v.rows, chunks[j]...)
		v.appendMarks(bottom-j, len(chunks[j]))
	}
	if over := len(v.rows) - v.height; over > 0 {
		v.rows, v.marks = v.rows[over:], v.marks[over:]
		v.firstRow, v.atTop = over, false
		return
	}
	for line := bottom; len(v.rows) < v.height; {
		if len(after) == 0 {
			if line++; line >= len(v.lines) {
				break
			}
			after, row = v.format(v.lines[line], v.width), -1
			continue
		}
		take := min(len(after), v.height-len(v.rows))
		v.rows = append(v.rows, after[:take]...)
		v.appendMarks(line, take)
		after, row = after[take:], row+take
		v.last, v.lastRow = line, row
	}
}

// clampRow limits an anchor row to the rows of its line.
func clampRow(row int, rows []string) int {
	return max(min(row, len(rows)-1), 0)
}
