package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/proc"
)

// commandLine returns a shell command line that starts the project the way
// runr does, with extra arguments args: in dir (its directory if ""), with runr's
// default variables, its env_file and its env, run by the system shell. The
// rest of runr's own environment (PATH, HOME, …) is left to the shell it is
// pasted into.
func commandLine(p *config.Project, args, dir string) (string, error) {
	var err error
	if dir == "" {
		if dir, err = p.ResolvedDir(); err != nil {
			return "", err
		}
	}
	envFile, err := p.ResolvedEnvFile()
	if err != nil {
		return "", err
	}
	var fileVars map[string]string
	if envFile != "" {
		if fileVars, err = proc.LoadEnvFile(envFile); err != nil {
			return "", err
		}
	}
	return proc.CommandLine(dir, proc.BuildEnv(nil, proc.DefaultEnv, fileVars, p.Env), p.CommandWith(args)), nil
}

// copyCommand copies the project's command line, with extra arguments args
// and in dir (its directory if ""), to the clipboard (OSC 52) and returns the notice to show: the line itself,
// so it can also be read off the screen where the terminal doesn't support
// OSC 52.
func copyCommand(cfg *config.Config, name, args, dir string) (string, tea.Cmd) {
	p := cfg.Projects[name]
	if p == nil {
		return "", nil
	}
	line, err := commandLine(p, args, dir)
	if err != nil {
		return "can't copy the command of " + name + ": " + oneLine(err.Error()), nil
	}
	return "copied: " + line, tea.SetClipboard(line)
}
