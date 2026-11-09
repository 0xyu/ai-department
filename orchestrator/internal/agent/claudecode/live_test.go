//go:build live

package claudecode

// Live tests call the real Claude Code CLI (AC-05.7). They use quota, so
// they run only with -tags=live and ACO_LIVE_CLAUDE=1:
//
//	ACO_LIVE_CLAUDE=1 go test -tags=live -run TestLive -v -timeout 40m ./internal/agent/claudecode/
//
// ACO_LIVE_AUTH_MODE=api runs them in api mode (needs ANTHROPIC_API_KEY).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/secure"
)

func liveAdapter(t *testing.T) (*Adapter, auth.Mode, *agent.Budget) {
	t.Helper()
	if os.Getenv("ACO_LIVE_CLAUDE") != "1" {
		t.Skip("set ACO_LIVE_CLAUDE=1 to call the real Claude Code CLI")
	}
	mode := auth.Subscription
	if os.Getenv("ACO_LIVE_AUTH_MODE") == "api" {
		mode = auth.API
	}
	cfg := config.Default()
	cfg.Limits.ClaudeMaxTurns = 60
	cfg.Implementer.ExtraAllowedBash = []string{"go "}
	red := secure.NewRedactor()
	red.AddFromEnv(map[string]string{"ANTHROPIC_API_KEY": os.Getenv("ANTHROPIC_API_KEY")})
	opts := OptionsFromConfig(cfg, mode, auth.NewEnv(os.LookupEnv, nil), red)
	opts.HookBin = buildLiveACO(t)
	opts.InvocationTimeout, opts.IdleTimeout = 20*time.Minute, 5*time.Minute
	var budget *agent.Budget
	if mode == auth.API {
		budget = agent.NewBudget(config.BudgetLimits{Run: 10, SoftRatio: 0.8})
		opts.Budget = budget
	}
	return New(opts), mode, budget
}

func buildLiveACO(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "aco")
	if filepath.Separator == '\\' {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/aco")
	cmd.Dir = filepath.Join("..", "..", "..")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

func liveRepo(t *testing.T) (string, *artifacts.RunDir) {
	t.Helper()
	dir := t.TempDir()
	ws := filepath.Join(dir, "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.com/greet\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, _ = filepath.EvalSymlinks(ws)
	run, err := artifacts.New(filepath.Join(dir, "aco"), nil).Run("r-live-1")
	if err != nil {
		t.Fatal(err)
	}
	return ws, run
}

func liveWorkspace(dir string) agent.Workspace {
	return agent.Workspace{Dir: dir, ProtectedPaths: config.Default().Policy.ProtectedPaths, ScopeEnforcement: "flag", Network: "allowlist"}
}

func TestLivePlanner(t *testing.T) {
	a, mode, budget := liveAdapter(t)
	ws, run := liveRepo(t)
	res, err := a.Planner().Create(context.Background(), agent.PlanRequest{
		RunID: "r-live-1", Workspace: liveWorkspace(ws), Output: agent.Output{Run: run, Prefix: "planning/round-1"},
		Requirements:    "Add a Go package `greet` with a function Hello(name string) string that returns \"Hello, <name>!\". Keep it to one phase.",
		RepoOverview:    "go.mod only (module example.com/greet). No code yet.",
		PlanFormatGuide: "Use the JSON schema. One phase is enough.",
		GateCatalogue:   "typecheck: go vet ./...; lint: gofmt -l .; unit: go test ./...; integration: not applicable; build: go build ./...; security: secrets scanner",
		BudgetScopes:    []agent.Scope{agent.ScopeRun, agent.ScopePlanning},
	})
	if err != nil {
		dumpLive(t, run, "planning/round-1")
		t.Fatal(err)
	}
	if res.Package == nil || len(res.Package.Phases) == 0 || len(res.Package.Requirements) == 0 {
		t.Fatalf("no plan package: %+v", res.Package)
	}
	for _, d := range res.Package.Diagnostics {
		t.Errorf("diagnostic %s: %s", d.Code, d.Message)
	}
	t.Logf("plan: %d requirements, %d phases; session %s; model %s; auth %s; turns %d; cost $%.4f",
		len(res.Package.Requirements), len(res.Package.Phases), res.SessionID, res.Model, res.AuthSource, res.Usage.Turns, res.Usage.CostUSD)
	if mode == auth.API && (!res.Charged || budget.Spent(agent.ScopeRun) <= 0) {
		t.Error("api mode: cost not charged")
	}
	if _, err := os.Stat(filepath.Join(ws, "greet")); err == nil {
		t.Error("the planner wrote to the repository")
	}
}

func TestLiveImplementerStartAndContinue(t *testing.T) {
	a, _, _ := liveAdapter(t)
	ws, run := liveRepo(t)
	spec := `## Phase 1: Greeting

### Goal
Provide greet.Hello.

### Scope
- In: package greet (greet/greet.go) and its test.
- Paths: greet/**

### Acceptance Criteria
- AC-01.1: greet.Hello("Ada") returns "Hello, Ada!" and a unit test in greet/greet_test.go checks it.
`
	w := liveWorkspace(ws)
	w.ScopePaths = []string{"greet/**"}
	res, err := a.Implementer().Start(context.Background(), agent.ImplementRequest{
		RunID: "r-live-1", Kind: agent.KindPhase, Phase: 1, PhaseTitle: "Greeting", Attempt: 1, Workspace: w,
		Output: agent.Output{Run: run, Prefix: "phases/P01/a1/step-1-implement"}, PlanOverview: "Tiny greeting library.",
		PhaseSpec: spec, ACIDs: []string{"AC-01.1"},
		Gates: []agent.GateSpec{{Type: "unit", Commands: []string{"go test ./..."}}, {Type: "build", Commands: []string{"go build ./..."}}},
	})
	if err != nil {
		dumpLive(t, run, "phases/P01/a1/step-1-implement")
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(ws, "greet", "greet.go"))
	if err != nil || !strings.Contains(string(src), "Hello") {
		t.Fatalf("greet/greet.go not written: %v", err)
	}
	t.Logf("start: session %s, %d turns, %d guard denials, report: %s", res.SessionID, res.Usage.Turns, len(res.GuardDenials), res.Report.Summary)

	cres, err := a.Implementer().Continue(context.Background(), agent.ContinueRequest{
		RunID: "r-live-1", SessionID: res.SessionID, Kind: agent.ContinueReviewFix, Phase: 1, Workspace: w,
		Output: agent.Output{Run: run, Prefix: "phases/P01/a1/step-2-review-fix"}, ACIDs: []string{"AC-01.1"},
		Findings: []agent.CodeFinding{{ID: "F-001", Severity: "minor", Category: "docs", Location: "greet/greet.go",
			Title: "Missing doc comment", Description: "Hello has no doc comment.", RequiredChange: "Add a Go doc comment to Hello."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cres.SessionID != res.SessionID {
		t.Errorf("Continue changed the session: %s -> %s", res.SessionID, cres.SessionID)
	}
	src, _ = os.ReadFile(filepath.Join(ws, "greet", "greet.go"))
	if !strings.Contains(string(src), "// Hello") {
		t.Error("doc comment not added")
	}
	t.Logf("continue: %d turns, responses %+v", cres.Usage.Turns, cres.Report.FindingResponses)
}

func dumpLive(t *testing.T, run *artifacts.RunDir, prefix string) {
	for _, f := range []string{"stderr.log", "transcript.jsonl"} {
		b, _ := run.Read(prefix + "/" + f)
		if len(b) > 4000 {
			b = b[len(b)-4000:]
		}
		t.Logf("--- %s:\n%s", f, b)
	}
}
