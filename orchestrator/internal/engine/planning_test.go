package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/agent/fake"
	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/plan"
	"github.com/0xyu/aco/internal/state"
)

const requirements = "# Toy calculator\n\n- Add two numbers.\n- Subtract two numbers.\n"

// assertFrozen checks the freeze commit, the tag, and the lock (AC-08.1).
func assertFrozen(t *testing.T, e *env, out *PlanningOutcome, rounds int, agentsGenerated bool) {
	t.Helper()
	if !out.Frozen() || out.Lock == nil {
		t.Fatalf("not frozen: %+v", out)
	}
	tag := gitops.TagName(e.run.ID, "plan-v1")
	if out.Tag != tag {
		t.Fatalf("tag %q", out.Tag)
	}
	if got := e.git("rev-parse", "refs/tags/"+tag+"^{commit}"); got != out.Commit {
		t.Fatalf("tag points to %s, freeze commit %s", got, out.Commit)
	}
	if got := e.git("rev-parse", "refs/heads/"+e.branch); got != out.Commit {
		t.Fatal("run branch is not at the freeze commit")
	}
	if parent := e.git("rev-parse", out.Commit+"^"); parent != e.base {
		t.Fatal("freeze commit is not the first commit on the run branch")
	}
	if subj := e.git("log", "-1", "--format=%s", out.Commit); subj != "docs(aco): freeze implementation plan v1" {
		t.Fatalf("subject %q", subj)
	}
	trailers := e.git("log", "-1", "--format=%(trailers:only,unfold)", out.Commit)
	for _, want := range []string{"ACO-Run: " + e.run.ID, "ACO-Plan-Hash: " + out.Lock.PlanHash,
		"ACO-Plan-Review-Rounds: " + strconv.Itoa(rounds), "Reviewed-by: " + out.Lock.ReviewerModel} {
		if !strings.Contains(trailers, want) {
			t.Fatalf("trailers lack %q:\n%s", want, trailers)
		}
	}
	// The freeze commit holds only .aco/plan/** and (if generated) AGENTS.md.
	files := strings.Split(e.git("diff-tree", "--no-commit-id", "--name-only", "-r", out.Commit), "\n")
	for _, f := range files {
		if !strings.HasPrefix(f, ".aco/plan/") && f != "AGENTS.md" {
			t.Fatalf("freeze commit contains %s", f)
		}
	}
	if slices.Contains(files, "AGENTS.md") != agentsGenerated {
		t.Fatalf("AGENTS.md in commit = %v, want %v (%v)", !agentsGenerated, agentsGenerated, files)
	}
	for _, f := range []string{".aco/plan/IMPLEMENTATION_PLAN.md", ".aco/plan/requirements.md", ".aco/plan/traceability.json",
		".aco/plan/phases/P01.md", ".aco/plan/plan-review.md", ".aco/plan/plan.lock.json"} {
		if !slices.Contains(files, f) {
			t.Fatalf("freeze commit lacks %s: %v", f, files)
		}
	}
	// Hashes match: the committed lock equals the written one and verifies.
	committed := e.git("show", tag+":.aco/plan/plan.lock.json")
	if strings.TrimSpace(committed) != strings.TrimSpace(string(out.Lock.Marshal())) {
		t.Fatal("committed lock differs")
	}
	lock, err := plan.ReadLock(e.wt.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.VerifyLock(e.wt.Dir(), lock); err != nil {
		t.Fatalf("lock does not verify: %v", err)
	}
	if lock.ReviewRounds != rounds || lock.PlanVersion != "v1" || lock.RunID != e.run.ID || lock.Verdict != "pass" || lock.ReviewerModel == "" {
		t.Fatalf("lock meta %+v", lock)
	}
	for _, lf := range lock.Files {
		blob := e.git("show", tag+":"+lf.Path)
		if plan.HashText([]byte(blob+"\n")) != lf.SHA256 && plan.HashText([]byte(blob)) != lf.SHA256 {
			t.Fatalf("hash of committed %s differs from the lock", lf.Path)
		}
	}
	if !e.mustClean() {
		t.Fatal("worktree not clean after the freeze")
	}
	run, _ := e.store.GetRun(e.ctx, e.run.ID)
	if run.PlanHash != out.Lock.PlanHash {
		t.Fatal("run plan hash not recorded")
	}
	commits, _ := e.store.ListCommits(e.ctx, e.run.ID)
	if len(commits) != 1 || commits[0].Kind != "plan_freeze" || commits[0].SHA != out.Commit {
		t.Fatalf("commits %+v", commits)
	}
	phases, _ := e.store.ListPhases(e.ctx, e.run.ID)
	if len(phases) != len(out.Package.Phases) || phases[0].Status != state.PhasePending {
		t.Fatalf("phases %+v", phases)
	}
	if n := count(e.eventTypes(), events.PlanFrozen); n != 1 {
		t.Fatalf("plan.frozen %d times", n)
	}
}

func (e *env) mustClean() bool {
	ok, err := e.wt.IsClean(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
}

// AC-08.1: the plan passes its first review and is frozen.
func TestPlanPassesFirstReview(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.planner.Steps = []fake.PlanStep{plannerPass(planJSON(planSpec{phases: 2, agentsMD: "# Conventions\n\nRun go test.\n"}))}
	e.reviewer.PlanSteps = []fake.ReviewStep{{Message: fake.PlanReviewPass(reqIDs(2))}}

	out := e.plan(PlanningInput{Requirements: requirements})
	if out.Status != state.RunCompleted || out.Rounds != 1 || out.Revisions != 0 {
		t.Fatalf("outcome %+v", out)
	}
	assertFrozen(t, e, out, 1, true)
	if out.Lock.ReviewerModel != "fake-reviewer" {
		t.Fatalf("reviewer model %q", out.Lock.ReviewerModel)
	}
	if got := e.git("show", out.Tag+":AGENTS.md"); !strings.Contains(got, "Run go test.") {
		t.Fatalf("generated AGENTS.md %q", got)
	}
	// Stage 1 got its inputs.
	c := e.planner.Creates[0]
	if c.Requirements != requirements || !strings.Contains(c.RepoOverview, "calc/calc.go") || c.AgentsMD != "" ||
		!strings.Contains(c.PlanFormatGuide, "AC-NN.M") || !strings.Contains(c.GateCatalogue, "typecheck") {
		t.Fatalf("planner request %+v", c)
	}
	// Stage 2 got the package, the requirement IDs, and the check results.
	r := e.reviewer.PlanReviews[0]
	if r.Round != 1 || len(r.Previous) != 0 || !slices.Equal(r.ReqIDs, reqIDs(2)) ||
		!strings.HasPrefix(r.CheckResults, "PASS") || !strings.Contains(r.AgentsMD, "Run go test.") {
		t.Fatalf("review request %+v", r)
	}
	if !slices.ContainsFunc(r.PackageFiles, func(f agent.PackFile) bool { return f.Path == "phases/P02.md" }) {
		t.Fatal("review lacks the phase specifications")
	}
	reviews, _ := e.store.ListPlanReviews(e.ctx, e.run.ID)
	if len(reviews) != 1 || !reviews[0].GatePassed || reviews[0].Verdict != "pass" {
		t.Fatalf("plan reviews %+v", reviews)
	}
	ts := e.eventTypes()
	for _, want := range []events.Type{events.PlanCreated, events.PlanReviewCompleted, events.PlanGatePassed,
		events.GitCommit, events.PlanFrozen, events.RunCompleted, events.AgentInvocationCompleted} {
		if count(ts, want) == 0 {
			t.Fatalf("no %s event in %v", want, ts)
		}
	}
	invs, _ := e.store.ListInvocations(e.ctx, e.run.ID)
	if len(invs) != 2 || invs[0].Role != "planner" || invs[1].Role != "plan_reviewer" || invs[1].Status != "completed" {
		t.Fatalf("invocations %+v", invs)
	}
	stages, _ := e.store.ListStages(e.ctx, e.run.ID)
	if len(stages) != 2 || stages[0].Stage != state.StagePlanning || stages[1].Stage != state.StagePlanReview {
		t.Fatalf("stages %+v", stages)
	}
	// The user's checkout was not touched.
	if _, err := os.Stat(filepath.Join(e.user, ".aco")); err == nil {
		t.Fatal(".aco appeared in the user's checkout")
	}
	if e.git("rev-parse", "refs/heads/main") != e.base {
		t.Fatal("base branch moved")
	}
}

// AC-08.2: uncovered REQ and forward dependency → revision → frozen at round 2.
func TestPlanRevisedThenFrozen(t *testing.T) {
	e := newEnv(t, envOpts{mode: state.ModeFull, agentsMD: true})
	e.planner.Steps = []fake.PlanStep{
		plannerPass(planJSON(planSpec{phases: 2, agentsMD: "ignored: the repo has one"})),
		plannerPass(planJSON(planSpec{phases: 3}),
			agent.FindingResponse{ID: "PF-001", Response: agent.ResponseFixed, Detail: "added AC-02.1 for REQ-002"},
			agent.FindingResponse{ID: "PF-002", Response: agent.ResponseFixed, Detail: "moved the parser to P01"}),
	}
	e.reviewer.PlanSteps = []fake.ReviewStep{
		{Message: failingReview(reqIDs(2), "REQ-002", nil, uncoveredFinding, forwardDepFinding)},
		{Message: fake.PlanReviewPass(reqIDs(3), "PF-001", "PF-002")},
	}
	out := e.plan(PlanningInput{Requirements: requirements})
	if out.Status != state.RunExecuting || out.Rounds != 2 || out.Revisions != 1 {
		t.Fatalf("outcome %+v", out)
	}
	assertFrozen(t, e, out, 2, false)
	if got := e.git("show", out.Tag+":AGENTS.md"); got != "# Existing conventions" {
		t.Fatalf("existing AGENTS.md changed: %q", got)
	}
	// The revision resumed the planning session with both findings.
	if len(e.planner.Revisions) != 1 {
		t.Fatalf("revisions %d", len(e.planner.Revisions))
	}
	rv := e.planner.Revisions[0]
	if rv.SessionID != "fake-plan-session" || len(rv.Findings) != 2 || rv.Findings[0].ID != "PF-001" ||
		rv.Findings[1].Dimension != "dependencies" || !strings.HasPrefix(rv.CheckResults, "PASS") {
		t.Fatalf("revision %+v", rv)
	}
	// The re-review carried the findings with the planner's answers.
	rr := e.reviewer.PlanReviews[1]
	if rr.Round != 2 || len(rr.Previous) != 2 || rr.Previous[0].Response == nil || rr.Previous[0].Response.Response != "fixed" {
		t.Fatalf("re-review %+v", rr.Previous)
	}
	fs, _ := e.store.ListFindings(e.ctx, e.run.ID, "plan")
	if len(fs) != 2 || fs[0].Status != FindingResolved || fs[1].Status != FindingResolved || fs[0].ClosedRound != 2 {
		t.Fatalf("findings %+v", fs)
	}
	reviews, _ := e.store.ListPlanReviews(e.ctx, e.run.ID)
	if len(reviews) != 2 || reviews[0].GatePassed || !reviews[1].GatePassed {
		t.Fatalf("plan reviews %+v", reviews)
	}
	ts := e.eventTypes()
	if count(ts, events.PlanGateFailed) != 1 || count(ts, events.PlanRevised) != 1 || count(ts, events.FindingOpened) != 2 ||
		count(ts, events.FindingResolved) != 2 || count(ts, events.RunStageStarted) < 4 {
		t.Fatalf("events %v", ts)
	}
	review := e.git("show", out.Tag+":.aco/plan/plan-review.md")
	if !strings.Contains(review, "PF-002") || !strings.Contains(review, "Review rounds: 2") {
		t.Fatalf("plan-review.md:\n%s", review)
	}
	phases, _ := e.store.ListPhases(e.ctx, e.run.ID)
	if len(phases) != 3 {
		t.Fatalf("registered %d phases, want the revised 3", len(phases))
	}
}

// AC-08.3: the review keeps failing → BLOCKED at plan_review, no freeze.
func TestPlanReviewRoundsExhausted(t *testing.T) {
	e := newEnv(t, envOpts{})
	pj := planJSON(planSpec{phases: 2})
	e.planner.Steps = []fake.PlanStep{plannerPass(pj)}
	e.reviewer.PlanSteps = []fake.ReviewStep{{Message: failingReview(reqIDs(2), "REQ-002", nil, uncoveredFinding)}}
	for i := 1; i < e.cfg.Limits.PlanReviewRounds; i++ {
		e.planner.Steps = append(e.planner.Steps, plannerPass(pj,
			agent.FindingResponse{ID: "PF-001", Response: agent.ResponseFixed, Detail: "tried"}))
		e.reviewer.PlanSteps = append(e.reviewer.PlanSteps, fake.ReviewStep{Message: failingReview(reqIDs(2), "REQ-002",
			[]agent.PreviousStatus{{ID: "PF-001", Status: "unresolved", Note: "still missing"}})})
	}
	out := e.plan(PlanningInput{Requirements: requirements})
	if out.Status != state.RunBlocked || out.Reason.Class != state.ClassPlanReviewRounds || out.Reason.Stage != state.StagePlanReview {
		t.Fatalf("outcome %+v", out)
	}
	if out.Rounds != 4 || out.Revisions != 3 || len(e.reviewer.PlanReviews) != 4 || out.Frozen() {
		t.Fatalf("rounds %d revisions %d reviews %d", out.Rounds, out.Revisions, len(e.reviewer.PlanReviews))
	}
	// No freeze commit, no tag.
	if e.git("rev-parse", "refs/heads/"+e.branch) != e.base {
		t.Fatal("run branch moved")
	}
	if tags := e.git("tag", "--list", "aco/*"); tags != "" {
		t.Fatalf("tags %q", tags)
	}
	// The latest package and review are saved for a human.
	if _, err := os.Stat(filepath.Join(e.wt.Dir(), ".aco", "plan", "phases", "P02.md")); err != nil {
		t.Fatal("latest package not in the worktree")
	}
	if _, err := e.runDir.Read("planning/package-04/phases/P01.md"); err != nil {
		t.Fatal("latest package not in the artifacts")
	}
	if _, err := e.runDir.Read("plan-review/round-04/review.json"); err != nil {
		t.Fatal("latest review not saved")
	}
	rep, err := e.runDir.Read("reports/plan-blocked.md")
	if err != nil || !strings.Contains(string(rep), "PF-001") || !strings.Contains(string(rep), "plan_review_rounds") {
		t.Fatalf("blocked report %q %v", rep, err)
	}
	reviews, _ := e.store.ListPlanReviews(e.ctx, e.run.ID)
	if len(reviews) != 4 {
		t.Fatalf("stored %d reviews", len(reviews))
	}
	fs, _ := e.store.ListFindings(e.ctx, e.run.ID, "plan")
	if len(fs) != 1 || fs[0].Status != FindingOpen || fs[0].ReopenCount != 3 {
		t.Fatalf("findings %+v", fs)
	}
	run, _ := e.store.GetRun(e.ctx, e.run.ID)
	if run.Status != state.RunBlocked || count(e.eventTypes(), events.RunBlocked) != 1 {
		t.Fatal("run not blocked")
	}
	phases, _ := e.store.ListPhases(e.ctx, e.run.ID)
	if len(phases) != 0 {
		t.Fatal("phases registered without a freeze")
	}
}

// Deterministic checks run before every review: a package that fails them
// goes back to Claude without a GPT call.
func TestChecksFailSkipReview(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.planner.Steps = []fake.PlanStep{
		plannerPass(planJSON(planSpec{phases: 2, breakGate: true})),
		plannerPass(planJSON(planSpec{phases: 2})),
	}
	e.reviewer.PlanSteps = []fake.ReviewStep{{Message: fake.PlanReviewPass(reqIDs(2))}}
	out := e.plan(PlanningInput{Requirements: requirements})
	if out.Status != state.RunCompleted || out.Rounds != 1 || out.Revisions != 1 {
		t.Fatalf("outcome %+v", out)
	}
	if len(e.reviewer.PlanReviews) != 1 {
		t.Fatalf("reviewer called %d times", len(e.reviewer.PlanReviews))
	}
	rv := e.planner.Revisions[0]
	if len(rv.Findings) != 0 || !strings.HasPrefix(rv.CheckResults, "FAIL") || !strings.Contains(rv.CheckResults, "security") {
		t.Fatalf("revision got %q", rv.CheckResults)
	}
	assertFrozen(t, e, out, 1, false)
	ts := e.eventTypes()
	// plan.gate.failed (checks), no review for that package.
	if count(ts, events.PlanGateFailed) != 1 || count(ts, events.PlanReviewCompleted) != 1 {
		t.Fatalf("events %v", ts)
	}
}

// A planner that never produces a checkable package blocks with plan_invalid.
func TestChecksNeverPassBlocks(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.cfg.Limits.PlanReviewRounds = 2
	bad := planJSON(planSpec{phases: 2, breakGate: true})
	e.planner.Steps = []fake.PlanStep{plannerPass(bad), plannerPass(bad)}
	out := e.plan(PlanningInput{Requirements: requirements})
	if out.Status != state.RunBlocked || out.Reason.Class != state.ClassPlanInvalid || len(e.reviewer.PlanReviews) != 0 {
		t.Fatalf("outcome %+v", out)
	}
}

const markdownPlan = `# Toy calculator plan

## Requirements

- REQ-001 (functional): Add two numbers.
- REQ-002 (functional): Subtract two numbers.

## Phase 1: Addition

### Goal
Implement Add.

### Scope
- calc.Add

### Acceptance Criteria
- AC-01.1: Add(2, 3) returns 5 in a table test.

### Deliverables
- calc/calc.go

### Requirements
- REQ-001: AC-01.1

### Quality Gates
- typecheck: ` + "`go vet ./...`" + `
- lint: ` + "`gofmt -l .`" + `
- unit: ` + "`go test ./...`" + `
- integration: not_applicable — no external services
- build: ` + "`go build ./...`" + `
- security: scanners: secrets

## Phase 2: Subtraction

### Goal
Implement Sub.

### Scope
- calc.Sub

### Acceptance Criteria
- AC-02.1: Sub(5, 3) returns 2 in a table test.

### Deliverables
- calc/calc.go

### Depends On
Phase 1

### Requirements
- REQ-002: AC-02.1

### Quality Gates
- typecheck: ` + "`go vet ./...`" + `
- lint: ` + "`gofmt -l .`" + `
- unit: ` + "`go test ./...`" + `
- integration: not_applicable — no external services
- build: ` + "`go build ./...`" + `
- security: scanners: secrets
`

func loadMarkdownPlan(t *testing.T, text string) *plan.Package {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg, err := plan.Load(p, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r := plan.ValidateFull(pkg, plan.DefaultCheckOptions(), false); !r.Valid {
		t.Fatalf("fixture plan invalid: %+v", r.Diagnostics)
	}
	return pkg
}

// AC-08.4: --plan with an existing Markdown plan skips Stage 1 and still
// goes through Stage 2.
func TestSuppliedMarkdownPlan(t *testing.T) {
	e := newEnv(t, envOpts{input: state.InputPlan})
	e.reviewer.PlanSteps = []fake.ReviewStep{{Message: fake.PlanReviewPass(reqIDs(2))}}
	out := e.plan(PlanningInput{Plan: loadMarkdownPlan(t, markdownPlan)})
	if out.Status != state.RunCompleted || out.Rounds != 1 {
		t.Fatalf("outcome %+v", out)
	}
	if len(e.planner.Creates)+len(e.planner.Revisions) != 0 {
		t.Fatal("Stage 1 ran for a supplied plan")
	}
	if len(e.reviewer.PlanReviews) != 1 || !strings.Contains(e.reviewer.PlanReviews[0].Requirements, "REQ-002") {
		t.Fatal("Stage 2 did not review the supplied plan with its requirements")
	}
	assertFrozen(t, e, out, 1, false)
	stages, _ := e.store.ListStages(e.ctx, e.run.ID)
	if len(stages) != 1 || stages[0].Stage != state.StagePlanReview {
		t.Fatalf("stages %+v", stages)
	}
	if got := e.git("show", out.Tag+":.aco/plan/phases/P02.md"); !strings.Contains(got, "Sub(5, 3) returns 2") {
		t.Fatalf("frozen P02:\n%s", got)
	}
}

// A supplied plan that fails the review is revised by Claude in a new
// session that converts and fixes it, then reviewed again.
func TestSuppliedPlanRevised(t *testing.T) {
	e := newEnv(t, envOpts{input: state.InputPlan})
	e.planner.Steps = []fake.PlanStep{plannerPass(planJSON(planSpec{phases: 2}),
		agent.FindingResponse{ID: "PF-001", Response: agent.ResponseFixed, Detail: "added AC"})}
	e.reviewer.PlanSteps = []fake.ReviewStep{
		{Message: failingReview(reqIDs(2), "REQ-002", nil, uncoveredFinding)},
		{Message: fake.PlanReviewPass(reqIDs(2), "PF-001")},
	}
	out := e.plan(PlanningInput{Plan: loadMarkdownPlan(t, markdownPlan), Requirements: requirements})
	if out.Status != state.RunCompleted || out.Rounds != 2 || out.Revisions != 1 {
		t.Fatalf("outcome %+v", out)
	}
	if len(e.planner.Creates) != 1 || len(e.planner.Revisions) != 0 {
		t.Fatal("expected one converting Create")
	}
	c := e.planner.Creates[0]
	if !strings.Contains(c.SuppliedPlan, "phases/P01.md") || len(c.Findings) != 1 || c.Findings[0].ID != "PF-001" || c.Requirements != requirements {
		t.Fatalf("create %+v", c)
	}
	assertFrozen(t, e, out, 2, false)
}

func TestAgentErrors(t *testing.T) {
	reset := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		err    error
		status state.RunStatus
		class  state.Class
	}{
		{"auth", &agent.Error{Agent: "claude", Class: agent.ClassAuth, Message: "not logged in"}, state.RunFailed, state.ClassAuth},
		{"quota", &agent.Error{Agent: "claude", Class: agent.ClassAPIQuota}, state.RunFailed, state.ClassAPIQuota},
		{"budget", &agent.Error{Agent: "claude", Class: agent.ClassBudget}, state.RunFailed, state.ClassBudgetExceeded},
		{"subscription limit", &agent.Error{Agent: "claude", Class: agent.ClassSubscriptionLimit, ResetAt: reset}, state.RunPaused, ""},
		{"cancelled", &agent.Error{Agent: "claude", Class: agent.ClassCancelled}, state.RunCancelled, ""},
		{"other", errors.New("boom"), state.RunFailed, state.ClassInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, envOpts{})
			e.planner.Steps = []fake.PlanStep{{Err: tt.err}}
			out := e.plan(PlanningInput{Requirements: requirements})
			if out.Status != tt.status || out.Reason.Class != tt.class {
				t.Fatalf("outcome %+v", out)
			}
			if tt.status == state.RunPaused {
				run, _ := e.store.GetRun(e.ctx, e.run.ID)
				if run.ResumeAt == nil || !run.ResumeAt.Equal(reset.Add(pauseMargin)) || run.PausedFrom != state.RunPlanning {
					t.Fatalf("paused run %+v", run)
				}
			}
			invs, _ := e.store.ListInvocations(e.ctx, e.run.ID)
			if len(invs) != 1 || invs[0].Status != "failed" {
				t.Fatalf("invocations %+v", invs)
			}
		})
	}
}

