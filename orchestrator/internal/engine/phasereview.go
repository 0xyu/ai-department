package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/plan"
	"github.com/0xyu/aco/internal/policy"
	"github.com/0xyu/aco/internal/reports"
	"github.com/0xyu/aco/internal/state"
)

// reviewRetries is how often a transient reviewer failure is retried.
const reviewRetries = 2

// reviewRound runs one independent review of the current tree (§11.6) and
// the Review Decision Gate (§11.7). On FAIL it triages: a blocking class
// blocks the phase, a guard ends the attempt, otherwise the fixable work
// goes to the implementer. It reports whether the gate passed.
func (p *phaseRun) reviewRound(ctx context.Context) (bool, *GuardTrip, error) {
	repo := p.d.Worktree
	tree, err := repo.WorkingTree(ctx)
	if err != nil {
		return false, nil, err
	}
	// The gates must be for the tree that is reviewed: back from the commit
	// gate with a different tree, or the tree moved after the gates ran.
	// A stale round is rerun on the new candidate (bounded by
	// limits.gate_fix_attempts).
	for evidenceTree(p.att.lastRound) != tree {
		round, stale, trip, err := p.phaseRound(ctx)
		if trip != nil || err != nil {
			return false, trip, err
		}
		if stale {
			if tree, err = repo.WorkingTree(ctx); err != nil {
				return false, nil, err
			}
			continue
		}
		if !round.Passed() {
			if err := p.toPhase(ctx, state.PhaseTriage); err != nil {
				return false, nil, err
			}
			if env := environmentFailure(round); env != nil {
				return false, nil, p.block(ctx, state.ClassEnvironment, env.Summary)
			}
			p.att.gateFixes++
			if trip := p.convergence(); trip != nil {
				return false, trip, nil
			}
			if err := p.toPhase(ctx, state.PhaseFixing); err != nil {
				return false, nil, err
			}
			_, trip, err := p.fixGates(ctx, round)
			return false, trip, err
		}
		tree = round.TreeHash
	}
	if trip, err := p.beforeCall(ctx); trip != nil || err != nil {
		return false, trip, err
	}
	p.att.reviewRound++
	round := p.att.reviewRound
	req, changed, err := p.reviewRequest(ctx, tree, round)
	if err != nil {
		return false, nil, err
	}
	paths, err := repo.ChangedPaths(ctx, p.base, tree)
	if err != nil {
		return false, nil, err
	}
	previous := req.Ledger
	res, trip, err := p.callReviewer(ctx, req)
	if trip != nil || err != nil {
		return false, trip, err
	}
	rev := res.Review
	changes := p.ledger.Apply(round, p.d.Config.Limits.NewBlockerCutoffRound, rev, changed)
	accepted := false
	for _, c := range changes {
		if c.Kind == "dispute_accepted" {
			accepted = true
		}
	}
	// An accepted dispute is progress (the fix rounds count unchanged trees).
	if accepted {
		p.att.noProgress = 0
	}
	tr := p.ledger.Triage(round, p.triageRules())
	var prevIDs []string
	for _, l := range previous {
		prevIDs = append(prevIDs, l.ID)
	}
	decision := DecideReviewGate(ReviewGateInput{
		GatesGreen: p.att.lastRound.Passed(), FailedGates: failedGates(p.att.lastRound), Review: rev, ACIDs: p.acIDs,
		AllowUnverifiable: p.d.Config.Policy.AllowUnverifiableAC,
		Tests:             roundTests(p.att.lastRound),
		Findings:          p.ledger.GateFindings(), PreviousIDs: prevIDs,
	})
	p.att.lastReview, p.att.lastDecision, p.att.triage = res, decision, tr
	p.att.reviewedTree, p.att.reviewedPaths = tree, paths
	p.att.reviewPassed, p.att.reviewPassTree = decision.Pass, ""
	if decision.Pass {
		p.att.reviewPassTree = tree
	}
	if err := p.recordReview(ctx, res, decision, tree, changes, tr); err != nil {
		return false, nil, err
	}
	if decision.Pass {
		return true, nil, nil
	}

	// Review gate FAIL → triage (§11.7).
	p.att.reviewFails++
	if err := p.toPhase(ctx, state.PhaseTriage); err != nil {
		return false, nil, err
	}
	if tr.Blocked != "" {
		return false, nil, p.block(ctx, state.Class(tr.Blocked), fmt.Sprintf("triage classified %s as %s",
			strings.Join(tr.BlockedIDs, ", "), tr.Blocked))
	}
	if trip := p.convergence(); trip != nil {
		return false, trip, nil
	}
	if err := p.toPhase(ctx, state.PhaseFixing); err != nil {
		return false, nil, err
	}
	trip, err = p.fixReview(ctx, decision)
	return false, trip, err
}

