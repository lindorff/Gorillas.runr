package main

import (
	"maps"
	"slices"
	"testing"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/state"
	"github.com/Skaanning/runr/internal/supervisor"
)

// TestRecordingSession checks that starts and arguments, including those
// from the running view after the first start, become the last session.
func TestRecordingSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(state.EnvVar, dir)
	cfg := config.New()
	for _, name := range []string{"api", "web"} {
		cfg.Projects[name] = &config.Project{Dir: t.TempDir(), Cmd: "echo " + name}
	}
	sess := newRecordingSession(cfg, supervisor.Options{})
	defer sess.Close()

	if err := sess.begin([]string{"api"}, map[string]string{"api": "--v", "web": ""}, nil); err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("nope"); err == nil {
		t.Fatal("started an unknown project")
	}
	if err := sess.Start("web"); err != nil { // e.g. + in the running view
		t.Fatal(err)
	}
	if err := sess.SetArgs("web", "--port 1"); err != nil { // R
		t.Fatal(err)
	}
	l, ok := state.LoadLast(dir)
	if !ok || !slices.Equal(l.Names, []string{"api", "web"}) || !maps.Equal(l.Args, map[string]string{"api": "--v", "web": "--port 1"}) {
		t.Errorf("last session = %+v", l)
	}
}
