package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/plan"
	"github.com/0xyu/aco/internal/state"
)

// PlanningDeps are the collaborators of Stages 1 and 2.
type PlanningDeps struct {
	Store    *state.Store
	Run      *artifacts.RunDir
	Planner  agent.Planner
	Reviewer agent.Reviewer
	// Worktree is the run worktree (gitops.AddWorktree) with HEAD at the
	// base commit. The planner and the reviewer read it; only the engine
	// writes the plan package into it.
	Worktree *gitops.Repo
	Config   *config.Config
	// PlannerAuth and ReviewerAuth are the resolved auth modes
	// (subscription | api), recorded with each invocation.
	PlannerAuth, ReviewerAuth string
	// LookPath finds optional tools for the gate catalogue (nil: none).
	LookPath gates.LookPathFunc
	Now      func() time.Time
}

// PlanningInput says what the planning stages start from.
type PlanningInput struct {
	RunID string
	// Requirements is the requirement document. It may be empty when Plan
	// is given; the plan's own requirements are then used.
	Requirements string
	// Plan is an existing plan (--plan): Stage 1 is skipped and it goes
	// straight to Stage 2 (§10.2).
	Plan *plan.Package
	// BaseCommit is the run's base commit (HEAD of the worktree).
	BaseCommit string
}

// PlanningOutcome is where Stages 1–2 left the run.
type PlanningOutcome struct {
	Status   state.RunStatus // completed (plan-only) or executing after a freeze; else blocked, failed, paused, cancelled
	Reason   state.Reason
	ResumeAt time.Time
	// Rounds is the number of GPT plan reviews, Revisions the number of
	// planner revisions.
	Rounds, Revisions int
	Decision          PlanGateDecision // the last gate decision
	Package           *plan.Package    // the latest package, as written to the worktree
	Lock              *plan.Lock       // set when frozen
	Commit, Tag       string           // freeze commit and tag
	Ledger            *PlanLedger
}

// Frozen reports whether the plan was frozen.
func (o *PlanningOutcome) Frozen() bool { return o.Commit != "" }

// ErrPlanningState reports a run that is not ready for the planning stages.
var ErrPlanningState = errors.New("engine: the run must be in preflight to start planning")

// planRun is the state of one planning pipeline.
type planRun struct {
	d   PlanningDeps
	in  PlanningInput
	run state.Run
	out *PlanningOutcome

	reqText        string // requirement text for the reviewer
	overview       string
	catalogue      string
	agentsInBase   bool   // the base commit has AGENTS.md: never generated
	baseAgents     string // its content
	proposedAgents string // AGENTS.md proposed by the planner
	session        string // planning session (Revise)
	plannerCalls   int
	agentCalls     int
	warned         bool
	ledger         *PlanLedger
	lastReview     *agent.PlanReviewResult
}

// stopError carries an outcome out of the pipeline: the run already moved
// to a terminal or paused status.
type stopError struct{}

func (stopError) Error() string { return "engine: planning stopped" }