func (p *phaseRun) triageRules() TriageRules {
	return TriageRules{ProtectedPaths: p.d.Config.Policy.ProtectedPaths,
		MatchPath: func(ps []string, path string) bool { return policy.MatchAny(ps, path, true) }}
}

// roundTests are the per-test results of an accepted round: the only
// evidence a high-risk regression item can cite.
func roundTests(r *gates.Round) []gates.TestCase {
	if r == nil {
		return nil
	}
	var out []gates.TestCase
	for _, g := range r.Results {
		out = append(out, g.Tests...)
	}
	return out
}

// callReviewer runs the review as an agent step with transient retries.
// A malformed review or a confinement breach ends the attempt (§14.1).
func (p *phaseRun) callReviewer(ctx context.Context, req agent.CodeReviewRequest) (*agent.CodeReviewResult, *GuardTrip, error) {
	var res *agent.CodeReviewResult
	var err error
	for try := 0; ; try++ {
		err = p.agentStep(ctx, "review", "codex", "reviewer", p.d.Config.Reviewer.Model, p.d.ReviewerAuth,
			func(ctx context.Context, out agent.Output) (agent.Invocation, int, error) {
				req.Output = out
				var cerr error
				res, cerr = p.d.Reviewer.ReviewCode(ctx, req)
				inv, calls := agent.Invocation{}, 1
				if res != nil {
					inv = res.Invocation
					inv.Usage, inv.SessionID = res.Total, ""
					calls = max(len(res.Calls), 1)
				} else {
					var ae *agent.Error
					if errors.As(cerr, &ae) {
						inv = ae.Invocation
						calls = max(len(ae.Calls), 1)
					}
				}
				if cerr == nil && (res == nil || res.Review == nil) {
					cerr = &agent.Error{Agent: "reviewer", Class: agent.ClassSchemaInvalid, Message: "no review returned"}
				}
				return inv, calls, cerr
			})
		if err == nil {
			return res, nil, nil
		}
		class := agent.ClassOf(err)
		if (class != agent.ClassTransient && class != agent.ClassAPIRateLimit && class != agent.ClassCrash && class != agent.ClassTimeout) ||
			try >= reviewRetries || ctx.Err() != nil {
			break
		}
		if trip, berr := p.beforeCall(ctx); trip != nil || berr != nil {
			return nil, trip, berr
		}
	}
	switch agent.ClassOf(err) {
	case agent.ClassSchemaInvalid:
		return nil, &GuardTrip{Guard: GuardIncomplete, Class: state.ClassIterationLimit, Detail: "the reviewer did not return a valid review: " + err.Error()}, nil
	case agent.ClassPolicy:
		_ = p.emit(ctx, events.PolicyViolation, events.Str("agent", "codex"), events.Str("role", "reviewer"))
		return nil, &GuardTrip{Guard: GuardPolicy, Class: state.ClassPolicyViolations, Detail: "the reviewer broke its confinement: " + err.Error()}, nil
	case agent.ClassTransient, agent.ClassAPIRateLimit, agent.ClassCrash, agent.ClassTimeout:
		if ctx.Err() == nil {
			return nil, &GuardTrip{Guard: GuardIncomplete, Class: state.ClassIterationLimit, Detail: "the reviewer kept failing: " + err.Error()}, nil
		}
	}
	return nil, nil, p.stopFor(ctx, err)
}

