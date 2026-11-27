package gitinfo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// helperEnv makes the test binary stand in for git: "hang" never
// finishes, "session" prints its pid and session ID (Unix).
const helperEnv = "RUNR_GITINFO_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "hang":
		time.Sleep(time.Hour)
	case "session":
		printSession()
		os.Exit(0)
	}
	// Keep the user's git config out of the tests, and keep repository
	// discovery from walking above the temp dir.
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":       os.DevNull,
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_AUTHOR_NAME":         "runr test",
		"GIT_AUTHOR_EMAIL":        "test@example.com",
		"GIT_COMMITTER_NAME":      "runr test",
		"GIT_COMMITTER_EMAIL":     "test@example.com",
		"GIT_CEILING_DIRECTORIES": os.TempDir(),
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

func newGit(t *testing.T) *Git {
	t.Helper()
	g, err := New()
	if errors.Is(err, ErrNotInstalled) {
		t.Skip("git is not installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// git runs a git command for test setup and returns its output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitAllowFail(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func gitAllowFail(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// realDir returns a fresh temp dir with symlinks resolved, as git reports paths.
func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo creates a repository on branch main with one commit of file f.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := realDir(t)
	git(t, dir, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(dir, "f"), "one\n")
	git(t, dir, "add", "f")
	git(t, dir, "commit", "-q", "-m", "one")
	return dir
}

// newClone returns an upstream repository and a clone tracking its main.
func newClone(t *testing.T) (upstream, clone string) {
	t.Helper()
	upstream = newRepo(t)
	clone = filepath.Join(realDir(t), "clone")
	git(t, upstream, "clone", "-q", upstream, clone)
	return upstream, clone
}

func status(t *testing.T, g *Git, dir string) Status {
	t.Helper()
	repo, err := g.Find(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := g.Status(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFind(t *testing.T) {
	g := newGit(t)
	root := newRepo(t)
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	repo, err := g.Find(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	want := Repo{Root: root, GitDir: filepath.Join(root, ".git"), CommonDir: filepath.Join(root, ".git")}
	if repo != want {
		t.Errorf("Find = %+v, want %+v", repo, want)
	}

	if _, err := g.Find(context.Background(), realDir(t)); !errors.Is(err, ErrNotRepo) {
		t.Errorf("outside a repo: err = %v, want ErrNotRepo", err)
	}
	missing := filepath.Join(root, "missing")
	if _, err := g.Find(context.Background(), missing); err == nil || errors.Is(err, ErrNotRepo) ||
		err.Error() != "dir "+missing+" does not exist" {
		t.Errorf("missing dir: err = %v", err)
	}
}

func TestFindWorktree(t *testing.T) {
	g := newGit(t)
	root := newRepo(t)
	wt := filepath.Join(realDir(t), "wt")
	git(t, root, "worktree", "add", "-q", "-b", "other", wt)

	repo, err := g.Find(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	want := Repo{Root: wt, GitDir: filepath.Join(root, ".git", "worktrees", "wt"), CommonDir: filepath.Join(root, ".git")}
	if repo != want {
		t.Errorf("Find = %+v, want %+v", repo, want)
	}
	if s := status(t, g, wt); s.Branch != "other" {
		t.Errorf("worktree branch = %q", s.Branch)
	}
}

func TestFindAll(t *testing.T) {
	g := newGit(t)
	root := newRepo(t)
	other := newRepo(t)
	sub := filepath.Join(root, "web")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := realDir(t)
	missing := filepath.Join(plain, "missing")

	r := g.FindAll(context.Background(), map[string]string{
		"api":    root,
		"worker": root,
		"web":    sub,
		"auth":   other,
		"notes":  plain,
		"gone":   missing,
	})

	if want := map[string][]string{root: {"api", "web", "worker"}, other: {"auth"}}; !reflect.DeepEqual(r.Members, want) {
		t.Errorf("Members = %v, want %v", r.Members, want)
	}
	if want := map[string]string{"api": root, "worker": root, "web": root, "auth": other}; !reflect.DeepEqual(r.Of, want) {
		t.Errorf("Of = %v, want %v", r.Of, want)
	}
	if len(r.ByRoot) != 2 || r.ByRoot[root].GitDir != filepath.Join(root, ".git") {
		t.Errorf("ByRoot = %v", r.ByRoot)
	}
	if len(r.Errors) != 0 {
		t.Errorf("Errors = %v", r.Errors)
	}

	// Failures other than "not a repo" are reported per project.
	g.Timeout = time.Nanosecond
	r = g.FindAll(context.Background(), map[string]string{"api": root, "gone": missing})
	if len(r.Errors) != 1 || !errors.Is(r.Errors["api"], context.DeadlineExceeded) || len(r.Of) != 0 {
		t.Errorf("with timeouts: %+v", r)
	}
}

func TestStatusTracking(t *testing.T) {
	g := newGit(t)
	upstream, clone := newClone(t)

	s := status(t, g, clone)
	want := Status{Branch: "main", Commit: strings.TrimSpace(git(t, clone, "rev-parse", "HEAD")), Upstream: "origin/main"}
	if s != want {
		t.Errorf("clean clone:\ngot  %+v\nwant %+v", s, want)
	}

	writeFile(t, filepath.Join(clone, "untracked"), "x\n")
	if s := status(t, g, clone); s.Changed {
		t.Error("untracked file counted as a change")
	}

	writeFile(t, filepath.Join(clone, "f"), "changed\n")
	if s := status(t, g, clone); !s.Changed {
		t.Error("modified file not counted as a change")
	}
	git(t, clone, "add", "f")
	if s := status(t, g, clone); !s.Changed {
		t.Error("staged change not counted")
	}
	git(t, clone, "commit", "-q", "-m", "local")
	if s := status(t, g, clone); s.Changed || s.Ahead != 1 || s.Behind != 0 {
		t.Errorf("after local commit: %+v", s)
	}

	writeFile(t, filepath.Join(upstream, "g"), "upstream\n")
	git(t, upstream, "add", "g")
	git(t, upstream, "commit", "-q", "-m", "upstream")
	if s := status(t, g, clone); s.Behind != 0 {
		t.Errorf("behind before fetch: %+v", s)
	}
	git(t, clone, "fetch", "-q")
	if s := status(t, g, clone); s.Ahead != 1 || s.Behind != 1 {
		t.Errorf("after fetch: %+v", s)
	}
}

func TestStatusDetachedAndUnborn(t *testing.T) {
	g := newGit(t)
	root := newRepo(t)
	git(t, root, "checkout", "-q", "--detach")
	if s := status(t, g, root); !s.Detached || s.Branch != "" || len(s.ShortCommit()) != 7 {
		t.Errorf("detached: %+v", s)
	}

	empty := realDir(t)
	git(t, empty, "init", "-q", "-b", "trunk")
	if s := status(t, g, empty); s != (Status{Branch: "trunk"}) {
		t.Errorf("unborn: %+v", s)
	}
}

// conflictingBranches makes branch "other" and main change f differently.
func conflictingBranches(t *testing.T, root string) {
	t.Helper()
	git(t, root, "checkout", "-q", "-b", "other")
	writeFile(t, filepath.Join(root, "f"), "other\n")
	git(t, root, "commit", "-q", "-am", "other")
	git(t, root, "checkout", "-q", "main")
	writeFile(t, filepath.Join(root, "f"), "main\n")
	git(t, root, "commit", "-q", "-am", "main")
}

func TestStatusOperations(t *testing.T) {
	g := newGit(t)
	tests := []struct {
		name string
		run  []string
		want Operation
	}{
		{"merge", []string{"merge", "other"}, Merge},
		{"rebase", []string{"rebase", "other"}, Rebase},
		{"cherry-pick", []string{"cherry-pick", "other"}, CherryPick},
		// Reverting "one" (which created f) conflicts with "main" changing f since.
		{"revert", []string{"revert", "--no-edit", "HEAD~1"}, Revert},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newRepo(t)
			conflictingBranches(t, root)
			if out, err := gitAllowFail(root, tt.run...); err == nil {
				t.Fatalf("git %v did not stop with a conflict:\n%s", tt.run, out)
			}
			s := status(t, g, root)
			if s.Operation != tt.want || !s.Conflicts {
				t.Errorf("status = %+v, want operation %v with conflicts", s, tt.want)
			}
		})
	}
}

func TestStatusUpstreamGone(t *testing.T) {
	g := newGit(t)
	upstream, clone := newClone(t)
	git(t, upstream, "branch", "feature")
	git(t, clone, "fetch", "-q")
	git(t, clone, "checkout", "-q", "-b", "feature", "--track", "origin/feature")
	git(t, upstream, "branch", "-q", "-D", "feature")
	git(t, clone, "fetch", "-q", "--prune")

	if s := status(t, g, clone); !s.UpstreamGone || s.Upstream != "origin/feature" {
		t.Errorf("status = %+v", s)
	}
}

func TestStatusDoesNotTouchIndex(t *testing.T) {
	g := newGit(t)
	root := newRepo(t)
	index := filepath.Join(root, ".git", "index")
	// Make the index stale so that a plain git status would refresh it.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "f"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(index, old, old); err != nil {
		t.Fatal(err)
	}
	status(t, g, root)
	fi, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Error("status rewrote the index")
	}
}

func TestTimeout(t *testing.T) {
	t.Setenv(helperEnv, "hang")
	g := &Git{path: os.Args[0], Timeout: 50 * time.Millisecond}
	_, err := g.Find(context.Background(), realDir(t))
	if !errors.Is(err, context.DeadlineExceeded) || err.Error() != "git rev-parse: timed out" {
		t.Errorf("err = %v", err)
	}
}

func TestErrorMessage(t *testing.T) {
	g := newGit(t)
	root := newRepo(t)
	_, err := g.run(context.Background(), root, nil, "rev-parse", "--verify", "-q", "nope^{commit}")
	var gitErr *Error
	if !errors.As(err, &gitErr) || err.Error() != "git rev-parse: exit status 1" {
		t.Errorf("no stderr: err = %v", err)
	}
	_, err = g.run(context.Background(), root, []string{"LC_ALL=C"}, "switch", "nope")
	if err == nil || err.Error() != "git switch: fatal: invalid reference: nope" {
		t.Errorf("with stderr: err = %v", err)
	}
}

func TestNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := New(); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v", err)
	}
}

func TestGitEnv(t *testing.T) {
	env := gitEnv([]string{
		"HOME=/home/u", "GIT_DIR=/elsewhere/.git", "GIT_WORK_TREE=/elsewhere",
		"GIT_TERMINAL_PROMPT=1", "GIT_AUTHOR_NAME=u",
	})
	want := []string{"HOME=/home/u", "GIT_AUTHOR_NAME=u", "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"}
	if !slices.Equal(env, want) {
		t.Errorf("env = %q\nwant  %q", env, want)
	}

	for _, custom := range []string{"GIT_SSH_COMMAND=ssh -i key", "GIT_SSH=plink"} {
		env := gitEnv([]string{custom})
		if want := []string{custom, "GIT_TERMINAL_PROMPT=0"}; !slices.Equal(env, want) {
			t.Errorf("env = %q, want %q", env, want)
		}
	}
}
