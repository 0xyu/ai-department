package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/policy"
	"github.com/0xyu/aco/internal/reports"
	"github.com/0xyu/aco/internal/state"
)

// attemptLoop runs one attempt: implementation, then gates → review →
// commit gate with the fix loop in between. It returns the guard that
// ended the attempt, or a stopError once the phase is committed or blocked
// (or the run stopped).
func (p *phaseRun) attemptLoop(ctx context.Context) (*GuardTrip, error) {
	if trip, err := p.implement(ctx); trip != nil || err != nil {
		return trip, err
	}
	const (
		stGates = iota
		stReview
		stCommit
	)
	next := stGates
	for {
		switch next {
		case stGates:
			if err := p.toPhase(ctx, state.PhaseQualityGates); err != nil {
				return nil, err
			}
			green, trip, err := p.gateRound(ctx)
			if trip != nil || err != nil {
				return trip, err
			}
			if !green {
				continue // fixed; gates again
			}
			next = stReview
		case stReview:
			if err := p.toPhase(ctx, state.PhaseReviewing); err != nil {
				return nil, err
			}
			pass, trip, err := p.reviewRound(ctx)
			if trip != nil || err != nil {
				return trip, err
			}
			if pass {
				next = stCommit
			} else {
				next = stGates
			}
		case stCommit:
			if err := p.toPhase(ctx, state.PhaseCommitGate); err != nil {
				return nil, err
			}
			again, trip, err := p.commitGate(ctx)
			if trip != nil || err != nil {
				return trip, err
			}
			if again {
				next = stReview
			}
		}
	}
}

// guards checks the convergence guards of the current attempt.
func (p *phaseRun) guards() *GuardTrip {
	l := p.d.Config.Limits
	reopen, rejections := p.ledger.MaxCounts()
	return CheckGuards(GuardInput{
		ReviewRounds: p.att.reviewFails, ReviewLimit: l.ReviewRounds,
		GateFixes: p.att.gateFixes, GateFixLimit: l.GateFixAttempts,
		MaxReopen: reopen, ReopenLimit: l.MaxReopen, Rejections: rejections,
		NoProgress: p.att.noProgress, PolicyViolations: p.att.policyViolations, PolicyLimit: l.PolicyViolations,
		AgentCalls: p.att.calls, CallLimit: l.AgentCalls.Phase, Turns: p.att.turns, TurnLimit: l.ClaudeTurns.Phase,
		Elapsed: p.d.Now().Sub(p.att.started), Timeout: time.Duration(l.PhaseTimeout),
		IncompleteReports: p.att.incomplete,
	})
}

// convergence checks the guards that end an attempt between steps; the
// usage guards are checked before each agent call (beforeCall).
func (p *phaseRun) convergence() *GuardTrip {
	if trip := p.guards(); trip != nil && trip.Guard != GuardUsage {
		return trip
	}
	return nil
}

// --- implementation (§11.4) -----------------------------------------------------

func (p *phaseRun) workspace() agent.Workspace {
	pc := p.d.Config.Policy
	return agent.Workspace{Dir: p.wt(), ProtectedPaths: pc.ProtectedPaths, ScopePaths: p.spec.Scope.Paths,
		ScopeEnforcement: pc.ScopeEnforcement, Network: pc.Network}
}

func (p *phaseRun) scopes() []agent.Scope { return []agent.Scope{agent.ScopeRun, agent.ScopePhase} }

func (p *phaseRun) gateSpecs() []agent.GateSpec {
	var out []agent.GateSpec
	for _, s := range p.specs {
		g := agent.GateSpec{Type: s.Type, NotApplicable: !s.Required}
		for _, c := range s.Commands {
			g.Commands = append(g.Commands, c.Run)
		}
		if s.Type == gates.Security && s.Required {
			g.Commands = append(g.Commands, "diff policy, anti-gaming checks, and secret scan of the phase diff (built in)")
		}
		out = append(out, g)
	}
	return out
}

