package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/plan"
	"github.com/0xyu/aco/internal/reports"
	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/state"
)

// This file and its phase*.go neighbours implement Stage 3 for one phase
// (§11): initialization checks, implementation, quality gates, independent
// review, the Review Decision Gate, triage, the fix loop with convergence
// guards, phase restarts, BLOCKED handling, the Commit Gate, and the Phase
// Completion Report. docs/phase-loop.md describes the behaviour.

// GateRunner runs one round of quality gates (gates.Runner implements it).
type GateRunner interface {
	Run(ctx context.Context, specs []gates.Spec, ro gates.RunOptions) (*gates.Round, error)
}

// GateRunnerFunc builds the gate runner for one round. The engine fills in
// the worktree, the phase base, the diff policy, and the artifact prefix.
type GateRunnerFunc func(opts gates.Options) (GateRunner, error)

// DefaultGateRunner is gates.New.
func DefaultGateRunner(opts gates.Options) (GateRunner, error) { return gates.New(opts) }

// PhaseDeps are the collaborators of the phase engine.
type PhaseDeps struct {
	Store       *state.Store
	Run         *artifacts.RunDir
	Implementer agent.Implementer
	Reviewer    agent.Reviewer
	// Worktree is the run worktree on the run branch, HEAD at the previous
	// phase's commit (or the freeze commit for phase 1).
	Worktree *gitops.Repo
	Config   *config.Config
	// Owner is the run lock owner token (state.Lock.Owner); every step
	// records it.
	Owner string
	// ImplementerAuth and ReviewerAuth are the resolved auth modes.
	ImplementerAuth, ReviewerAuth string
	// Gates builds a gate runner (nil: DefaultGateRunner).
	Gates GateRunnerFunc
	// LookPath resolves gate programs (nil: no program check).
	LookPath gates.LookPathFunc
	// Redactor masks reports and logs (nil: a new empty one).
	Redactor *secure.Redactor
	Now      func() time.Time
}

// PhaseInput selects the phase to run.
type PhaseInput struct {
	RunID string
	Phase int
}

// PhaseOutcome is where the phase (and the run) ended up.
type PhaseOutcome struct {
	Phase int
	// Status is committed or blocked; empty when the run stopped first
	// (paused, failed, cancelled: see RunStatus).
	Status    state.PhaseStatus
	RunStatus state.RunStatus
	Reason    state.Reason
	ResumeAt  time.Time
	Attempts  int
	// ReviewRounds are the review rounds of the last attempt.
	ReviewRounds int
	Commit, Tag  string
	ReviewedTree string
	Report       *reports.PhaseReport
	// BlockedRef holds the work in progress of a blocked phase;
	// BlockedReport is the Blocked Report artifact.
	BlockedRef, BlockedReport string
	Ledger                    *CodeLedger
	// Trip is the guard that ended the last attempt, if any.
	Trip *GuardTrip
}

// ErrPhaseState reports a run or phase that is not ready to run the phase.
var ErrPhaseState = errors.New("engine: the run must be executing and the phase pending or initializing")

// phaseRun is the state of one phase.
type phaseRun struct {
	d   PhaseDeps
	in  PhaseInput
	run state.Run
	ph  state.Phase
	out *PhaseOutcome

	pkg        *plan.Package
	spec       *plan.Phase
	lock       *plan.Lock
	overview   string
	specText   string
	agentsText string
	digests    []agent.PhaseDigest
	acIDs      []string
	specs      []gates.Spec
	base       string // phase base commit (HEAD at initialization)
	baseline   *gates.Baseline
	ledger     *CodeLedger
	started    time.Time

	// Run-level counters (from the store at start, then incremented).
	runCalls, runTurns int

	attempt  int
	att      attemptState
	lessons  string
	sessions []reports.Session
	usage    map[string]*reports.AgentUsage
	reviews  []reports.ReviewRound
	seenRegr []string
}

