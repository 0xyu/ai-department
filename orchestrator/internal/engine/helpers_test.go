package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/agent/fake"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/state"
)

// testEnv isolates git from the user's and the system's configuration.
func testEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	content := "[user]\n\tname = Test User\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n[core]\n\tautocrlf = false\n[commit]\n\tgpgSign = false\n[tag]\n\tgpgSign = false\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"GIT_CONFIG_GLOBAL=" + cfg, "GIT_CONFIG_NOSYSTEM=1", "HOME=" + home}
}

func gitT(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// env is a planning test environment: a user repository with one commit,
// a run worktree, a state store, and a run in preflight.
type env struct {
	t        *testing.T
	ctx      context.Context
	gitEnv   []string
	user     string
	repo     *gitops.Repo
	wt       *gitops.Repo
	base     string
	branch   string
	store    *state.Store
	run      state.Run
	runDir   *artifacts.RunDir
	cfg      *config.Config
	planner  *fake.Planner
	reviewer *fake.Reviewer
}

type envOpts struct {
	mode     state.RunMode
	input    state.InputKind
	agentsMD bool // the base commit has AGENTS.md
}

func newEnv(t *testing.T, o envOpts) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if o.mode == "" {
		o.mode = state.ModePlanOnly
	}
	if o.input == "" {
		o.input = state.InputRequirements
	}
	ctx := context.Background()
	ge := testEnv(t)
	user := filepath.Join(t.TempDir(), "user")
	writeFile(t, user, "README.md", "# toy\n")
	writeFile(t, user, "go.mod", "module example.com/toy\n\ngo 1.22\n")
	writeFile(t, user, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	writeFile(t, user, "calc/calc_test.go", "package calc\n")
	if o.agentsMD {
		writeFile(t, user, "AGENTS.md", "# Existing conventions\n")
	}
	gitT(t, ge, user, "init", "-q", "-b", "main")
	gitT(t, ge, user, "add", "-A")
	gitT(t, ge, user, "commit", "-q", "-m", "initial")
	repo, err := gitops.Open(user, gitops.Options{Env: ge, Identity: gitops.Identity{Name: "ACO Test", Email: "aco@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	pre, err := repo.Preflight(ctx, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	run, err := store.CreateRun(ctx, state.NewRun{RepoPath: user, BaseBranch: "main", Mode: o.mode, Input: o.input})
	if err != nil {
		t.Fatal(err)
	}
	if run, err = store.TransitionRun(ctx, run.ID, state.RunPreflight, state.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	branch := gitops.BranchName(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), "toy plan", "t1")
	wt, err := repo.AddWorktree(ctx, filepath.Join(t.TempDir(), "worktrees", run.ID), branch, pre.BaseCommit)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := artifacts.New(t.TempDir(), nil).Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: ctx, gitEnv: ge, user: user, repo: repo, wt: wt, base: pre.BaseCommit, branch: branch,
		store: store, run: run, runDir: rd, cfg: config.Default(), planner: &fake.Planner{}, reviewer: &fake.Reviewer{}}
}

func (e *env) deps() PlanningDeps {
	return PlanningDeps{Store: e.store, Run: e.runDir, Planner: e.planner, Reviewer: e.reviewer, Worktree: e.wt, Config: e.cfg,
		Now: func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }}
}

func (e *env) plan(in PlanningInput) *PlanningOutcome {
	e.t.Helper()
	in.RunID, in.BaseCommit = e.run.ID, e.base
	out, err := RunPlanning(e.ctx, e.deps(), in)
	if err != nil {
		e.t.Fatalf("RunPlanning: %v", err)
	}
	return out
}

func (e *env) git(args ...string) string { return gitT(e.t, e.gitEnv, e.user, args...) }

func (e *env) eventTypes() []events.Type {
	e.t.Helper()
	evs, err := e.store.EventsAfter(e.ctx, e.run.ID, 0, 10000)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []events.Type
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}

func count(ts []events.Type, t events.Type) int {
	n := 0
	for _, x := range ts {
		if x == t {
			n++
		}
	}
	return n
}

// --- plan documents ---

type planSpec struct {
	phases    int
	reqs      int
	agentsMD  string
	breakGate bool // phase 1 lacks its security gate (a deterministic check error)
	// integration declares an integration gate command instead of
	// not_applicable.
	integration bool
}

// planJSON builds a structured plan package: phase i covers REQ-i (the
// last phase covers the remaining requirements), two AC per phase, all six
// gates.
func planJSON(s planSpec) []byte {
	if s.phases == 0 {
		s.phases = 2
	}
	if s.reqs == 0 {
		s.reqs = s.phases
	}
	type m = map[string]any
	var reqs, phases, trace []any
	for i := 1; i <= s.reqs; i++ {
		reqs = append(reqs, m{"id": fmt.Sprintf("REQ-%03d", i), "kind": "functional", "text": fmt.Sprintf("The calculator supports operation %d.", i)})
	}
	for n := 1; n <= s.phases; n++ {
		gatesM := m{"typecheck": m{"commands": []string{"go vet ./..."}}, "lint": m{"commands": []string{"gofmt -l ."}},
			"unit":        m{"commands": []string{"go test ./..."}},
			"integration": m{"not_applicable": true, "justification": "no external services"},
			"build":       m{"commands": []string{"go build ./..."}}, "security": m{"scanners": []string{"secrets"}}}
		if s.integration {
			gatesM["integration"] = m{"commands": []string{"go test -tags=integration ./..."}}
		}
		if s.breakGate && n == 1 {
			delete(gatesM, "security")
		}
		var phReqs []string
		for i := 1; i <= s.reqs; i++ {
			if i == n || (n == s.phases && i > s.phases) {
				phReqs = append(phReqs, fmt.Sprintf("REQ-%03d", i))
			}
		}
		ph := m{
			"number": n, "title": fmt.Sprintf("Operation %d", n), "goal": fmt.Sprintf("Implement operation %d.", n),
			"scope":                  m{"in": []string{fmt.Sprintf("operation %d", n)}, "out": []string{"UI"}, "paths": []string{"calc/**"}},
			"technical_requirements": []string{"standard library only"},
			"acceptance_criteria": []any{
				m{"text": fmt.Sprintf("Operation %d returns the right result for positive inputs.", n)},
				m{"text": fmt.Sprintf("Operation %d is covered by a table test.", n)},
			},
			"deliverables": []string{"calc/calc.go", "calc/calc_test.go"},
			"requirements": phReqs, "gates": gatesM,
		}
		if n > 1 {
			ph["depends_on"] = []int{n - 1}
		}
		phases = append(phases, ph)
	}
	for i := 1; i <= s.reqs; i++ {
		n := i
		if n > s.phases {
			n = s.phases
		}
		trace = append(trace, m{"requirement": fmt.Sprintf("REQ-%03d", i), "phases": []int{n},
			"acceptance_criteria": []string{fmt.Sprintf("AC-%02d.1", n)}})
	}
	doc := m{"version": 1, "title": "Toy calculator", "overview": "## Goals\n\nA tiny calculator.\n\n## Architecture\n\nOne package.\n",
		"requirements": reqs, "phases": phases, "traceability": trace}
	if s.agentsMD != "" {
		doc["agents_md"] = s.agentsMD
	}
	b, _ := json.Marshal(doc)
	return b
}

func reqIDs(n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("REQ-%03d", i))
	}
	return out
}