// ImplementRequest is the request of the first implementer call of the
// attempt: a fresh session with the frozen context (§11.3 steps 5–6).
func (p *phaseRun) implementRequest(out agent.Output) agent.ImplementRequest {
	return agent.ImplementRequest{
		RunID: p.in.RunID, Kind: agent.KindPhase, Phase: p.in.Phase, PhaseTitle: p.spec.Title, Attempt: p.attempt,
		Workspace: p.workspace(), Output: out, PlanOverview: p.overview, PhaseSpec: p.specText, AgentsMD: p.agentsText,
		Digests: p.digests, Gates: p.gateSpecs(), ACIDs: p.acIDs, Lessons: p.lessons, BudgetScopes: p.scopes(),
	}
}

func (p *phaseRun) implement(ctx context.Context) (*GuardTrip, error) {
	res, trip, err := p.callImplementer(ctx, "implement", func(ctx context.Context, out agent.Output) (*agent.ImplResult, error) {
		return p.d.Implementer.Start(ctx, p.implementRequest(out))
	})
	if trip != nil || err != nil {
		return trip, err
	}
	p.att.lastReport = res.Report
	return p.convergence(), nil
}

// implStep runs one implementer invocation as a step: checkpoint, intent,
// invocation record, the call, checkpoint, outcome (§14.2).
func (p *phaseRun) implStep(ctx context.Context, kind string,
	call func(context.Context, agent.Output) (*agent.ImplResult, error)) (*agent.ImplResult, error) {
	var res *agent.ImplResult
	err := p.agentStep(ctx, kind, "claude", "implementer", p.d.Config.Implementer.Model, p.d.ImplementerAuth,
		func(ctx context.Context, out agent.Output) (agent.Invocation, int, error) {
			var err error
			res, err = call(ctx, out)
			inv := invocationOfImpl(res, err)
			return inv, 1, err
		})
	if res != nil && res.SessionID != "" {
		p.setSession(ctx, res.SessionID)
	}
	return res, err
}

func invocationOfImpl(res *agent.ImplResult, err error) agent.Invocation {
	if res != nil {
		return res.Invocation
	}
	var ae *agent.Error
	if errors.As(err, &ae) {
		return ae.Invocation
	}
	return agent.Invocation{}
}

func (p *phaseRun) setSession(ctx context.Context, id string) {
	if id == p.att.session {
		return
	}
	p.att.session = id
	p.sessions = append(p.sessions, reports.Session{Attempt: p.attempt, SessionID: id})
	_ = p.d.Store.SetAttemptSession(context.WithoutCancel(ctx), p.in.RunID, p.in.Phase, p.attempt, id)
}

// implRetries is how often a failed implementer call is retried or
// continued (§14.1: incomplete → continue, max 2; crash → reset and retry).
const implRetries = 2

// callImplementer runs an implementer call with the recovery of §14.1. A
// result is returned when a usable report came back. A guard trip ends the
// attempt; an error from stopFor ends the run.
func (p *phaseRun) callImplementer(ctx context.Context, kind string,
	call func(context.Context, agent.Output) (*agent.ImplResult, error)) (*agent.ImplResult, *GuardTrip, error) {
	if trip, err := p.beforeCall(ctx); trip != nil || err != nil {
		return nil, trip, err
	}
	res, err := p.implStep(ctx, kind, call)
	for try := 0; ; try++ {
		if err == nil && (res == nil || res.Report == nil) {
			err = &agent.Error{Agent: "implementer", Class: agent.ClassIncomplete, Message: "no implementation report returned"}
		}
		class := agent.ClassOf(err)
		// A report that only misses answers is usable: the reviewer and the
		// gates judge the code, not the report.
		if err != nil && class == agent.ClassIncomplete && res != nil && res.Report != nil {
			err = nil
		}
		if err == nil {
			return res, p.convergence(), nil
		}
		if try >= implRetries || ctx.Err() != nil || !class.Retryable() || class == agent.ClassSubscriptionLimit {
			break
		}
		if trip, err := p.beforeCall(ctx); trip != nil || err != nil {
			return nil, trip, err
		}
		switch class {
		case agent.ClassIncomplete, agent.ClassMaxTurns:
			session := p.att.session
			if session == "" {
				res, err = p.implStep(ctx, kind, call)
				continue
			}
			reason := err.Error()
			res, err = p.implStep(ctx, "continue", func(ctx context.Context, out agent.Output) (*agent.ImplResult, error) {
				return p.d.Implementer.Continue(ctx, agent.ContinueRequest{RunID: p.in.RunID, SessionID: session,
					Kind: agent.ContinueFinish, Phase: p.in.Phase, Workspace: p.workspace(), Output: out, Reason: reason,
					ACIDs: p.acIDs, BudgetScopes: p.scopes()})
			})
		case agent.ClassCrash, agent.ClassTimeout:
			if p.att.lastBefore != "" {
				if rerr := p.d.Worktree.ResetTo(ctx, p.att.lastBefore); rerr != nil {
					return nil, nil, rerr
				}
			}
			res, err = p.implStep(ctx, kind, call)
		default: // transient, rate limit
			res, err = p.implStep(ctx, kind, call)
		}
	}
	switch agent.ClassOf(err) {
	case agent.ClassIncomplete, agent.ClassMaxTurns, agent.ClassCrash, agent.ClassTimeout, agent.ClassTransient, agent.ClassAPIRateLimit:
		if ctx.Err() == nil {
			p.att.incomplete++
			return nil, p.convergence(), nil
		}
	}
	return nil, nil, p.stopFor(ctx, err)
}