// attemptState holds the counters of the current attempt.
type attemptState struct {
	started          time.Time
	seq              int64 // checkpoint counter
	session          string
	calls, turns     int
	policyViolations int
	gateFixes        int
	gateRound        int
	reviewRound      int
	reviewFails      int
	noProgress       int
	incomplete       int
	lastBefore       string // checkpoint commit before the latest agent step
	lastRound        *gates.Round
	lastReport       *agent.ImplReport
	lastReview       *agent.CodeReviewResult
	lastDecision     PlanGateDecision
	reviewedTree     string
	reviewedPaths    []string
	reviewPassed     bool
	reviewPassTree   string
	commitTries      int // commit steps of this attempt (worktree drift retries)
	triage           TriageResult
	lastTrip         *GuardTrip
}

// RunPhase runs one phase of an executing run until it is committed or
// blocked (§11). Agent problems that stop the run (subscription limit,
// auth, quota, budget, cancellation) end it in the matching status with a
// nil error; the outcome says which. A system problem (state store, Git,
// file system) fails the run with class internal and is returned.
func RunPhase(ctx context.Context, d PhaseDeps, in PhaseInput) (*PhaseOutcome, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Gates == nil {
		d.Gates = DefaultGateRunner
	}
	if d.Redactor == nil {
		d.Redactor = secure.NewRedactor()
	}
	if d.ImplementerAuth == "" {
		d.ImplementerAuth = "subscription"
	}
	if d.ReviewerAuth == "" {
		d.ReviewerAuth = "subscription"
	}
	if d.Store == nil || d.Run == nil || d.Implementer == nil || d.Reviewer == nil || d.Worktree == nil || d.Config == nil {
		return nil, errors.New("engine: incomplete phase dependencies")
	}
	if d.Owner == "" {
		return nil, errors.New("engine: the phase engine needs the run lock owner")
	}
	run, err := d.Store.GetRun(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	ph, err := d.Store.GetPhase(ctx, in.RunID, in.Phase)
	if err != nil {
		return nil, err
	}
	if run.Status != state.RunExecuting || (ph.Status != state.PhasePending && ph.Status != state.PhaseInitializing) {
		return nil, fmt.Errorf("%w (run %s, phase %s)", ErrPhaseState, run.Status, ph.Status)
	}
	p := &phaseRun{d: d, in: in, run: run, ph: ph, out: &PhaseOutcome{Phase: in.Phase},
		ledger: &CodeLedger{Phase: in.Phase}, started: d.Now(), usage: map[string]*reports.AgentUsage{}}
	p.out.Ledger = p.ledger
	if err := p.countRunUsage(ctx); err != nil {
		return nil, err
	}
	err = p.loop(ctx)
	p.out.Attempts = p.attempt
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
		_ = p.fail(ctx, state.ClassInternal, err.Error())
		return p.out, err
	}
}

func (p *phaseRun) wt() string { return p.d.Worktree.Dir() }

func (p *phaseRun) countRunUsage(ctx context.Context) error {
	invs, err := p.d.Store.ListInvocations(ctx, p.in.RunID)
	if err != nil {
		return err
	}
	for _, inv := range invs {
		p.runCalls++
		p.runTurns += inv.Turns
	}
	return nil
}

// --- run and phase status -------------------------------------------------------

func (p *phaseRun) emit(ctx context.Context, t events.Type, fields ...events.Field) error {
	_, err := p.d.Store.AppendEvent(context.WithoutCancel(ctx), events.Event{
		RunID: p.in.RunID, Type: t, Stage: string(state.StageExecution), Phase: p.in.Phase, Payload: fields,
	})
	return err
}

func (p *phaseRun) endRun(ctx context.Context, s state.RunStatus, opt state.TransitionOptions) error {
	r, err := p.d.Store.TransitionRun(context.WithoutCancel(ctx), p.in.RunID, s, opt)
	if err != nil {
		return err
	}
	p.run = r
	p.out.RunStatus, p.out.Reason = r.Status, r.Reason
	return stopError{}
}

