//go:build integration

package gitops

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSimulatedRun drives a whole run through the Git manager: worktree →
// change → checkpoint → change → reset → freeze commit → phase commits →
// blocked WIP → tags and notes → Stage 4 worktree → prune → cleanup. The
// user's checkout (files, index, HEAD, base branch) must be byte-identical
// before and after (AC-04.1, AC-04.6).
func TestSimulatedRun(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx
	// Leave uncommitted user work in the checkout: it must survive untouched.
	write(t, f.user, "README.md", "# demo\nuser edit in progress\n")
	write(t, f.user, "notes.txt", "untracked user file\n")
	gitT(t, f.env, f.user, "add", "notes.txt") // staged in the user's index

	userFiles := snapshotDir(t, f.user, ".git")
	gitFiles := map[string]string{}
	for _, p := range []string{".git/index", ".git/HEAD"} {
		gitFiles[p] = read(t, f.user, p)
	}
	baseRef := gitT(t, f.env, f.user, "rev-parse", "refs/heads/main")
	userStatus := gitT(t, f.env, f.user, "status", "--porcelain")

	pre := must(f.repo.Preflight(ctx, "main", ""))
	runID := "20261109-r1"
	branch := BranchName(testDate, "Demo plan", "r1")
	wtPath := filepath.Join(t.TempDir(), "worktrees", runID)
	wt := must(f.repo.AddWorktree(ctx, wtPath, branch, pre.BaseCommit))
	if _, err := os.Stat(filepath.Join(wtPath, "notes.txt")); err == nil {
		t.Fatal("user's uncommitted file leaked into the run worktree")
	}

	// Stage 1–2 output and freeze.
	write(t, wt.Dir(), ".aco/plan/IMPLEMENTATION_PLAN.md", "# plan\n")
	write(t, wt.Dir(), ".aco/plan/plan.lock.json", "{}\n")
	write(t, wt.Dir(), "AGENTS.md", "follow the plan\n")
	freeze := must(wt.Commit(ctx, CommitSpec{
		Paths:        []string{".aco/plan/IMPLEMENTATION_PLAN.md", ".aco/plan/plan.lock.json", "AGENTS.md"},
		RequireExact: true,
		ExpectedHead: pre.BaseCommit,
		Message:      FreezeMessage(FreezeInfo{RunID: runID, PlanHash: "sha256:p", ReviewRounds: 1, ReviewerModel: "gpt"}),
	}))
	check(wt.Tag(ctx, TagName(runID, "plan-v1"), freeze.Commit, ""))

	// Phase 1: change → checkpoint → change → reset restores the exact tree.
	cp0 := must(wt.Checkpoint(ctx, CheckpointRef(runID, 1, 1, 0), "before implement"))
	write(t, wt.Dir(), "src/app.go", "package src\n\nfunc A() int { return 10 }\n")
	write(t, wt.Dir(), "src/app_test.go", "package src\n\n// test A\n")
	cp1 := must(wt.Checkpoint(ctx, CheckpointRef(runID, 1, 1, 1), "after implement"))
	write(t, wt.Dir(), "src/app.go", "package src\n// broken fix attempt\n")
	write(t, wt.Dir(), "src/junk.go", "package src\n")
	check(wt.ResetTo(ctx, cp1.Ref))
	if got := must(wt.WorkingTree(ctx)); got != cp1.Tree {
		t.Fatal("reset did not restore the checkpoint")
	}
	if cp0.Tree == cp1.Tree {
		t.Fatal("checkpoints should differ")
	}

	reviewed := must(wt.WorkingTree(ctx))
	paths := must(wt.ChangedPaths(ctx, "HEAD", reviewed))
	write(t, wt.Dir(), ".aco/reports/phase-01.md", "# Phase 1\n")
	write(t, wt.Dir(), ".aco/reports/phase-01.json", "{\"phase\":1}\n")
	reports := []string{".aco/reports/phase-01.md", ".aco/reports/phase-01.json"}
	key := PhaseAttemptKey(runID, 1, 1)
	if dup := must(wt.FindCommitByTrailer(ctx, "HEAD", "ACO-Phase-Attempt", key)); dup != "" {
		t.Fatal("duplicate check found a commit before committing")
	}
	msg := PhaseMessage(PhaseInfo{RunID: runID, Phase: 1, Title: "App", Attempt: 1, PlanHash: "sha256:p", ReviewedTree: reviewed, ReviewRounds: 1, ReviewerModel: "gpt"})
	p1 := must(wt.CommitReviewed(ctx, paths, reviewed, reports, msg))
	check(wt.Tag(ctx, PhaseTag(runID, 1), p1.Commit, ""))
	check(wt.AddNote(ctx, p1.Commit, []byte(`{"phase":1}`)))
	if dup := must(wt.FindCommitByTrailer(ctx, "HEAD", "ACO-Phase-Attempt", key)); dup != p1.Commit {
		t.Fatal("duplicate check missed the phase commit")
	}

	// Phase 2 gets blocked: WIP saved, run branch untouched, worktree reset.
	write(t, wt.Dir(), "src/wip.go", "package src\n// unfinished\n")
	headBefore := must(wt.Head(ctx))
	bl := must(wt.SaveBlocked(ctx, runID, 2, "iteration_limit"))
	if must(wt.Head(ctx)) != headBefore {
		t.Fatal("blocked phase moved the run branch")
	}
	if ls := gitT(t, f.env, wt.Dir(), "ls-tree", "-r", "--name-only", bl.Commit); !strings.Contains(ls, "src/wip.go") {
		t.Fatal("WIP ref lacks the work in progress")
	}
	check(wt.ResetTo(ctx, "HEAD"))

	// Trailers and notes are readable from the user's repository.
	tr := must(f.repo.TrailersOf(ctx, "refs/tags/"+PhaseTag(runID, 1)))
	if !slices.ContainsFunc(tr, func(x Trailer) bool { return x.Key == "ACO-Reviewed-Tree" && x.Value == reviewed }) {
		t.Fatalf("trailers %v", tr)
	}
	if n := must(f.repo.Note(ctx, p1.Commit)); !strings.Contains(string(n), `"phase":1`) {
		t.Fatalf("note %q", n)
	}
	log := gitT(t, f.env, f.user, "log", "--format=%s", branch)
	if log != "feat(phase-01): App\ndocs(aco): freeze implementation plan v1\ninitial" {
		t.Fatalf("run branch history:\n%s", log)
	}

	// Stage 4: clean detached worktree at the final HEAD.
	finalPath := filepath.Join(t.TempDir(), "final")
	final := must(f.repo.AddDetachedWorktree(ctx, finalPath, p1.Commit))
	if !must(final.IsClean(ctx)) || read(t, finalPath, "src/app.go") != "package src\n\nfunc A() int { return 10 }\n" {
		t.Fatal("final worktree content")
	}
	check(f.repo.RemoveWorktree(ctx, final))

	// Cleanup after success: prune checkpoints, keep the blocked WIP.
	if n := must(wt.PruneRefs(ctx, runID, true)); n != 2 {
		t.Fatalf("pruned %d refs", n)
	}
	check(f.repo.RemoveWorktree(ctx, wt))

	// The user's checkout is byte-identical.
	if d := diffSnapshots(userFiles, snapshotDir(t, f.user, ".git")); len(d) > 0 {
		t.Fatalf("user checkout changed: %v", d)
	}
	for p, v := range gitFiles {
		if read(t, f.user, p) != v {
			t.Fatalf("%s changed", p)
		}
	}
	if gitT(t, f.env, f.user, "rev-parse", "refs/heads/main") != baseRef {
		t.Fatal("base branch moved")
	}
	if gitT(t, f.env, f.user, "status", "--porcelain") != userStatus {
		t.Fatal("user status changed")
	}
}