// beforeCall enforces the run-level call and turn limits (Failed) and the
// phase usage guards (attempt ends) before an agent call.
func (p *phaseRun) beforeCall(ctx context.Context) (*GuardTrip, error) {
	l := p.d.Config.Limits
	if l.AgentCalls.Run > 0 && p.runCalls >= l.AgentCalls.Run {
		_ = p.emit(ctx, events.UsageExceeded, events.Str("limit", "agent_calls"), events.Int("value", int64(l.AgentCalls.Run)))
		return nil, p.fail(ctx, state.ClassUsageLimitExceeded, fmt.Sprintf("agent call limit %d reached for the run", l.AgentCalls.Run))
	}
	if l.ClaudeTurns.Run > 0 && p.runTurns >= l.ClaudeTurns.Run {
		_ = p.emit(ctx, events.UsageExceeded, events.Str("limit", "claude_turns"), events.Int("value", int64(l.ClaudeTurns.Run)))
		return nil, p.fail(ctx, state.ClassUsageLimitExceeded, fmt.Sprintf("Claude turn limit %d reached for the run", l.ClaudeTurns.Run))
	}
	if trip := p.guards(); trip != nil && trip.Guard == GuardUsage {
		return trip, nil
	}
	return nil, nil
}

// agentStep wraps one agent invocation (§14.2): a checkpoint before, the
// step intent, the invocation record, the call, a checkpoint after, and
// the outcome. calls is how many CLI calls the invocation made.
func (p *phaseRun) agentStep(ctx context.Context, kind, ag, role, model, mode string,
	call func(context.Context, agent.Output) (agent.Invocation, int, error)) error {
	repo := p.d.Worktree
	p.att.seq++
	before, err := repo.Checkpoint(ctx, gitops.CheckpointRef(p.in.RunID, p.in.Phase, p.attempt, int64(p.att.seq)), "aco: before "+kind)
	if err != nil {
		return err
	}
	p.att.lastBefore = before.Commit
	intent, _ := json.Marshal(map[string]any{"kind": kind, "agent": ag, "role": role, "model": model, "auth_mode": mode,
		"tree_before": before.Tree, "session": p.att.session})
	st, err := p.d.Store.BeginStep(ctx, state.StepIntent{RunID: p.in.RunID, Phase: p.in.Phase, Attempt: p.attempt, Kind: kind,
		Key: fmt.Sprintf("P%02d/a%d/s%d/%s", p.in.Phase, p.attempt, p.att.seq, kind), Owner: p.d.Owner, Intent: intent,
		CheckpointRef: before.Ref})
	if err != nil {
		return err
	}
	invID, err := p.d.Store.StartInvocation(ctx, state.Invocation{RunID: p.in.RunID, StepID: st.ID, Agent: ag, Role: role,
		Model: model, AuthMode: mode})
	if err != nil {
		return err
	}
	if err := p.emit(ctx, events.AgentInvocationStarted, events.Str("agent", ag), events.Str("role", role), events.Str("model", model)); err != nil {
		return err
	}
	out := agent.Output{Run: p.d.Run, Prefix: strings.TrimSuffix(artifacts.StepFile(p.in.Phase, p.attempt, st.ID, kind, ""), "/")}
	inv, calls, callErr := call(ctx, out)
	wctx := context.WithoutCancel(ctx)
	p.account(ag, role, model, mode, inv, calls)

	r := state.InvocationResult{SessionID: inv.SessionID, Turns: inv.Usage.Turns, InputTokens: inv.Usage.InputTokens,
		OutputTokens: inv.Usage.OutputTokens, CostUSD: inv.Usage.CostUSD, Transcript: inv.TranscriptRef}
	evt, fields := events.AgentInvocationCompleted, []events.Field{events.Str("role", role)}
	if callErr != nil {
		r.Failed, r.Error = true, callErr.Error()
		evt = events.AgentInvocationFailed
		fields = append(fields, events.Str("class", string(agent.ClassOf(callErr))))
	}
	_ = p.d.Store.FinishInvocation(wctx, invID, r)
	_ = p.emit(wctx, evt, fields...)
	if n := len(inv.GuardDenials); n > 0 {
		p.att.policyViolations += n
		var rules []string
		for _, g := range inv.GuardDenials {
			rules = append(rules, g.Tool+":"+g.Rule)
		}
		_ = p.emit(wctx, events.PolicyViolation, events.Str("agent", ag), events.Str("role", role), events.Int("count", int64(n)),
			events.Strs("rules", rules...))
	}

	after, err := repo.Checkpoint(wctx, gitops.CheckpointRef(p.in.RunID, p.in.Phase, p.attempt, int64(p.att.seq+1)), "aco: after "+kind)
	if err != nil {
		return err
	}
	p.att.seq++
	outcome, _ := json.Marshal(map[string]any{"tree_after": after.Tree, "checkpoint_after": after.Ref, "session": inv.SessionID,
		"class": string(agent.ClassOf(callErr))})
	if callErr != nil {
		_, err = p.d.Store.FailStep(wctx, st.ID, callErr.Error(), outcome)
	} else {
		_, err = p.d.Store.CompleteStep(wctx, st.ID, outcome)
	}
	if err != nil {
		return err
	}
	return callErr
}

