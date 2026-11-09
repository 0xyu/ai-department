//go:build integration

package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/agent/fake"
	"github.com/0xyu/aco/internal/plan"
	"github.com/0xyu/aco/internal/state"
)

// snapshot hashes every file under dir except .git.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(h[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPlanningEndToEnd runs Stages 1–2 in full mode through a check
// failure, a failed review, a dispute, and a passing re-review; then checks
// that the frozen package passes aco validate-plan's checks with a verified
// lock, and that the user's checkout is byte-identical.
func TestPlanningEndToEnd(t *testing.T) {
	e := newEnv(t, envOpts{mode: state.ModeFull})
	writeFile(t, e.user, "scratch.txt", "uncommitted user work\n")
	before := snapshot(t, e.user)
	userHead := e.git("rev-parse", "HEAD")

	minor := agent.PlanReviewFinding{Severity: "minor", Dimension: "risks", Location: "IMPLEMENTATION_PLAN.md",
		Title: "Risk list could mention overflow", Description: "d", RequiredChange: "Mention integer overflow."}
	e.planner.Steps = []fake.PlanStep{
		plannerPass(planJSON(planSpec{phases: 2, breakGate: true, agentsMD: "# Rules\n\nUse go test.\n"})),
		plannerPass(planJSON(planSpec{phases: 2, agentsMD: "# Rules\n\nUse go test.\n"})),
		plannerPass(planJSON(planSpec{phases: 3, agentsMD: "# Rules\n\nUse go test.\n"}),
			agent.FindingResponse{ID: "PF-001", Response: agent.ResponseFixed, Detail: "added P03"},
			agent.FindingResponse{ID: "PF-002", Response: agent.ResponseDisputed, Detail: "overflow is impossible with int64 inputs here"}),
	}
	e.reviewer.PlanSteps = []fake.ReviewStep{
		{Message: failingReview(reqIDs(2), "REQ-002", nil, uncoveredFinding, minor)},
		{Message: fake.PlanReviewPass(reqIDs(3))},
	}
	// The second review must give both earlier findings a status.
	e.reviewer.PlanSteps[1].Message = func() string {
		r := failingReview(reqIDs(3), "", []agent.PreviousStatus{{ID: "PF-001", Status: "resolved", Note: "ok"},
			{ID: "PF-002", Status: "dispute_accepted", Note: "agreed"}})
		return strings.Replace(r, `"verdict":"revise"`, `"verdict":"pass"`, 1)
	}()

	out := e.plan(PlanningInput{Requirements: requirements})
	if out.Status != state.RunExecuting || out.Rounds != 2 || out.Revisions != 2 {
		t.Fatalf("outcome %+v", out)
	}
	assertFrozen(t, e, out, 2, true)
	if len(e.reviewer.PlanReviews) != 2 {
		t.Fatalf("reviews %d (the check failure must not reach the reviewer)", len(e.reviewer.PlanReviews))
	}
	fs, _ := e.store.ListFindings(e.ctx, e.run.ID, "plan")
	if len(fs) != 2 || fs[1].Status != FindingDisputeAccepted || fs[1].DisputeCount != 1 {
		t.Fatalf("findings %+v", fs)
	}

	// aco validate-plan's checks accept the frozen package and verify the lock.
	pkg, err := plan.Load(filepath.Join(e.wt.Dir(), ".aco", "plan"), plan.Options{RepoRoot: e.wt.Dir()})
	if err != nil {
		t.Fatal(err)
	}
	rep := plan.ValidateFull(pkg, plan.DefaultCheckOptions(), false)
	if !rep.Valid || rep.Lock == nil || !rep.Lock.Verified || rep.Lock.PlanHash != out.Lock.PlanHash {
		t.Fatalf("validate-plan report %+v", rep)
	}

	// The user's checkout (including uncommitted work) is untouched.
	after := snapshot(t, e.user)
	if len(after) != len(before) {
		t.Fatalf("user checkout files changed: %v -> %v", before, after)
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("user file %s changed", k)
		}
	}
	if e.git("rev-parse", "HEAD") != userHead || !strings.Contains(e.git("status", "--porcelain"), "scratch.txt") {
		t.Fatal("user HEAD or status changed")
	}
}