// failingReview: every requirement covered except missing (marked missing),
// plus the given new findings.
func failingReview(ids []string, missing string, prev []agent.PreviousStatus, nf ...agent.PlanReviewFinding) string {
	r := agent.PlanReview{Verdict: "revise", Summary: "needs work", DimensionRatings: sound(),
		RequirementCoverage: []agent.ReqCoverage{}, PreviousFindings: []agent.PreviousStatus{}, NewFindings: []agent.PlanReviewFinding{}}
	for _, id := range ids {
		st := "covered"
		if id == missing {
			st = "missing"
		}
		r.RequirementCoverage = append(r.RequirementCoverage, agent.ReqCoverage{ReqID: id, Status: st, Phases: []string{"P01"}, Notes: "n"})
	}
	r.PreviousFindings = append(r.PreviousFindings, prev...)
	r.NewFindings = append(r.NewFindings, nf...)
	b, _ := json.Marshal(r)
	return string(b)
}

var (
	uncoveredFinding = agent.PlanReviewFinding{Severity: "blocker", Dimension: "coverage", Location: "requirements.md",
		Title: "REQ-002 has no testable acceptance criterion", Description: "No AC verifies REQ-002.", RequiredChange: "Add an AC for REQ-002."}
	forwardDepFinding = agent.PlanReviewFinding{Severity: "major", Dimension: "dependencies", Location: "phases/P01.md#scope",
		Title: "Phase 1 relies on the parser built in phase 2", Description: "P01 uses work of P02.", RequiredChange: "Move the parser into P01."}
)

func plannerPass(planJSON []byte, responses ...agent.FindingResponse) fake.PlanStep {
	return fake.PlanStep{Result: fake.PlanResult(planJSON, responses...)}
}
