package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// BranchName returns the run branch name aco/<yyyymmdd>-<plan-slug>-<short-id>
// (§15). The slug is derived from the plan title: lower-case ASCII letters and
// digits joined by "-", at most 40 characters.
func BranchName(date time.Time, planTitle, shortID string) string {
	slug := Slug(planTitle, 40)
	if slug == "" {
		slug = "plan"
	}
	id := Slug(shortID, 12)
	if id == "" {
		id = "run"
	}
	return "aco/" + date.UTC().Format("20060102") + "-" + slug + "-" + id
}

// Slug lowers s and keeps ASCII letters and digits, joining runs of other
// characters with a single "-". The result has at most max bytes.
func Slug(s string, max int) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(s) {
		if c < unicode.MaxASCII && (unicode.IsLetter(c) || unicode.IsDigit(c)) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(c)
			if b.Len() >= max {
				break
			}
			continue
		}
		dash = true
	}
	return strings.Trim(b.String(), "-")
}

var runIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validRunID guards ref names built from a run ID.
func validRunID(id string) error {
	if !runIDRE.MatchString(id) || strings.Contains(id, "..") || strings.HasSuffix(id, ".lock") || strings.HasSuffix(id, ".") {
		return fmt.Errorf("gitops: invalid run id")
	}
	return nil
}

// CheckBranchName validates a branch name with git check-ref-format.
func (r *Repo) CheckBranchName(ctx context.Context, name string) error {
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("gitops: invalid branch name")
	}
	if _, err := r.run(ctx, call{args: []string{"check-ref-format", "--branch", name}}); err != nil {
		return fmt.Errorf("gitops: invalid branch name")
	}
	return nil
}

// AddWorktree creates the run worktree at path on a new branch that starts at
// baseCommit. The user's checkout and index are not touched; the new branch
// must not exist yet. ACO's ownership marker is written into the worktree's
// private git dir; the returned Repo is the only kind that may commit to
// the run branch.
func (r *Repo) AddWorktree(ctx context.Context, path, branch, baseCommit string) (*Repo, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.CheckBranchName(ctx, branch); err != nil {
		return nil, err
	}
	ref := "refs/heads/" + branch
	if ok, err := r.RefExists(ctx, ref); err != nil {
		return nil, err
	} else if ok {
		return nil, fmt.Errorf("gitops: branch %s already exists", branch)
	}
	abs, err := prepareWorktreePath(path)
	if err != nil {
		return nil, err
	}
	if _, err := r.run(ctx, call{args: []string{"worktree", "add", "--quiet", "-b", branch, "--end-of-options", abs, baseCommit}}); err != nil {
		r.undoWorktree(ctx, abs, ref, baseCommit)
		return nil, err
	}
	wt, err := r.claimWorktree(ctx, abs, roleRun, ref)
	if err != nil {
		r.undoWorktree(ctx, abs, ref, baseCommit)
		return nil, err
	}
	return wt, nil
}

// AddDetachedWorktree creates a clean worktree at path with HEAD detached at
// commit (Stage 4 final verification checkout), marked as owned by ACO.
func (r *Repo) AddDetachedWorktree(ctx context.Context, path, commit string) (*Repo, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return nil, err
	}
	abs, err := prepareWorktreePath(path)
	if err != nil {
		return nil, err
	}
	if _, err := r.run(ctx, call{args: []string{"worktree", "add", "--quiet", "--detach", "--end-of-options", abs, commit}}); err != nil {
		r.undoWorktree(ctx, abs, "", "")
		return nil, err
	}
	wt, err := r.claimWorktree(ctx, abs, roleVerify, "")
	if err != nil {
		r.undoWorktree(ctx, abs, "", "")
		return nil, err
	}
	return wt, nil
}