// Retryable planner errors are retried (max 2), then the run blocks.
func TestPlannerRetries(t *testing.T) {
	e := newEnv(t, envOpts{})
	crash := &agent.Error{Agent: "claude", Class: agent.ClassCrash}
	e.planner.Steps = []fake.PlanStep{{Err: crash}, {Err: crash}, plannerPass(planJSON(planSpec{phases: 2}))}
	e.reviewer.PlanSteps = []fake.ReviewStep{{Message: fake.PlanReviewPass(reqIDs(2))}}
	if out := e.plan(PlanningInput{Requirements: requirements}); out.Status != state.RunCompleted {
		t.Fatalf("outcome %+v", out)
	}

	e = newEnv(t, envOpts{})
	inc := &agent.Error{Agent: "claude", Class: agent.ClassIncomplete}
	e.planner.Steps = []fake.PlanStep{{Err: inc}, {Err: inc}, {Err: inc}}
	if out := e.plan(PlanningInput{Requirements: requirements}); out.Status != state.RunBlocked || out.Reason.Class != state.ClassPlanInvalid {
		t.Fatalf("outcome %+v", out)
	}
}

func TestReviewerErrors(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.planner.Steps = []fake.PlanStep{plannerPass(planJSON(planSpec{phases: 2}))}
	e.reviewer.PlanSteps = []fake.ReviewStep{{Err: &agent.Error{Agent: "codex", Class: agent.ClassPolicy, Message: "read ~/.ssh/id_rsa"}}}
	out := e.plan(PlanningInput{Requirements: requirements})
	if out.Status != state.RunBlocked || out.Reason.Class != state.ClassPolicyViolations || out.Reason.Stage != state.StagePlanReview {
		t.Fatalf("outcome %+v", out)
	}
	if count(e.eventTypes(), events.PolicyViolation) != 1 {
		t.Fatal("no policy.violation event")
	}

	// A review that fails its schema (after the adapter's retries) fails the run.
	e = newEnv(t, envOpts{})
	e.planner.Steps = []fake.PlanStep{plannerPass(planJSON(planSpec{phases: 2}))}
	e.reviewer.PlanSteps = []fake.ReviewStep{{Message: `{"verdict":"pass"}`}}
	if out := e.plan(PlanningInput{Requirements: requirements}); out.Status != state.RunFailed {
		t.Fatalf("outcome %+v", out)
	}
}

