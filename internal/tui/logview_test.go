package tui

import (
	"fmt"
	"slices"
	"testing"

	"github.com/Skaanning/runr/internal/supervisor"
)

func plainFormat(l supervisor.LogLine, width int) []string {
	return wrap(l.Text, width)
}

// numbered returns lines "0", "1", … "n-1".
func numbered(n int) []supervisor.LogLine {
	out := make([]supervisor.LogLine, n)
	for i := range out {
		out[i] = supervisor.LogLine{Seq: uint64(i + 1), Text: fmt.Sprint(i)}
	}
	return out
}

func newTestView(lines, height int) *logView {
	v := newLogView(1000, plainFormat)
	v.setSize(10, height)
	v.append(numbered(lines)...)
	return v
}

func checkRows(t *testing.T, v *logView, want ...string) {
	t.Helper()
	for len(want) < v.height {
		want = append(want, "")
	}
	if got := v.view(); !slices.Equal(got, want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
}

func TestLogViewFollow(t *testing.T) {
	v := newTestView(2, 4)
	checkRows(t, v, "0", "1") // few lines start at the top
	v.append(numbered(6)[2:]...)
	checkRows(t, v, "2", "3", "4", "5")
	if !v.following() || v.below() != 0 {
		t.Fatalf("following = %v, below = %d", v.following(), v.below())
	}
}

func TestLogViewPaging(t *testing.T) {
	v := newTestView(10, 4)
	v.pageUp()
	checkRows(t, v, "3", "4", "5", "6") // old top line "6" is now at the bottom
	if v.following() || v.below() != 3 {
		t.Fatalf("following = %v, below = %d", v.following(), v.below())
	}
	v.pageUp()
	checkRows(t, v, "0", "1", "2", "3")
	v.pageUp()
	checkRows(t, v, "0", "1", "2", "3")

	v.pageDown()
	checkRows(t, v, "3", "4", "5", "6")
	if v.following() {
		t.Fatal("following before reaching the end")
	}
	v.pageDown()
	checkRows(t, v, "6", "7", "8", "9")
	v.pageDown()
	checkRows(t, v, "6", "7", "8", "9")
	if !v.following() {
		t.Fatal("not following after paging to the end")
	}
}

func TestLogViewPageDownPastEndFollows(t *testing.T) {
	v := newTestView(10, 4)
	v.pageUp() // 3..6
	v.append(numbered(12)[10:]...)
	v.pageDown() // top-anchored at 6: 6..9, more below
	checkRows(t, v, "6", "7", "8", "9")
	v.pageDown() // only 9..11 left: show the newest lines and follow
	checkRows(t, v, "8", "9", "10", "11")
	if !v.following() {
		t.Fatal("not following")
	}
}

func TestLogViewPausedViewIsStable(t *testing.T) {
	v := newTestView(10, 3)
	v.scrollTop()
	checkRows(t, v, "0", "1", "2")
	v.append(numbered(20)[10:]...)
	checkRows(t, v, "0", "1", "2")
	if v.below() != 17 {
		t.Fatalf("below = %d, want 17", v.below())
	}

	v.toggleFollow()
	checkRows(t, v, "17", "18", "19")
	v.toggleFollow()
	v.append(supervisor.LogLine{Text: "20"})
	checkRows(t, v, "17", "18", "19") // frozen at the bottom anchor
	v.scrollBottom()
	checkRows(t, v, "18", "19", "20")
}

func TestLogViewScrollTopFewLinesFollows(t *testing.T) {
	v := newTestView(2, 4)
	v.scrollTop()
	checkRows(t, v, "0", "1")
	if !v.following() {
		t.Fatal("everything fits, so the view should follow")
	}
}

func TestLogViewWrapping(t *testing.T) {
	v := newLogView(100, plainFormat)
	v.setSize(4, 3)
	v.append(
		supervisor.LogLine{Text: "a"},
		supervisor.LogLine{Text: "bbbbbbbbbb"}, // 3 rows
		supervisor.LogLine{Text: "c"},
	)
	checkRows(t, v, "bbbb", "bb", "c") // top of the wrapped line is cut
	v.pageUp()
	checkRows(t, v, "a", "bbbb", "bbbb") // the old top row is now at the bottom
	if v.below() != 1 {
		t.Fatalf("below = %d, want 1", v.below())
	}
	v.pageDown()
	checkRows(t, v, "bbbb", "bb", "c")
	v.pageDown()
	if !v.following() {
		t.Fatal("not following after paging to the end")
	}
}

func TestLogViewScrollRows(t *testing.T) {
	v := newTestView(10, 4)
	v.scrollUp(3)
	checkRows(t, v, "3", "4", "5", "6")
	if v.following() {
		t.Fatal("following after scrolling up")
	}
	v.scrollUp(5) // stops at the top
	checkRows(t, v, "0", "1", "2", "3")
	v.scrollUp(1)
	checkRows(t, v, "0", "1", "2", "3")
	v.append(numbered(11)[10]) // a paused view stays put
	checkRows(t, v, "0", "1", "2", "3")
	v.scrollDown(2)
	checkRows(t, v, "2", "3", "4", "5")
	v.scrollDown(5) // the newest line reaches the bottom: following again
	checkRows(t, v, "7", "8", "9", "10")
	if !v.following() {
		t.Fatal("not following after scrolling to the end")
	}
	v.scrollDown(1)
	checkRows(t, v, "7", "8", "9", "10")
}

func TestLogViewScrollWrapped(t *testing.T) {
	v := newLogView(100, plainFormat)
	v.setSize(4, 3)
	v.append(
		supervisor.LogLine{Text: "a"},
		supervisor.LogLine{Text: "bbbbbbbbbb"}, // 3 rows
		supervisor.LogLine{Text: "c"},
	)
	checkRows(t, v, "bbbb", "bb", "c")
	v.scrollUp(1) // a row at a time, within the wrapped line
	checkRows(t, v, "bbbb", "bbbb", "bb")
	v.scrollUp(1)
	checkRows(t, v, "a", "bbbb", "bbbb")
	v.scrollDown(1)
	checkRows(t, v, "bbbb", "bbbb", "bb")
	if v.following() {
		t.Fatal("following before the end")
	}
	v.scrollDown(1)
	checkRows(t, v, "bbbb", "bb", "c")
	if !v.following() {
		t.Fatal("not following at the end")
	}
}

func TestLogViewScrollEmpty(t *testing.T) {
	v := newTestView(0, 4)
	v.scrollUp(3)
	v.scrollDown(3)
	checkRows(t, v)
	if !v.following() {
		t.Fatal("an empty view stopped following")
	}
}

func TestLogViewHugeLine(t *testing.T) {
	v := newLogView(100, plainFormat)
	v.setSize(2, 3)
	v.append(
		supervisor.LogLine{Text: "a"},
		supervisor.LogLine{Text: "0123456789ab"}, // 6 rows
		supervisor.LogLine{Text: "c"},
	)
	checkRows(t, v, "89", "ab", "c")
	// Paging moves through the line taller than the screen.
	v.pageUp()
	checkRows(t, v, "45", "67", "89")
	v.pageUp()
	checkRows(t, v, "01", "23", "45")
	v.pageUp()
	checkRows(t, v, "a", "01", "23")
	v.pageUp()
	checkRows(t, v, "a", "01", "23")

	v.pageDown()
	checkRows(t, v, "23", "45", "67")
	v.pageDown()
	checkRows(t, v, "67", "89", "ab")
	if v.following() {
		t.Fatal("following before reaching the end")
	}
	v.toggleFollow()
	v.toggleFollow() // keeps the exact rows
	checkRows(t, v, "89", "ab", "c")
	v.pageUp()
	v.pageUp()
	v.toggleFollow()
	v.toggleFollow()
	checkRows(t, v, "89", "ab", "c")
}

func TestLogViewHugeLinePausedRowsStable(t *testing.T) {
	v := newLogView(100, plainFormat)
	v.setSize(2, 2)
	v.append(supervisor.LogLine{Text: "0123456789"}) // 5 rows
	v.pageUp()
	checkRows(t, v, "45", "67")
	v.append(supervisor.LogLine{Text: "x"})
	checkRows(t, v, "45", "67")
	v.scrollTop()
	v.pageDown()
	checkRows(t, v, "23", "45")
	v.append(supervisor.LogLine{Text: "y"})
	checkRows(t, v, "23", "45")
}

func TestLogViewHeightOne(t *testing.T) {
	v := newLogView(100, plainFormat)
	v.setSize(2, 1)
	v.append(supervisor.LogLine{Text: "a"}, supervisor.LogLine{Text: "0123"})
	checkRows(t, v, "23")
	v.pageUp()
	checkRows(t, v, "01")
	v.pageUp()
	checkRows(t, v, "a")
	v.pageDown()
	checkRows(t, v, "01")
	v.pageDown()
	checkRows(t, v, "23")
}

func TestLogViewTrimKeepsPosition(t *testing.T) {
	v := newLogView(10, plainFormat)
	v.setSize(10, 2)
	v.append(numbered(10)...)
	v.scrollTop()
	v.pageDown() // top-anchored at "1"
	checkRows(t, v, "1", "2")
	more := make([]supervisor.LogLine, 3)
	for i := range more {
		more[i] = supervisor.LogLine{Text: fmt.Sprint(10 + i)}
	}
	v.append(more...) // drops 0, 1, 2
	checkRows(t, v, "3", "4")
}

func TestLogViewResize(t *testing.T) {
	v := newTestView(10, 4)
	v.pageUp()
	checkRows(t, v, "3", "4", "5", "6")
	v.setSize(10, 2)
	checkRows(t, v, "5", "6") // bottom anchor kept
	v.setSize(10, 6)
	checkRows(t, v, "1", "2", "3", "4", "5", "6")
}

func TestLogViewEmpty(t *testing.T) {
	v := newLogView(10, plainFormat)
	v.setSize(10, 2)
	v.pageUp()
	v.pageDown()
	v.toggleFollow()
	checkRows(t, v)
	if v.below() != 0 {
		t.Fatal("below on empty view")
	}
}