// recordReview stores the review, the gate decision, the ledger, and the
// events of one round.
func (p *phaseRun) recordReview(ctx context.Context, res *agent.CodeReviewResult, d PlanGateDecision, tree string,
	changes []LedgerChange, tr TriageResult) error {
	round, rev := p.att.reviewRound, res.Review
	model := res.Model
	if model == "" {
		model = p.d.Config.Reviewer.Model
	}
	ref := res.ReviewRef
	if ref == "" {
		ref = artifacts.ReviewFile(p.in.Phase, p.attempt, round, "review.json")
		if _, err := p.d.Run.Write(ref, res.Raw); err != nil {
			return err
		}
	}
	if b, err := json.MarshalIndent(map[string]any{"decision": d, "triage": tr}, "", "  "); err == nil {
		_, _ = p.d.Run.Write(artifacts.ReviewFile(p.in.Phase, p.attempt, round, "gate.json"), b)
	}
	if err := p.d.Store.AddReview(ctx, state.Review{RunID: p.in.RunID, Kind: "code", Phase: p.in.Phase, Attempt: p.attempt,
		Round: round, ReviewerModel: model, Verdict: rev.Verdict, GatePassed: d.Pass, TreeHash: tree, Review: res.Raw,
		Artifact: ref}); err != nil {
		return err
	}
	if err := p.persistLedger(ctx); err != nil {
		return err
	}
	var newIDs, reasons []string
	for _, c := range changes {
		if c.Kind == "opened" {
			newIDs = append(newIDs, c.ID)
		}
	}
	for _, r := range d.Reasons {
		s := r.Code
		if r.Ref != "" {
			s += ":" + r.Ref
		}
		reasons = append(reasons, s)
	}
	p.reviews = append(p.reviews, reports.ReviewRound{Attempt: p.attempt, Round: round, Verdict: rev.Verdict, GatePassed: d.Pass,
		TreeHash: tree, NewFindings: newIDs, Reasons: reasons})
	if err := p.emit(ctx, events.ReviewCompleted, events.Int("attempt", int64(p.attempt)), events.Int("round", int64(round)),
		events.Str("verdict", rev.Verdict), events.Bool("gate_passed", d.Pass), events.Int("new_findings", int64(len(rev.NewFindings))),
		events.Str("model", model), events.Strs("reasons", reasons...)); err != nil {
		return err
	}
	// The verdict is recorded, not trusted: approve with a failing gate is
	// inconsistent (§11.7).
	inconsistent := append([]string(nil), res.Inconsistent...)
	if rev.Verdict == "approve" && !d.Pass && len(inconsistent) == 0 {
		inconsistent = append(inconsistent, "verdict approve but the review gate fails: "+strings.Join(reasons, ", "))
	}
	if len(inconsistent) > 0 {
		if err := p.emit(ctx, events.ReviewInconsistent, events.Int("round", int64(round)), events.Str("verdict", rev.Verdict),
			events.Strs("inconsistencies", inconsistent...)); err != nil {
			return err
		}
	}
	for _, c := range changes {
		if err := p.emitCodeChange(ctx, c); err != nil {
			return err
		}
	}
	for _, id := range tr.Deferred {
		e := p.ledger.Get(id)
		if err := p.emit(ctx, events.FindingDeferred, events.Str("id", id), events.Str("severity", e.Finding.Severity),
			events.Str("category", e.Finding.Category), events.Str("reason", e.DeferReason)); err != nil {
			return err
		}
	}
	if !d.Pass {
		return p.emit(ctx, events.TriageCompleted, events.Int("round", int64(round)), events.Str("blocked", tr.Blocked),
			events.Strs("blocked_ids", tr.BlockedIDs...), events.Strs("fixable", tr.Fixable...), events.Strs("deferred", tr.Deferred...))
	}
	return nil
}

func (p *phaseRun) emitCodeChange(ctx context.Context, c LedgerChange) error {
	e := p.ledger.Get(c.ID)
	fields := []events.Field{events.Str("id", c.ID), events.Str("severity", e.Finding.Severity),
		events.Str("category", e.Finding.Category), events.Str("title", e.Finding.Title)}
	switch c.Kind {
	case "opened":
		return p.emit(ctx, events.FindingOpened, append(fields, events.Str("proposed_class", e.Proposed))...)
	case "resolved":
		return p.emit(ctx, events.FindingResolved, fields...)
	case "dispute_accepted":
		return p.emit(ctx, events.FindingResolved, append(fields, events.Str("status", CodeDisputeAccepted))...)
	case "reopened":
		return p.emit(ctx, events.FindingReopened, append(fields, events.Int("reopen_count", int64(e.ReopenCount)))...)
	case "dispute_rejected":
		return p.emit(ctx, events.FindingDisputed, append(fields, events.Str("status", CodeDisputeRejected))...)
	case "confirmed":
		return p.emit(ctx, events.FindingDisputed, append(fields, events.Str("status", "confirmed_"+e.Confirmed))...)
	}
	return nil
}