func TestAgentCallLimit(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.cfg.Limits.AgentCalls.Planning = 3
	pj := planJSON(planSpec{phases: 2})
	e.planner.Steps = []fake.PlanStep{plannerPass(pj), plannerPass(pj, agent.FindingResponse{ID: "PF-001", Response: "fixed", Detail: "x"})}
	e.reviewer.PlanSteps = []fake.ReviewStep{
		{Message: failingReview(reqIDs(2), "REQ-002", nil, uncoveredFinding)},
		{Message: fake.PlanReviewPass(reqIDs(2), "PF-001")},
	}
	out := e.plan(PlanningInput{Requirements: requirements})
	if out.Status != state.RunFailed || out.Reason.Class != state.ClassUsageLimitExceeded {
		t.Fatalf("outcome %+v", out)
	}
	ts := e.eventTypes()
	if count(ts, events.UsageWarning) != 1 || count(ts, events.UsageExceeded) != 1 {
		t.Fatalf("events %v", ts)
	}
}

// cancellingPlanner cancels the run's context while "working".
type cancellingPlanner struct{ cancel context.CancelFunc }

func (c cancellingPlanner) Create(ctx context.Context, _ agent.PlanRequest) (*agent.PlanResult, error) {
	c.cancel()
	return nil, &agent.Error{Agent: "claude", Class: agent.ClassCancelled, Err: context.Cause(ctx)}
}

