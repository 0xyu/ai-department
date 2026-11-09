package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/agent/fake"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/exitcode"
	"github.com/0xyu/aco/internal/state"
)

// gitRepo turns h.repo into a Git repository with one commit, with Git
// isolated from the user's configuration.
func gitRepo(t *testing.T, h *harness) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	h.write(cfg, "[user]\n\tname = T\n\temail = t@example.com\n[init]\n\tdefaultBranch = main\n[core]\n\tautocrlf = false\n[commit]\n\tgpgSign = false\n[tag]\n\tgpgSign = false\n")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	h.write(filepath.Join(h.repo, "README.md"), "# toy\n")
	h.write(filepath.Join(h.repo, "AGENTS.md"), "# Rules\n")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = h.repo
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, errb.String())
		}
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	b, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(b))
}

const cliPlanMD = "# Toy plan\n\n## Requirements\n\n- REQ-001 (functional): Add numbers.\n\n" +
	"## Phase 1: Add\n\n### Goal\nAdd.\n\n### Scope\n- Add\n\n### Acceptance Criteria\n- AC-01.1: Add(1, 2) is 3.\n\n" +
	"### Deliverables\n- add.go\n\n### Requirements\n- REQ-001: AC-01.1\n\n### Quality Gates\n" +
	"- typecheck: `go vet ./...`\n- lint: `gofmt -l .`\n- unit: `go test ./...`\n" +
	"- integration: not_applicable — no services\n- build: `go build ./...`\n- security: scanners: secrets\n"

func revisePlanReview() string {
	r := agent.PlanReview{Verdict: "revise", Summary: "s", RequirementCoverage: []agent.ReqCoverage{{ReqID: "REQ-001", Status: "partial", Phases: []string{"P01"}, Notes: "n"}},
		PreviousFindings: []agent.PreviousStatus{}, NewFindings: []agent.PlanReviewFinding{{Severity: "major", Dimension: "acceptance_criteria",
			Location: "phases/P01.md", Title: "AC too weak", Description: "d", RequiredChange: "c"}}}
	for _, d := range agent.PlanDimensions {
		r.DimensionRatings = append(r.DimensionRatings, agent.DimensionRating{Dimension: d, Rating: "sound", Notes: "n"})
	}
	b, _ := json.Marshal(r)
	return string(b)
}

func TestPlanCommandFreezesSuppliedPlan(t *testing.T) {
	h := newHarness(t)
	gitRepo(t, h)
	planFile := filepath.Join(t.TempDir(), "plan.md")
	h.write(planFile, cliPlanMD)
	rev := &fake.Reviewer{PlanSteps: []fake.ReviewStep{{Message: fake.PlanReviewPass([]string{"REQ-001"})}}}
	pl := &fake.Planner{}
	h.agents = func(*config.Config, *auth.Resolution, *auth.Env) (agent.Planner, agent.Reviewer) { return pl, rev }

	code := h.run("plan", "--plan", planFile, "--set", "language.operator=en")
	if code != exitcode.Completed {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, h.stdout.String(), h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "Plan frozen: 1 phases, 1 review rounds") || !strings.Contains(out, "Plan hash: sha256:") {
		t.Fatalf("stdout:\n%s", out)
	}
	// The run is recorded as a completed plan-only run with a frozen plan.
	store, err := state.Open(context.Background(), filepath.Join(h.home, ".aco", "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runs, _ := store.ListRuns(context.Background(), "")
	if len(runs) != 1 || runs[0].Status != state.RunCompleted || runs[0].Mode != state.ModePlanOnly ||
		runs[0].Input != state.InputPlan || runs[0].PlanHash == "" || !strings.HasPrefix(runs[0].RunBranch, "aco/") {
		t.Fatalf("runs %+v", runs)
	}
	tag := "aco/" + runs[0].ID + "/plan-v1"
	if got := gitOut(t, h.repo, "log", "-1", "--format=%s", tag); got != "docs(aco): freeze implementation plan v1" {
		t.Fatalf("tag commit %q", got)
	}
	// The user's checkout is untouched; the lock was released.
	if _, err := os.Stat(filepath.Join(h.repo, ".aco")); err == nil {
		t.Fatal(".aco written into the user's checkout")
	}
	if gitOut(t, h.repo, "status", "--porcelain") != "" {
		t.Fatal("user checkout dirty")
	}
	if _, err := store.GetLock(context.Background(), h.repo); err == nil {
		t.Fatal("run lock not released")
	}
	if len(pl.Creates) != 0 {
		t.Fatal("Stage 1 ran for --plan")
	}
}

func TestPlanCommandBlocked(t *testing.T) {
	h := newHarness(t)
	gitRepo(t, h)
	planFile := filepath.Join(t.TempDir(), "plan.md")
	h.write(planFile, cliPlanMD)
	rev := &fake.Reviewer{PlanSteps: []fake.ReviewStep{{Message: revisePlanReview()}}}
	h.agents = func(*config.Config, *auth.Resolution, *auth.Env) (agent.Planner, agent.Reviewer) {
		return &fake.Planner{}, rev
	}
	code := h.run("plan", "--plan", planFile, "--set", "limits.plan_review_rounds=1")
	if code != exitcode.Blocked {
		t.Fatalf("exit %d\n%s\n%s", code, h.stdout.String(), h.stderr.String())
	}
	if out := h.stdout.String(); !strings.Contains(out, "plan_review_rounds") || !strings.Contains(out, "PF-001") || !strings.Contains(out, "已阻塞") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestPlanCommandUsage(t *testing.T) {
	h := newHarness(t)
	if code := h.run("plan"); code != exitcode.Usage || !strings.Contains(h.stderr.String(), "--requirements") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if code := h.run("plan", "--requirements", filepath.Join(h.repo, "missing.md")); code != exitcode.Usage {
		t.Fatalf("missing requirements: exit %d", code)
	}
	// Not a Git repository: preflight fails (exit 3) and the run is failed.
	h.write(filepath.Join(h.repo, "req.md"), "# R\n- do it\n")
	h.agents = func(*config.Config, *auth.Resolution, *auth.Env) (agent.Planner, agent.Reviewer) {
		return &fake.Planner{}, &fake.Reviewer{}
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(h.repo))
	if code := h.run("plan", "--requirements", filepath.Join(h.repo, "req.md")); code != exitcode.PreflightFailed {
		t.Fatalf("not a repo: exit %d %s", code, h.stderr.String())
	}
}
