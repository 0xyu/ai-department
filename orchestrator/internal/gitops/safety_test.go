package gitops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// filterCmd is a filter command that appends to marker when Git runs it.
func filterCmd(marker, tail string) string {
	return "sh -c 'echo ran >> \"" + filepath.ToSlash(marker) + "\"; " + tail + "'"
}

// TestExternalFiltersNeverRun configures clean, smudge, and process filters
// selected by .gitattributes and drives every staging and checkout path ACO
// uses: worktree creation, working-tree hashing, checkpoints, status, reset,
// and commits. No filter may run, and blobs keep the raw file bytes.
func TestExternalFiltersNeverRun(t *testing.T) {
	f := newFixture(t)
	write(t, f.user, ".gitattributes", "*.txt filter=mark\n*.dat filter=proc\n")
	write(t, f.user, "a.txt", "original text\n")
	write(t, f.user, "b.dat", "original data\n")
	gitT(t, f.env, f.user, "add", "-A")
	gitT(t, f.env, f.user, "commit", "-q", "-m", "filtered files")

	marker := filepath.Join(t.TempDir(), "filter-ran")
	gitT(t, f.env, f.user, "config", "filter.mark.clean", filterCmd(marker, "cat"))
	gitT(t, f.env, f.user, "config", "filter.mark.smudge", filterCmd(marker, "cat"))
	gitT(t, f.env, f.user, "config", "filter.proc.process", filterCmd(marker, "exit 1"))

	wt := f.worktree("aco/test-filters") // checkout: smudge / process
	head := must(wt.Head(f.ctx))
	write(t, wt.Dir(), "a.txt", "changed text\n")
	write(t, wt.Dir(), "b.dat", "changed data\n")
	tree := must(wt.WorkingTree(f.ctx)) // add: clean / process
	cp := must(wt.Checkpoint(f.ctx, CheckpointRef("r1", 1, 1, 1), "cp"))
	must(wt.IsClean(f.ctx)) // status
	check(wt.ResetTo(f.ctx, head))
	check(wt.ResetTo(f.ctx, cp.Ref))
	res := must(wt.CommitReviewed(f.ctx, []string{"a.txt", "b.dat"}, tree, nil, phaseMsg(tree)))
	verify := must(f.repo.AddDetachedWorktree(f.ctx, filepath.Join(t.TempDir(), "verify"), res.Commit))
	check(f.repo.RemoveWorktree(f.ctx, verify))

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an external Git filter ran during an ACO operation")
	}
	if got := gitT(t, f.env, wt.Dir(), "cat-file", "blob", res.Commit+":a.txt"); got != "changed text" {
		t.Fatalf("blob content %q", got)
	}
	// The setup is meaningful: plain git runs the configured filter.
	gitT(t, append(f.env, "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "idx")), wt.Dir(), "add", "a.txt")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("control: the filter did not run under plain git, so the test proves nothing")
	}
}

// TestRequiredFilterFailsClearly: a path that needs a filter marked required
// (such as Git LFS) cannot be staged or checked out without running it, so
// ACO fails with ErrFilterUnavailable and leaves no half-created worktree.
func TestRequiredFilterFailsClearly(t *testing.T) {
	f := newFixture(t)
	write(t, f.user, ".gitattributes", "*.req filter=req\n")
	write(t, f.user, "c.req", "content\n")
	gitT(t, f.env, f.user, "add", "-A")
	gitT(t, f.env, f.user, "commit", "-q", "-m", "required filter")
	marker := filepath.Join(t.TempDir(), "filter-ran")
	gitT(t, f.env, f.user, "config", "filter.req.clean", filterCmd(marker, "cat"))
	gitT(t, f.env, f.user, "config", "filter.req.smudge", filterCmd(marker, "cat"))
	gitT(t, f.env, f.user, "config", "filter.req.required", "true")

	write(t, f.user, "c.req", "changed\n")
	if _, err := f.repo.WorkingTree(f.ctx); !errors.Is(err, ErrFilterUnavailable) {
		t.Fatalf("staging: got %v", err)
	}
	base := must(f.repo.ResolveCommit(f.ctx, "refs/heads/main"))
	path := filepath.Join(t.TempDir(), "wt")
	if _, err := f.repo.AddWorktree(f.ctx, path, "aco/test-required", base); !errors.Is(err, ErrFilterUnavailable) {
		t.Fatalf("checkout: got %v", err)
	}
	if ok := must(f.repo.RefExists(f.ctx, "refs/heads/aco/test-required")); ok {
		t.Fatal("failed worktree creation left its branch")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("failed worktree creation left its directory")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the required filter ran")
	}
}

// userState captures everything of the user's checkout that ACO must never
// change: working files, the index, HEAD, and the base branch.
type userState struct {
	files map[string]string
	index string
	head  string
	base  string
}

func captureUser(t *testing.T, f *fixture) userState {
	t.Helper()
	return userState{
		files: snapshotDir(t, f.user, ".git"),
		index: read(t, f.user, ".git/index"),
		head:  read(t, f.user, ".git/HEAD"),
		base:  gitT(t, f.env, f.user, "rev-parse", "refs/heads/main"),
	}
}

func (s userState) requireSame(t *testing.T, f *fixture) {
	t.Helper()
	now := captureUser(t, f)
	if d := diffSnapshots(s.files, now.files); len(d) > 0 {
		t.Fatalf("user files changed: %v", d)
	}
	if now.index != s.index || now.head != s.head || now.base != s.base {
		t.Fatal("user index, HEAD, or base branch changed")
	}
}