func (p *phaseRun) fail(ctx context.Context, class state.Class, detail string) error {
	return p.endRun(ctx, state.RunFailed, state.TransitionOptions{Reason: state.Reason{Class: class,
		Stage: state.StageExecution, Phase: p.in.Phase, Detail: detail}})
}

func (p *phaseRun) cancel(ctx context.Context) error {
	return p.endRun(ctx, state.RunCancelled, state.TransitionOptions{})
}

// toPhase moves the phase (no-op when it is already there).
func (p *phaseRun) toPhase(ctx context.Context, s state.PhaseStatus) error {
	if p.ph.Status == s {
		return nil
	}
	ph, err := p.d.Store.TransitionPhase(context.WithoutCancel(ctx), p.in.RunID, p.in.Phase, s, state.Reason{})
	if err != nil {
		return err
	}
	p.ph = ph
	return nil
}

// toExhausted ends the attempt from any active status (via fixing or
// reviewing where the phase machine has no direct edge).
func (p *phaseRun) toExhausted(ctx context.Context) error {
	switch p.ph.Status {
	case state.PhaseQualityGates, state.PhaseTriage:
		if err := p.toPhase(ctx, state.PhaseFixing); err != nil {
			return err
		}
	case state.PhaseCommitGate:
		if err := p.toPhase(ctx, state.PhaseReviewing); err != nil {
			return err
		}
	}
	return p.toPhase(ctx, state.PhaseAttemptExhausted)
}

// toBlockable moves the phase to a status with an edge to blocked.
func (p *phaseRun) toBlockable(ctx context.Context) error {
	switch p.ph.Status {
	case state.PhaseInitializing, state.PhaseQualityGates, state.PhaseTriage, state.PhaseAttemptExhausted, state.PhaseCommitGate:
		return nil
	case state.PhaseReviewing:
		return p.toPhase(ctx, state.PhaseTriage)
	case state.PhaseImplementing, state.PhaseFixing:
		return p.toPhase(ctx, state.PhaseAttemptExhausted)
	case state.PhasePending:
		return p.toPhase(ctx, state.PhaseInitializing)
	}
	return fmt.Errorf("engine: phase %d is %s and cannot be blocked", p.in.Phase, p.ph.Status)
}

// block blocks the phase and the run (§11.7): the work in progress is saved
// to refs/aco/<run>/blocked/PNN, a Blocked Report is written, and nothing
// is committed to the run branch.
func (p *phaseRun) block(ctx context.Context, class state.Class, detail string) error {
	ctx = context.WithoutCancel(ctx)
	if err := p.toBlockable(ctx); err != nil {
		return err
	}
	cp, err := p.d.Worktree.SaveBlocked(ctx, p.in.RunID, p.in.Phase, string(class))
	if err != nil {
		return err
	}
	p.out.BlockedRef = cp.Ref
	ref, err := p.writeBlockedReport(ctx, class, detail, cp)
	if err != nil {
		return err
	}
	p.out.BlockedReport = ref
	ph, err := p.d.Store.TransitionPhase(ctx, p.in.RunID, p.in.Phase, state.PhaseBlocked, state.Reason{Class: class, Detail: detail})
	if err != nil {
		return err
	}
	p.ph = ph
	p.out.Status = state.PhaseBlocked
	return p.endRun(ctx, state.RunBlocked, state.TransitionOptions{Reason: state.Reason{Class: class,
		Stage: state.StageExecution, Phase: p.in.Phase, Detail: detail}})
}

