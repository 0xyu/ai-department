package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MinVersion is the oldest git ACO supports (update-ref --stdin transactions,
// worktree remove, interpret-trailers --parse).
var MinVersion = [3]int{2, 30, 0}

// Preflight errors.
var (
	ErrGitTooOld        = errors.New("git is older than the supported minimum")
	ErrNotRepository    = errors.New("not a git repository")
	ErrBareRepository   = errors.New("bare repositories are not supported")
	ErrNoBase           = errors.New("base branch does not resolve to a commit")
	ErrWorktreePathUsed = errors.New("worktree path exists and is not empty")
	ErrOperationPending = errors.New("a merge, rebase, cherry-pick, or bisect is in progress")
)

// PreflightResult describes the repository a run starts from.
type PreflightResult struct {
	Version    string // git version, e.g. "2.47.1"
	Root       string // top-level directory of the user's checkout
	BaseBranch string // short branch name
	BaseCommit string // commit the run starts from; never modified
}

// Preflight checks that r is a usable, non-bare repository, that base (a
// branch name; empty means the current branch) resolves to a commit, and that
// worktreePath (if given) is free. It writes nothing.
func (r *Repo) Preflight(ctx context.Context, base, worktreePath string) (*PreflightResult, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return nil, err
	}
	v, err := r.Version(ctx)
	if err != nil {
		return nil, err
	}
	res := &PreflightResult{Version: v}
	if !versionAtLeast(v, MinVersion) {
		return res, fmt.Errorf("%w: %s < %d.%d.%d", ErrGitTooOld, v, MinVersion[0], MinVersion[1], MinVersion[2])
	}
	bare, err := r.out(ctx, "rev-parse", "--is-bare-repository")
	if err != nil {
		return res, ErrNotRepository
	}
	if bare == "true" {
		return res, ErrBareRepository
	}
	if res.Root, err = r.out(ctx, "rev-parse", "--show-toplevel"); err != nil {
		return res, ErrNotRepository
	}
	res.Root = filepath.FromSlash(res.Root)
	if base == "" {
		if base, err = r.out(ctx, "symbolic-ref", "--short", "-q", "HEAD"); err != nil || base == "" {
			return res, fmt.Errorf("%w: HEAD is detached; pass a base branch", ErrNoBase)
		}
	}
	res.BaseBranch = base
	if res.BaseCommit, err = r.ResolveCommit(ctx, "refs/heads/"+base); err != nil {
		return res, fmt.Errorf("%w: %s", ErrNoBase, base)
	}
	for _, f := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG", "rebase-merge", "rebase-apply"} {
		p, err := r.gitPath(ctx, f)
		if err != nil {
			return res, err
		}
		if _, err := os.Lstat(p); err == nil {
			return res, ErrOperationPending
		}
	}
	if worktreePath != "" {
		if entries, err := os.ReadDir(worktreePath); err == nil && len(entries) > 0 {
			return res, ErrWorktreePathUsed
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return res, ErrWorktreePathUsed
		}
	}
	return res, nil
}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// Version returns the git version (e.g. "2.47.1").
func (r *Repo) Version(ctx context.Context) (string, error) {
	s, err := r.out(ctx, "version")
	if err != nil {
		return "", err
	}
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return "", fmt.Errorf("gitops: cannot parse git version")
	}
	if m[3] == "" {
		m[3] = "0"
	}
	return m[1] + "." + m[2] + "." + m[3], nil
}

func versionAtLeast(v string, min [3]int) bool {
	parts := strings.Split(v, ".")
	for i := 0; i < 3; i++ {
		n := 0
		if i < len(parts) {
			n, _ = strconv.Atoi(parts[i])
		}
		if n != min[i] {
			return n > min[i]
		}
	}
	return true
}

// ResolveCommit returns the commit SHA rev points to.
func (r *Repo) ResolveCommit(ctx context.Context, rev string) (string, error) {
	return r.out(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
}

// ResolveTree returns the tree SHA of rev (a commit or a tree).
func (r *Repo) ResolveTree(ctx context.Context, rev string) (string, error) {
	return r.out(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{tree}")
}

// Head returns the commit HEAD points to.
func (r *Repo) Head(ctx context.Context) (string, error) { return r.ResolveCommit(ctx, "HEAD") }

// CurrentBranch returns the full ref HEAD points to (refs/heads/…), or "" when
// HEAD is detached.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	s, err := r.out(ctx, "symbolic-ref", "-q", "HEAD")
	if err != nil && exitCode(err) == 1 {
		return "", nil
	}
	return s, err
}

// RefExists reports whether a full ref name exists.
func (r *Repo) RefExists(ctx context.Context, ref string) (bool, error) {
	_, err := r.run(ctx, call{args: []string{"show-ref", "--verify", "--quiet", ref}})
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// IsAncestor reports whether commit a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.run(ctx, call{args: []string{"merge-base", "--is-ancestor", a, b}})
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// CommitParent is a commit with its first parent ("" for a root commit).
type CommitParent struct {
	Commit string
	Parent string
}

// RevList lists the commits reachable from to but not from from (all
// commits reachable from to when from is ""), oldest first, each with its
// first parent.
func (r *Repo) RevList(ctx context.Context, from, to string) ([]CommitParent, error) {
	rng := to
	if from != "" {
		rng = from + ".." + to
	}
	s, err := r.out(ctx, "rev-list", "--reverse", "--parents", "--end-of-options", rng)
	if err != nil {
		return nil, err
	}
	var out []CommitParent
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		cp := CommitParent{Commit: f[0]}
		if len(f) > 1 {
			cp.Parent = f[1]
		}
		out = append(out, cp)
	}
	return out, nil
}

// IsClean reports whether the working state equals HEAD: no staged,
// unstaged, or untracked (non-ignored) changes.
func (r *Repo) IsClean(ctx context.Context) (bool, error) {
	s, err := r.out(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	return s == "", err
}

// gitPath resolves a path inside the git directory (git rev-parse --git-path).
func (r *Repo) gitPath(ctx context.Context, name string) (string, error) {
	p, err := r.out(ctx, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.dir, p)
	}
	return p, nil
}
