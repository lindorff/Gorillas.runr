package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/control"
	"github.com/Skaanning/runr/internal/gitinfo"
	"github.com/Skaanning/runr/internal/sessions"
	"github.com/Skaanning/runr/internal/supervisor"
	"github.com/Skaanning/runr/internal/tui"
)

func newUpCmd() *cobra.Command {
	var plain, last, detach, inProcess bool
	var logDirFlag, worktree string
	cmd := &cobra.Command{
		Use:   "up <name...>",
		Short: "Start groups and/or projects",
		Long: "Start the given groups and/or projects, plus everything they depend on, and show\n" +
			"the running view. d there leaves the session running in the background (runr\n" +
			"attach shows it again); closing the terminal without d stops it, as q does.\n\n" +
			"With -d, start the session in the background without showing it.\n" +
			"With --last, start what the last session started, with the same extra arguments.\n\n" +
			"With --worktree <dir>, the projects in the same git repository as <dir> run from\n" +
			"that work tree instead: from the same place under its top-level directory, e.g.\n" +
			"~/wt/fix/src/api for a project in ~/code/app/src/api. Restarts keep running\n" +
			"there. Projects of other repositories run as configured.",
		Args: func(cmd *cobra.Command, args []string) error {
			if last {
				if len(args) > 0 {
					return errors.New("--last starts the last session; leave out the names")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		ValidArgsFunction: completeProjectsAndGroups,
		RunE: func(cmd *cobra.Command, args []string) error {
			if detach && (plain || inProcess) {
				return errors.New("-d runs the session in the background; leave out --plain and --in-process")
			}
			if detach && !hostSupported {
				return errors.New("-d isn't supported on this system yet")
			}
			s, err := loadStore()
			if err != nil {
				return err
			}
			var extra map[string]string
			if last {
				if args, extra, err = lastSession(s.cfg, cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			if worktree != "" {
				// The host runs elsewhere: it gets the absolute path.
				if worktree, err = filepath.Abs(worktree); err != nil {
					return err
				}
			}
			projects, dirs, err := prepareUpIn(s.cfg, args, worktree, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			dir, err := logDir(s.cfg, logDirFlag)
			if err != nil {
				return err
			}
			// Processes left over by a runr that was killed are offered to
			// be stopped first.
			sessDir := sessionsDir()
			orphans := findLeftovers(sessDir)
			stderr := cmd.ErrOrStderr()
			// The TUI needs a terminal for both keys and drawing; otherwise
			// (e.g. piped to a file) behave like --plain.
			tty := isTerminal(cmd.InOrStdin()) && isTerminal(cmd.OutOrStdout())
			if detach || plain || !tty {
				if len(orphans) > 0 {
					fmt.Fprintf(stderr, "runr: %s; left running (runr cleanup stops them)\n", sessions.Summary(orphans))
				}
			}
			left := leftoversFor(orphans, s.cfg, s.cfg.Settings.ShutdownTimeout())
			switch {
			case detach:
				h, err := startHost(args, hostOptions{logDir: dir, args: extra, worktree: worktree})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "started session %d · runr attach to watch it\n", h.pid)
				return nil
			case !plain && tty && !inProcess && hostSupported:
				return upHosted(stderr, args, hostOptions{logDir: dir, args: extra, worktree: worktree, hold: left != nil, forView: true}, left)
			}

			// In this process: this session's processes are recorded for
			// runr cleanup, in case runr is killed.
			opts := supervisor.Options{LogDir: dir}
			rec := track(sessDir, &opts)
			defer rec.Close() // runs after everything has stopped
			sigs := make(chan os.Signal, 2)
			signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			defer signal.Stop(sigs)
			if plain || !tty {
				return runPlain(cmd.OutOrStdout(), s.cfg, opts, args, extra, dirs, projects, sigs, colorsFor(cmd.OutOrStdout()))
			}
			return runTUI(stderr, s.cfg, opts, args, extra, dirs, left, sigs)
		},
	}
	cmd.Flags().BoolVar(&last, "last", false, "start what the last session started, with the same extra arguments")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "start the session in the background (runr attach shows it)")
	cmd.Flags().StringVar(&logDirFlag, "log-dir", "", "append each project's output to <dir>/<project>.log (overrides settings.log_dir)")
	_ = cmd.MarkFlagDirname("log-dir")
	cmd.Flags().StringVar(&worktree, "worktree", "", "run the projects of the git repository <dir> is in from that work tree")
	_ = cmd.MarkFlagDirname("worktree")
	// Plain interleaved output, for debugging and non-interactive use.
	cmd.Flags().BoolVar(&plain, "plain", false, "print interleaved output instead of opening the TUI")
	_ = cmd.Flags().MarkHidden("plain")
	// The session inside this runr, not detachable (as before T59): for
	// debugging.
	cmd.Flags().BoolVar(&inProcess, "in-process", false, "run the session inside this runr (not detachable)")
	_ = cmd.Flags().MarkHidden("in-process")
	return cmd
}

// upHosted starts names in a session host and shows its running view. With
// left set, the host waits: the view asks about the leftover processes,
// then starts the projects.
func upHosted(stderr io.Writer, names []string, o hostOptions, left *tui.Leftovers) error {
	h, err := startHost(names, o)
	if err != nil {
		return err
	}
	r, err := control.Attach(h.path)
	if err != nil {
		h.down()
		return err
	}
	return runAttached(stderr, r, left)
}

// runTUI starts the projects, with extra arguments and directories by
// project, and shows
// the running view until the user quits. Projects that ended crashed or
// blocked are listed on stderr afterwards, since the TUI's screen is gone
// by then.
//
// With left set, the projects start once the question about those
// leftover processes is answered.
func runTUI(stderr io.Writer, cfg *config.Config, opts supervisor.Options, names []string, args, dirs map[string]string, left *tui.Leftovers, sigs chan os.Signal) error {
	sess := newRecordingSession(cfg, opts)
	stopControl := serveControl(sess, cfg, names, sigs)
	var err error
	if left == nil {
		err = sess.begin(names, args, dirs)
	} else {
		err = sess.configure(args, dirs)
	}
	if err == nil {
		err = tui.Run(sess, cfg, names, gitReader(), left, sigs)
	}
	finish(stderr, sess.Supervisor)
	stopControl() // after the last lines, for followers
	return err
}

// logDir returns the directory for persistent logs: flag if set, else
// settings.log_dir, with ~ expanded and created if needed; "" for none.
func logDir(cfg *config.Config, flag string) (string, error) {
	dir, err := cfg.Settings.LogDir()
	if flag != "" {
		dir, err = config.ExpandHome(flag)
	}
	if err != nil || dir == "" {
		return "", err
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("log dir: %w", err)
	}
	return dir, nil
}

// gitReader returns the reader for the TUI's git display, or nil (hiding
// it) when git is not installed.
func gitReader() tui.GitReader {
	g, err := gitinfo.New()
	if err != nil {
		return nil
	}
	return g
}

// finish shuts a supervisor down after the TUI has exited and reports
// projects that ended crashed or blocked.
func finish(stderr io.Writer, sup *supervisor.Supervisor) {
	final := sup.Processes()
	sup.Close() // kills anything still alive if the TUI failed
	for range sup.Events() {
	}
	if s := summarize(final); s != "" {
		fmt.Fprintf(stderr, "runr: %s\n", s)
	}
}

// summarize describes the projects that ended crashed or blocked, or
// returns "" if there are none.
func summarize(infos []supervisor.ProcessInfo) string {
	var parts []string
	for _, info := range infos {
		switch info.State {
		case supervisor.Crashed:
			parts = append(parts, fmt.Sprintf("%s crashed (%s)", info.Name, info.Exit))
		case supervisor.Blocked:
			parts = append(parts, info.Name+" was blocked")
		}
	}
	return strings.Join(parts, ", ")
}

// prepareUp validates the config and checks that every project that would be
// started can be: names resolve and working directories exist. It returns
// the resolved projects in start order.
func prepareUp(cfg *config.Config, names []string, stderr io.Writer) ([]string, error) {
	projects, _, err := prepareUpIn(cfg, names, "", stderr)
	return projects, err
}

// prepareUpIn is prepareUp for starting in a git worktree (none if ""):
// it also returns the directories of the projects that run in it (see
// worktreeDirs), whose configured dirs then don't have to exist.
func prepareUpIn(cfg *config.Config, names []string, worktree string, stderr io.Writer) ([]string, map[string]string, error) {
	warnings, err := cfg.Validate()
	if err != nil {
		return nil, nil, err
	}
	projects, err := cfg.Resolve(names...)
	if err != nil {
		return nil, nil, err
	}
	if len(projects) == 0 {
		return nil, nil, fmt.Errorf("nothing to start: %s contains no projects", strings.Join(names, ", "))
	}
	var dirs map[string]string
	if worktree != "" {
		if dirs, err = worktreeDirs(cfg, projects, worktree); err != nil {
			return nil, nil, err
		}
	}

	inSet := map[string]bool{}
	for _, name := range projects {
		inSet[name] = true
	}
	var missing []string
	for _, w := range warnings {
		if !inSet[w.Name] || w.Field == "dir" && dirs[w.Name] != "" {
			continue
		}
		if w.Field == "dir" {
			missing = append(missing, w.String())
		} else {
			fmt.Fprintf(stderr, "warning: %s\n", w)
		}
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("cannot start:\n  %s", strings.Join(missing, "\n  "))
	}
	return projects, dirs, nil
}

// completeProjectsAndGroups offers every project and group name.
func completeProjectsAndGroups(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	s, err := loadStore()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := append(sortedNames(s.cfg.Groups), sortedNames(s.cfg.Projects)...)
	return names, cobra.ShellCompDirectiveNoFileComp
}
