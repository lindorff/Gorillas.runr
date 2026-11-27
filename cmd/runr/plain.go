package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/plaintext"
	"github.com/Skaanning/runr/internal/supervisor"
	"github.com/Skaanning/runr/internal/tui"
)

// plainColors is how plain output is colored.
type plainColors struct {
	profile colorprofile.Profile // below ANSI: no colors
	dark    bool                 // the terminal's background, for the theme's palette
}

// runPlain starts the projects, with extra arguments by project, and
// prints their output interleaved, one line per output line, until every
// project has finished. The first signal on sigs stops everything
// gracefully; a second one kills it.
func runPlain(out io.Writer, cfg *config.Config, opts supervisor.Options, names []string, args, dirs map[string]string, projects []string, sigs chan os.Signal, colors plainColors) error {
	pr := newPlainPrinter(out, cfg, projects, colors)
	sess := newRecordingSession(cfg, opts)
	sup := sess.Supervisor
	if cols := pr.textWidth(); cols > 0 {
		_ = sup.Resize(cols, 0) // for pty = true; otherwise the default 120
	}
	if err := sess.begin(names, args, dirs); err != nil {
		sup.Close()
		return err
	}
	stopControl := serveControl(sess, cfg, names, sigs)

	latest := map[string]supervisor.ProcessInfo{}
	events := sup.Events()
	var stopped <-chan struct{}
	userStopped := false

loop:
	for {
		select {
		case e := <-events:
			pr.event(e)
			if se, ok := e.(supervisor.StateEvent); ok {
				latest[se.Info.Name] = se.Info
				if stopped == nil && allFinished(projects, latest) {
					break loop
				}
			}
		case <-sigs:
			if stopped == nil {
				userStopped = true
				pr.notice("stopping… (press Ctrl+C again to force)")
				stopped = sup.StopAll()
			} else {
				pr.notice("forcing shutdown")
				sup.KillAll()
			}
		case <-stopped:
			break loop
		}
	}

	sup.Close()
	stopControl()           // after the last lines, for followers
	for e := range events { // remaining events, e.g. final "stopped" lines
		pr.event(e)
		if se, ok := e.(supervisor.StateEvent); ok {
			latest[se.Info.Name] = se.Info
		}
	}

	if userStopped {
		return nil
	}
	return failures(projects, latest)
}

// allFinished reports whether every project has reached a final state.
func allFinished(projects []string, latest map[string]supervisor.ProcessInfo) bool {
	for _, name := range projects {
		switch latest[name].State {
		case supervisor.Exited, supervisor.Crashed, supervisor.Stopped, supervisor.Blocked:
		default:
			return false // includes projects with no state event yet (Pending)
		}
	}
	return true
}

// failures returns an error listing projects that crashed or were blocked.
func failures(projects []string, latest map[string]supervisor.ProcessInfo) error {
	var failed []string
	for _, name := range projects {
		info := latest[name]
		switch info.State {
		case supervisor.Crashed:
			failed = append(failed, fmt.Sprintf("%s (%s)", name, info.Exit))
		case supervisor.Blocked:
			failed = append(failed, name+" (blocked)")
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

// plainPrinter formats supervisor events as prefixed lines.
type plainPrinter struct {
	w       io.Writer
	color   bool
	width   int
	labels  map[string]string // project name → padded, colored label
	noticeP string
}

func newPlainPrinter(w io.Writer, cfg *config.Config, projects []string, colors plainColors) *plainPrinter {
	p := &plainPrinter{w: w, color: colors.profile >= colorprofile.ANSI, labels: map[string]string{}}
	for _, name := range projects {
		p.width = max(p.width, len(cfg.Projects[name].Label(name)))
	}
	p.width = max(p.width, len("runr"))

	for _, name := range projects {
		proj := cfg.Projects[name]
		// Named colors come from the theme, converted for the terminal.
		fg := colors.profile.Convert(tui.ProjectColor(cfg.Settings.Theme(), cfg.ColorOf(name), colors.dark))
		p.labels[name] = p.paint(strings.Join(ansi.Style{}.ForegroundColor(fg), ";"), pad(proj.Label(name), p.width))
	}
	p.noticeP = p.paint("1", pad("runr", p.width))
	return p
}

func (p *plainPrinter) event(e supervisor.Event) {
	le, ok := e.(supervisor.LineEvent)
	if !ok {
		return
	}
	label := p.labels[le.Line.Project]
	switch le.Line.Stream {
	case supervisor.System:
		fmt.Fprintf(p.w, "%s » %s\n", label, p.paint("2", le.Line.Text))
		return
	case supervisor.SystemError:
		fmt.Fprintf(p.w, "%s » %s\n", label, p.paint("31", le.Line.Text))
		return
	}
	text := le.Line.Text
	if p.color {
		text += "\x1b[0m" // keep the program's colors from bleeding into the next line
	} else {
		// Programs keep their colors (runr sets FORCE_COLOR), so remove them
		// when the output is a file or NO_COLOR is set.
		text = plaintext.StripANSI(text)
	}
	fmt.Fprintf(p.w, "%s │ %s\n", label, text)
}

// textWidth is the room for a program's text after the label on a
// terminal, 0 if the output isn't one.
func (p *plainPrinter) textWidth() int {
	f, ok := p.w.(*os.File)
	if !ok || !term.IsTerminal(f.Fd()) {
		return 0
	}
	w, _, err := term.GetSize(f.Fd())
	if err != nil {
		return 0
	}
	return max(w-p.width-3, 20) // "label │ "
}

func (p *plainPrinter) notice(msg string) {
	fmt.Fprintf(p.w, "%s » %s\n", p.noticeP, msg)
}

// paint wraps s in an SGR sequence when color is enabled.
func (p *plainPrinter) paint(sgr, s string) string {
	if !p.color || sgr == "" {
		return s
	}
	return "\x1b[" + sgr + "m" + s + "\x1b[0m"
}

func pad(s string, width int) string {
	if n := width - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// colorEnabled reports whether w is a terminal and NO_COLOR is not set.
func colorEnabled(w io.Writer) bool {
	return os.Getenv("NO_COLOR") == "" && isTerminal(w)
}

// colorsFor returns how to color plain output to w: not at all unless
// colorEnabled; with the terminal's color profile otherwise. The background
// is assumed dark: asking the terminal blocks when it doesn't answer.
func colorsFor(w io.Writer) plainColors {
	if !colorEnabled(w) {
		return plainColors{}
	}
	return plainColors{profile: colorprofile.Detect(w, os.Environ()), dark: true}
}

// isTerminal reports whether f (a reader or writer) is a terminal.
func isTerminal(f any) bool {
	file, ok := f.(*os.File)
	return ok && term.IsTerminal(file.Fd())
}