// RunPlanning runs Stage 1 (unless a plan is given) and Stage 2 for a run
// in preflight, then freezes the plan (§10). Agent problems end the run in
// the matching status (the outcome says which) with a nil error. A system
// problem (state store, Git, file system) returns an error; the run is then
// failed with class internal when the store still works. Invalid input
// returns an error with no outcome and leaves the run unchanged.
func RunPlanning(ctx context.Context, d PlanningDeps, in PlanningInput) (*PlanningOutcome, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.PlannerAuth == "" {
		d.PlannerAuth = "subscription"
	}
	if d.ReviewerAuth == "" {
		d.ReviewerAuth = "subscription"
	}
	if d.Store == nil || d.Run == nil || d.Planner == nil || d.Reviewer == nil || d.Worktree == nil || d.Config == nil {
		return nil, errors.New("engine: incomplete planning dependencies")
	}
	if in.Plan == nil && strings.TrimSpace(in.Requirements) == "" {
		return nil, errors.New("engine: planning needs requirements or a plan")
	}
	run, err := d.Store.GetRun(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	if run.Status != state.RunPreflight {
		return nil, fmt.Errorf("%w (status %s)", ErrPlanningState, run.Status)
	}
	p := &planRun{d: d, in: in, run: run, out: &PlanningOutcome{}, ledger: &PlanLedger{}}
	p.out.Ledger = p.ledger
	err = p.pipeline(ctx)
	var stop stopError
	switch {
	case err == nil, errors.As(err, &stop):
		return p.out, nil
	case ctx.Err() != nil:
		if errors.As(p.cancel(ctx), &stop) {
			return p.out, nil
		}
		return p.out, err
	default:
		// A system problem: the run fails (if the store still works) and
		// the error is returned for the operator.
		_ = p.fail(ctx, state.ClassInternal, err.Error())
		return p.out, err
	}
}

func (p *planRun) wt() string { return p.d.Worktree.Dir() }

func (p *planRun) stage() state.Stage { return state.StageOf(p.run.Status) }

// to moves the run (no-op when it is already there).
func (p *planRun) to(ctx context.Context, s state.RunStatus) error {
	if p.run.Status == s {
		return nil
	}
	r, err := p.d.Store.TransitionRun(ctx, p.in.RunID, s, state.TransitionOptions{})
	if err != nil {
		return err
	}
	p.run = r
	p.out.Status = r.Status
	return nil
}

func (p *planRun) end(ctx context.Context, s state.RunStatus, opt state.TransitionOptions) error {
	r, err := p.d.Store.TransitionRun(context.WithoutCancel(ctx), p.in.RunID, s, opt)
	if err != nil {
		return err
	}
	p.run = r
	p.out.Status, p.out.Reason = r.Status, r.Reason
	return stopError{}
}

func (p *planRun) block(ctx context.Context, class state.Class, detail string) error {
	_ = p.writeBlockedReport(class, detail)
	return p.end(ctx, state.RunBlocked, state.TransitionOptions{Reason: state.Reason{Class: class, Stage: p.stage(), Detail: detail}})
}

func (p *planRun) fail(ctx context.Context, class state.Class, detail string) error {
	stage := p.stage()
	if stage == "" {
		stage = state.StagePreflight
	}
	return p.end(ctx, state.RunFailed, state.TransitionOptions{Reason: state.Reason{Class: class, Stage: stage, Detail: detail}})
}

func (p *planRun) cancel(ctx context.Context) error {
	return p.end(ctx, state.RunCancelled, state.TransitionOptions{})
}

// emit appends an event in the current stage.
func (p *planRun) emit(ctx context.Context, t events.Type, fields ...events.Field) error {
	_, err := p.d.Store.AppendEvent(context.WithoutCancel(ctx), events.Event{
		RunID: p.in.RunID, Type: t, Stage: string(p.stage()), Payload: fields,
	})
	return err
}

func (p *planRun) pipeline(ctx context.Context) error {
	wt := p.wt()
	if f, err := plan.DiscoverAgentsMD(wt); err != nil {
		return fmt.Errorf("engine: AGENTS.md: %w", err)
	} else if f != nil {
		p.agentsInBase, p.baseAgents = true, string(f.Raw)
	}
	p.overview = RepoOverview(wt, p.d.LookPath)
	p.catalogue = GateCatalogue(wt, p.d.Config, p.d.LookPath)
	p.reqText = p.in.Requirements

	var pkg *plan.Package
	if p.in.Plan != nil {
		// --plan: straight to Stage 2 (§10.2).
		if err := p.to(ctx, state.RunPlanReview); err != nil {
			return err
		}
		pkg = p.in.Plan
		if strings.TrimSpace(p.reqText) == "" {
			p.reqText = plan.RenderRequirements(pkg.Requirements)
		}
		p.proposedAgents = pkg.ProposedAgentsMD
		if err := p.emit(ctx, events.PlanCreated, events.Str("source", "supplied"),
			events.Int("phases", int64(len(pkg.Phases))), events.Int("requirements", int64(len(pkg.Requirements)))); err != nil {
			return err
		}
	} else {
		if err := p.to(ctx, state.RunPlanning); err != nil {
			return err
		}
		res, err := p.callPlanner(ctx, nil, "")
		if err != nil {
			return err
		}
		pkg = res.Package
		if err := p.emit(ctx, events.PlanCreated, events.Str("source", "planner"),
			events.Int("phases", int64(len(pkg.Phases))), events.Int("requirements", int64(len(pkg.Requirements)))); err != nil {
			return err
		}
	}

	limit := p.d.Config.Limits.PlanReviewRounds
	if limit < 1 {
		limit = 1
	}
	for it := 1; ; it++ {
		loaded, report, err := p.materialize(it, pkg)
		if err != nil {
			return err
		}
		p.out.Package = loaded
		checks := RenderChecks(report.Diagnostics)
		var decision PlanGateDecision
		if report.Valid {
			if err := p.to(ctx, state.RunPlanReview); err != nil {
				return err
			}
			if decision, err = p.review(ctx, loaded, checks); err != nil {
				return err
			}
		} else {
			// No GPT call for a package that fails the checks (§10.4).
			decision = DecidePlanGate(PlanGateInput{ChecksPassed: false})
			if err := p.emitGate(ctx, decision, false); err != nil {
				return err
			}
		}
		p.out.Decision = decision
		if decision.Pass {
			return p.freeze(ctx, loaded)
		}
		if it >= limit {
			if !report.Valid {
				return p.block(ctx, state.ClassPlanInvalid,
					fmt.Sprintf("the plan package still fails the deterministic checks after %d rounds", it))
			}
			return p.block(ctx, state.ClassPlanReviewRounds,
				fmt.Sprintf("the Plan Decision Gate still fails after %d rounds (limits.plan_review_rounds)", it))
		}
		if err := p.to(ctx, state.RunPlanning); err != nil {
			return err
		}
		res, err := p.callPlanner(ctx, loaded, checks)
		if err != nil {
			return err
		}
		pkg = res.Package
	}
}

// --- planner ---

// plannerRetries is how often a failed planner call is retried (§14.1:
// incomplete → continue, max 2).
const plannerRetries = 2

// callPlanner creates (current == nil) or revises the plan package.
func (p *planRun) callPlanner(ctx context.Context, current *plan.Package, checks string) (*agent.PlanResult, error) {
	ws := agent.Workspace{Dir: p.wt(), ProtectedPaths: p.d.Config.Policy.ProtectedPaths, Network: p.d.Config.Policy.Network}
	scopes := []agent.Scope{agent.ScopeRun, agent.ScopePlanning}
	var findings []agent.PlanFinding
	for _, e := range p.ledger.OpenEntries() {
		findings = append(findings, e.Finding)
	}
	revising := current != nil
	var lastErr error
	for try := 0; try <= plannerRetries; try++ {
		if err := p.useCalls(ctx, 1); err != nil {
			return nil, err
		}
		p.plannerCalls++
		out := agent.Output{Run: p.d.Run, Prefix: strings.TrimSuffix(artifacts.Planning(p.plannerCalls, ""), "/")}
		rec, err := p.startInvocation(ctx, "claude", "planner", p.d.Config.Planner.Model, p.d.PlannerAuth)
		if err != nil {
			return nil, err
		}
		var res *agent.PlanResult
		switch {
		case revising && p.session != "":
			res, err = p.d.Planner.Revise(ctx, agent.PlanRevision{
				RunID: p.in.RunID, SessionID: p.session, Workspace: ws, Output: out, Round: p.out.Revisions + 1,
				Findings: findings, CheckResults: checks, BudgetScopes: scopes,
			})
		default:
			req := agent.PlanRequest{
				RunID: p.in.RunID, Workspace: ws, Output: out, Requirements: p.in.Requirements,
				RepoOverview: p.overview, AgentsMD: p.baseAgents, PlanFormatGuide: PlanFormatGuide,
				GateCatalogue: p.catalogue, BudgetScopes: scopes,
			}
			if revising {
				// A supplied plan has no session yet: convert and revise it
				// in one call.
				req.SuppliedPlan, req.Findings, req.CheckResults = packageText(current), findings, checks
				if req.Requirements == "" {
					req.Requirements = p.reqText
				}
			}
			res, err = p.d.Planner.Create(ctx, req)
		}
		// An incomplete revision (an unanswered finding) still carries a
		// usable package; the missing answer stays nil for the reviewer.
		if err != nil && res != nil && res.Package != nil && agent.ClassOf(err) == agent.ClassIncomplete && revising {
			err = nil
		}
		if err == nil && (res == nil || res.Package == nil) {
			err = &agent.Error{Agent: "planner", Class: agent.ClassIncomplete, Message: "no plan package returned"}
		}
		p.finishInvocation(ctx, rec, invocationOf(res, err), err, "planner")
		if err == nil {
			if res.SessionID != "" {
				p.session = res.SessionID
			}
			if res.Package.ProposedAgentsMD != "" {
				p.proposedAgents = res.Package.ProposedAgentsMD
			}
			if revising {
				p.out.Revisions++
				disputed := p.ledger.SetResponses(res.Responses)
				if err := p.persistLedger(ctx); err != nil {
					return nil, err
				}
				for _, id := range disputed {
					if err := p.emit(ctx, events.FindingDisputed, events.Str("id", id)); err != nil {
						return nil, err
					}
				}
				if err := p.emit(ctx, events.PlanRevised, events.Int("revision", int64(p.out.Revisions)),
					events.Int("responses", int64(len(res.Responses))), events.Strs("disputed", disputed...)); err != nil {
					return nil, err
				}
			}
			return res, nil
		}
		lastErr = err
		if !retryable(err) || ctx.Err() != nil {
			break
		}
	}
	return nil, p.stopFor(ctx, lastErr)
}

// retryable reports whether the same call may be repeated at once.
func retryable(err error) bool {
	switch agent.ClassOf(err) {
	case agent.ClassTransient, agent.ClassAPIRateLimit, agent.ClassMaxTurns, agent.ClassIncomplete,
		agent.ClassCrash, agent.ClassTimeout:
		return true
	}
	return false
}

// pauseMargin is added to a subscription reset time (§14.1).
const pauseMargin = 2 * time.Minute

// stopFor ends the run for an agent error that cannot be retried (§14.1).
func (p *planRun) stopFor(ctx context.Context, err error) error {
	if ctx.Err() != nil || agent.ClassOf(err) == agent.ClassCancelled {
		return p.cancel(ctx)
	}
	detail := err.Error()
	var ae *agent.Error
	errors.As(err, &ae)
	switch agent.ClassOf(err) {
	case agent.ClassSubscriptionLimit:
		at := p.d.Now().Add(15 * time.Minute)
		if ae != nil && !ae.ResetAt.IsZero() {
			at = ae.ResetAt.Add(pauseMargin)
		}
		p.out.ResumeAt = at
		who := ""
		if ae != nil {
			who = ae.Agent
		}
		_ = p.emit(ctx, events.SubscriptionLimitReached, events.Str("agent", who),
			events.Str("resume_at", at.UTC().Format(time.RFC3339)))
		return p.end(ctx, state.RunPaused, state.TransitionOptions{ResumeAt: at})
	case agent.ClassAuth:
		return p.fail(ctx, state.ClassAuth, detail)
	case agent.ClassAPIQuota:
		return p.fail(ctx, state.ClassAPIQuota, detail)
	case agent.ClassBudget:
		_ = p.emit(ctx, events.BudgetExceeded, events.Str("scope", string(agent.ScopePlanning)))
		return p.fail(ctx, state.ClassBudgetExceeded, detail)
	case agent.ClassPolicy:
		return p.block(ctx, state.ClassPolicyViolations, detail)
	case agent.ClassIncomplete:
		if p.run.Status == state.RunPlanning {
			return p.block(ctx, state.ClassPlanInvalid, "the planner did not return a valid plan package: "+detail)
		}
	}
	return p.fail(ctx, state.ClassInternal, detail)
}

// useCalls counts n agent calls against limits.agent_calls (§14.3).
func (p *planRun) useCalls(ctx context.Context, n int) error {
	limit := 0
	for _, l := range []int{p.d.Config.Limits.AgentCalls.Planning, p.d.Config.Limits.AgentCalls.Run} {
		if l > 0 && (limit == 0 || l < limit) {
			limit = l
		}
	}
	if limit > 0 && p.agentCalls+n > limit {
		_ = p.emit(ctx, events.UsageExceeded, events.Str("limit", "agent_calls"), events.Int("value", int64(limit)))
		return p.fail(ctx, state.ClassUsageLimitExceeded, fmt.Sprintf("agent call limit %d reached in planning", limit))
	}
	p.agentCalls += n
	if ratio := p.d.Config.Limits.AgentCalls.SoftRatio; limit > 0 && ratio > 0 && !p.warned &&
		float64(p.agentCalls) >= ratio*float64(limit) {
		p.warned = true
		_ = p.emit(ctx, events.UsageWarning, events.Str("limit", "agent_calls"), events.Int("used", int64(p.agentCalls)),
			events.Int("value", int64(limit)))
	}
	return nil
}

// --- invocations ---

func invocationOf(res *agent.PlanResult, err error) agent.Invocation {
	if res != nil {
		return res.Invocation
	}
	var ae *agent.Error
	if errors.As(err, &ae) {
		return ae.Invocation
	}
	return agent.Invocation{}
}

func (p *planRun) startInvocation(ctx context.Context, ag, role, model, mode string) (int64, error) {
	id, err := p.d.Store.StartInvocation(ctx, state.Invocation{RunID: p.in.RunID, Agent: ag, Role: role, Model: model, AuthMode: mode})
	if err != nil {
		return 0, err
	}
	return id, p.emit(ctx, events.AgentInvocationStarted, events.Str("agent", ag), events.Str("role", role), events.Str("model", model))
}

func (p *planRun) finishInvocation(ctx context.Context, id int64, inv agent.Invocation, err error, role string) {
	r := state.InvocationResult{SessionID: inv.SessionID, Turns: inv.Usage.Turns, InputTokens: inv.Usage.InputTokens,
		OutputTokens: inv.Usage.OutputTokens, CostUSD: inv.Usage.CostUSD, Transcript: inv.TranscriptRef}
	t := events.AgentInvocationCompleted
	fields := []events.Field{events.Str("role", role)}
	if err != nil {
		r.Failed, r.Error = true, err.Error()
		t = events.AgentInvocationFailed
		fields = append(fields, events.Str("class", string(agent.ClassOf(err))))
	}
	ctx = context.WithoutCancel(ctx)
	_ = p.d.Store.FinishInvocation(ctx, id, r)
	_ = p.emit(ctx, t, fields...)
}

// --- package files ---

// realDir refuses a path component that is a link, junction, or file.
func realDir(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return fmt.Errorf("engine: %s is not a plain directory: %w", path, plan.ErrNotContained)
	}
	return nil
}