func (p *phaseRun) persistLedger(ctx context.Context) error {
	for _, e := range p.ledger.Entries {
		data, _ := json.Marshal(map[string]any{"finding": e.Finding, "response": e.Response, "note": e.Note,
			"proposed_class": e.Proposed, "confirmed": e.Confirmed, "downgraded": e.Downgraded, "attempt": e.Attempt})
		if err := p.d.Store.UpsertFinding(ctx, state.Finding{
			RunID: p.in.RunID, ID: e.ID, Scope: "code", Phase: p.in.Phase, Fingerprint: e.Fingerprint(),
			Severity: e.Finding.Severity, Category: e.Finding.Category, ResolutionClass: e.Class, Status: e.Status,
			Title: e.Finding.Title, File: e.Finding.File, LineStart: e.Finding.LineStart, LineEnd: e.Finding.LineEnd,
			OpenedRound: e.OpenedRound, ClosedRound: e.ClosedRound, ReopenCount: e.ReopenCount, DisputeCount: e.DisputeCount,
			Data: data,
		}); err != nil {
			return err
		}
	}
	return nil
}

// fixReview sends the fixable findings (most severe first) and the review
// gate's other reasons to the implementer in the attempt's session.
func (p *phaseRun) fixReview(ctx context.Context, d PlanGateDecision) (*GuardTrip, error) {
	var work []agent.CodeFinding
	var open []*CodeLedgerEntry
	for _, e := range p.ledger.OpenEntries() {
		if e.Class == ClassFixable {
			open = append(open, e)
		}
	}
	for _, e := range FixOrder(open) {
		f := e.Finding
		work = append(work, agent.CodeFinding{ID: e.ID, Severity: f.Severity, Category: f.Category, Location: e.Location(),
			Title: f.Title, Description: f.Description + evidenceSuffix(f.Evidence), RequiredChange: f.RequiredChange})
	}
	work = append(work, p.gateReasonWork(d)...)
	before := p.att.reviewedTree
	res, trip, err := p.callImplementer(ctx, "review_fix", func(ctx context.Context, out agent.Output) (*agent.ImplResult, error) {
		return p.d.Implementer.Continue(ctx, agent.ContinueRequest{RunID: p.in.RunID, SessionID: p.att.session,
			Kind: agent.ContinueReviewFix, Phase: p.in.Phase, Workspace: p.workspace(), Output: out, Findings: work,
			ACIDs: p.acIDs, BudgetScopes: p.scopes()})
	})
	if trip != nil || err != nil {
		return trip, err
	}
	p.att.lastReport = res.Report
	disputed := p.ledger.SetResponses(res.Report.FindingResponses)
	_ = p.persistLedger(ctx)
	for _, id := range disputed {
		_ = p.emit(ctx, events.FindingDisputed, events.Str("id", id), events.Str("status", "disputed"))
	}
	p.progress(ctx, before, len(disputed) > 0)
	return p.convergence(), nil
}

func evidenceSuffix(e string) string {
	if strings.TrimSpace(e) == "" {
		return ""
	}
	return "\nEvidence: " + e
}

// gateReasonWork turns review gate reasons that are not ledger findings
// (criteria not met, plan deviation, high regression risk) into work items.
func (p *phaseRun) gateReasonWork(d PlanGateDecision) []agent.CodeFinding {
	rev := p.att.lastReview.Review
	evidence := map[string]string{}
	for _, a := range rev.AcceptanceCriteria {
		evidence[a.ID] = a.Status + ": " + a.Evidence
	}
	var out []agent.CodeFinding
	n := 0
	add := func(sev, cat, title, desc, change string) {
		n++
		out = append(out, agent.CodeFinding{ID: fmt.Sprintf("R-%02d-%d-%d", p.in.Phase, p.att.reviewRound, n), Severity: sev,
			Category: cat, Title: title, Description: desc, RequiredChange: change})
	}
	for _, r := range d.Reasons {
		switch r.Code {
		case RGACNotMet, RGACMissing:
			text := ""
			if _, ac := p.pkg.AcceptanceCriterion(r.Ref); ac != nil {
				text = ac.Text
			}
			add("major", "spec_mismatch", r.Ref+" is not met", fmt.Sprintf("Criterion: %s\nReviewer: %s", text, evidence[r.Ref]),
				"Implement and test "+r.Ref+" so that it is met.")
		case RGPlanDeviates:
			add("major", "spec_mismatch", "the change deviates from the frozen plan", rev.PlanConformance.Notes,
				"Make the implementation conform to the Phase Specification.")
		case RGRegressionHigh:
			title := "high regression risk without passing test evidence"
			if r.Ref != "" {
				title = "regression " + r.Ref + " has no passing test evidence"
			}
			desc := rev.RegressionAssessment.Notes
			if r.Detail != "" {
				desc = r.Detail + "\n" + desc
			}
			add("major", "regression", title, desc,
				"Add or update tests that cover the risky change so they run and pass in the quality gates, or reduce the risk.")
		}
	}
	return out
}