func (p *phaseRun) account(ag, role, model, mode string, inv agent.Invocation, calls int) {
	if calls < 1 {
		calls = 1
	}
	p.att.calls += calls
	p.runCalls += calls
	if ag == "claude" {
		p.att.turns += inv.Usage.Turns
		p.runTurns += inv.Usage.Turns
	}
	if inv.Model != "" {
		model = inv.Model
	}
	key := ag + "/" + role
	u := p.usage[key]
	if u == nil {
		u = &reports.AgentUsage{Agent: ag, Role: role, Model: model, AuthMode: mode}
		p.usage[key] = u
	}
	u.Calls += calls
	u.Turns += inv.Usage.Turns
	u.InputTokens += inv.Usage.InputTokens
	u.OutputTokens += inv.Usage.OutputTokens
	u.CostUSD += inv.Usage.CostUSD
	u.Charged = u.Charged || inv.Charged
}

// --- quality gates (§11.5) ------------------------------------------------------

// evidenceTree returns the tree that every result of a gate round is bound
// to, or "" when the evidence is not for one tree: a gate saw a different
// tree from the round's (another writer, or an earlier gate, changed it),
// a gate changed the tree while it ran (TreeAfter), or a result names no
// tree. Only such a round may authorize review or commit.
func evidenceTree(r *gates.Round) string {
	if r == nil || r.TreeHash == "" {
		return ""
	}
	for _, g := range r.Results {
		if g.TreeHash != r.TreeHash || g.TreeAfter != "" {
			return ""
		}
	}
	return r.TreeHash
}