// materialize writes pkg as the worktree's .aco/plan (replacing what was
// there) plus a generated AGENTS.md, reads it back exactly as it will be
// frozen, and runs the deterministic checks — the same checks as
// aco validate-plan (plan.ValidateFull).
func (p *planRun) materialize(it int, pkg *plan.Package) (*plan.Package, plan.Report, error) {
	wt := p.wt()
	aco := filepath.Join(wt, ".aco")
	dir := filepath.Join(wt, filepath.FromSlash(plan.PackageDir))
	for _, d := range []string{aco, dir} {
		if err := realDir(d); err != nil {
			return nil, plan.Report{}, err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, plan.Report{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, plan.Report{}, err
	}
	if err := plan.WriteDir(dir, pkg); err != nil {
		return nil, plan.Report{}, err
	}
	if !p.agentsInBase {
		target := filepath.Join(wt, plan.AgentsMDName)
		if strings.TrimSpace(p.proposedAgents) != "" {
			text := p.proposedAgents
			if !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
			if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
				return nil, plan.Report{}, err
			}
		} else if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, plan.Report{}, err
		}
	}
	loaded, err := plan.Load(dir, plan.Options{RepoRoot: wt})
	if err != nil {
		return nil, plan.Report{}, err
	}
	report := plan.ValidateFull(loaded, plan.DefaultCheckOptions(), false)
	// Problems only visible in the source form (e.g. a structured planner
	// answer that does not parse cleanly) still count.
	if extra := sourceErrors(pkg, report.Diagnostics); len(extra) > 0 {
		report = plan.NewReport(loaded, append(report.Diagnostics, extra...), false, report.Lock)
	}
	// Keep a copy of every package version for a human.
	for _, f := range agent.PlanPackageFiles(loaded) {
		_, _ = p.d.Run.Write(fmt.Sprintf("planning/package-%02d/%s", it, f.Path), []byte(f.Content))
	}
	if b, err := json.MarshalIndent(report, "", "  "); err == nil {
		_, _ = p.d.Run.Write(fmt.Sprintf("planning/package-%02d/checks.json", it), b)
	}
	return loaded, report, nil
}

