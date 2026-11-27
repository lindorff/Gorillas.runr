package gitinfo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNotRepo is returned by Find for a directory that is not inside a git
// work tree.
var ErrNotRepo = errors.New("not in a git repository")

// Repo is a git work tree.
type Repo struct {
	Root      string // top-level directory of the work tree
	GitDir    string // its git directory (for a linked worktree, the worktree's own)
	CommonDir string // the git directory its worktrees share: the same for every work tree of a repository
}

// Find returns the work tree containing dir.
func (g *Git) Find(ctx context.Context, dir string) (Repo, error) {
	if fi, err := os.Stat(dir); err != nil {
		return Repo{}, fmt.Errorf("dir %s does not exist", dir)
	} else if !fi.IsDir() {
		return Repo{}, fmt.Errorf("dir %s is not a directory", dir)
	}
	// LC_ALL=C: the "not a git repository" message is matched below.
	out, err := g.run(ctx, dir, []string{"LC_ALL=C"}, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		var gitErr *Error
		if errors.As(err, &gitErr) && strings.Contains(gitErr.Stderr, "not a git repository") {
			return Repo{}, ErrNotRepo
		}
		return Repo{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 {
		return Repo{}, fmt.Errorf("git rev-parse: unexpected output %q", out)
	}
	// git prints forward slashes on Windows too, and the common dir
	// relative to dir unless it is elsewhere.
	path := func(line string) string { return filepath.Clean(filepath.FromSlash(strings.TrimSpace(line))) }
	common := path(lines[2])
	if !filepath.IsAbs(common) {
		// Real like the other two (dir may have symlinks, or 8.3 names
		// on Windows), so that work trees of a repository compare equal.
		common = filepath.Join(dir, common)
		if real, err := filepath.EvalSymlinks(common); err == nil {
			common = real
		}
	}
	return Repo{Root: path(lines[0]), GitDir: path(lines[1]), CommonDir: common}, nil
}

// Repos says which repository each of a set of projects belongs to.
type Repos struct {
	ByRoot  map[string]Repo     // the repositories, by root
	Members map[string][]string // root → names of the projects in it, sorted
	Of      map[string]string   // project name → root; projects outside a repo are absent
	Errors  map[string]error    // project name → why it could not be looked up
}

// FindAll looks up the repositories of projects, given as name → directory,
// so that projects sharing a repository can be queried once. Each distinct
// directory is looked up once. Projects whose directory does not exist are
// treated like projects outside a repository.
func (g *Git) FindAll(ctx context.Context, dirs map[string]string) Repos {
	r := Repos{
		ByRoot:  map[string]Repo{},
		Members: map[string][]string{},
		Of:      map[string]string{},
		Errors:  map[string]error{},
	}
	type result struct {
		repo Repo
		err  error
	}
	found := map[string]result{}
	for _, name := range sortedKeys(dirs) {
		dir := dirs[name]
		res, ok := found[dir]
		if !ok {
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				res.err = ErrNotRepo
			} else {
				res.repo, res.err = g.Find(ctx, dir)
			}
			found[dir] = res
		}
		switch {
		case errors.Is(res.err, ErrNotRepo):
		case res.err != nil:
			r.Errors[name] = res.err
		default:
			r.ByRoot[res.repo.Root] = res.repo
			r.Members[res.repo.Root] = append(r.Members[res.repo.Root], name)
			r.Of[name] = res.repo.Root
		}
	}
	return r
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
