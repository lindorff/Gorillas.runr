package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Skaanning/runr/internal/proc"
	"github.com/Skaanning/runr/internal/supervisor"
)

var testNow = func() time.Time { return time.Date(2026, 10, 25, 14, 32, 10, 0, time.UTC) }

func newPaneModel(t *testing.T, width, height int) (*runningModel, *fakeSession) {
	t.Helper()
	m, sess := newTestModel(t, width, height)
	m.Update(eventsMsg{events: []supervisor.Event{
		state("db", supervisor.Running),
		state("api", supervisor.Starting),
		supervisor.StateEvent{Info: supervisor.ProcessInfo{Name: "worker", State: supervisor.Crashed, Exit: proc.ExitStatus{Code: 1}}},
		system("db", "starting: db"),
		line("api", "listening on :8080"),
	}})
	return m, sess
}

func TestPaneLayout(t *testing.T) {
	m, _ := newPaneModel(t, 72, 10)
	want := []string{
		"╭─ processes ───────────────╮╭─ logs · combined ─────────── following ─╮",
		"│ ▌● db            ▁▁▁▁▁▁▁▁ ││ db     starting: db                     │",
		"│ ▌                         ││ api    listening on :8080               │",
		"│  ⠋ api           ▁▁▁▁▁▁▁▂ ││                                         │",
		"│                           ││                                         │",
		"│  ✗ Worker exit 1 ▁▁▁▁▁▁▁▁ ││                                         │",
		"│                           ││                                         │",
		"╰───────────────────────────╯╰─────────────────────────────────────────╯",
		"  FOLLOW  backend  ● 1 ⠋ 1 ✗ 1                               db  14:32",
		"  ↑↓  select  tab  view  r  restart  s  stop  +  group  q  quit",
	}
	if got := screen(m); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("screen:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPaneModesAndBorders(t *testing.T) {
	m, sess := newPaneModel(t, 90, 10)
	logBorder := func() string {
		_, after, _ := strings.Cut(strings.Split(m.render(), "\n")[0], "╮")
		return strings.TrimPrefix(after, "\x1b[m")
	}
	check := func(mode, border string) {
		t.Helper()
		if got := statusRow(m); !strings.HasPrefix(got, "  "+mode+" ") {
			t.Errorf("status bar = %q, want mode %s", got, mode)
		}
		if got := logBorder(); !strings.HasPrefix(got, border+"╭─ ") {
			t.Errorf("%s: log pane border = %q, want %q", mode, got, border)
		}
	}
	check("FOLLOW", "\x1b[2m") // dim

	var events []supervisor.Event
	for range 20 {
		events = append(events, emit(sess, "db", "line"))
	}
	m.Update(eventsMsg{events: events})
	m.Update(key("pgup"))
	check("PAUSED", "\x1b[33m") // yellow
	if !strings.Contains(screen(m)[0], " paused · ") {
		t.Errorf("top border = %q", screen(m)[0])
	}
	m.Update(key("G"))

	m.Update(key("/"))
	check("SEARCH", "\x1b[36m") // accent
	m.Update(key("esc"))

	m.Update(key("+"))
	check("START", "\x1b[36m")
	if got := screen(m)[0]; !strings.Contains(got, "╭─ start ─") {
		t.Errorf("picker pane title = %q", got)
	}
	m.Update(key("esc"))

	m.Update(tea.WindowSizeMsg{Width: 100, Height: 10})
	m.Update(key("q"))
	check("STOPPING", "\x1b[2m")
	if got := screen(m)[0]; !strings.Contains(got, "stopping… 2 left · ctrl+c to force") {
		t.Errorf("top border = %q", got)
	}
}

func TestWheelScrollsLog(t *testing.T) {
	m, sess := newPaneModel(t, 90, 10)
	if mode := m.View().MouseMode; mode != tea.MouseModeCellMotion {
		t.Fatalf("MouseMode = %v: the terminal would send the wheel as arrow keys", mode)
	}
	var events []supervisor.Event
	for range 20 {
		events = append(events, emit(sess, "db", "line"))
	}
	m.Update(eventsMsg{events: events})
	selected := m.selected

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.view().following() {
		t.Error("still following after the wheel went up")
	}
	if m.selected != selected {
		t.Errorf("the wheel selected project %d, was %d", m.selected, selected)
	}
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if !m.view().following() {
		t.Error("not following after the wheel came back down")
	}

	// While a picker is open, the wheel leaves the log alone.
	m.Update(key("+"))
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	m.Update(key("esc"))
	if !m.view().following() {
		t.Error("the wheel scrolled the log behind the picker")
	}
}

func TestStatusBarNarrows(t *testing.T) {
	st := newStyles(terminalTheme)
	b := statusBar{mode: "FOLLOW", color: st.pal.accent, title: "backend + web", info: "● 2", project: "api · main", clock: "14:32"}
	for _, tt := range []struct {
		width int
		want  string
	}{
		{60, "  FOLLOW  backend + web  ● 2             api · main  14:32"},
		{45, "  FOLLOW  backend + web  ● 2          14:32"}, // project dropped
		{30, "  FOLLOW  ● 2          14:32"},                // title dropped
		{16, "  FOLLOW  ● 2"},                               // clock dropped
		{10, "  FOLLOW …"},
	} {
		if got := strings.TrimRight(ansi.Strip(st.statusBar(b, tt.width)), " "); got != tt.want {
			t.Errorf("width %d: %q, want %q", tt.width, got, tt.want)
		}
	}
}

func TestPanesFallBackWhenSmall(t *testing.T) {
	for _, size := range [][2]int{{49, 12}, {80, 9}} {
		m, _ := newPaneModel(t, size[0], size[1])
		if got := screen(m)[0]; !strings.HasPrefix(got, "  runr   backend") {
			t.Errorf("%dx%d: header = %q, want the layout without panes", size[0], size[1], got)
		}
	}
	m, _ := newPaneModel(t, 50, 10)
	if got := screen(m)[0]; !strings.HasPrefix(got, "╭─ processes") {
		t.Errorf("50x10: top row = %q, want panes", got)
	}
}

func TestRenderFitsEverySize(t *testing.T) {
	// Around the minimum size and the size panes need.
	for _, w := range []int{minWidth, minWidth + 7, paneMinWidth - 1, paneMinWidth, paneMinWidth + 1, 72, 100} {
		for _, h := range []int{minHeight, minHeight + 1, paneMinHeight - 1, paneMinHeight, paneMinHeight + 1, 16} {
			m, _ := newPaneModel(t, w, h)
			for _, keys := range [][]string{nil, {"down", "tab"}, {"+"}} {
				for _, k := range keys {
					m.Update(key(k))
				}
				rows := strings.Split(m.render(), "\n")
				if len(rows) != h {
					t.Errorf("%dx%d %v: %d rows", w, h, keys, len(rows))
				}
				for i, r := range rows {
					if rw := ansi.StringWidth(r); rw > w {
						t.Errorf("%dx%d %v: row %d is %d cells: %q", w, h, keys, i, rw, ansi.Strip(r))
					}
				}
			}
		}
	}
}

func TestClock(t *testing.T) {
	m, _ := newPaneModel(t, 72, 10)
	if !strings.HasSuffix(statusRow(m), " 14:32") {
		t.Fatalf("status bar = %q", statusRow(m))
	}
	m.now = func() time.Time { return testNow().Add(time.Minute) }
	if _, cmd := m.Update(clockMsg{owner: m}); cmd == nil {
		t.Error("clock not rescheduled")
	}
	if _, cmd := m.Update(clockMsg{owner: "launcher"}); cmd != nil {
		t.Error("another model's clock was rescheduled")
	}
	if !strings.HasSuffix(statusRow(m), " 14:33") {
		t.Errorf("status bar = %q", statusRow(m))
	}
}

func TestPaneTop(t *testing.T) {
	st := newStyles(terminalTheme)
	for _, tt := range []struct {
		width       int
		title, note string
		want        string
	}{
		{30, "logs", "following", "╭─ logs ───────── following ─╮"},
		{20, "logs", "following", "╭─ logs ───────────╮"}, // note dropped
		{12, "processes", "", "╭─ proces…─╮"},
		{15, "processes", "", "╭─ processes ─╮"},
	} {
		if got := ansi.Strip(paneTop(tt.width, tt.title, tt.note, st.faint)); got != tt.want {
			t.Errorf("paneTop(%d, %q, %q) = %q, want %q", tt.width, tt.title, tt.note, got, tt.want)
		}
	}
}

func TestLauncherStatusBar(t *testing.T) {
	m := newTestLauncher(t, launcherConfig(t))
	m.now = testNow
	status := func() string { rows := launcherScreen(m); return rows[len(rows)-2] }
	if got := status(); got != "  SELECT  3 projects · 2 groups"+strings.Repeat(" ", 82)+"14:32" {
		t.Errorf("status bar = %q", got)
	}
	m.Update(key("space"))
	if got := status(); !strings.HasPrefix(got, "  SELECT  3 projects · 2 groups · 2 selected ") {
		t.Errorf("status bar = %q", got)
	}
	m.Update(key("down")) // api
	if got := status(); !strings.HasSuffix(got, " api  14:32") {
		t.Errorf("status bar = %q", got)
	}
	m.Update(key("d"))
	if got := status(); !strings.HasPrefix(got, "  DELETE ") {
		t.Errorf("status bar = %q", got)
	}
	m.Update(key("n"))
	m.Update(key("n"))
	if got := status(); !strings.HasPrefix(got, "  NEW PROJECT ") {
		t.Errorf("status bar = %q", got)
	}
}