func sourceErrors(src *plan.Package, have []plan.Diagnostic) []plan.Diagnostic {
	seen := map[string]bool{}
	key := func(d plan.Diagnostic) string { return string(d.Code) + "\x00" + d.Message }
	for _, d := range have {
		seen[key(d)] = true
	}
	var out []plan.Diagnostic
	for _, d := range src.Diagnostics {
		if d.Severity == plan.SeverityError && !seen[key(d)] {
			seen[key(d)] = true
			out = append(out, d)
		}
	}
	return out
}

// packageText renders a package as one document for the planner.
func packageText(pkg *plan.Package) string {
	var b strings.Builder
	for _, f := range agent.PlanPackageFiles(pkg) {
		b.WriteString(fmt.Sprintf("=== %s ===\n%s\n", f.Path, f.Content))
	}
	return b.String()
}

func (p *planRun) agentsText() string {
	if p.agentsInBase {
		return p.baseAgents
	}
	return p.proposedAgents
}

// --- review ---

func (p *planRun) review(ctx context.Context, loaded *plan.Package, checks string) (PlanGateDecision, error) {
	round := p.out.Rounds + 1
	var reqIDs []string
	for _, r := range loaded.Requirements {
		reqIDs = append(reqIDs, r.ID)
	}
	previous := p.ledger.Previous()
	var prevIDs []string
	for _, r := range previous {
		prevIDs = append(prevIDs, r.Finding.ID)
	}
	if err := p.useCalls(ctx, 1); err != nil {
		return PlanGateDecision{}, err
	}
	rec, err := p.startInvocation(ctx, "codex", "plan_reviewer", p.d.Config.Reviewer.Model, p.d.ReviewerAuth)
	if err != nil {
		return PlanGateDecision{}, err
	}
	res, err := p.d.Reviewer.ReviewPlan(ctx, agent.PlanReviewRequest{
		RunID: p.in.RunID, Round: round,
		Workspace:    agent.Workspace{Dir: p.wt(), ProtectedPaths: p.d.Config.Policy.ProtectedPaths, Network: "deny"},
		Output:       agent.Output{Run: p.d.Run, Prefix: strings.TrimSuffix(artifacts.PlanReview(round, ""), "/")},
		Requirements: p.reqText, PackageFiles: agent.PlanPackageFiles(loaded), ReqIDs: reqIDs,
		RepoOverview: p.overview, AgentsMD: p.agentsText(), CheckResults: checks, Previous: previous,
		BudgetScopes: []agent.Scope{agent.ScopeRun, agent.ScopePlanning},
	})
	var inv agent.Invocation
	calls := 1
	if res != nil {
		inv = res.Invocation
		inv.Usage = res.Total
		inv.SessionID = ""
		if n := len(res.Calls); n > 1 {
			calls = n
		}
	} else {
		var ae *agent.Error
		if errors.As(err, &ae) {
			inv = ae.Invocation
			if n := len(ae.Calls); n > 1 {
				calls = n
			}
		}
	}
	if err == nil && (res == nil || res.Review == nil) {
		err = &agent.Error{Agent: "reviewer", Class: agent.ClassSchemaInvalid, Message: "no review returned"}
	}
	p.finishInvocation(ctx, rec, inv, err, "plan_reviewer")
	if calls > 1 {
		if uerr := p.useCalls(ctx, calls-1); uerr != nil {
			return PlanGateDecision{}, uerr
		}
	}
	if err != nil {
		if agent.ClassOf(err) == agent.ClassPolicy {
			_ = p.emit(ctx, events.PolicyViolation, events.Str("agent", "codex"), events.Str("role", "plan_reviewer"))
		}
		return PlanGateDecision{}, p.stopFor(ctx, err)
	}
	p.out.Rounds = round
	p.lastReview = res
	rev := res.Review

	changes := p.ledger.Apply(round, rev)
	if err := p.persistLedger(ctx); err != nil {
		return PlanGateDecision{}, err
	}
	decision := DecidePlanGate(PlanGateInput{
		ChecksPassed: true, Reviewed: true, ReqIDs: reqIDs, Coverage: rev.RequirementCoverage,
		Ratings: rev.DimensionRatings, Findings: p.ledger.GateFindings(), PreviousIDs: prevIDs, Statuses: rev.PreviousFindings,
	})
	model := inv.Model
	if model == "" {
		model = p.d.Config.Reviewer.Model
	}
	ref := res.ReviewRef
	if ref == "" {
		ref = artifacts.PlanReview(round, "review.json")
		if _, err := p.d.Run.Write(ref, res.Raw); err != nil {
			return PlanGateDecision{}, err
		}
	}
	if b, err := json.MarshalIndent(decision, "", "  "); err == nil {
		_, _ = p.d.Run.Write(artifacts.PlanReview(round, "gate.json"), b)
	}
	if err := p.d.Store.AddPlanReview(ctx, state.PlanReview{
		RunID: p.in.RunID, Round: round, ReviewerModel: model, Verdict: rev.Verdict, GatePassed: decision.Pass,
		Review: res.Raw, Artifact: ref,
	}); err != nil {
		return PlanGateDecision{}, err
	}
	if err := p.emit(ctx, events.PlanReviewCompleted, events.Int("round", int64(round)), events.Str("verdict", rev.Verdict),
		events.Int("new_findings", int64(len(rev.NewFindings))), events.Str("model", model)); err != nil {
		return PlanGateDecision{}, err
	}
	if len(res.Inconsistent) > 0 {
		if err := p.emit(ctx, events.ReviewInconsistent, events.Int("round", int64(round)),
			events.Strs("inconsistencies", res.Inconsistent...)); err != nil {
			return PlanGateDecision{}, err
		}
	}
	for _, c := range changes {
		if err := p.emitChange(ctx, c); err != nil {
			return PlanGateDecision{}, err
		}
	}
	return decision, p.emitGate(ctx, decision, true)
}