// stopFor ends the run for an agent error that cannot be handled within the
// attempt (§14.1).
func (p *phaseRun) stopFor(ctx context.Context, err error) error {
	if ctx.Err() != nil || agent.ClassOf(err) == agent.ClassCancelled {
		return p.cancel(ctx)
	}
	var ae *agent.Error
	errors.As(err, &ae)
	switch agent.ClassOf(err) {
	case agent.ClassSubscriptionLimit:
		at := p.d.Now().Add(15 * time.Minute)
		who := ""
		if ae != nil {
			who = ae.Agent
			if !ae.ResetAt.IsZero() {
				at = ae.ResetAt.Add(pauseMargin)
			}
		}
		p.out.ResumeAt = at
		_ = p.emit(ctx, events.SubscriptionLimitReached, events.Str("agent", who), events.Str("resume_at", at.UTC().Format(time.RFC3339)))
		return p.endRun(ctx, state.RunPaused, state.TransitionOptions{ResumeAt: at})
	case agent.ClassAuth:
		return p.fail(ctx, state.ClassAuth, err.Error())
	case agent.ClassAPIQuota:
		return p.fail(ctx, state.ClassAPIQuota, err.Error())
	case agent.ClassBudget:
		_ = p.emit(ctx, events.BudgetExceeded, events.Str("scope", string(agent.ScopePhase)))
		return p.fail(ctx, state.ClassBudgetExceeded, err.Error())
	}
	return p.fail(ctx, state.ClassInternal, err.Error())
}

// --- the loop -------------------------------------------------------------------

// loop runs attempts until the phase is committed or blocked: an attempt
// that ends by a guard restarts the phase from its base with a new session
// and a lessons brief, up to limits.phase_restarts times (§11.7).
func (p *phaseRun) loop(ctx context.Context) error {
	restarts := max(p.d.Config.Limits.PhaseRestarts, 0)
	for attempts := 1; ; attempts++ {
		if err := p.startAttempt(ctx); err != nil {
			return err
		}
		if err := p.initialize(ctx); err != nil {
			return err
		}
		trip, err := p.attemptLoop(ctx)
		if err != nil {
			return err
		}
		p.att.lastTrip, p.out.Trip = trip, trip
		if err := p.emit(ctx, events.GuardTripped, events.Str("guard", trip.Guard), events.Str("class", string(trip.Class)),
			events.Str("detail", trip.Detail), events.Int("attempt", int64(p.attempt))); err != nil {
			return err
		}
		if err := p.toExhausted(ctx); err != nil {
			return err
		}
		if attempts > restarts {
			return p.block(ctx, trip.Class, fmt.Sprintf("%s (after %d attempt(s))", trip.Detail, p.attempt))
		}
		p.lessons = p.buildLessons(trip)
		if err := p.d.Worktree.ResetTo(ctx, p.base); err != nil {
			return fmt.Errorf("engine: reset to the phase base: %w", err)
		}
	}
}

func (p *phaseRun) startAttempt(ctx context.Context) error {
	if p.ph.Status != state.PhaseInitializing {
		if err := p.toPhase(ctx, state.PhaseInitializing); err != nil {
			return err
		}
	}
	p.attempt = p.ph.Attempts
	p.att = attemptState{started: p.d.Now()}
	p.ledger.Attempt = p.attempt
	return nil
}

// --- initialization (§11.3) -----------------------------------------------------

// expectedHead is the commit the phase must start from: the previous
// phase's commit, or the freeze commit for phase 1.
func (p *phaseRun) expectedHead(ctx context.Context) (string, string, error) {
	if p.in.Phase > 1 {
		prev, err := p.d.Store.GetPhase(ctx, p.in.RunID, p.in.Phase-1)
		if err != nil {
			return "", "", err
		}
		if prev.Status != state.PhaseCommitted || prev.CommitSHA == "" {
			return "", fmt.Sprintf("previous phase %d is not committed (%s)", prev.Number, prev.Status), nil
		}
		return prev.CommitSHA, "", nil
	}
	commits, err := p.d.Store.ListCommits(ctx, p.in.RunID)
	if err != nil {
		return "", "", err
	}
	freeze := ""
	for _, c := range commits {
		if c.Kind == "plan_freeze" {
			freeze = c.SHA
		}
	}
	if freeze == "" {
		return "", "the run has no plan freeze commit", nil
	}
	return freeze, "", nil
}

