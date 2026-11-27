package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/control"
	"github.com/Skaanning/runr/internal/sessions"
	"github.com/Skaanning/runr/internal/supervisor"
	"github.com/Skaanning/runr/internal/tui"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

// buildVersion returns version, or for a binary built with
// `go install <module>@<version>` the module version Go recorded.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "runr",
		Short: "Run multiple local projects at once",
		Long: "runr runs multiple local projects (e.g. microservices) at once and shows\n" +
			"their output in a terminal UI. Run without arguments to open the launcher.",
		Version:       buildVersion(),
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLauncher(cmd)
		},
	}

	root.AddCommand(
		newUpCmd(),
		newAddCmd(),
		newEditCmd(),
		newRmCmd(),
		newRenameCmd(),
		newLsCmd(),
		newImportCmd(),
		newGroupCmd(),
		newThemeCmd(),
		newCleanupCmd(),
		newStatusCmd(),
		newLogsCmd(),
		newActionCmd(actions[0]),
		newActionCmd(actions[1]),
		newActionCmd(actions[2]),
		newDownCmd(),
		newAttachCmd(),
		newHostCmd(),
	)
	return root
}

// runLauncher opens the launcher. What the user starts from it runs in a
// session host, shown by the same program; sessions running elsewhere are
// listed to attach to.
func runLauncher(cmd *cobra.Command) error {
	if !isTerminal(cmd.InOrStdin()) || !isTerminal(cmd.OutOrStdout()) {
		return errors.New("the launcher needs a terminal; use runr up <name> instead")
	}
	s, err := loadStore()
	if err != nil {
		return err
	}
	if _, err := s.cfg.Validate(); err != nil {
		return err
	}
	// Processes left over by a runr that was killed are offered to be
	// stopped first.
	left := leftoversFor(findLeftovers(sessionsDir()), s.cfg, s.cfg.Settings.ShutdownTimeout())
	return runLauncherWith(cmd.ErrOrStderr(), s, nil, left)
}

// runLauncherWith runs the launcher on the config of s. With first set,
// that session's running view comes first (runr up, runr attach): d there
// shows the launcher, and left is asked about in the view, which then
// starts its projects; otherwise in the launcher.
func runLauncherWith(stderr io.Writer, s *store, first *control.Remote, left *tui.Leftovers) error {
	// Ctrl+C is q. A closed terminal (SIGHUP) or SIGTERM only ends the
	// view of a session in another process: whether it goes on is up to
	// the session (a host stops when its last view went away like that).
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	hangups := make(chan os.Signal, 2)
	signal.Notify(hangups, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(hangups)

	var sup *supervisor.Supervisor
	var rec *sessions.Recorder
	stopControl := func() {}
	start := func(names []string, args map[string]string) (tui.Session, error) {
		if _, err := prepareUp(s.cfg, names, io.Discard); err != nil {
			return nil, err
		}
		dir, err := logDir(s.cfg, "")
		if err != nil {
			return nil, err
		}
		if hostSupported {
			h, err := startHost(names, hostOptions{logDir: dir, args: args, forView: true})
			if err != nil {
				return nil, err
			}
			r, err := control.Attach(h.path)
			if err != nil {
				h.down()
				return nil, err
			}
			return r, nil
		}
		// In this process: its processes are recorded for runr cleanup,
		// in case runr is killed.
		opts := supervisor.Options{LogDir: dir}
		rec = track(sessionsDir(), &opts)
		sess := newRecordingSession(s.cfg, opts)
		if err := sess.begin(names, args, nil); err != nil {
			sess.Close()
			rec.Close()
			return nil, err
		}
		sup = sess.Supervisor
		stopControl = serveControl(sess, s.cfg, names, sigs)
		return sess, nil
	}
	// Forms edit a copy; once it validates and is written, the launcher
	// copies it into s.cfg, which start reads.
	save := func(c *config.Config) error {
		_, err := (&store{path: s.path, cfg: c}).save()
		return err
	}
	setup := tui.LauncherSetup{
		Config: s.cfg, Start: start, Save: save, Git: gitReader(), Last: loadLast(),
		Left: left, Background: newBackground(),
	}
	if first != nil {
		snap := first.Snapshot()
		setup.First = &tui.Attached{Session: first, Config: snap.Config, Titles: snap.Titles}
	}
	res, err := tui.RunLauncher(setup, sigs, hangups)
	defer reportDetached(stderr, res.Detached)
	if r, ok := res.Shown.(*control.Remote); ok {
		// The session runr was quit from, in another process.
		final := r.Processes()
		r.Close()
		return attachedEnd(stderr, r.Pid(), final, err)
	}
	if sup != nil {
		finish(stderr, sup)
	}
	stopControl() // after the last lines, for followers
	rec.Close()   // everything has stopped
	return err
}

// reportDetached says how to come back to the sessions the user detached
// from that still run.
func reportDetached(stderr io.Writer, pids []int) {
	if len(pids) == 0 {
		return
	}
	running := map[int]bool{}
	found, _ := control.Sessions(controlDir())
	for _, s := range found {
		running[s.Status.Pid] = true
	}
	reported := map[int]bool{}
	for _, pid := range pids {
		if running[pid] && !reported[pid] {
			reported[pid] = true
			fmt.Fprintf(stderr, "runr: detached from session %d; runr attach to come back\n", pid)
		}
	}
}