func (p *planRun) emitChange(ctx context.Context, c LedgerChange) error {
	e := p.ledger.Get(c.ID)
	fields := []events.Field{events.Str("id", c.ID), events.Str("severity", e.Finding.Severity),
		events.Str("dimension", e.Finding.Dimension), events.Str("title", e.Finding.Title)}
	switch c.Kind {
	case "opened":
		return p.emit(ctx, events.FindingOpened, fields...)
	case "resolved":
		return p.emit(ctx, events.FindingResolved, fields...)
	case "dispute_accepted":
		return p.emit(ctx, events.FindingResolved, append(fields, events.Str("status", FindingDisputeAccepted))...)
	case "reopened":
		return p.emit(ctx, events.FindingReopened, fields...)
	case "dispute_rejected":
		return p.emit(ctx, events.FindingDisputed, append(fields, events.Str("status", FindingDisputeRejected))...)
	}
	return nil
}

func (p *planRun) emitGate(ctx context.Context, d PlanGateDecision, reviewed bool) error {
	var codes []string
	for _, r := range d.Reasons {
		s := r.Code
		if r.Ref != "" {
			s += ":" + r.Ref
		}
		codes = append(codes, s)
	}
	t := events.PlanGateFailed
	if d.Pass {
		t = events.PlanGatePassed
	}
	return p.emit(ctx, t, events.Int("round", int64(p.out.Rounds)), events.Bool("reviewed", reviewed), events.Strs("reasons", codes...))
}

