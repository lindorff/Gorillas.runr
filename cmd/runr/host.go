package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/control"
	"github.com/Skaanning/runr/internal/supervisor"
)

// A session host (runr host) runs a session in the background, with no
// terminal: views attach to it over its control socket. runr up and the
// launcher start one and attach to it, so that every session can be
// detached from (d) and left running.

// Timing, variables so tests can shorten them.
var (
	hostWait = 10 * time.Second // for a host runr started to listen
	viewWait = 10 * time.Second // for the view of a host started for one to attach
)

func newHostCmd() *cobra.Command {
	var logDirFlag, worktree string
	var argList []string
	var hold bool
	var viewWithin time.Duration
	cmd := &cobra.Command{
		Use:   "host <name...>",
		Short: "Run a session in the background, for views to attach to",
		Long: "Run a session with no terminal until it is stopped (runr down, q in a view) or,\n" +
			"once a view has attached, until the last view is closed without detaching.\n" +
			"runr up and the launcher start it; it isn't meant to be run by hand.",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, names []string) error {
			s, err := loadStore()
			if err != nil {
				return err
			}
			args, err := parseArgList(argList)
			if err != nil {
				return err
			}
			// Warnings were shown by the runr that started the host.
			_, dirs, err := prepareUpIn(s.cfg, names, worktree, io.Discard)
			if err != nil {
				return err
			}
			dir, err := logDir(s.cfg, logDirFlag)
			if err != nil {
				return err
			}
			opts := supervisor.Options{LogDir: dir}
			rec := track(sessionsDir(), &opts) // found by runr cleanup if the host is killed
			defer rec.Close()                  // runs after everything has stopped

			// There is no terminal (nor its SIGHUP): signals come from runr
			// down (as Quit) or from kill.
			sigs := make(chan os.Signal, 2)
			signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sigs)
			if err := runHost(s.cfg, opts, names, args, dirs, hold, viewWithin, sigs); err != nil {
				return err
			}
			removeHostLog(os.Getpid()) // nothing went wrong: nothing to keep
			return nil
		},
	}
	cmd.Flags().StringVar(&logDirFlag, "log-dir", "", "append each project's output to <dir>/<project>.log")
	cmd.Flags().StringArrayVar(&argList, "arg", nil, "extra arguments of a project, as name=args")
	cmd.Flags().StringVar(&worktree, "worktree", "", "run the projects of that work tree's repository from it (see runr up)")
	cmd.Flags().BoolVar(&hold, "hold", false, "start nothing until a view starts it (after the leftovers question)")
	cmd.Flags().DurationVar(&viewWithin, "view-within", 0, "stop if no view has attached by then (0: never)")
	return cmd
}

// parseArgList turns name=args items into extra arguments by project.
func parseArgList(items []string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range items {
		name, args, ok := strings.Cut(item, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--arg %q: want name=args", item)
		}
		out[name] = args
	}
	return out, nil
}

// runHost starts names (with hold, only sets their extra arguments and
// directories) and
// serves the session until it is stopped: by a signal on sigs (runr down
// sends one, a second one kills), by a view (q), or by the last view going
// away without detaching. With viewWithin, it stops if no view has
// attached by then.
func runHost(cfg *config.Config, opts supervisor.Options, names []string, args, dirs map[string]string, hold bool, viewWithin time.Duration, sigs chan os.Signal) error {
	sess := newRecordingSession(cfg, opts)
	sup := sess.Supervisor
	var err error
	if hold {
		err = sess.configure(args, dirs)
	} else {
		err = sess.begin(names, args, dirs)
	}
	if err != nil {
		sup.Close()
		return err
	}
	quit := quitter(sigs)
	srv, err := listenControl(sess, cfg, names, quit, true)
	if err != nil {
		sup.Close()
		return fmt.Errorf("control socket: %w", err)
	}
	if viewWithin > 0 {
		t := time.AfterFunc(viewWithin, func() {
			if !srv.Viewed() {
				quit(false)
			}
		})
		defer t.Stop()
	}

	events := sup.Events() // views get theirs through Subscribe
	var stopped <-chan struct{}
loop:
	for {
		select {
		case <-events:
		case <-sigs:
			if stopped == nil {
				stopped = sup.StopAll()
			} else {
				sup.KillAll()
			}
		case <-stopped:
			break loop
		}
	}
	sup.Close()
	for range events {
	}
	srv.Close() // after the last events, for the views
	return nil
}

