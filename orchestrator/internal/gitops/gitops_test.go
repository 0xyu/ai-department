package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
)

func TestBranchName(t *testing.T) {
	got := BranchName(testDate, "AI Coding Orchestrator — v1 (draft)!", "a1B2c3")
	if got != "aco/20261109-ai-coding-orchestrator-v1-draft-a1b2c3" {
		t.Fatalf("got %q", got)
	}
	if g := BranchName(testDate, "计划", ""); g != "aco/20261109-plan-run" {
		t.Fatalf("non-ASCII title: %q", g)
	}
	f := newFixture(t)
	if err := f.repo.CheckBranchName(f.ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.CheckBranchName(f.ctx, "bad..name"); err == nil {
		t.Fatal("bad branch name accepted")
	}
}

func TestPreflight(t *testing.T) {
	f := newFixture(t)
	res, err := f.repo.Preflight(f.ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.BaseBranch != "main" || len(res.BaseCommit) < 40 {
		t.Fatalf("unexpected result %+v", res)
	}
	if _, err := f.repo.Preflight(f.ctx, "nope", ""); !errors.Is(err, ErrNoBase) {
		t.Fatalf("missing base: %v", err)
	}
	used := t.TempDir()
	write(t, used, "x", "x")
	if _, err := f.repo.Preflight(f.ctx, "main", used); !errors.Is(err, ErrWorktreePathUsed) {
		t.Fatalf("used worktree path: %v", err)
	}
	notRepo, _ := Open(t.TempDir(), Options{Env: f.env})
	if _, err := notRepo.Preflight(f.ctx, "", ""); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("not a repo: %v", err)
	}
	if !versionAtLeast("2.30.0", MinVersion) || versionAtLeast("2.29.9", MinVersion) || !versionAtLeast("3.0", MinVersion) {
		t.Fatal("versionAtLeast")
	}
}

func TestWorkingTreeDoesNotTouchIndex(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("aco/test-index")
	idxPath := must(wt.gitPath(f.ctx, "index"))
	before := must(os.ReadFile(idxPath))
	write(t, wt.Dir(), "src/new.go", "package src\n")
	write(t, wt.Dir(), "build/out.bin", "ignored")
	tree := must(wt.WorkingTree(f.ctx))
	cp := must(wt.Checkpoint(f.ctx, CheckpointRef("r1", 1, 1, 1), "cp"))
	if cp.Tree != tree {
		t.Fatal("checkpoint tree differs from working tree")
	}
	after := must(os.ReadFile(idxPath))
	if string(before) != string(after) {
		t.Fatal("worktree index changed")
	}
	// The untracked file is in the tree; the ignored one is not.
	ls := gitT(t, f.env, wt.Dir(), "ls-tree", "-r", "--name-only", tree)
	if !strings.Contains(ls, "src/new.go") || strings.Contains(ls, "build/out.bin") {
		t.Fatalf("tree contents: %s", ls)
	}
	if st := gitT(t, f.env, wt.Dir(), "status", "--porcelain"); st != "?? src/new.go" {
		t.Fatalf("status changed: %q", st)
	}
}

func TestCheckpointAndReset(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("aco/test-reset")
	head := must(wt.Head(f.ctx))
	write(t, wt.Dir(), "src/app.go", "package src\n\nfunc A() int { return 2 }\n")
	write(t, wt.Dir(), "src/extra.go", "package src\n")
	if err := os.Remove(filepath.Join(wt.Dir(), "README.md")); err != nil {
		t.Fatal(err)
	}
	cp := must(wt.Checkpoint(f.ctx, CheckpointRef("r1", 1, 1, 1), "after step 1"))

	// More changes after the checkpoint.
	write(t, wt.Dir(), "src/app.go", "package src\n// broken\n")
	write(t, wt.Dir(), "src/later.go", "package src\n")
	write(t, wt.Dir(), "deep/dir/file.txt", "x")
	if err := os.Remove(filepath.Join(wt.Dir(), "src", "extra.go")); err != nil {
		t.Fatal(err)
	}
	write(t, wt.Dir(), "app.log", "kept: ignored")

	if err := wt.ResetTo(f.ctx, cp.Ref); err != nil {
		t.Fatal(err)
	}
	if got := must(wt.WorkingTree(f.ctx)); got != cp.Tree {
		t.Fatal("reset did not restore the checkpoint tree")
	}
	if read(t, wt.Dir(), "src/app.go") != "package src\n\nfunc A() int { return 2 }\n" {
		t.Fatal("modified file not restored")
	}
	for _, gone := range []string{"src/later.go", "deep/dir/file.txt", "README.md"} {
		if _, err := os.Stat(filepath.Join(wt.Dir(), filepath.FromSlash(gone))); err == nil {
			t.Fatalf("%s still present", gone)
		}
	}
	if read(t, wt.Dir(), "src/extra.go") != "package src\n" {
		t.Fatal("untracked checkpoint file not restored")
	}
	if read(t, wt.Dir(), "app.log") != "kept: ignored" {
		t.Fatal("ignored file removed")
	}
	if must(wt.Head(f.ctx)) != head {
		t.Fatal("HEAD moved")
	}
	// Index equals HEAD again: no staged changes.
	if staged := gitT(t, f.env, wt.Dir(), "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("index not reset to HEAD: %q", staged)
	}
	// Reset to HEAD (phase restart) gives a clean worktree.
	if err := wt.ResetTo(f.ctx, head); err != nil {
		t.Fatal(err)
	}
	if !must(wt.IsClean(f.ctx)) {
		t.Fatal("not clean after reset to HEAD")
	}
	if err := wt.ResetTo(f.ctx, "refs/aco/nope"); err == nil {
		t.Fatal("reset to a missing ref succeeded")
	}
	if _, err := wt.Checkpoint(f.ctx, "refs/heads/x", ""); err == nil {
		t.Fatal("checkpoint outside refs/aco accepted")
	}
}

func phaseMsg(tree string) Message {
	return PhaseMessage(PhaseInfo{
		RunID: "r1", Phase: 1, Title: "Foundation", Attempt: 1, PlanHash: "sha256:abc",
		ReviewedTree: tree, ReviewRounds: 2, ReviewerModel: "gpt-sol-6.1",
		Summary: "Adds the foundation.", AC: []ACRow{{"AC-01.1", "met", "CLI | help"}},
	})
}

func TestCommitReviewed(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("aco/test-commit")
	head := must(wt.Head(f.ctx))
	write(t, wt.Dir(), "src/app.go", "package src\n\nfunc A() int { return 3 }\n")
	write(t, wt.Dir(), "src/b.go", "package src\n")
	if err := os.Remove(filepath.Join(wt.Dir(), "src", "app_test.go")); err != nil {
		t.Fatal(err)
	}
	reviewed := must(wt.WorkingTree(f.ctx))
	paths := must(wt.ChangedPaths(f.ctx, head, reviewed))
	if !slices.Equal(paths, []string{"src/app.go", "src/app_test.go", "src/b.go"}) {
		t.Fatalf("changed paths %v", paths)
	}

	// Report written after review.
	write(t, wt.Dir(), ".aco/reports/phase-01.md", "# report\n")
	write(t, wt.Dir(), ".aco/reports/phase-01.json", "{}\n")
	extras := []string{".aco/reports/phase-01.md", ".aco/reports/phase-01.json"}

	t.Run("unreviewed file", func(t *testing.T) {
		write(t, wt.Dir(), "src/sneaky.go", "package src\n")
		defer os.Remove(filepath.Join(wt.Dir(), "src", "sneaky.go"))
		if _, err := wt.CommitReviewed(f.ctx, paths, reviewed, extras, phaseMsg(reviewed)); !errors.Is(err, ErrUnreviewedChange) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unreviewed edit to a reviewed path", func(t *testing.T) {
		write(t, wt.Dir(), "src/b.go", "package src\n// edited after review\n")
		defer write(t, wt.Dir(), "src/b.go", "package src\n")
		if _, err := wt.CommitReviewed(f.ctx, paths, reviewed, extras, phaseMsg(reviewed)); !errors.Is(err, ErrTreeMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing reviewed path", func(t *testing.T) {
		if _, err := wt.CommitReviewed(f.ctx, paths[:2], reviewed, extras, phaseMsg(reviewed)); !errors.Is(err, ErrTreeMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("bad paths", func(t *testing.T) {
		for _, p := range []string{"../x", "/abs", `a\b`, ".git/config", "a/./b", "C:/x"} {
			if _, err := wt.CommitReviewed(f.ctx, []string{p}, reviewed, nil, phaseMsg(reviewed)); !errors.Is(err, ErrBadPath) {
				t.Fatalf("%s: got %v", p, err)
			}
		}
	})
	t.Run("branch moved", func(t *testing.T) {
		if _, err := wt.Commit(f.ctx, CommitSpec{Paths: paths, Extra: extras, ExpectedTree: reviewed, ExpectedHead: strings.Repeat("1", 40), Message: phaseMsg(reviewed)}); !errors.Is(err, ErrBranchMoved) {
			t.Fatalf("got %v", err)
		}
	})
	// ExtraData binds the commit to the generated report bytes and mode.
	data := map[string][]byte{".aco/reports/phase-01.md": []byte("# report\n"), ".aco/reports/phase-01.json": []byte("{}\n")}
	exact := func(d map[string][]byte) error {
		_, err := wt.Commit(f.ctx, CommitSpec{Paths: paths, Extra: extras, ExtraData: d, ExpectedTree: reviewed, ExpectedHead: head,
			Message: phaseMsg(reviewed)})
		return err
	}
	t.Run("report replaced", func(t *testing.T) {
		write(t, wt.Dir(), ".aco/reports/phase-01.json", "{\"tampered\":true}\n")
		defer write(t, wt.Dir(), ".aco/reports/phase-01.json", "{}\n")
		if err := exact(data); !errors.Is(err, ErrExtraMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("report not the generated bytes", func(t *testing.T) {
		d := map[string][]byte{".aco/reports/phase-01.md": []byte("# report\n"), ".aco/reports/phase-01.json": []byte("{ }\n")}
		if err := exact(d); !errors.Is(err, ErrExtraMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("report made executable", func(t *testing.T) {
		gitT(t, f.env, wt.Dir(), "config", "core.fileMode", "true")
		defer gitT(t, f.env, wt.Dir(), "config", "core.fileMode", "false")
		target := filepath.Join(wt.Dir(), ".aco", "reports", "phase-01.json")
		if err := os.Chmod(target, 0o755); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(target, 0o644)
		if fi, _ := os.Stat(target); fi.Mode().Perm()&0o100 == 0 {
			t.Skip("the file system has no executable bit")
		}
		if err := exact(data); !errors.Is(err, ErrExtraMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("blob mode and content", func(t *testing.T) {
		// Trees built directly, so the mode check runs on any file system.
		blob := gitT(t, f.env, wt.Dir(), "hash-object", "-w", ".aco/reports/phase-01.json")
		mk := func(mode string) string {
			cmd := exec.Command("git", "mktree")
			cmd.Dir, cmd.Env = wt.Dir(), append(os.Environ(), f.env...)
			cmd.Stdin = strings.NewReader(mode + " blob " + blob + "\tr.json\n")
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(string(out))
		}
		want := map[string][]byte{"r.json": []byte("{}\n")}
		if err := wt.verifyBlobs(f.ctx, mk("100644"), want); err != nil {
			t.Fatalf("regular file: %v", err)
		}
		if err := wt.verifyBlobs(f.ctx, mk("100755"), want); !errors.Is(err, ErrExtraMismatch) {
			t.Fatalf("executable: %v", err)
		}
		if err := wt.verifyBlobs(f.ctx, mk("100644"), map[string][]byte{"r.json": []byte("{}\r\n")}); !errors.Is(err, ErrExtraMismatch) {
			t.Fatalf("other bytes: %v", err)
		}
		if err := wt.verifyBlobs(f.ctx, mk("100644"), map[string][]byte{"missing.json": nil}); !errors.Is(err, ErrExtraMismatch) {
			t.Fatalf("missing: %v", err)
		}
	})
	t.Run("data for a path that is not extra", func(t *testing.T) {
		if err := exact(map[string][]byte{"src/b.go": []byte("package src\n")}); err == nil || errors.Is(err, ErrExtraMismatch) {
			t.Fatalf("got %v", err)
		}
	})

	if must(wt.Head(f.ctx)) != head {
		t.Fatal("a refused commit moved the branch")
	}
	res, err := wt.Commit(f.ctx, CommitSpec{Paths: paths, Extra: extras, ExtraData: data, ExpectedTree: reviewed, ExpectedHead: head,
		Message: phaseMsg(reviewed)})
	if err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, f.env, wt.Dir(), "cat-file", "blob", res.Commit+":.aco/reports/phase-01.json"); got != "{}" {
		t.Fatalf("committed report %q", got)
	}
	if must(wt.Head(f.ctx)) != res.Commit {
		t.Fatal("branch not advanced")
	}
	if !must(wt.IsClean(f.ctx)) {
		t.Fatal("worktree not clean after commit")
	}
	ls := gitT(t, f.env, wt.Dir(), "ls-tree", "-r", "--name-only", res.Commit)
	if strings.Contains(ls, "app_test.go") || !strings.Contains(ls, ".aco/reports/phase-01.json") {
		t.Fatalf("committed tree: %s", ls)
	}
	// The committed tree minus the reports is exactly the reviewed tree.
	gitT(t, f.env, wt.Dir(), "rm", "-q", "--cached", ".aco/reports/phase-01.md", ".aco/reports/phase-01.json")
	if got := gitT(t, f.env, wt.Dir(), "write-tree"); got != reviewed {
		t.Fatal("committed tree minus reports differs from the reviewed tree")
	}
}

func TestCommitTrailersTagsNotes(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree(BranchName(testDate, "demo plan", "abc123"))

	// Freeze commit: plan files only, nothing else in the working state.
	write(t, wt.Dir(), ".aco/plan/IMPLEMENTATION_PLAN.md", "# plan\n")
	write(t, wt.Dir(), ".aco/plan/plan.lock.json", "{}\n")
	freeze := must(wt.Commit(f.ctx, CommitSpec{
		Paths:        []string{".aco/plan/IMPLEMENTATION_PLAN.md", ".aco/plan/plan.lock.json"},
		RequireExact: true,
		Message:      FreezeMessage(FreezeInfo{RunID: "r1", PlanHash: "sha256:abc", ReviewRounds: 2, ReviewerModel: "gpt-sol-6.1"}),
	}))
	if err := wt.Tag(f.ctx, TagName("r1", "plan-v1"), freeze.Commit, "plan v1"); err != nil {
		t.Fatal(err)
	}
	got := must(wt.TrailersOf(f.ctx, freeze.Commit))
	want := []Trailer{{"ACO-Run", "r1"}, {"ACO-Plan-Hash", "sha256:abc"}, {"ACO-Plan-Review-Rounds", "2"}, {"Reviewed-by", "gpt-sol-6.1"}}
	if !slices.Equal(got, want) {
		t.Fatalf("freeze trailers %v", got)
	}
	subj := gitT(t, f.env, wt.Dir(), "log", "-1", "--format=%s", freeze.Commit)
	if subj != "docs(aco): freeze implementation plan v1" {
		t.Fatalf("subject %q", subj)
	}

	// RequireExact refuses a freeze with stray changes.
	write(t, wt.Dir(), "stray.txt", "x")
	if _, err := wt.Commit(f.ctx, CommitSpec{Paths: []string{".aco/plan/plan.lock.json"}, RequireExact: true, Message: FreezeMessage(FreezeInfo{})}); !errors.Is(err, ErrUnreviewedChange) {
		t.Fatalf("stray change: %v", err)
	}
	os.Remove(filepath.Join(wt.Dir(), "stray.txt"))

	// Phase commit.
	write(t, wt.Dir(), "src/app.go", "package src\n\nfunc A() int { return 4 }\n")
	reviewed := must(wt.WorkingTree(f.ctx))
	write(t, wt.Dir(), ".aco/reports/phase-01.md", "# r\n")
	phase := must(wt.CommitReviewed(f.ctx, []string{"src/app.go"}, reviewed, []string{".aco/reports/phase-01.md"}, phaseMsg(reviewed)))
	tr := must(wt.TrailersOf(f.ctx, phase.Commit))
	keys := map[string]string{}
	for _, x := range tr {
		keys[x.Key] = x.Value
	}
	for _, k := range []string{"ACO-Run", "ACO-Phase", "ACO-Phase-Attempt", "ACO-Plan-Hash", "ACO-Reviewed-Tree", "ACO-Review-Rounds", "Reviewed-by", "Co-Authored-By"} {
		if keys[k] == "" {
			t.Errorf("trailer %s missing in %v", k, tr)
		}
	}
	if keys["ACO-Reviewed-Tree"] != reviewed || keys["ACO-Phase"] != "01" || keys["ACO-Phase-Attempt"] != "r1/P01/a1" {
		t.Fatalf("trailer values %v", keys)
	}
	body := gitT(t, f.env, wt.Dir(), "log", "-1", "--format=%B", phase.Commit)
	if !strings.Contains(body, `| AC-01.1 | met | CLI \| help |`) || !strings.Contains(body, "Review rounds: 2") {
		t.Fatalf("body:\n%s", body)
	}
	if err := wt.Tag(f.ctx, PhaseTag("r1", 1), phase.Commit, ""); err != nil {
		t.Fatal(err)
	}
	if err := wt.Tag(f.ctx, PhaseTag("r1", 1), phase.Commit, ""); err != nil {
		t.Fatalf("re-tag same commit: %v", err)
	}
	if err := wt.Tag(f.ctx, PhaseTag("r1", 1), freeze.Commit, ""); !errors.Is(err, ErrTagExists) {
		t.Fatalf("re-tag other commit: %v", err)
	}
	if at := must(f.repo.ResolveCommit(f.ctx, "refs/tags/"+PhaseTag("r1", 1))); at != phase.Commit {
		t.Fatal("tag not readable from the main repository")
	}
	note := []byte(`{"phase":1,"commit":"` + phase.Commit + `"}`)
	if err := wt.AddNote(f.ctx, phase.Commit, note); err != nil {
		t.Fatal(err)
	}
	if n := must(f.repo.Note(f.ctx, phase.Commit)); strings.TrimSpace(string(n)) != string(note) {
		t.Fatalf("note %q", n)
	}
	if n := must(wt.Note(f.ctx, freeze.Commit)); n != nil {
		t.Fatal("unexpected note")
	}
	if sha := must(wt.FindCommitByTrailer(f.ctx, "HEAD", "ACO-Phase-Attempt", "r1/P01/a1")); sha != phase.Commit {
		t.Fatalf("FindCommitByTrailer = %q", sha)
	}
	if sha := must(wt.FindCommitByTrailer(f.ctx, "HEAD", "ACO-Phase-Attempt", "r1/P01/a2")); sha != "" {
		t.Fatal("found a commit for another attempt")
	}
}

func TestMessageCannotInjectTrailers(t *testing.T) {
	m := Message{Subject: "feat: x\n\nACO-Run: forged", Trailers: []Trailer{{"Reviewed-by", "m\nACO-Phase: 99"}}}
	s := m.String()
	if strings.Count(s, "\n") != 3 || strings.Contains(s, "\nACO-Phase") || strings.Contains(s, "\nACO-Run") {
		t.Fatalf("message %q", s)
	}
}

func TestChanges(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("aco/test-changes")
	head := must(wt.Head(f.ctx))
	if err := os.Rename(filepath.Join(wt.Dir(), "README.md"), filepath.Join(wt.Dir(), "docs.md")); err != nil {
		t.Fatal(err)
	}
	write(t, wt.Dir(), "img.bin", "\x00\x01\x02binary")
	write(t, wt.Dir(), "src/app.go", "package src\n\nfunc A() int { return 5 }\nfunc B() {}\n")
	if err := os.Remove(filepath.Join(wt.Dir(), "src", "app_test.go")); err != nil {
		t.Fatal(err)
	}
	tree := must(wt.WorkingTree(f.ctx))
	ch := must(wt.Changes(f.ctx, head, tree))
	by := map[string]Change{}
	for _, c := range ch {
		by[c.Path] = c
	}
	if c := by["docs.md"]; c.Status != Renamed || c.OldPath != "README.md" {
		t.Fatalf("rename: %+v", c)
	}
	if c := by["img.bin"]; c.Status != Added || !c.Binary || c.Size != 9 {
		t.Fatalf("binary: %+v", c)
	}
	if c := by["src/app.go"]; c.Status != Modified || c.Added != 2 || c.Deleted != 1 {
		t.Fatalf("modified: %+v", c)
	}
	if c := by["src/app_test.go"]; c.Status != Deleted {
		t.Fatalf("deleted: %+v", c)
	}
	patch := string(must(wt.Patch(f.ctx, head, tree, 0)))
	if !strings.Contains(patch, "+++ b/src/app.go") || !strings.Contains(patch, "+func B() {}") {
		t.Fatalf("patch:\n%s", patch)
	}
	if st := must(wt.DiffStat(f.ctx, head, tree)); !strings.Contains(st, "src/app.go") {
		t.Fatalf("stat: %s", st)
	}
}

func TestBlockedRefsAndPrune(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("aco/test-prune")
	head := must(wt.Head(f.ctx))
	for i := int64(1); i <= 3; i++ {
		write(t, wt.Dir(), "src/step.go", "package src\n// "+string(rune('0'+i))+"\n")
		must(wt.Checkpoint(f.ctx, CheckpointRef("r1", 2, 1, i), "step"))
	}
	bl := must(wt.SaveBlocked(f.ctx, "r1", 2, "iteration_limit\nextra"))
	if must(wt.Head(f.ctx)) != head {
		t.Fatal("SaveBlocked moved HEAD")
	}
	if subj := gitT(t, f.env, wt.Dir(), "log", "-1", "--format=%s", bl.Ref); subj != "aco: blocked phase 02: iteration_limit extra" {
		t.Fatalf("blocked subject %q", subj)
	}
	refs := must(wt.ShadowRefs(f.ctx, "r1"))
	if len(refs) != 4 {
		t.Fatalf("refs %v", refs)
	}
	n := must(wt.PruneRefs(f.ctx, "r1", true))
	if n != 3 {
		t.Fatalf("pruned %d", n)
	}
	if refs := must(wt.ShadowRefs(f.ctx, "r1")); !slices.Equal(refs, []string{BlockedRef("r1", 2)}) {
		t.Fatalf("after prune %v", refs)
	}
	must(wt.PruneRefs(f.ctx, "r1", false))
	if refs := must(wt.ShadowRefs(f.ctx, "r1")); len(refs) != 0 {
		t.Fatalf("after full prune %v", refs)
	}
	if _, err := wt.ShadowRefs(f.ctx, "../x"); err == nil {
		t.Fatal("bad run id accepted")
	}
}

func TestDetachedWorktreeAndRemove(t *testing.T) {
	f := newFixture(t)
	wt := f.worktree("aco/test-detached")
	head := must(wt.Head(f.ctx))
	path := filepath.Join(t.TempDir(), "final")
	clean := must(f.repo.AddDetachedWorktree(f.ctx, path, head))
	if b := must(clean.CurrentBranch(f.ctx)); b != "" {
		t.Fatalf("not detached: %s", b)
	}
	if _, err := clean.Commit(f.ctx, CommitSpec{Paths: []string{"README.md"}}); !errors.Is(err, ErrNotOwnedWorktree) {
		t.Fatalf("commit in the verification worktree: %v", err)
	}
	// The verification worktree may be reset and reopened.
	write(t, path, "build.tmp.go", "package src\n")
	check(clean.ResetTo(f.ctx, head))
	reopened := must(f.repo.OpenWorktree(f.ctx, path, ""))
	if _, err := f.repo.OpenWorktree(f.ctx, path, "aco/test-detached"); !errors.Is(err, ErrNotOwnedWorktree) {
		t.Fatalf("verification worktree opened as run worktree: %v", err)
	}
	if err := f.repo.RemoveWorktree(f.ctx, reopened); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("worktree still exists")
	}
	if _, err := f.repo.AddWorktree(f.ctx, filepath.Join(t.TempDir(), "w2"), "aco/test-detached", head); err == nil {
		t.Fatal("existing branch reused")
	}
}

func TestGitEnvScrubsSecrets(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", securetest.FakeAnthropic)
	t.Setenv("MY_SERVICE_TOKEN", securetest.FakeOpaque)
	t.Setenv("GITHUB_TOKEN", "ghp_FAKEFAKEFAKEFAKEFAKEFAKE00")
	t.Setenv("GIT_DIR", "/elsewhere")
	r := &Repo{opts: Options{}}
	env := strings.Join(r.gitEnv(call{}), "\n")
	for _, bad := range []string{"ANTHROPIC_API_KEY=", "MY_SERVICE_TOKEN=", "GITHUB_TOKEN=", "GIT_DIR="} {
		if strings.Contains(env, bad) {
			t.Errorf("env contains %s", bad)
		}
	}
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	push := strings.Join(r.gitEnv(call{tokens: true}), "\n")
	if !strings.Contains(push, "GITHUB_TOKEN=") || strings.Contains(push, "ANTHROPIC_API_KEY=") {
		t.Fatal("push environment")
	}
}

func TestHooksDoNotRun(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\necho ran > '" + filepath.ToSlash(marker) + "'\n"
	hooks := filepath.Join(f.user, ".git", "hooks")
	for _, h := range []string{"post-checkout", "pre-commit", "commit-msg", "post-commit", "reference-transaction"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(hook), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wt := f.worktree("aco/test-hooks")
	write(t, wt.Dir(), "src/h.go", "package src\n")
	tree := must(wt.WorkingTree(f.ctx))
	must(wt.CommitReviewed(f.ctx, []string{"src/h.go"}, tree, nil, phaseMsg(tree)))
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repository hook ran during an ACO git operation")
	}
}

func TestTimeoutAndErrorsAreMasked(t *testing.T) {
	f := newFixture(t)
	r, _ := Open(f.user, Options{Env: f.env, Timeout: 300 * time.Millisecond})
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	start := time.Now()
	_, err = r.run(context.Background(), call{args: []string{"hash-object", "--stdin"}, stdin: pr})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout took too long")
	}

	red := secure.NewRedactor(secure.NewSecret(securetest.FakeOpaque))
	r2, _ := Open(f.user, Options{Env: f.env, Redactor: red})
	_, err = r2.run(context.Background(), call{args: []string{"rev-parse", "--verify", securetest.FakeAnthropic + securetest.FakeOpaque}})
	if err == nil {
		t.Fatal("expected error")
	}
	var ge *Error
	if !errors.As(err, &ge) || ge.Op != "rev-parse" {
		t.Fatalf("error %v", err)
	}
	securetest.AssertNoLeak(t, "git error", err.Error(), securetest.FakeAnthropic, securetest.FakeOpaque)
}

func TestCommitMessagesAreMasked(t *testing.T) {
	f := newFixture(t)
	red := secure.NewRedactor(secure.NewSecret(securetest.FakeOpaque))
	r, _ := Open(f.user, Options{Env: f.env, Redactor: red})
	base := must(r.ResolveCommit(f.ctx, "HEAD"))
	wt := must(r.AddWorktree(f.ctx, filepath.Join(t.TempDir(), "wt"), "aco/test-mask", base))
	write(t, wt.Dir(), "src/m.go", "package src\n")
	tree := must(wt.WorkingTree(f.ctx))
	msg := phaseMsg(tree)
	msg.Subject = "feat(phase-01): leak " + securetest.FakeOpaque + " " + securetest.FakeAnthropic
	msg.Body = "summary " + securetest.FakeAnthropic + " " + securetest.FakeOpaque
	res := must(wt.CommitReviewed(f.ctx, []string{"src/m.go"}, tree, nil, msg))
	if err := wt.AddNote(f.ctx, res.Commit, []byte(`{"x":"`+securetest.FakeAnthropic+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := wt.Tag(f.ctx, PhaseTag("r1", 1), res.Commit, "tag "+securetest.FakeOpaque); err != nil {
		t.Fatal(err)
	}
	out := gitT(t, f.env, wt.Dir(), "log", "-1", "--format=%B", res.Commit) +
		string(must(wt.Note(f.ctx, res.Commit))) +
		gitT(t, f.env, wt.Dir(), "tag", "-l", "-n9", PhaseTag("r1", 1))
	securetest.AssertNoLeak(t, "commit message", out, securetest.FakeAnthropic, securetest.FakeOpaque)
	if subj := gitT(t, f.env, wt.Dir(), "log", "-1", "--format=%s", res.Commit); !strings.Contains(subj, secure.Mask) {
		t.Fatalf("subject not masked: %q", subj)
	}
	// The branch reflog gets a fixed message, never the subject.
	reflog := gitT(t, f.env, wt.Dir(), "reflog", "show", "--format=%gs", wt.Branch())
	securetest.AssertNoLeak(t, "branch reflog", reflog, securetest.FakeAnthropic, securetest.FakeOpaque)
	if !strings.Contains(reflog, reflogCommit) || strings.Contains(reflog, "leak") {
		t.Fatalf("reflog:\n%s", reflog)
	}
}