// --- commit gate (§11.8) --------------------------------------------------------

// commitGate runs the Commit Gate. It returns true when the phase goes back
// to review (the tree changed, or drifted while it was being staged); a
// guard trip ends the attempt; otherwise the phase is committed or blocked
// (a stopError).
func (p *phaseRun) commitGate(ctx context.Context) (bool, *GuardTrip, error) {
	repo, wt := p.d.Worktree, p.wt()
	cur, err := repo.WorkingTree(ctx)
	if err != nil {
		return false, nil, err
	}
	paths, err := repo.ChangedPaths(ctx, p.base, cur)
	if err != nil {
		return false, nil, err
	}
	pol, err := policy.CheckDiff(ctx, repo, p.base, cur, policy.FromConfig(p.d.Config.Policy, p.spec.Scope.Paths, p.d.Redactor))
	if err != nil {
		return false, nil, err
	}
	var polBlock []string
	for _, f := range pol.Blocking() {
		polBlock = append(polBlock, f.String())
	}
	intact, problem := true, ""
	if p.lock == nil {
		intact, problem = false, "no plan lock"
	} else if err := plan.VerifyLock(wt, p.lock); err != nil {
		intact, problem = false, err.Error()
	}
	// Gate evidence counts only when every result is for one tree.
	gateTree := evidenceTree(p.att.lastRound)
	openBlocking := 0
	for _, f := range p.ledger.GateFindings() {
		if f.blocking() {
			openBlocking++
		}
	}
	d := DecideCommitGate(CommitGateInput{
		ReviewedTree: p.att.reviewedTree, ReviewedPaths: p.att.reviewedPaths, CurrentTree: cur, CurrentPaths: paths,
		GateTree: gateTree, GatesGreen: p.att.lastRound != nil && p.att.lastRound.Passed(),
		ReviewPassed: p.att.reviewPassed, ReviewTree: p.att.reviewPassTree, OpenBlocking: openBlocking,
		TriageBlocked: p.att.triage.Blocked, PolicyBlocking: polBlock, PlanIntact: intact, PlanProblem: problem,
	})
	var codes []string
	for _, r := range d.Reasons {
		codes = append(codes, r.Code)
	}
	if b, err := json.MarshalIndent(d, "", "  "); err == nil {
		_, _ = p.d.Run.Write(artifacts.ReviewFile(p.in.Phase, p.attempt, p.att.reviewRound, "commit-gate.json"), b)
	}
	if d.Action != CommitProceed {
		if err := p.emit(ctx, events.CommitGateFailed, events.Str("action", d.Action), events.Str("class", string(d.Class)),
			events.Strs("reasons", codes...)); err != nil {
			return false, nil, err
		}
	}
	switch d.Action {
	case CommitReview:
		p.att.reviewPassed = false
		return true, nil, nil
	case CommitBlock:
		var detail []string
		for _, r := range d.Reasons {
			detail = append(detail, strings.TrimSpace(r.Code+" "+r.Detail))
		}
		return false, nil, p.block(ctx, d.Class, "commit gate: "+strings.Join(detail, "; "))
	}
	if err := p.emit(ctx, events.CommitGatePassed, events.Str("reviewed_tree", cur)); err != nil {
		return false, nil, err
	}
	err = p.commit(ctx, cur, paths, pol)
	if !errors.Is(err, errWorktreeDrift) {
		return false, nil, err
	}
	// The worktree drifted between the gate and staging: the gates and
	// the review are no longer for the candidate. Back through gates and
	// an independent review (the round counts toward limits.review_rounds,
	// so a tree that keeps moving ends the attempt).
	if err := p.emit(ctx, events.CommitGateFailed, events.Str("action", CommitReview), events.Str("class", ""),
		events.Strs("reasons", CGWorktreeDrift)); err != nil {
		return false, nil, err
	}
	p.att.lastRound, p.att.reviewPassed, p.att.reviewPassTree = nil, false, ""
	p.att.reviewFails++
	if trip := p.convergence(); trip != nil {
		return false, trip, nil
	}
	return true, nil, nil
}