func (p *planRun) persistLedger(ctx context.Context) error {
	for _, e := range p.ledger.Entries {
		data, _ := json.Marshal(map[string]any{"finding": e.Finding, "response": e.Response, "note": e.Note})
		if err := p.d.Store.UpsertFinding(ctx, state.Finding{
			RunID: p.in.RunID, ID: e.Finding.ID, Scope: "plan", Fingerprint: e.Fingerprint(), Severity: e.Finding.Severity,
			Category: e.Finding.Dimension, Status: e.Status, Title: e.Finding.Title, File: e.Finding.Location,
			OpenedRound: e.OpenedRound, ClosedRound: e.ClosedRound, ReopenCount: e.ReopenCount,
			DisputeCount: e.DisputeCount, Data: data,
		}); err != nil {
			return err
		}
	}
	return nil
}

// writeBlockedReport saves what a human needs after a blocked plan stage:
// the reason, the last gate decision, and the open findings. The latest
// package stays in the worktree and in planning/package-NN.
func (p *planRun) writeBlockedReport(class state.Class, detail string) error {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Planning blocked\n\n- Run: %s\n- Class: %s\n- Detail: %s\n- Review rounds: %d\n- Revisions: %d\n",
		p.in.RunID, class, detail, p.out.Rounds, p.out.Revisions))
	b.WriteString(fmt.Sprintf("- Latest package: worktree %s/%s (uncommitted) and planning/package-NN/ in the run artifacts\n\n",
		filepath.ToSlash(p.wt()), plan.PackageDir))
	b.WriteString("## Last Plan Decision Gate\n\n")
	if len(p.out.Decision.Reasons) == 0 {
		b.WriteString("(none)\n")
	}
	for _, r := range p.out.Decision.Reasons {
		b.WriteString(fmt.Sprintf("- %s %s %s\n", r.Code, r.Ref, r.Detail))
	}
	b.WriteString("\n## Open findings\n\n")
	open := p.ledger.OpenEntries()
	if len(open) == 0 {
		b.WriteString("(none)\n")
	}
	for _, e := range open {
		b.WriteString(fmt.Sprintf("- %s [%s/%s] %s — %s\n", e.Finding.ID, e.Finding.Severity, e.Finding.Dimension, e.Finding.Title, e.Finding.RequiredChange))
	}
	_, err := p.d.Run.Write(artifacts.ReportFile("plan-blocked.md"), []byte(b.String()))
	return err
}