// initialize runs the initialization checks (§11.3 steps 1–3; any failure
// blocks with init_check_failed and no agent is called), resolves the
// gates, loads the context, and records the regression baseline (step 4).
func (p *phaseRun) initialize(ctx context.Context) error {
	wt, repo := p.wt(), p.d.Worktree
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	// 1. Plan integrity.
	lock, err := plan.ReadLock(wt)
	switch {
	case err != nil:
		bad("plan.lock.json: %v", err)
	default:
		if err := plan.VerifyLock(wt, lock); err != nil {
			bad("plan integrity: %v", err)
		}
		if p.run.PlanHash != "" && lock.PlanHash != p.run.PlanHash {
			bad("plan integrity: plan.lock.json has plan hash %s, the run froze %s", lock.PlanHash, p.run.PlanHash)
		}
		p.lock = lock
	}
	pkg, err := plan.Load(filepath.Join(wt, filepath.FromSlash(plan.PackageDir)), plan.Options{RepoRoot: wt})
	if err != nil {
		bad("frozen plan: %v", err)
	} else if p.spec = pkg.Phase(p.in.Phase); p.spec == nil {
		bad("the frozen plan has no phase %d", p.in.Phase)
	}
	p.pkg = pkg

	// 2. Git baseline.
	head, err := repo.Head(ctx)
	if err != nil {
		return err
	}
	expected, why, err := p.expectedHead(ctx)
	if err != nil {
		return err
	}
	switch {
	case why != "":
		bad("git baseline: %s", why)
	case head != expected:
		bad("git baseline: HEAD is %s, expected %s", head, expected)
	}
	if clean, err := repo.IsClean(ctx); err != nil {
		return err
	} else if !clean {
		bad("git baseline: the worktree has uncommitted changes")
	}
	if cur, err := repo.CurrentBranch(ctx); err != nil {
		return err
	} else if cur != repo.Branch() {
		bad("git baseline: HEAD is on %q, not on the run branch %s", cur, repo.Branch())
	} else if tip, err := repo.ResolveCommit(ctx, repo.Branch()); err != nil || tip != head {
		bad("git baseline: the run branch was moved")
	}

	// 3. Dependencies.
	if p.spec != nil {
		for _, dep := range p.spec.DependsOn {
			dp, err := p.d.Store.GetPhase(ctx, p.in.RunID, dep)
			if err != nil {
				bad("dependency phase %d: %v", dep, err)
				continue
			}
			if dp.Status != state.PhaseCommitted || dp.CommitSHA == "" {
				bad("dependency phase %d is not committed (%s)", dep, dp.Status)
				continue
			}
			if ok, err := repo.IsAncestor(ctx, dp.CommitSHA, head); err != nil || !ok {
				bad("dependency phase %d: its commit is not an ancestor of HEAD", dep)
			}
			if _, err := reports.ReadPhase(wt, dep); err != nil {
				bad("dependency phase %d: Phase Completion Report: %v", dep, err)
			}
		}
	}
	if len(problems) > 0 {
		return p.block(ctx, state.ClassInitCheckFailed, strings.Join(problems, "; "))
	}

	// Gates: a required gate with no resolvable command cannot run.
	specs, probs := gates.Resolve(gates.ResolveInput{Mode: gates.ModePhase, Phase: p.spec, Config: p.d.Config.Gates,
		Dir: wt, LookPath: p.d.LookPath})
	if len(probs) > 0 {
		var ps []string
		for _, pr := range probs {
			ps = append(ps, pr.String())
		}
		return p.block(ctx, state.ClassEnvironment, "quality gates cannot run: "+strings.Join(ps, "; "))
	}
	p.specs = specs
	p.acIDs = nil
	for _, ac := range p.spec.AcceptanceCriteria {
		p.acIDs = append(p.acIDs, ac.ID)
	}
	if err := p.loadContext(ctx); err != nil {
		return err
	}
	p.base = head
	if err := p.d.Store.SetPhaseCommits(ctx, p.in.RunID, p.in.Phase, head, ""); err != nil {
		return err
	}
	// 4. Baseline gates: the results recorded at the previous commit, or one
	// run of all gates now (pre-existing failures are not this phase's).
	if err := p.loadBaseline(ctx); err != nil {
		return err
	}
	// 5. A fresh session is started by implement; 6. the context is loaded.
	return p.toPhase(ctx, state.PhaseImplementing)
}