// runGates runs one gate round as a step and records the results. stale
// reports that the round's evidence is not bound to one tree, or that the
// working tree is no longer the round's tree when it ends: such a round is
// recorded but never accepted.
func (p *phaseRun) runGates(ctx context.Context, ro gates.RunOptions, prefix string, roundNo int) (round *gates.Round, stale bool, err error) {
	runner, err := p.d.Gates(gates.Options{Dir: p.wt(), Repo: p.d.Worktree, Base: p.base,
		Policy:       policy.FromConfig(p.d.Config.Policy, p.spec.Scope.Paths, p.d.Redactor),
		FailSeverity: gates.Severity(p.d.Config.Gates.Security.FailSeverity), Redactor: p.d.Redactor,
		Out: p.d.Run, Prefix: prefix, Now: p.d.Now})
	if err != nil {
		return nil, false, err
	}
	kind := "gates"
	if ro.Baseline {
		kind = "baseline"
	}
	intent, _ := json.Marshal(map[string]any{"round": roundNo, "baseline": ro.Baseline, "base": p.base})
	p.att.seq++
	st, err := p.d.Store.BeginStep(ctx, state.StepIntent{RunID: p.in.RunID, Phase: p.in.Phase, Attempt: p.attempt, Kind: kind,
		Key: fmt.Sprintf("P%02d/a%d/s%d/%s", p.in.Phase, p.attempt, p.att.seq, kind), Owner: p.d.Owner, Intent: intent})
	if err != nil {
		return nil, false, err
	}
	for _, s := range p.specs {
		_ = p.emit(ctx, events.GateStarted, events.Str("gate", s.Type), events.Int("round", int64(roundNo)), events.Bool("baseline", ro.Baseline))
	}
	round, runErr := runner.Run(ctx, p.specs, ro)
	wctx := context.WithoutCancel(ctx)
	if runErr != nil {
		_, _ = p.d.Store.FailStep(wctx, st.ID, runErr.Error(), nil)
		return nil, false, runErr
	}
	after, err := p.d.Worktree.WorkingTree(wctx)
	if err != nil {
		return nil, false, err
	}
	stale = evidenceTree(round) == "" || after != round.TreeHash
	recs, err := gates.StateRecords(round, p.in.RunID, p.in.Phase, p.attempt, roundNo)
	if err != nil {
		return nil, false, err
	}
	for _, rec := range recs {
		if _, err := p.d.Store.AddGateResult(wctx, rec); err != nil {
			return nil, false, err
		}
	}
	var failed []string
	for _, g := range round.Results {
		if g.Status != gates.Skipped {
			_ = p.emit(wctx, events.GateCompleted, events.Str("gate", g.Gate), events.Str("status", string(g.Status)),
				events.Str("class", string(g.Class)), events.Int("round", int64(roundNo)), events.Bool("baseline", ro.Baseline),
				events.Str("summary", g.Summary), events.Bool("stale", stale))
		}
		if !g.Status.OK() {
			failed = append(failed, g.Gate)
		}
		if g.Comparison != nil && !stale {
			for _, t := range g.Comparison.Regressions {
				if !slices.Contains(p.seenRegr, t) {
					p.seenRegr = append(p.seenRegr, t)
				}
			}
		}
	}
	if b, err := json.MarshalIndent(round, "", "  "); err == nil && prefix != "" {
		_, _ = p.d.Run.Write(prefix+"/round.json", b)
	}
	outcome, _ := json.Marshal(map[string]any{"tree": round.TreeHash, "tree_after": after, "passed": round.Passed(),
		"failed": failed, "stale": stale})
	if _, err := p.d.Store.CompleteStep(wctx, st.ID, outcome); err != nil {
		return nil, false, err
	}
	return round, stale, nil
}

// phaseRound runs a phase gate round on the current candidate. A stale
// round (see runGates) is not kept: lastRound is cleared and the round
// counts as a gate-fix attempt, so a tree that keeps moving under the gates
// ends the attempt by limits.gate_fix_attempts. The caller reruns the gates
// on the new candidate unless a guard tripped.
func (p *phaseRun) phaseRound(ctx context.Context) (*gates.Round, bool, *GuardTrip, error) {
	p.att.gateRound++
	prefix := fmt.Sprintf("%s/gates/r%02d", artifacts.Attempt(p.in.Phase, p.attempt), p.att.gateRound)
	round, stale, err := p.runGates(ctx, gates.RunOptions{Mode: gates.ModePhase, FailFast: true, Compare: p.baseline}, prefix, p.att.gateRound)
	if err != nil {
		return nil, false, nil, err
	}
	if stale {
		p.att.lastRound = nil
		p.att.gateFixes++
		return round, true, p.convergence(), nil
	}
	p.att.lastRound = round
	return round, false, nil, nil
}