// TestMutationsRefuseUnownedTargets: commits, resets, and worktree removal
// refuse the user's checkout, a worktree the user created, a worktree of
// another repository, and a run worktree whose HEAD left the run branch,
// before writing anything.
func TestMutationsRefuseUnownedTargets(t *testing.T) {
	f := newFixture(t)
	// Uncommitted user work that a reset or commit would destroy or capture.
	write(t, f.user, "README.md", "# demo\nuser edit\n")
	write(t, f.user, "scratch.txt", "untracked user file\n")
	gitT(t, f.env, f.user, "add", "scratch.txt")
	userWT := filepath.Join(t.TempDir(), "user-wt")
	gitT(t, f.env, f.user, "worktree", "add", "-q", "-b", "user-branch", userWT)
	write(t, userWT, "mine.txt", "user worktree work\n")
	userWTFiles := snapshotDir(t, userWT)
	userBranch := gitT(t, f.env, f.user, "rev-parse", "refs/heads/user-branch")
	before := captureUser(t, f)
	tree := must(f.repo.WorkingTree(f.ctx))
	common := must(f.repo.locate(f.ctx)).commonDir

	refuse := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrNotOwnedWorktree) {
			t.Fatalf("%s: got %v, want ErrNotOwnedWorktree", name, err)
		}
	}
	commit := func(r *Repo) error {
		_, err := r.Commit(f.ctx, CommitSpec{Paths: []string{"README.md", "scratch.txt"}, Message: phaseMsg(tree)})
		return err
	}
	reviewed := func(r *Repo) error {
		_, err := r.CommitReviewed(f.ctx, []string{"README.md", "scratch.txt"}, tree, nil, phaseMsg(tree))
		return err
	}

	// The user's checkout through Open.
	refuse("commit user checkout", commit(f.repo))
	refuse("reviewed commit user checkout", reviewed(f.repo))
	refuse("reset user checkout", f.repo.ResetTo(f.ctx, "HEAD"))

	// Forged ownership of the user's checkout and of the user's own worktree.
	forgedUser := f.repo.at(f.user)
	forgedUser.role, forgedUser.branch, forgedUser.commonDir = roleRun, "refs/heads/main", common
	refuse("commit forged user checkout", commit(forgedUser))
	refuse("reset forged user checkout", forgedUser.ResetTo(f.ctx, "HEAD"))
	refuse("remove forged user checkout", f.repo.RemoveWorktree(f.ctx, forgedUser))
	forgedWT := f.repo.at(userWT)
	forgedWT.role, forgedWT.branch, forgedWT.commonDir = roleRun, "refs/heads/user-branch", common
	refuse("commit user worktree", commit(forgedWT))
	refuse("reset user worktree", forgedWT.ResetTo(f.ctx, "HEAD"))
	refuse("remove user worktree", f.repo.RemoveWorktree(f.ctx, forgedWT))
	forgedWT.role, forgedWT.branch = roleVerify, ""
	refuse("remove user worktree as verify", f.repo.RemoveWorktree(f.ctx, forgedWT))

	// OpenWorktree validates the target.
	for _, c := range []struct{ path, branch string }{
		{f.user, "main"}, {f.user, ""}, {userWT, "user-branch"}, {userWT, ""}, {t.TempDir(), "x"},
	} {
		if _, err := f.repo.OpenWorktree(f.ctx, c.path, c.branch); err == nil {
			t.Fatalf("OpenWorktree(%s, %q) accepted", c.path, c.branch)
		}
	}

	// A run worktree whose HEAD was switched to another branch.
	wt := f.worktree("aco/test-own")
	runHead := must(wt.Head(f.ctx))
	gitT(t, f.env, wt.Dir(), "switch", "-q", "-c", "elsewhere")
	write(t, wt.Dir(), "src/x.go", "package src\n")
	x := must(wt.WorkingTree(f.ctx))
	if _, err := wt.CommitReviewed(f.ctx, []string{"src/x.go"}, x, nil, phaseMsg(x)); !errors.Is(err, ErrNotOwnedWorktree) {
		t.Fatalf("commit after HEAD left the run branch: %v", err)
	}
	refuse("reset after HEAD left the run branch", wt.ResetTo(f.ctx, "HEAD"))
	if _, err := f.repo.OpenWorktree(f.ctx, wt.Dir(), "elsewhere"); err == nil {
		t.Fatal("run worktree opened on a branch it was not created for")
	}
	if must(f.repo.ResolveCommit(f.ctx, "refs/heads/aco/test-own")) != runHead ||
		must(f.repo.ResolveCommit(f.ctx, "refs/heads/elsewhere")) != runHead {
		t.Fatal("a refused commit moved a branch")
	}
	gitT(t, f.env, wt.Dir(), "switch", "-q", "aco/test-own")
	reopened := must(f.repo.OpenWorktree(f.ctx, wt.Dir(), "aco/test-own"))
	if reopened.Branch() != "refs/heads/aco/test-own" {
		t.Fatalf("reopened branch %q", reopened.Branch())
	}

	// A worktree of another repository.
	other := newFixture(t)
	owt := other.worktree("aco/test-other")
	refuse("remove other repository's worktree", f.repo.RemoveWorktree(f.ctx, owt))
	if _, err := os.Stat(owt.Dir()); err != nil {
		t.Fatal("other repository's worktree removed")
	}

	before.requireSame(t, f)
	if d := diffSnapshots(userWTFiles, snapshotDir(t, userWT)); len(d) > 0 {
		t.Fatalf("user worktree changed: %v", d)
	}
	if gitT(t, f.env, f.user, "rev-parse", "refs/heads/user-branch") != userBranch {
		t.Fatal("user branch moved")
	}

	// The owned run worktree is still usable and removable.
	check(f.repo.RemoveWorktree(f.ctx, reopened))
	before.requireSame(t, f)
}