// claimWorktree writes the ownership marker into a worktree ACO has just
// created and returns its Repo after verifying ownership.
func (r *Repo) claimWorktree(ctx context.Context, abs string, ro role, branch string) (*Repo, error) {
	parent, err := r.locate(ctx)
	if err != nil {
		return nil, err
	}
	dir, err := canonical(abs)
	if err != nil {
		return nil, err
	}
	wt := r.at(dir)
	wt.role, wt.branch, wt.commonDir = ro, branch, parent.commonDir
	loc, err := wt.locate(ctx)
	if err != nil {
		return nil, err
	}
	if !samePath(loc.commonDir, parent.commonDir) || samePath(loc.gitDir, loc.commonDir) || !samePath(loc.top, dir) {
		return nil, ErrNotOwnedWorktree
	}
	if err := os.WriteFile(filepath.Join(loc.gitDir, markerFile), []byte(markerContent(ro, branch)), 0o600); err != nil {
		return nil, err
	}
	if err := wt.assertOwned(ctx, true, ro); err != nil {
		return nil, err
	}
	return wt, nil
}

// undoWorktree removes a worktree ACO just created but could not claim, and
// the branch it created if that branch still points at base.
func (r *Repo) undoWorktree(ctx context.Context, abs, ref, base string) {
	_, _ = r.run(ctx, call{args: []string{"worktree", "remove", "--force", "--force", "--end-of-options", abs}})
	// prepareWorktreePath ensured the path was absent or empty, so anything
	// left there was written by the failed worktree add.
	_ = os.RemoveAll(abs)
	_, _ = r.run(ctx, call{args: []string{"worktree", "prune"}})
	if ref != "" && base != "" {
		_, _ = r.run(ctx, call{args: []string{"update-ref", "-m", "aco: undo worktree", "-d", ref, base}})
	}
}

// OpenWorktree returns a Repo for an existing ACO worktree of r (after a
// restart). branch is the run branch (short name) for a run worktree, or ""
// for a detached verification worktree. It fails with ErrNotOwnedWorktree
// unless path is the top of a linked worktree of the same repository that
// carries ACO's marker for that role and branch, with HEAD on that branch
// (or detached).
func (r *Repo) OpenWorktree(ctx context.Context, path, branch string) (*Repo, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return nil, err
	}
	parent, err := r.locate(ctx)
	if err != nil {
		return nil, err
	}
	dir, err := canonical(path)
	if err != nil {
		return nil, err
	}
	wt := r.at(dir)
	wt.commonDir = parent.commonDir
	if branch == "" {
		wt.role = roleVerify
	} else {
		if err := r.CheckBranchName(ctx, branch); err != nil {
			return nil, err
		}
		wt.role, wt.branch = roleRun, "refs/heads/"+branch
	}
	if err := wt.assertOwned(ctx, true, wt.role); err != nil {
		return nil, err
	}
	return wt, nil
}

// RemoveWorktree removes an ACO-owned worktree (discarding its changes) and
// prunes stale worktree metadata. The branch stays. It refuses, before
// removing anything, a Repo that is not an ACO worktree of r's repository:
// the user's checkout, a worktree the user created, or one of another
// repository. A worktree whose directory is already gone is only pruned.
func (r *Repo) RemoveWorktree(ctx context.Context, wt *Repo) error {
	ctx, err := r.scope(ctx)
	if err != nil {
		return err
	}
	if wt == nil || (wt.role != roleRun && wt.role != roleVerify) {
		return ErrNotOwnedWorktree
	}
	parent, err := r.locate(ctx)
	if err != nil {
		return err
	}
	if !samePath(parent.commonDir, wt.commonDir) {
		return ErrNotOwnedWorktree
	}
	if _, err := os.Stat(wt.dir); errors.Is(err, os.ErrNotExist) {
		_, err := r.run(ctx, call{args: []string{"worktree", "prune"}})
		return err
	}
	if samePath(wt.dir, r.dir) || samePath(wt.dir, parent.top) {
		return ErrNotOwnedWorktree
	}
	if err := wt.assertOwned(ctx, false, roleRun, roleVerify); err != nil {
		return err
	}
	if _, err := r.run(ctx, call{args: []string{"worktree", "remove", "--force", "--force", "--end-of-options", wt.dir}}); err != nil {
		return err
	}
	_, err = r.run(ctx, call{args: []string{"worktree", "prune"}})
	return err
}

func prepareWorktreePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
		return "", ErrWorktreePathUsed
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", err
	}
	return abs, nil
}
