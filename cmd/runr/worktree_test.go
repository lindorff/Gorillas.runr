package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/gitinfo"
)

// gitEnv isolates git from the user's configuration, with an identity for
// commits.
func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := gitinfo.New(); errors.Is(err, gitinfo.ErrNotInstalled) {
		t.Skip("git is not installed")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":       os.DevNull,
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_AUTHOR_NAME":         "runr test",
		"GIT_AUTHOR_EMAIL":        "test@example.com",
		"GIT_COMMITTER_NAME":      "runr test",
		"GIT_COMMITTER_EMAIL":     "test@example.com",
		"GIT_CEILING_DIRECTORIES": os.TempDir(),
	} {
		t.Setenv(k, v)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// newWorktreeRepo returns a repository with committed src/api and src/web,
// and a linked worktree of it.
func newWorktreeRepo(t *testing.T) (repo, wt string) {
	t.Helper()
	gitEnv(t)
	repo = t.TempDir()
	for _, d := range []string{"src/api", "src/web"} {
		mkdirs(t, filepath.Join(repo, d))
		if err := os.WriteFile(filepath.Join(repo, d, "main.go"), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "one")
	wt = filepath.Join(t.TempDir(), "fix")
	runGit(t, repo, "worktree", "add", "-q", "-b", "fix", wt)
	if real, err := filepath.EvalSymlinks(wt); err == nil {
		wt = real // as git prints it
	}
	return repo, wt
}

func TestWorktreeDirs(t *testing.T) {
	repo, wt := newWorktreeRepo(t)
	other := t.TempDir() // not in the repository
	mkdirs(t, filepath.Join(repo, "local"), filepath.Join(wt, "src", "new"))

	cfg := config.New()
	for name, dir := range map[string]string{
		"api":   filepath.Join(repo, "src", "api"),
		"web":   filepath.Join(repo, "src", "web"),
		"other": other,
		"local": filepath.Join(repo, "local"),      // untracked: not in the worktree
		"fresh": filepath.Join(repo, "src", "new"), // only in the worktree
	} {
		cfg.Projects[name] = &config.Project{Dir: dir, Cmd: "run"}
	}

	want := map[string]string{
		"api": filepath.Join(wt, "src", "api"),
		"web": filepath.Join(wt, "src", "web"),
	}
	// Any directory of the worktree will do; other repositories' projects
	// are left out.
	for _, at := range []string{wt, filepath.Join(wt, "src", "web")} {
		dirs, err := worktreeDirs(cfg, []string{"api", "web", "other"}, at)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(dirs, want) {
			t.Errorf("worktreeDirs(%s) = %v, want %v", at, dirs, want)
		}
	}
	// The main checkout is a work tree of the repository too.
	dirs, err := worktreeDirs(cfg, []string{"api"}, repo)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := filepath.EvalSymlinks(repo); err != nil || !samePath(dirs["api"], filepath.Join(got, "src", "api")) {
		t.Errorf("in the main checkout: %v", dirs)
	}
	// A dir that doesn't exist where configured, only in the worktree.
	if dirs, err := worktreeDirs(cfg, []string{"fresh"}, wt); err != nil || dirs["fresh"] != filepath.Join(wt, "src", "new") {
		t.Errorf("fresh: %v, %v", dirs, err)
	}

	for _, c := range []struct {
		projects []string
		at       string
		want     string
	}{
		{[]string{"api", "local"}, wt, "local: " + filepath.Join(wt, "local") + " does not exist in the worktree"},
		{[]string{"other"}, wt, "none of other is in that repository"},
		{[]string{"api"}, other, "is not in a git repository"},
		{[]string{"api"}, filepath.Join(wt, "missing"), "does not exist"},
	} {
		if _, err := worktreeDirs(cfg, c.projects, c.at); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("worktreeDirs(%v, %s): err = %v, want %q", c.projects, c.at, err, c.want)
		}
	}
}

func TestPrepareUpWorktree(t *testing.T) {
	repo, wt := newWorktreeRepo(t)
	mkdirs(t, filepath.Join(wt, "src", "new"))
	cfg := config.New()
	cfg.Projects["api"] = &config.Project{Dir: filepath.Join(repo, "src", "api"), Cmd: "run"}
	cfg.Projects["fresh"] = &config.Project{Dir: filepath.Join(repo, "src", "new"), Cmd: "run", DependsOn: []string{"api"}}

	// Without the worktree, fresh's dir is missing.
	if _, err := prepareUp(cfg, []string{"fresh"}, os.Stderr); err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Errorf("without --worktree: err = %v", err)
	}
	projects, dirs, err := prepareUpIn(cfg, []string{"fresh"}, wt, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projects, []string{"api", "fresh"}) {
		t.Errorf("projects = %v", projects)
	}
	want := map[string]string{"api": filepath.Join(wt, "src", "api"), "fresh": filepath.Join(wt, "src", "new")}
	if !reflect.DeepEqual(dirs, want) {
		t.Errorf("dirs = %v, want %v", dirs, want)
	}
}
