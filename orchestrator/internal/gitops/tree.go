package gitops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// EmptyTree is the hash of the empty tree.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// RefPrefix is the namespace of ACO's shadow refs.
const RefPrefix = "refs/aco/"

// CheckpointRef names the checkpoint of step `step` of attempt `attempt` of
// phase `phase` (§14.2): refs/aco/<run>/PNN/aK/sM.
func CheckpointRef(runID string, phase, attempt int, step int64) string {
	return fmt.Sprintf("%s%s/P%02d/a%d/s%d", RefPrefix, runID, phase, attempt, step)
}

// BlockedRef names the work-in-progress ref of a blocked phase:
// refs/aco/<run>/blocked/PNN.
func BlockedRef(runID string, phase int) string {
	return fmt.Sprintf("%s%s/blocked/P%02d", RefPrefix, runID, phase)
}

// runRefs is the shadow-ref namespace of one run.
func runRefs(runID string) string { return RefPrefix + runID + "/" }

// tempIndex creates an empty temporary index file inside the git directory
// and returns its path and a cleanup function. Using a separate index means
// the worktree's own index is never changed.
func (r *Repo) tempIndex(ctx context.Context) (string, func(), error) {
	gitDir, err := r.out(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(gitDir, "aco-index-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	_ = f.Close()
	// Git refuses an empty file as an index; it creates the file itself.
	_ = os.Remove(name)
	return name, func() { _ = os.Remove(name); _ = os.Remove(name + ".lock") }, nil
}

// withIndex runs git with GIT_INDEX_FILE set to idx.
func (r *Repo) withIndex(ctx context.Context, idx string, stdin io.Reader, args ...string) ([]byte, error) {
	return r.run(ctx, call{args: args, stdin: stdin, env: []string{"GIT_INDEX_FILE=" + idx}})
}

// hasHead reports whether HEAD points to a commit (false in an empty repo).
func (r *Repo) hasHead(ctx context.Context) (bool, error) {
	_, err := r.ResolveCommit(ctx, "HEAD")
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// loadIndex fills the temporary index idx with tree (or HEAD when tree is "",
// or the empty tree when there is no HEAD).
func (r *Repo) loadIndex(ctx context.Context, idx, tree string) error {
	if tree == "" {
		ok, err := r.hasHead(ctx)
		if err != nil {
			return err
		}
		if !ok {
			_, err := r.withIndex(ctx, idx, nil, "read-tree", "--empty")
			return err
		}
		tree = "HEAD"
	}
	_, err := r.withIndex(ctx, idx, nil, "read-tree", "--end-of-options", tree)
	return err
}

// WorkingTree returns the tree hash of the current working state: HEAD plus
// every tracked change and every untracked, non-ignored file. The worktree's
// index is not changed.
func (r *Repo) WorkingTree(ctx context.Context) (string, error) {
	return r.workingTree(ctx, nil)
}

// workingTree is WorkingTree plus forced, which are added even if ignored.
func (r *Repo) workingTree(ctx context.Context, forced []string) (string, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return "", err
	}
	idx, done, err := r.tempIndex(ctx)
	if err != nil {
		return "", err
	}
	defer done()
	if err := r.loadIndex(ctx, idx, ""); err != nil {
		return "", err
	}
	if _, err := r.withIndex(ctx, idx, nil, "add", "--all", "--", ":/"); err != nil {
		return "", err
	}
	if len(forced) > 0 {
		in := strings.Join(forced, "\x00") + "\x00"
		if _, err := r.withIndex(ctx, idx, strings.NewReader(in),
			"--literal-pathspecs", "add", "--all", "--force", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return "", err
		}
	}
	b, err := r.withIndex(ctx, idx, nil, "write-tree")
	return strings.TrimSpace(string(b)), err
}

// Checkpoint is a snapshot of the working state stored in a shadow ref.
type Checkpoint struct {
	Ref    string
	Commit string
	Tree   string
}

// Checkpoint snapshots the working state (including untracked, non-ignored
// files) into ref, which must be under refs/aco/. The snapshot commit's parent
// is HEAD. Neither the worktree, its index, nor any branch changes.
func (r *Repo) Checkpoint(ctx context.Context, ref, message string) (*Checkpoint, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(ref, RefPrefix) {
		return nil, fmt.Errorf("gitops: checkpoint ref must be under %s", RefPrefix)
	}
	if err := r.checkRefFormat(ctx, ref); err != nil {
		return nil, err
	}
	tree, err := r.WorkingTree(ctx)
	if err != nil {
		return nil, err
	}
	var parents []string
	if ok, err := r.hasHead(ctx); err != nil {
		return nil, err
	} else if ok {
		head, err := r.Head(ctx)
		if err != nil {
			return nil, err
		}
		parents = append(parents, head)
	}
	if message == "" {
		message = "aco checkpoint"
	}
	commit, err := r.commitTree(ctx, tree, parents, message, checkpointIdentity, false)
	if err != nil {
		return nil, err
	}
	if _, err := r.run(ctx, call{args: []string{"update-ref", "-m", "aco checkpoint", ref, commit}}); err != nil {
		return nil, err
	}
	return &Checkpoint{Ref: ref, Commit: commit, Tree: tree}, nil
}

// ErrResetMismatch reports that a reset did not reproduce the target tree.
var ErrResetMismatch = errors.New("gitops: working state differs from the target tree after reset")

// ResetTo restores the working state to exactly the tree of rev (a checkpoint
// ref, commit, or tree): tracked files are overwritten or deleted, untracked
// non-ignored files are removed, and files of the target tree are written.
// Ignored files stay. HEAD and the branch do not move; the index is set back
// to HEAD. It fails with ErrResetMismatch if the result differs from rev's
// tree, and with ErrNotOwnedWorktree, before touching anything, unless r is an
// ACO run or verification worktree (never the user's checkout or a worktree
// the user created).
func (r *Repo) ResetTo(ctx context.Context, rev string) error {
	ctx, err := r.scope(ctx)
	if err != nil {
		return err
	}
	if err := r.assertOwned(ctx, true, roleRun, roleVerify); err != nil {
		return err
	}
	tree, err := r.ResolveTree(ctx, rev)
	if err != nil {
		return err
	}
	if _, err := r.run(ctx, call{args: []string{"clean", "-f", "-d", "-q"}}); err != nil {
		return err
	}
	if _, err := r.run(ctx, call{args: []string{"read-tree", "--reset", "-u", "--end-of-options", tree}}); err != nil {
		return err
	}
	if err := r.syncIndexToHead(ctx); err != nil {
		return err
	}
	got, err := r.WorkingTree(ctx)
	if err != nil {
		return err
	}
	if got != tree {
		return ErrResetMismatch
	}
	return nil
}

// syncIndexToHead makes the worktree's index equal HEAD without touching files.
func (r *Repo) syncIndexToHead(ctx context.Context) error {
	ok, err := r.hasHead(ctx)
	if err != nil {
		return err
	}
	args := []string{"read-tree", "HEAD"}
	if !ok {
		args = []string{"read-tree", "--empty"}
	}
	if _, err := r.run(ctx, call{args: args}); err != nil {
		return err
	}
	// Refresh stat information; exit code 1 only means files differ from HEAD.
	if _, err := r.run(ctx, call{args: []string{"update-index", "-q", "--refresh"}}); err != nil && exitCode(err) != 1 {
		return err
	}
	return nil
}

func (r *Repo) checkRefFormat(ctx context.Context, ref string) error {
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("gitops: invalid ref name")
	}
	if _, err := r.run(ctx, call{args: []string{"check-ref-format", ref}}); err != nil {
		return fmt.Errorf("gitops: invalid ref name")
	}
	return nil
}
