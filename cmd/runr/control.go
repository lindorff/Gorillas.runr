package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/control"
	"github.com/Skaanning/runr/internal/logfile"
	"github.com/Skaanning/runr/internal/plaintext"
	"github.com/Skaanning/runr/internal/supervisor"
	"github.com/Skaanning/runr/internal/tui"
)

// controlDir is where sessions put their control sockets, or "" if runr's
// state directory can't be found (then sessions can't be reached).
func controlDir() string {
	dir, err := control.Dir()
	if err != nil {
		return ""
	}
	return dir
}

// serveControl lets runr commands in other terminals reach sess, which was
// started with titles, until the returned function is called. `runr down`
// sends a signal on sigs, as Ctrl+C does (force: a second one). Nothing
// happens if the socket can't be created.
func serveControl(sess recordingSession, cfg *config.Config, titles []string, sigs chan<- os.Signal) (stop func()) {
	srv, err := listenControl(sess, cfg, titles, quitter(sigs), false)
	if err != nil {
		return func() {}
	}
	return srv.Close
}

// listenControl serves sess, which was started with titles, on its control
// socket. quit stops the session. A host's session also stops when the
// last view attached to it goes away without detaching.
func listenControl(sess recordingSession, cfg *config.Config, titles []string, quit func(force bool), host bool) (*control.Server, error) {
	dir, err := control.Dir()
	if err != nil {
		return nil, err
	}
	labels := map[string]string{} // copied: the launcher's forms change cfg
	for name, p := range cfg.Projects {
		labels[name] = p.Label(name)
	}
	info := control.Info{
		Pid:     os.Getpid(),
		Started: time.Now(),
		Titles:  append([]string(nil), titles...),
		Labels:  labels,
		Quit:    quit,
		Config:  copyConfig(cfg),
		Host:    host,
	}
	if host {
		info.Dropped = func() { quit(false) }
	}
	return control.Listen(dir, sess, info)
}

// copyConfig returns a deep copy of cfg, for attached views: the
// launcher's forms change cfg later.
func copyConfig(cfg *config.Config) *config.Config {
	b, err := json.Marshal(cfg)
	out := config.New()
	if err == nil {
		json.Unmarshal(b, out)
	}
	return out
}

// quitter returns what stops the session for `runr down`: the first call
// sends one signal on sigs (stop gracefully), a forced call makes it two
// (kill). Repeating a call sends nothing more, so a second `runr down`
// doesn't turn into a kill.
func quitter(sigs chan<- os.Signal) func(force bool) {
	var mu sync.Mutex
	sent := 0
	return func(force bool) {
		mu.Lock()
		defer mu.Unlock()
		want := 1
		if force {
			want = 2
		}
		for ; sent < want; sent++ {
			select {
			case sigs <- os.Interrupt:
			default:
				return // full: signals are on their way already
			}
		}
	}
}

// sessionFlag adds --session, for choosing between several sessions.
func sessionFlag(cmd *cobra.Command, pid *int) {
	cmd.Flags().IntVar(pid, "session", 0, "the session to use, by runr's pid (see runr status), when several run")
}

// runningSessions returns the running sessions; problems reaching some are
// reported on stderr.
func runningSessions(stderr io.Writer) []control.Session {
	found, problems := control.Sessions(controlDir())
	for _, p := range problems {
		fmt.Fprintf(stderr, "runr: warning: %v\n", p)
	}
	return found
}

