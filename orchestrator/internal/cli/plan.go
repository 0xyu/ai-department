package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/agent/claudecode"
	"github.com/0xyu/aco/internal/agent/codexreview"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/doctor"
	"github.com/0xyu/aco/internal/engine"
	"github.com/0xyu/aco/internal/exitcode"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/i18n"
	"github.com/0xyu/aco/internal/plan"
	"github.com/0xyu/aco/internal/state"
)

// PlanAgents builds the planner and the reviewer for a run (System.Agents;
// tests inject fakes).
type PlanAgents func(cfg *config.Config, res *auth.Resolution, env *auth.Env) (agent.Planner, agent.Reviewer)

// maxRequirementsBytes caps the requirement document.
const maxRequirementsBytes = 1 << 20

// runPlan is `aco plan`: Stages 1–2 only, stopping after the freeze (§17.1).
func runPlan(inv *invocation) int {
	if len(inv.positional) > 0 {
		inv.errOut.Say(i18n.CLIErrUnexpectedArgs, echoArgs(inv.redactor, inv.positional))
		return exitcode.Usage
	}
	if inv.loadErr != nil {
		renderConfigError(inv.errOut, inv.redactor, inv.loadErr)
		return exitcode.Usage
	}
	reqPath, planPath := inv.str("requirements"), inv.str("plan")
	if reqPath == "" && planPath == "" {
		inv.errOut.Say(i18n.PlanErrNeedInput)
		inv.errOut.Say(i18n.CLIErrHelpHint, inv.cmd.name)
		return exitcode.Usage
	}
	var in engine.PlanningInput
	if reqPath != "" {
		b, err := readCapped(reqPath, maxRequirementsBytes)
		if err != nil {
			inv.errOut.Say(i18n.PlanErrReadInput, inv.redactor.Echo(reqPath), inv.redactor.String(err.Error()))
			return exitcode.Usage
		}
		in.Requirements = string(b)
	}
	loaded := inv.loaded
	if planPath != "" {
		pkg, err := plan.Load(planPath, plan.Options{})
		if err != nil {
			inv.errOut.Say(i18n.PlanErrReadInput, inv.redactor.Echo(planPath), inv.redactor.String(err.Error()))
			return exitcode.Usage
		}
		in.Plan = pkg
		// The plan's front matter is the "plan" config layer.
		if len(pkg.FrontMatter) > 0 {
			l, err := inv.loadConfigWithPlan(pkg.FrontMatter, planPath)
			if err != nil {
				renderConfigError(inv.errOut, inv.redactor, err)
				return exitcode.Usage
			}
			loaded = l
		}
	}
	cfg := loaded.Config
	env, res, ok := inv.loadEnvAndAuth(cfg)
	if !ok {
		return exitcode.Usage
	}
	repo := inv.str("repo")
	if repo == "" {
		repo = inv.sys.cwd()
	}
	if abs, err := filepath.Abs(repo); err == nil {
		repo = abs
	}
	input := state.InputRequirements
	if in.Plan != nil {
		input = state.InputPlan
	}
	return inv.planRun(cfg, res, env, repo, input, in)
}

func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("file too large")
	}
	return b, nil
}

func (inv *invocation) loadConfigWithPlan(front []byte, source string) (*config.Loaded, error) {
	repo := inv.str("repo")
	if repo == "" {
		repo = inv.sys.cwd()
	}
	return config.Load(config.LoadOptions{
		UserConfigPath:  filepath.Join(doctor.DefaultACOHome(inv.sys.home()), "config.yaml"),
		RepoDir:         repo,
		ConfigFiles:     inv.multi("config"),
		PlanFrontMatter: front,
		PlanSource:      source,
		Overrides:       inv.multi("set"),
	})
}