// hostsDir is where hosts write what goes wrong (their stderr).
func hostsDir() (string, error) {
	dir := stateDir()
	if dir == "" {
		return "", errors.New("runr's state directory can't be found")
	}
	return filepath.Join(dir, "hosts"), nil
}

func hostLogPath(dir string, pid int) string {
	return filepath.Join(dir, strconv.Itoa(pid)+".log")
}

// removeHostLog removes the log of the host with pid.
func removeHostLog(pid int) {
	if dir, err := hostsDir(); err == nil {
		os.Remove(hostLogPath(dir, pid))
	}
}

// pruneHostLogs removes the empty logs of hosts that are gone (killed:
// one that ends normally removes its own). Logs with something in them,
// e.g. a crash, are kept.
func pruneHostLogs(logs string) {
	entries, _ := os.ReadDir(logs)
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".log")
		pid, err := strconv.Atoi(name)
		if !ok || err != nil || processAlive(pid) {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.Size() == 0 && time.Since(fi.ModTime()) > time.Minute {
			os.Remove(filepath.Join(logs, e.Name()))
		}
	}
}

// host is a session host this runr started.
type host struct {
	pid     int
	process *os.Process
	path    string // of its control socket
	log     string // its stderr
	exited  chan struct{}
	err     error // how it ended, once exited is closed
}

// hostOptions are what a host is started with besides the names.
type hostOptions struct {
	logDir   string            // for persistent logs; "": none
	args     map[string]string // extra arguments by project
	worktree string            // absolute; run the projects of its repository from it (see worktreeDirs); "": none
	hold     bool              // a view starts the projects (see runHost)
	forView  bool              // a view will attach right away: stop if none does
}

// startHost starts a session host for names in a session of its own,
// detached from the terminal, and returns once it listens. If it ends
// before that, its error output is the error.
func startHost(names []string, o hostOptions) (*host, error) {
	ctl, err := control.Dir()
	if err != nil {
		return nil, err
	}
	logs, err := hostsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(logs, 0o700); err != nil {
		return nil, err
	}
	pruneHostLogs(logs)
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	argv := []string{"host"}
	if o.logDir != "" {
		argv = append(argv, "--log-dir="+o.logDir)
	}
	for _, name := range slices.Sorted(maps.Keys(o.args)) {
		argv = append(argv, "--arg="+name+"="+o.args[name])
	}
	if o.worktree != "" {
		argv = append(argv, "--worktree="+o.worktree)
	}
	if o.hold {
		argv = append(argv, "--hold")
	}
	if o.forView {
		argv = append(argv, "--view-within="+viewWait.String())
	}
	argv = append(argv, "--")
	argv = append(argv, names...)

	// The log is named after the host's pid once it is known.
	f, err := createHostLog(logs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var c *exec.Cmd
	for _, attr := range hostAttrs() {
		c = exec.Command(exe, argv...)
		c.Stderr = f // stdin and stdout: the null device
		c.SysProcAttr = attr
		if err = c.Start(); err == nil {
			break
		}
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	h := &host{pid: c.Process.Pid, process: c.Process, log: hostLogPath(logs, c.Process.Pid), exited: make(chan struct{})}
	if os.Rename(f.Name(), h.log) != nil {
		h.log = f.Name()
	}
	go func() {
		h.err = c.Wait()
		close(h.exited)
	}()
	if h.path, err = control.SocketPath(ctl, h.pid); err != nil {
		c.Process.Kill()
		return nil, err
	}
	if err := h.waitListening(); err != nil {
		return nil, err
	}
	return h, nil
}

// waitListening waits until the host answers on its socket, or ends.
func (h *host) waitListening() error {
	deadline := time.Now().Add(hostWait)
	for {
		select {
		case <-h.exited:
			return h.failure()
		case <-time.After(20 * time.Millisecond):
		}
		if control.Do(h.path, control.Request{Op: control.OpStatus}, func(control.Reply) error { return nil }) == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the session didn't start within %v (its log: %s)", hostWait, h.log)
		}
	}
}

// failure is why the host ended before it listened: its error output,
// which is removed then.
func (h *host) failure() error {
	b, _ := os.ReadFile(h.log)
	os.Remove(h.log)
	text := strings.TrimSpace(string(b))
	if text == "" {
		return fmt.Errorf("the session ended at once (%v)", h.err)
	}
	// Its own "runr: " is added again by main.
	return errors.New(strings.TrimPrefix(text, "runr: "))
}

// down stops the host's session and waits until it has.
func (h *host) down() {
	control.Do(h.path, control.Request{Op: control.OpDown}, func(control.Reply) error { return nil })
	<-h.exited
}
