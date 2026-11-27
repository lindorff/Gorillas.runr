package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/Skaanning/runr/internal/config"
	"github.com/Skaanning/runr/internal/gitinfo"
)

// worktreeDirs returns where projects run when started with --worktree:
// each project whose configured dir is in the same git repository as
// worktree (any of its work trees) runs in the same place under worktree's
// top-level directory instead, e.g. ~/code/app/src/api in ~/wt/fix/src/api.
// Projects of other repositories are left out: they run where configured.
//
// It is an error if none of the projects is in that repository, or if one
// that is has no such directory in the worktree.
func worktreeDirs(cfg *config.Config, projects []string, worktree string) (map[string]string, error) {
	g, err := gitinfo.New()
	if err != nil {
		return nil, fmt.Errorf("--worktree: %w", err)
	}
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return nil, fmt.Errorf("--worktree: %w", err)
	}
	ctx := context.Background()
	wt, err := g.Find(ctx, abs)
	if errors.Is(err, gitinfo.ErrNotRepo) {
		return nil, fmt.Errorf("--worktree %s is not in a git repository", worktree)
	}
	if err != nil {
		return nil, fmt.Errorf("--worktree: %w", err)
	}

	dirs := map[string]string{}
	var missing []string
	for _, name := range projects {
		p := cfg.Projects[name]
		if p == nil {
			continue
		}
		dir, err := p.ResolvedDir()
		if err != nil {
			continue // e.g. $RUNR_ROOT unset: reported like other bad dirs
		}
		if dir, err = filepath.Abs(dir); err != nil {
			continue
		}
		// The dir may not exist in this checkout, e.g. a build output.
		// git prints real paths: symlinks in dir are resolved to match.
		existing := existingDir(dir)
		repo, err := g.Find(ctx, existing)
		if err != nil || !samePath(repo.CommonDir, wt.CommonDir) {
			continue
		}
		if real, err := filepath.EvalSymlinks(existing); err == nil {
			if rest, err := filepath.Rel(existing, dir); err == nil {
				dir = filepath.Join(real, rest)
			}
		}
		rel, err := filepath.Rel(repo.Root, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		to := filepath.Join(wt.Root, rel)
		if fi, err := os.Stat(to); err != nil || !fi.IsDir() {
			missing = append(missing, fmt.Sprintf("%s: %s does not exist in the worktree", name, to))
			continue
		}
		dirs[name] = to
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("cannot start:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("--worktree %s: none of %s is in that repository", worktree, strings.Join(slices.Sorted(slices.Values(projects)), ", "))
	}
	return dirs, nil
}

// existingDir returns dir, or the nearest of its parents that exists.
func existingDir(dir string) string {
	for d := dir; ; {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// samePath reports whether two clean absolute paths name the same
// directory, ignoring case on Windows.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