// planRun creates the run, takes the repository lock, runs the preflight,
// creates the run worktree, and runs Stages 1–2.
func (inv *invocation) planRun(cfg *config.Config, res *auth.Resolution, env *auth.Env, repo string, input state.InputKind, in engine.PlanningInput) int {
	ctx := inv.ctx
	red := inv.redactor
	lang := inv.out.Lang()
	acoHome := doctor.DefaultACOHome(inv.sys.home())
	sysErr := func(err error) int {
		inv.errOut.Say(i18n.PlanErrSystem, red.String(err.Error()))
		return exitcode.Failed
	}

	store, err := state.Open(ctx, filepath.Join(acoHome, "state.db"), state.Options{Redactor: red, Lang: lang})
	if err != nil {
		return sysErr(err)
	}
	defer store.Close()

	runID := state.NewRunID(time.Now())
	lock, _, err := store.AcquireLock(ctx, repo, runID, 0)
	var locked *state.LockedError
	if errors.As(err, &locked) {
		inv.errOut.Say(i18n.PlanErrLocked, red.Echo(repo), locked.Holder.RunID)
		return exitcode.Failed
	}
	if err != nil {
		return sysErr(err)
	}
	defer lock.Release(context.WithoutCancel(ctx))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		if err, ok := <-lock.KeepAlive(runCtx, 0); ok && err != nil {
			cancel() // the lock was lost: stop at once
		}
	}()

	run, err := store.CreateRun(ctx, state.NewRun{ID: runID, RepoPath: repo, BaseBranch: inv.str("base"),
		Mode: state.ModePlanOnly, Input: input})
	if err != nil {
		return sysErr(err)
	}
	if _, err := store.TransitionRun(ctx, runID, state.RunPreflight, state.TransitionOptions{}); err != nil {
		return sysErr(err)
	}
	preflightFail := func(err error) int {
		_, _ = store.TransitionRun(context.WithoutCancel(ctx), runID, state.RunFailed, state.TransitionOptions{
			Reason: state.Reason{Class: state.ClassPreflightFailed, Stage: state.StagePreflight, Detail: err.Error()}})
		inv.errOut.Say(i18n.PlanErrPreflight, red.String(err.Error()))
		return exitcode.PreflightFailed
	}

	g, err := gitops.Open(repo, gitops.Options{Redactor: red})
	if err != nil {
		return preflightFail(err)
	}
	wtPath := filepath.Join(acoHome, "worktrees", runID)
	pre, err := g.Preflight(ctx, inv.str("base"), wtPath)
	if err != nil {
		return preflightFail(err)
	}
	title := planTitle(in)
	short := runID[strings.LastIndex(runID, "-")+1:]
	branch := gitops.BranchName(time.Now(), title, short)
	wt, err := g.AddWorktree(ctx, wtPath, branch, pre.BaseCommit)
	if err != nil {
		return preflightFail(err)
	}
	if _, err := store.UpdateRunInfo(ctx, runID, state.RunInfo{RunBranch: &branch}); err != nil {
		return sysErr(err)
	}
	runDir, err := artifacts.New(acoHome, red).Run(runID)
	if err != nil {
		return sysErr(err)
	}
	inv.out.Say(i18n.PlanStarted, run.ID, pre.BaseBranch, branch)

	agents := inv.sys.Agents
	if agents == nil {
		agents = inv.realAgents
	}
	planner, reviewer := agents(cfg, res, env)
	look := inv.sys.LookPath
	if look == nil {
		look = exec.LookPath
	}
	in.RunID, in.BaseCommit = runID, pre.BaseCommit
	out, err := engine.RunPlanning(runCtx, engine.PlanningDeps{
		Store: store, Run: runDir, Planner: planner, Reviewer: reviewer, Worktree: wt, Config: cfg,
		PlannerAuth: string(res.Claude.Mode), ReviewerAuth: string(res.Codex.Mode), LookPath: look,
	}, in)
	if err != nil && out == nil {
		return sysErr(err)
	}
	return inv.renderPlanOutcome(out, err, runDir.Path(), wt.Dir())
}

// planTitle names the run branch: the plan's title or the requirement
// document's first heading.
func planTitle(in engine.PlanningInput) string {
	if in.Plan != nil && in.Plan.Title != "" {
		return in.Plan.Title
	}
	for _, line := range strings.Split(in.Requirements, "\n") {
		if t := strings.TrimSpace(strings.TrimLeft(line, "#")); strings.HasPrefix(line, "#") && t != "" {
			return t
		}
	}
	return "plan"
}

func (inv *invocation) renderPlanOutcome(out *engine.PlanningOutcome, runErr error, runDir, wtDir string) int {
	p, red := inv.out, inv.redactor
	code := exitcode.Failed
	switch out.Status {
	case state.RunCompleted, state.RunExecuting:
		pkg := out.Package
		p.Say(i18n.PlanFrozenLine, len(pkg.Phases), out.Rounds, out.Revisions, out.Commit, out.Tag)
		p.Say(i18n.PlanHashLine, out.Lock.PlanHash)
		code = exitcode.Completed
	case state.RunBlocked:
		p.Say(i18n.PlanBlockedLine, string(out.Reason.Class), red.String(out.Reason.Detail))
		for _, r := range out.Decision.Reasons {
			p.Say(i18n.PlanGateReasonRow, r.Code, r.Ref)
		}
		for _, e := range out.Ledger.OpenEntries() {
			f := e.Finding
			p.Say(i18n.PlanOpenFindingRow, f.ID, f.Severity, f.Dimension, red.String(f.Title))
		}
		code = exitcode.Blocked
	case state.RunPaused:
		p.Say(i18n.PlanPausedLine, out.ResumeAt.Local().Format(time.RFC3339))
	case state.RunCancelled:
		p.Say(i18n.PlanCancelledLine)
		code = exitcode.Cancelled
	default:
		detail := out.Reason.Detail
		if runErr != nil {
			detail = runErr.Error()
		}
		inv.errOut.Say(i18n.PlanFailedLine, string(out.Reason.Class), red.String(detail))
	}
	p.Say(i18n.PlanWorktreeLine, wtDir)
	p.Say(i18n.PlanArtifactsLine, runDir)
	return code
}

// realAgents wires Claude Code (planner) and Codex CLI (reviewer) with a
// shared USD budget (api mode).
func (inv *invocation) realAgents(cfg *config.Config, res *auth.Resolution, env *auth.Env) (agent.Planner, agent.Reviewer) {
	budget := agent.NewBudget(cfg.Limits.BudgetUSD)
	co := claudecode.OptionsFromConfig(cfg, res.Claude.Mode, env, inv.redactor)
	co.Keys, co.Budget = inv.sys.Keychain, budget
	xo := codexreview.OptionsFromConfig(cfg, res.Codex.Mode, env, inv.redactor, doctor.DefaultACOHome(inv.sys.home()), inv.sys.home())
	xo.Keys, xo.Budget = inv.sys.Keychain, budget
	if inv.sys.Environ != nil {
		co.BaseEnv, xo.BaseEnv = inv.sys.Environ(), inv.sys.Environ()
	}
	return claudecode.New(co).Planner(), codexreview.New(xo)
}