// failedGates names the gates of a round that did not pass.
func failedGates(r *gates.Round) []string {
	var out []string
	for _, g := range r.Results {
		if !g.Status.OK() && g.Status != gates.Skipped {
			out = append(out, g.Gate)
		}
	}
	return out
}

// environmentFailure returns the first gate that failed for a reason that
// is not the code's: a missing tool or service, or a gate ACO could not run.
func environmentFailure(r *gates.Round) *gates.Result {
	for _, g := range r.Results {
		if g.Class == gates.ClassEnvironment || g.Status == gates.Errored {
			return g
		}
	}
	return nil
}

// gateRound runs the gates on the current tree (in status quality_gates).
// Green moves on to review. A failure is fixed by the implementer (gate
// fix), unless it is an environment problem (BLOCKED) or a guard trips.
func (p *phaseRun) gateRound(ctx context.Context) (bool, *GuardTrip, error) {
	round, stale, trip, err := p.phaseRound(ctx)
	if trip != nil || err != nil || stale {
		return false, trip, err // stale: gates again on the new candidate
	}
	if round.Passed() {
		p.att.gateFixes = 0
		return true, nil, nil
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
	return p.fixGates(ctx, round)
}

// gateFailures turns a failed round into the implementer's work list.
func (p *phaseRun) gateFailures(round *gates.Round) []agent.GateFailure {
	var out []agent.GateFailure
	for _, g := range round.Results {
		if g.Status.OK() || g.Status == gates.Skipped {
			continue
		}
		var cmd, log string
		if fc := g.FailedCommand(); fc != nil {
			cmd, log = fc.Command.Run, fc.Log
		}
		var b strings.Builder
		b.WriteString(g.Summary + "\n")
		if c := g.Comparison; c != nil {
			if len(c.Regressions) > 0 {
				b.WriteString("Regressions (passed at baseline, fail now): " + strings.Join(c.Regressions, ", ") + "\n")
			}
			if len(c.NewFailingTests) > 0 {
				b.WriteString("New failing tests: " + strings.Join(c.NewFailingTests, ", ") + "\n")
			}
			for _, i := range c.BlockingIssues {
				b.WriteString(fmt.Sprintf("Issue: %s %s:%d %s %s\n", i.Tool, i.Path, i.Line, i.Rule, i.Message))
			}
			for _, v := range c.BlockingVulns {
				b.WriteString(fmt.Sprintf("Vulnerability: %s %s (%s)\n", v.ID, v.Package, v.Severity))
			}
		}
		b.WriteString("\n" + log)
		out = append(out, agent.GateFailure{ID: fmt.Sprintf("G-%s-%d", g.Gate, p.att.gateRound), Gate: g.Gate, Command: cmd,
			Log: p.d.Redactor.String(b.String())})
	}
	return out
}

func (p *phaseRun) fixGates(ctx context.Context, round *gates.Round) (bool, *GuardTrip, error) {
	failures := p.gateFailures(round)
	before := round.TreeHash
	res, trip, err := p.callImplementer(ctx, "gate_fix", func(ctx context.Context, out agent.Output) (*agent.ImplResult, error) {
		return p.d.Implementer.Continue(ctx, agent.ContinueRequest{RunID: p.in.RunID, SessionID: p.att.session,
			Kind: agent.ContinueGateFix, Phase: p.in.Phase, Workspace: p.workspace(), Output: out, GateFailures: failures,
			ACIDs: p.acIDs, BudgetScopes: p.scopes()})
	})
	if trip != nil || err != nil {
		return false, trip, err
	}
	p.att.lastReport = res.Report
	p.progress(ctx, before, false)
	return false, p.convergence(), nil
}

// progress updates the no-progress counter after a fix: an unchanged tree
// counts unless the implementer disputed findings (decided at re-review).
func (p *phaseRun) progress(ctx context.Context, before string, disputed bool) {
	tree, err := p.d.Worktree.WorkingTree(ctx)
	switch {
	case err != nil:
	case tree != before:
		p.att.noProgress = 0
	case !disputed:
		p.att.noProgress++
	}
}