func readRepoFile(root, rel string) (string, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("engine: %s is not a regular file", rel)
	}
	b, err := os.ReadFile(p)
	return string(b), err
}

// loadContext reads the frozen texts verbatim and the digests of the
// committed phases (§11.3 step 6).
func (p *phaseRun) loadContext(ctx context.Context) error {
	wt := p.wt()
	var err error
	if p.overview, err = readRepoFile(wt, plan.PackageDir+"/"+plan.PlanFileName); err != nil {
		return fmt.Errorf("engine: frozen plan overview: %w", err)
	}
	if p.specText, err = readRepoFile(wt, fmt.Sprintf("%s/%s/%s.md", plan.PackageDir, plan.PhasesDirName, plan.PhaseID(p.in.Phase))); err != nil {
		// A package whose phase files have another name: use the parsed text.
		p.specText = p.spec.Raw
	}
	p.agentsText = ""
	if f, err := plan.DiscoverAgentsMD(wt); err != nil {
		return fmt.Errorf("engine: AGENTS.md: %w", err)
	} else if f != nil {
		p.agentsText = string(f.Raw)
	}
	phases, err := p.d.Store.ListPhases(ctx, p.in.RunID)
	if err != nil {
		return err
	}
	p.digests = nil
	for _, ph := range phases {
		if ph.Number >= p.in.Phase || ph.Status != state.PhaseCommitted {
			continue
		}
		if r, err := reports.ReadPhase(wt, ph.Number); err == nil {
			p.digests = append(p.digests, agent.PhaseDigest{Phase: ph.Number, Title: ph.Title, Digest: r.Digest})
		}
	}
	return nil
}

func (p *phaseRun) baselineStore() gates.BaselineStore { return gates.BaselineStore{Dir: p.d.Run} }

func (p *phaseRun) loadBaseline(ctx context.Context) error {
	b, err := p.baselineStore().Load(p.base)
	if err == nil {
		p.baseline = b
		return nil
	}
	if !errors.Is(err, gates.ErrNoBaseline) {
		return err
	}
	// The baseline must be evidence for the clean base tree: a round that is
	// stale (the tree moved during or after it) or ran on another tree is
	// not saved, or a failure it saw could later pass as pre-existing. The
	// worktree is never reset; the round is retried a bounded number of
	// times, then the phase blocks before the implementer is called.
	baseTree, err := p.d.Worktree.ResolveTree(ctx, p.base)
	if err != nil {
		return err
	}
	prefix := strings.TrimSuffix(artifacts.Baseline(p.in.Phase, p.attempt, ""), "/")
	var why string
	for try := 1; try <= baselineTries; try++ {
		pre := prefix
		if try > 1 {
			pre = fmt.Sprintf("%s-%d", prefix, try)
		}
		round, stale, err := p.runGates(ctx, gates.RunOptions{Mode: gates.ModePhase, Baseline: true}, pre, 0)
		if err != nil {
			return err
		}
		switch {
		case stale:
			why = "the working tree changed while the baseline gates ran"
		case round.TreeHash != baseTree:
			why = fmt.Sprintf("the baseline gates ran on tree %s, not on the base tree %s", round.TreeHash, baseTree)
		default:
			p.baseline = gates.NewBaseline(round, p.base, p.d.Now())
			_, err = p.baselineStore().Save(p.baseline)
			return err
		}
	}
	return p.block(ctx, state.ClassInitCheckFailed,
		fmt.Sprintf("baseline gates: no clean run on the base commit %s after %d tries: %s", p.base, baselineTries, why))
}

// baselineTries bounds the runs of the initial baseline gates when the
// working tree moves under them.
const baselineTries = 3