// --- freeze ---

// PlanVersion is the version of the first frozen plan of a run.
const PlanVersion = "v1"

// freeze writes plan-review.md and plan.lock.json, commits exactly the
// plan package and a generated AGENTS.md, tags plan-v1, and registers the
// phases (§10.5).
func (p *planRun) freeze(ctx context.Context, loaded *plan.Package) error {
	wt, repo := p.wt(), p.d.Worktree
	model := p.d.Config.Reviewer.Model
	if p.lastReview != nil && p.lastReview.Model != "" {
		model = p.lastReview.Model
	}
	summary := renderPlanReview(p.out.Rounds, model, p.lastReview, p.ledger)
	if err := os.WriteFile(filepath.Join(wt, filepath.FromSlash(plan.PackageDir), plan.PlanReviewName), []byte(summary), 0o644); err != nil {
		return err
	}
	lock, err := plan.BuildLock(wt, plan.LockMeta{PlanVersion: PlanVersion, RunID: p.in.RunID, ReviewerModel: model,
		ReviewRounds: p.out.Rounds, Verdict: "pass", FrozenAt: p.d.Now()})
	if err != nil {
		return err
	}
	if err := plan.WriteLock(wt, lock); err != nil {
		return err
	}
	tree, err := repo.WorkingTree(ctx)
	if err != nil {
		return err
	}
	paths, err := repo.ChangedPaths(ctx, "HEAD", tree)
	if err != nil {
		return err
	}
	// The freeze commit holds only .aco/plan/** and a generated AGENTS.md.
	for _, path := range paths {
		switch {
		case strings.HasPrefix(path, plan.PackageDir+"/"):
		case path == plan.AgentsMDName && !p.agentsInBase:
		default:
			return fmt.Errorf("engine: the run worktree has a change outside the plan package: %s", path)
		}
	}
	res, err := repo.Commit(ctx, gitops.CommitSpec{
		Paths: paths, RequireExact: true, ExpectedHead: p.in.BaseCommit,
		Message: gitops.FreezeMessage(gitops.FreezeInfo{RunID: p.in.RunID, PlanVersion: PlanVersion, PlanHash: lock.PlanHash,
			ReviewRounds: p.out.Rounds, ReviewerModel: model}),
	})
	if err != nil {
		return err
	}
	if err := plan.VerifyLock(wt, lock); err != nil {
		return fmt.Errorf("engine: frozen plan does not match its lock: %w", err)
	}
	tag := ""
	if p.d.Config.Git.Tag {
		tag = gitops.TagName(p.in.RunID, "plan-"+PlanVersion)
		if err := repo.Tag(ctx, tag, res.Commit, "ACO frozen implementation plan "+PlanVersion); err != nil {
			return err
		}
	}
	if p.d.Config.Git.Notes {
		note, _ := json.Marshal(map[string]any{"kind": "plan_freeze", "run_id": p.in.RunID, "plan_version": PlanVersion,
			"plan_hash": lock.PlanHash, "review_rounds": p.out.Rounds, "reviewer_model": model})
		if err := repo.AddNote(ctx, res.Commit, note); err != nil {
			return err
		}
	}
	p.out.Lock, p.out.Commit, p.out.Tag = lock, res.Commit, tag
	if err := p.d.Store.AddCommit(ctx, state.Commit{RunID: p.in.RunID, SHA: res.Commit, Kind: "plan_freeze",
		TreeHash: res.Tree, Tag: tag}); err != nil {
		return err
	}
	hash := lock.PlanHash
	if _, err := p.d.Store.UpdateRunInfo(ctx, p.in.RunID, state.RunInfo{PlanHash: &hash}); err != nil {
		return err
	}
	if err := p.emit(ctx, events.GitCommit, events.Str("kind", "plan_freeze"), events.Str("sha", res.Commit), events.Str("tag", tag)); err != nil {
		return err
	}
	if err := p.emit(ctx, events.PlanFrozen, events.Str("plan_version", PlanVersion), events.Str("plan_hash", lock.PlanHash),
		events.Str("commit", res.Commit), events.Int("review_rounds", int64(p.out.Rounds)),
		events.Int("phases", int64(len(loaded.Phases)))); err != nil {
		return err
	}
	defs := make([]state.PhaseDef, 0, len(loaded.Phases))
	for _, ph := range loaded.Phases {
		defs = append(defs, state.PhaseDef{Number: ph.Number, Title: ph.Title})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Number < defs[j].Number })
	if err := p.d.Store.CreatePhases(ctx, p.in.RunID, defs); err != nil {
		return err
	}
	next := state.RunExecuting
	if p.run.Mode == state.ModePlanOnly {
		next = state.RunCompleted
	}
	return p.to(ctx, next)
}

