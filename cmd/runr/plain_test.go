//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/state"
	"github.com/Skaanning/runr/internal/supervisor"
)

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func plainConfig(t *testing.T, projects map[string]*config.Project) *config.Config {
	t.Helper()
	t.Setenv(state.EnvVar, t.TempDir()) // runPlain records the last session
	cfg := config.New()
	cfg.Settings.ShutdownTimeoutValue.Duration = 5 * time.Second
	dir := t.TempDir()
	for name, p := range projects {
		if p.Dir == "" {
			p.Dir = dir
		}
		cfg.Projects[name] = p
	}
	return cfg
}

// startPlain runs runPlain in the background and returns its output buffer,
// signal channel and result channel.
func startPlain(t *testing.T, cfg *config.Config, names ...string) (*syncBuffer, chan os.Signal, chan error) {
	t.Helper()
	projects, err := prepareUp(cfg, names, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	sigs := make(chan os.Signal, 2)
	result := make(chan error, 1)
	go func() {
		result <- runPlain(out, cfg, supervisor.Options{}, names, nil, nil, projects, sigs, plainColors{})
	}()
	return out, sigs, result
}

func waitResult(t *testing.T, result chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("runPlain did not return")
		return nil
	}
}

func waitOutput(t *testing.T, out *syncBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q:\n%s", substr, out)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPlainRunsToCompletion(t *testing.T) {
	cfg := plainConfig(t, map[string]*config.Project{
		"db":  {Cmd: "echo db up", ReadyLog: "up"},
		"api": {Cmd: "echo listening; echo warn >&2", DependsOn: []string{"db"}, DisplayName: "API"},
	})
	out, _, result := startPlain(t, cfg, "api")
	if err := waitResult(t, result); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{
		"db   » starting: echo db up\n", // labels are at least as wide as "runr"
		"db   │ db up\n",
		"db   » ready\n",
		"API  » waiting for db\n",
		"API  │ listening\n",
		"API  │ warn\n",
		"API  » exited (exit code 0)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestPlainReportsFailures(t *testing.T) {
	cfg := plainConfig(t, map[string]*config.Project{
		"db":  {Cmd: "exit 2", ReadyLog: "never"},
		"api": {Cmd: "true", DependsOn: []string{"db"}},
		"web": {Cmd: "true"},
	})
	out, _, result := startPlain(t, cfg, "api", "web")
	err := waitResult(t, result)
	if err == nil || err.Error() != "failed: db (exit code 2), api (blocked)" {
		t.Errorf("err = %v\n%s", err, out)
	}
}

func TestPlainStopsOnSignal(t *testing.T) {
	cfg := plainConfig(t, map[string]*config.Project{
		"db":  {Cmd: "trap 'echo db bye; exit 0' TERM; echo ready; while :; do sleep 0.05; done", ReadyLog: "ready"},
		"api": {Cmd: "trap 'echo api bye; exit 0' TERM; echo serving; while :; do sleep 0.05; done", DependsOn: []string{"db"}},
	})
	out, sigs, result := startPlain(t, cfg, "api")
	waitOutput(t, out, "api  │ serving")

	sigs <- os.Interrupt
	if err := waitResult(t, result); err != nil {
		t.Errorf("err = %v (a user stop is not a failure)", err)
	}

	got := out.String()
	stopping := strings.Index(got, "runr » stopping…")
	apiBye := strings.Index(got, "api  │ api bye")
	dbBye := strings.Index(got, "db   │ db bye")
	if stopping < 0 || apiBye < stopping || dbBye < apiBye {
		t.Errorf("expected stopping, then api, then db:\n%s", got)
	}
}

func TestPlainSecondSignalForces(t *testing.T) {
	cfg := plainConfig(t, map[string]*config.Project{
		"stubborn": {Cmd: "trap '' TERM; echo trapped; while :; do sleep 0.05; done"},
	})
	out, sigs, result := startPlain(t, cfg, "stubborn")
	waitOutput(t, out, "stubborn │ trapped") // not the "starting: …" line, which has it too

	start := time.Now()
	sigs <- os.Interrupt
	waitOutput(t, out, "stopping…")
	sigs <- os.Interrupt
	if err := waitResult(t, result); err != nil {
		t.Errorf("err = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("forced shutdown took %v (shutdown timeout is 5s)", elapsed)
	}
	if !strings.Contains(out.String(), "stubborn » stopped (signal: killed)") {
		t.Errorf("output:\n%s", out)
	}
}

func TestPrepareUp(t *testing.T) {
	dir := t.TempDir()
	cfg := plainConfig(t, map[string]*config.Project{
		"api":  {Dir: dir, Cmd: "true", EnvFile: "missing.env"},
		"gone": {Dir: filepath.Join(dir, "nope"), Cmd: "true"},
		"web":  {Dir: dir, Cmd: "true"},
	})
	cfg.Groups["empty"] = []string{}

	var stderr bytes.Buffer
	projects, err := prepareUp(cfg, []string{"api", "web"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(projects, ",") != "api,web" {
		t.Errorf("projects = %v", projects)
	}
	// Only warnings for projects being started are shown.
	if got := stderr.String(); !strings.Contains(got, `project "api": env_file`) || strings.Contains(got, "gone") {
		t.Errorf("stderr = %q", got)
	}

	if _, err := prepareUp(cfg, []string{"gone"}, &stderr); err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Errorf("missing dir: %v", err)
	}
	if _, err := prepareUp(cfg, []string{"empty"}, &stderr); err == nil || !strings.Contains(err.Error(), "nothing to start") {
		t.Errorf("empty group: %v", err)
	}
	if _, err := prepareUp(cfg, []string{"nope"}, &stderr); err == nil {
		t.Error("expected error for unknown name")
	}
}

var truecolor = plainColors{profile: colorprofile.TrueColor, dark: true}

func TestPlainPrinterColors(t *testing.T) {
	cfg := plainConfig(t, map[string]*config.Project{
		"api": {Cmd: "x", Color: "bright-red"},
		"db":  {Cmd: "x"},
		"web": {Cmd: "x", Color: "#ff8800"},
	})
	p := newPlainPrinter(&bytes.Buffer{}, cfg, []string{"db", "api", "web"}, truecolor)

	tests := map[string]string{
		"db":  "\x1b[36mdb  \x1b[0m",             // first default color: cyan
		"api": "\x1b[91mapi \x1b[0m",             // bright-red
		"web": "\x1b[38;2;255;136;0mweb \x1b[0m", // hex
	}
	for name, want := range tests {
		if got := p.labels[name]; got != want {
			t.Errorf("label %s = %q, want %q", name, got, want)
		}
	}
}

func TestPlainPrinterSystemLines(t *testing.T) {
	cfg := plainConfig(t, map[string]*config.Project{"db": {Cmd: "x", Color: "cyan"}})
	var out bytes.Buffer
	p := newPlainPrinter(&out, cfg, []string{"db"}, truecolor)
	p.event(supervisor.LineEvent{Line: supervisor.LogLine{Project: "db", Stream: supervisor.System, Text: "ready"}})
	p.event(supervisor.LineEvent{Line: supervisor.LogLine{Project: "db", Stream: supervisor.SystemError, Text: "crashed (exit code 1)"}})
	want := "\x1b[36mdb  \x1b[0m » \x1b[2mready\x1b[0m\n" +
		"\x1b[36mdb  \x1b[0m » \x1b[31mcrashed (exit code 1)\x1b[0m\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestPlainPrinterProgramColors(t *testing.T) {
	cfg := plainConfig(t, map[string]*config.Project{"db": {Cmd: "x", Color: "cyan"}})
	line := supervisor.LineEvent{Line: supervisor.LogLine{Project: "db", Stream: supervisor.Stdout, Text: "\x1b[32mok\x1b[0m \x1b]8;;http://x\x07link\x1b]8;;\x07"}}

	var out bytes.Buffer
	newPlainPrinter(&out, cfg, []string{"db"}, truecolor).event(line)
	if want := "\x1b[36mdb  \x1b[0m │ \x1b[32mok\x1b[0m \x1b]8;;http://x\x07link\x1b]8;;\x07\x1b[0m\n"; out.String() != want {
		t.Errorf("with color: %q, want %q", out.String(), want)
	}
	out.Reset()
	newPlainPrinter(&out, cfg, []string{"db"}, plainColors{}).event(line)
	if want := "db   │ ok link\n"; out.String() != want {
		t.Errorf("without color: %q, want %q", out.String(), want)
	}
}

func TestPlainPrinterThemeColors(t *testing.T) {
	cfg := plainConfig(t, map[string]*config.Project{
		"api": {Cmd: "x", Color: "red"},
		"db":  {Cmd: "x", Color: "208"},
	})
	cfg.Settings.ThemeValue = "catppuccin"
	tests := []struct {
		colors  plainColors
		api, db string
	}{
		{truecolor, "\x1b[38;2;243;139;168mapi \x1b[0m", "\x1b[38;5;208mdb  \x1b[0m"},                                  // mocha red
		{plainColors{profile: colorprofile.TrueColor}, "\x1b[38;2;210;15;57mapi \x1b[0m", "\x1b[38;5;208mdb  \x1b[0m"}, // latte red
		{plainColors{profile: colorprofile.ANSI256, dark: true}, "\x1b[38;5;211mapi \x1b[0m", "\x1b[38;5;208mdb  \x1b[0m"},
		{plainColors{profile: colorprofile.ANSI, dark: true}, "\x1b[91mapi \x1b[0m", "\x1b[91mdb  \x1b[0m"},
		{plainColors{}, "api ", "db  "},
	}
	for _, tt := range tests {
		p := newPlainPrinter(&bytes.Buffer{}, cfg, []string{"api", "db"}, tt.colors)
		if p.labels["api"] != tt.api || p.labels["db"] != tt.db {
			t.Errorf("%+v: labels %q, %q, want %q, %q", tt.colors, p.labels["api"], p.labels["db"], tt.api, tt.db)
		}
	}
}

func TestPlainLogDir(t *testing.T) {
	logs := t.TempDir()
	cfg := plainConfig(t, map[string]*config.Project{"api": {Cmd: "echo hello; exit 1"}})
	projects, err := prepareUp(cfg, []string{"api"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	err = runPlain(&syncBuffer{}, cfg, supervisor.Options{LogDir: logs}, []string{"api"}, nil, nil, projects, make(chan os.Signal), plainColors{})
	if err == nil {
		t.Error("a crash should fail the run")
	}
	b, err := os.ReadFile(filepath.Join(logs, "api.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--- runr session ", " » starting: echo hello; exit 1\n", " │ hello\n", " » crashed (exit code 1)\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("api.log lacks %q:\n%s", want, b)
		}
	}
}