func (c cancellingPlanner) Revise(ctx context.Context, _ agent.PlanRevision) (*agent.PlanResult, error) {
	return c.Create(ctx, agent.PlanRequest{})
}

func TestCancelledContext(t *testing.T) {
	e := newEnv(t, envOpts{})
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	d := e.deps()
	d.Planner = cancellingPlanner{cancel}
	out, err := RunPlanning(ctx, d, PlanningInput{RunID: e.run.ID, BaseCommit: e.base, Requirements: requirements})
	if err != nil || out.Status != state.RunCancelled {
		t.Fatalf("err %v outcome %+v", err, out)
	}
	run, _ := e.store.GetRun(e.ctx, e.run.ID)
	if run.Status != state.RunCancelled {
		t.Fatalf("run %s", run.Status)
	}
}

func TestRunPlanningInput(t *testing.T) {
	e := newEnv(t, envOpts{})
	if _, err := RunPlanning(e.ctx, e.deps(), PlanningInput{RunID: e.run.ID}); err == nil {
		t.Fatal("no requirements and no plan accepted")
	}
	if _, err := e.store.TransitionRun(e.ctx, e.run.ID, state.RunPlanning, state.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunPlanning(e.ctx, e.deps(), PlanningInput{RunID: e.run.ID, Requirements: "x"}); !errors.Is(err, ErrPlanningState) {
		t.Fatalf("err %v", err)
	}
}

// A base commit that holds a linked .aco is refused before anything is
// written through it.
func TestLinkedPlanDirRefused(t *testing.T) {
	e := newEnv(t, envOpts{})
	outside := t.TempDir()
	linkDir(t, outside, filepath.Join(e.wt.Dir(), ".aco"))
	e.planner.Steps = []fake.PlanStep{plannerPass(planJSON(planSpec{phases: 2}))}
	out, err := RunPlanning(e.ctx, e.deps(), PlanningInput{RunID: e.run.ID, BaseCommit: e.base, Requirements: requirements})
	if err == nil || out.Status != state.RunFailed {
		t.Fatalf("err %v outcome %+v", err, out)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatal("wrote through the link")
	}
}

// linkDir makes a directory symlink (a junction on Windows).
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create junction: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
}