// expandGroups replaces the groups among names by their projects, as far
// as the config can be read.
func expandGroups(names []string) []string {
	s, err := loadStore()
	if err != nil {
		return names
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		members := []string{n}
		if g, ok := s.cfg.Groups[n]; ok && s.cfg.Projects[n] == nil {
			members = g
		}
		for _, m := range members {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	var pid int
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the processes of the running sessions",
		Long: "Show every runr session running in another terminal, and the state of its\n" +
			"processes. --json prints the same as JSON, for scripts.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			found := runningSessions(cmd.ErrOrStderr())
			if pid != 0 {
				s, err := control.Pick(found, nil, pid)
				if err != nil {
					return err
				}
				found = []control.Session{s}
			}
			if len(found) == 0 {
				return control.ErrNoSession
			}
			if asJSON {
				return printStatusJSON(cmd.OutOrStdout(), found)
			}
			printStatus(cmd.OutOrStdout(), found, time.Now())
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	sessionFlag(cmd, &pid)
	return cmd
}

func printStatusJSON(w io.Writer, sessions []control.Session) error {
	out := make([]control.Status, len(sessions))
	for i, s := range sessions {
		out[i] = s.Status
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// printStatus prints each session's header and a table of its processes.
func printStatus(w io.Writer, sessions []control.Session, now time.Time) {
	for i, s := range sessions {
		if i > 0 {
			fmt.Fprintln(w)
		}
		st := s.Status
		header := "session " + strconv.Itoa(st.Pid)
		if len(st.Titles) > 0 {
			header += " · " + strings.Join(st.Titles, ", ")
		}
		fmt.Fprintf(w, "%s · started %s · attached: %d\n", header, startedAt(st.Started.Local(), now), st.Attached)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATE\tUPTIME\tPID\tPORTS\tERRORS\tRESTARTS")
		for _, p := range st.Processes {
			state, up, pidText, portsText := p.State, "-", "-", "-"
			if p.Exit != "" {
				state += " (" + p.Exit + ")"
			}
			if p.Pid != 0 {
				up, pidText = tui.Uptime(now.Sub(p.StartedAt)), strconv.Itoa(p.Pid)
			}
			if len(p.Ports) > 0 {
				parts := make([]string, len(p.Ports))
				for i, port := range p.Ports {
					parts[i] = ":" + strconv.Itoa(port)
				}
				portsText = strings.Join(parts, " ")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n", p.Name, state, up, pidText, portsText, p.Errors, p.Restarts)
		}
		tw.Flush()
	}
}

// startedAt formats when a session started: the time if it was today,
// with the date otherwise.
func startedAt(t, now time.Time) string {
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// errMatched ends a follow once a line matches --until.
var errMatched = errors.New("a line matched")

func newLogsCmd() *cobra.Command {
	var lines, pid int
	var follow, errorsOnly bool
	var until string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "logs [name...]",
		Short: "Print the output of a running session's projects",
		Long: "Print the most recent output of projects in a runr session running in another\n" +
			"terminal: the given projects and groups, or all of them, interleaved as in the\n" +
			"combined view, in plain text. -f keeps printing new lines until Ctrl+C, the\n" +
			"session ends or nothing reads them any more (| head, | grep -m1).\n\n" +
			"--until follows until a new line matches a regular expression, then exits;\n" +
			"with an error if none has within --timeout or the session ends. Only lines\n" +
			"that arrive after runr logs starts count, not the ones printed before them,\n" +
			"so an earlier run's line can't match. The expression is matched against a\n" +
			"line's text, without the time and label; runr's own lines count too, e.g.\n" +
			"--until '^(exited|crashed)' to wait until a job is done.",
		ValidArgsFunction: completeProjectsAndGroups,
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 0 {
				return errors.New("--lines can't be negative")
			}
			var re *regexp.Regexp
			if cmd.Flags().Changed("until") {
				var err error
				if re, err = regexp.Compile(until); err != nil {
					return fmt.Errorf("--until: %w", err)
				}
				follow = true
			} else if cmd.Flags().Changed("timeout") {
				return errors.New("--timeout needs --until")
			}
			since := time.Now()
			names := expandGroups(args)
			sess, err := control.Pick(runningSessions(cmd.ErrOrStderr()), names, pid)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithCancelCause(cmd.Context())
			defer cancel(nil)
			if re != nil && timeout > 0 {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeoutCause(ctx, timeout, fmt.Errorf("no line matched %q within %v", until, timeout))
				defer stop()
			}
			out := cmd.OutOrStdout()
			if follow {
				watchOutput(ctx, out, func() { cancel(errOutputGone) })
			}
			labels := logLabels(sess.Status, names)
			w := bufio.NewWriter(out)
			req := control.Request{Op: control.OpLogs, Names: names, Tail: lines, Errors: errorsOnly, Follow: follow}
			err = control.DoContext(ctx, sess.Path, req, func(r control.Reply) error {
				l := r.Line
				if l != nil {
					w.WriteString(logfile.Format(l.Time.Local(), labels[l.Project], l.Runr, l.Text))
					w.WriteByte('\n')
					if re != nil && !l.Time.Before(since) && re.MatchString(plaintext.StripANSI(l.Text)) {
						return errMatched
					}
				}
				if follow {
					return w.Flush()
				}
				return nil
			})
			matched := errors.Is(err, errMatched)
			if matched {
				err = nil
			}
			if ferr := w.Flush(); err == nil {
				err = ferr
			}
			if err != nil && ctx.Err() == nil {
				err = outputError(out, err) // e.g. a failed write, which Do passes on
			}
			switch {
			case errors.Is(err, errOutputGone) && re != nil:
				return fmt.Errorf("no line matched %q before the output was closed", until)
			case errors.Is(err, errOutputGone):
				return nil // what head and grep -m1 wanted was printed
			case err != nil || matched || !follow:
				return err
			case re != nil:
				return fmt.Errorf("the session ended before a line matched %q", until)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "runr: the session ended")
			return nil
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "print the last this many lines (0: all that are kept)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines")
	cmd.Flags().BoolVar(&errorsOnly, "errors", false, "only lines that look like errors (settings.error_pattern) and crashes")
	cmd.Flags().StringVar(&until, "until", "", "follow until a new line matches this regular expression, then exit")
	cmd.Flags().DurationVar(&timeout, "timeout", time.Minute, "with --until, give up after this long (0: never)")
	sessionFlag(cmd, &pid)
	return cmd
}

// logLabels returns the labels for log lines by project, padded to align:
// none when there is only one project.
func logLabels(st control.Status, names []string) map[string]string {
	if len(names) == 1 || len(st.Processes) == 1 {
		return nil
	}
	labels := map[string]string{}
	width := 0
	for _, p := range st.Processes {
		labels[p.Name] = p.Label
		width = max(width, utf8.RuneCountInString(p.Label))
	}
	for name, l := range labels {
		labels[name] = pad(l, width)
	}
	return labels
}

// action is what runr start, stop and restart do.
type action struct {
	op, verb string // e.g. "restart", "restarted"
	short    string
	long     string
}

var actions = []action{
	{control.OpStart, "started", "Start projects in a running session",
		"Start projects or groups, plus what they depend on, in a runr session running in\n" +
			"another terminal; they don't have to be part of it yet."},
	{control.OpStop, "stopped", "Stop projects in a running session",
		"Stop projects or groups (their projects only) in a runr session running in\n" +
			"another terminal, as s does there."},
	{control.OpRestart, "restarted", "Restart projects in a running session",
		"Restart projects or groups (their projects only) in a runr session running in\n" +
			"another terminal, as r does there."},
}

// waitTail is how many of a project's last lines are printed when waiting
// for it fails.
const waitTail = 20

// pollEvery is how often --wait asks for the states. A variable so tests
// can shorten it.
var pollEvery = 100 * time.Millisecond

func newActionCmd(a action) *cobra.Command {
	var wait bool
	var timeout time.Duration
	var pid int
	cmd := &cobra.Command{
		Use:   a.op + " <name...>",
		Short: a.short,
		Long: a.long + "\n\nWith --wait, runr waits until they are ready (stopped, for stop), and exits\n" +
			"with an error, printing a project's last lines, if one crashes, is blocked or\n" +
			"isn't done before --timeout.",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completeProjectsAndGroups,
		RunE: func(cmd *cobra.Command, args []string) error {
			targets := expandGroups(args)
			sessions := runningSessions(cmd.ErrOrStderr())
			sess, err := control.Pick(sessions, targets, pid)
			var noMatch *control.NoMatchError
			if a.op == control.OpStart && errors.As(err, &noMatch) {
				// Starting adds projects to a session.
				if len(sessions) > 1 {
					return fmt.Errorf("%w; choose one with --session", err)
				}
				sess, err = sessions[0], nil
			}
			if err != nil {
				return err
			}
			names := targets
			if a.op == control.OpStart {
				names = args // groups too: the session pulls in dependencies
			}
			if err := control.Do(sess.Path, control.Request{Op: a.op, Names: names}, func(control.Reply) error { return nil }); err != nil {
				return err
			}
			if !wait {
				return nil
			}
			return waitFor(cmd.OutOrStdout(), sess.Path, a.op, targets, timeout)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until they are ready (stop: stopped) and report how it went")
	cmd.Flags().DurationVar(&timeout, "timeout", time.Minute, "with --wait, give up after this long")
	sessionFlag(cmd, &pid)
	return cmd
}

// outcome tells whether a process is done with what op asked of it, and
// whether that went wrong.
func outcome(op string, p control.Process) (done, failed bool) {
	state := p.State
	if op == control.OpStop {
		switch state {
		case supervisor.Starting.String(), supervisor.Running.String(), supervisor.Stopping.String(), supervisor.Waiting.String():
			return false, false
		}
		return true, false
	}
	switch state {
	case supervisor.Running.String(), supervisor.Exited.String():
		return true, false
	case supervisor.Crashed.String(), supervisor.Blocked.String(), supervisor.Stopped.String():
		return true, true
	}
	return false, false
}

// describe says how a process is, e.g. "api running (pid 4130, :8080)" or
// "api crashed (exit code 1)".
func describe(p control.Process) string {
	var details []string
	if p.Pid != 0 {
		details = append(details, "pid "+strconv.Itoa(p.Pid))
	}
	for _, port := range p.Ports {
		details = append(details, ":"+strconv.Itoa(port))
	}
	if p.Exit != "" {
		details = append(details, p.Exit)
	}
	s := p.Name + " " + p.State
	if len(details) > 0 {
		s += " (" + strings.Join(details, ", ") + ")"
	}
	return s
}

// waitFor reports each of names once op is done with it, until all are or
// timeout passes. Each one that failed gets its last lines printed, and
// makes the result an error.
func waitFor(w io.Writer, path, op string, names []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pending := append([]string(nil), names...)
	var failed []string
	for {
		var st control.Status
		err := control.Do(path, control.Request{Op: control.OpStatus}, func(r control.Reply) error {
			if r.Status != nil {
				st = *r.Status
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("the session ended while waiting: %w", err)
		}
		var still []string
		for _, name := range pending {
			p, ok := findProcess(st, name)
			if !ok {
				return fmt.Errorf("%s is not in the session", name)
			}
			done, bad := outcome(op, p)
			switch {
			case !done && time.Now().After(deadline):
				printTail(w, path, name)
				failed = append(failed, fmt.Sprintf("%s still %s after %v", name, p.State, timeout))
			case !done:
				still = append(still, name)
			case bad:
				printTail(w, path, name)
				failed = append(failed, describe(p))
			default:
				fmt.Fprintln(w, describe(p))
			}
		}
		if pending = still; len(pending) == 0 {
			break
		}
		time.Sleep(pollEvery)
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, "; "))
	}
	return nil
}

func findProcess(st control.Status, name string) (control.Process, bool) {
	for _, p := range st.Processes {
		if p.Name == name {
			return p, true
		}
	}
	return control.Process{}, false
}

// printTail prints a project's last lines, after a heading.
func printTail(w io.Writer, path, name string) {
	fmt.Fprintf(w, "--- last lines of %s ---\n", name)
	control.Do(path, control.Request{Op: control.OpLogs, Names: []string{name}, Tail: waitTail}, func(r control.Reply) error {
		if l := r.Line; l != nil {
			fmt.Fprintln(w, logfile.Format(l.Time.Local(), "", l.Runr, l.Text))
		}
		return nil
	})
}

func newDownCmd() *cobra.Command {
	var force bool
	var pid int
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop a running session",
		Long: "Stop a runr session running in another terminal, as q does there: everything\n" +
			"stops in reverse dependency order, and runr waits until it has. --force kills\n" +
			"the processes instead, like a second q.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sess, err := control.Pick(runningSessions(cmd.ErrOrStderr()), nil, pid)
			if err != nil {
				return err
			}
			if err := control.Do(sess.Path, control.Request{Op: control.OpDown, Force: force}, func(control.Reply) error { return nil }); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "session %d stopped\n", sess.Status.Pid)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "kill the processes instead of stopping them gracefully")
	sessionFlag(cmd, &pid)
	return cmd
}

func newAttachCmd() *cobra.Command {
	var pid int
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Open the running view of a session running elsewhere",
		Long: "Open the running view of a runr session running in another terminal or in the\n" +
			"background. q stops the session, as in its own terminal; d detaches and leaves it\n" +
			"running.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isTerminal(cmd.InOrStdin()) || !isTerminal(cmd.OutOrStdout()) {
				return errors.New("attach needs a terminal; use runr status and runr logs instead")
			}
			sess, err := control.Pick(runningSessions(cmd.ErrOrStderr()), nil, pid)
			if err != nil {
				return err
			}
			r, err := control.Attach(sess.Path)
			if err != nil {
				return err
			}
			return runAttached(cmd.ErrOrStderr(), r, nil)
		},
	}
	sessionFlag(cmd, &pid)
	return cmd
}

// runAttached shows the running view of an attached session until the
// user stops it or it ends, and the launcher once the user detaches. With left set, the session hasn't
// started its projects yet: the view asks about those leftover processes
// first, then starts them.
//
// d shows the launcher, which needs the config: if it can't be read, the
// view is shown alone, and d ends runr.
func runAttached(stderr io.Writer, r *control.Remote, left *tui.Leftovers) error {
	if s, err := loadStore(); err == nil {
		if _, err := s.cfg.Validate(); err == nil {
			return runLauncherWith(stderr, s, r, left)
		}
	}
	snap := r.Snapshot()
	// SIGINT is handled like q. A closed terminal (SIGHUP) or SIGTERM only
	// ends this view: whether the session goes on is up to the session.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	gone := make(chan os.Signal, 1)
	signal.Notify(gone, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(gone)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case sig := <-gone:
			r.Drop(fmt.Errorf("the view got %v and closed", sig))
		case <-done:
		}
	}()
	err := tui.Run(r, snap.Config, snap.Titles, gitReader(), left, sigs)
	final := r.Processes()
	r.Close()
	return attachedEnd(stderr, snap.Pid, final, err)
}

// attachedEnd reports how a view of the session with pid ended: detached,
// or the session ended with final processes (crashed and blocked ones are
// listed).
func attachedEnd(stderr io.Writer, pid int, final []supervisor.ProcessInfo, err error) error {
	switch {
	case errors.Is(err, tui.ErrDetached):
		fmt.Fprintf(stderr, "runr: detached from session %d; runr attach to come back\n", pid)
		return nil
	case err != nil:
		return err
	}
	if s := summarize(final); s != "" {
		fmt.Fprintf(stderr, "runr: %s\n", s)
	}
	return nil
}