// renderPlanReview is plan-review.md: the final Stage 2 review summary.
func renderPlanReview(rounds int, model string, res *agent.PlanReviewResult, l *PlanLedger) string {
	var b strings.Builder
	b.WriteString("# Plan Review\n\n")
	b.WriteString(fmt.Sprintf("- Reviewer: %s\n- Review rounds: %d\n- Plan Decision Gate: PASS\n", model, rounds))
	if res == nil || res.Review == nil {
		return b.String()
	}
	r := res.Review
	b.WriteString(fmt.Sprintf("- Reviewer verdict: %s\n\n## Summary\n\n%s\n\n## Dimensions\n\n| Dimension | Rating | Notes |\n|---|---|---|\n",
		r.Verdict, strings.TrimSpace(r.Summary)))
	cell := func(s string) string { return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", "\\|") }
	for _, d := range r.DimensionRatings {
		b.WriteString(fmt.Sprintf("| %s | %s | %s |\n", d.Dimension, d.Rating, cell(d.Notes)))
	}
	b.WriteString("\n## Requirement coverage\n\n| Requirement | Status | Phases |\n|---|---|---|\n")
	for _, c := range r.RequirementCoverage {
		b.WriteString(fmt.Sprintf("| %s | %s | %s |\n", c.ReqID, c.Status, strings.Join(c.Phases, ", ")))
	}
	b.WriteString("\n## Findings\n\n")
	if len(l.Entries) == 0 {
		b.WriteString("None.\n")
		return b.String()
	}
	b.WriteString("| ID | Severity | Dimension | Title | Status | Round opened | Round closed |\n|---|---|---|---|---|---|---|\n")
	for _, e := range l.Entries {
		closed := "-"
		if e.ClosedRound > 0 {
			closed = fmt.Sprint(e.ClosedRound)
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %d | %s |\n", e.Finding.ID, e.Finding.Severity, e.Finding.Dimension,
			cell(e.Finding.Title), e.Status, e.OpenedRound, closed))
	}
	return b.String()
}
