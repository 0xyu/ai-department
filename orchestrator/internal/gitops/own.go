package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNotOwnedWorktree reports a mutation aimed at something other than an
// ACO-owned worktree of this repository: the user's checkout, an unrelated
// linked worktree, another repository, or a run worktree whose HEAD left
// the run branch.
var ErrNotOwnedWorktree = errors.New("gitops: target is not an ACO-owned worktree of this repository")

// markerFile is written into the private git directory of every worktree
// ACO creates (<common>/worktrees/<name>/aco-worktree). Worktrees the user
// created never have it, and agents may not write inside .git.
const markerFile = "aco-worktree"

func markerContent(ro role, branch string) string {
	switch ro {
	case roleRun:
		return "aco-worktree/v1\nrole=run\nbranch=" + branch + "\n"
	case roleVerify:
		return "aco-worktree/v1\nrole=verify\n"
	}
	return ""
}

// location is where Git says a directory lives.
type location struct {
	top       string // worktree top level
	gitDir    string // the worktree's private git dir
	commonDir string // the repository's shared git dir
}

func (r *Repo) locate(ctx context.Context) (location, error) {
	s, err := r.out(ctx, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return location{}, err
	}
	lines := strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
	if len(lines) != 3 {
		return location{}, fmt.Errorf("gitops: unexpected rev-parse output")
	}
	loc := location{top: filepath.FromSlash(lines[0]), gitDir: filepath.FromSlash(lines[1]), commonDir: filepath.FromSlash(lines[2])}
	if !filepath.IsAbs(loc.commonDir) {
		loc.commonDir = filepath.Join(r.dir, loc.commonDir)
	}
	return loc, nil
}

// samePath reports whether two paths name the same existing file or
// directory (compared by file identity, so case, symlinks, and 8.3 names on
// Windows do not matter).
func samePath(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// canonical returns the absolute path with symlinks resolved when it exists.
func canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	return abs, nil
}

// assertOwned checks, against the file system and Git right now, that r is
// an ACO-owned linked worktree with one of the allowed roles: its role is
// allowed; Git places r's directory at the top of a linked worktree (not the
// main checkout) of the same repository that created or opened it; that
// worktree's private git dir holds ACO's marker for this role and branch;
// and, when checkHead is set, HEAD is on the run branch (run) or detached
// (verify). Nothing is written.
func (r *Repo) assertOwned(ctx context.Context, checkHead bool, allowed ...role) error {
	if r == nil || !slices.Contains(allowed, r.role) || r.role == roleUser || r.commonDir == "" {
		return ErrNotOwnedWorktree
	}
	loc, err := r.locate(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotOwnedWorktree, err)
	}
	switch {
	case !samePath(loc.top, r.dir):
		return fmt.Errorf("%w: directory is not a worktree top level", ErrNotOwnedWorktree)
	case samePath(loc.gitDir, loc.commonDir):
		return fmt.Errorf("%w: directory is the main checkout", ErrNotOwnedWorktree)
	case !samePath(loc.commonDir, r.commonDir):
		return fmt.Errorf("%w: worktree belongs to another repository", ErrNotOwnedWorktree)
	}
	b, err := os.ReadFile(filepath.Join(loc.gitDir, markerFile)) // #nosec G304 -- path from git rev-parse
	if err != nil || string(b) != markerContent(r.role, r.branch) {
		return fmt.Errorf("%w: worktree was not created by ACO for this role", ErrNotOwnedWorktree)
	}
	if !checkHead {
		return nil
	}
	cur, err := r.CurrentBranch(ctx)
	if err != nil {
		return err
	}
	if r.role == roleRun && cur != r.branch {
		return fmt.Errorf("%w: HEAD is not on the run branch", ErrNotOwnedWorktree)
	}
	if r.role == roleVerify && cur != "" {
		return fmt.Errorf("%w: verification worktree HEAD is not detached", ErrNotOwnedWorktree)
	}
	return nil
}
