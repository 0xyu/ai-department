//go:build live

package codexreview

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

// Live reviews with the real Codex CLI and GPT Sol 6.1 (AC-07.7). Run with
//
//	ACO_LIVE_CODEX=1 go test -tags=live ./internal/agent/codexreview/ -run Live -v -timeout 60m
//
// ACO_LIVE_AUTH_MODE=api uses OPENAI_API_KEY and a temporary dedicated
// CODEX_HOME; the default is the subscription login in ~/.codex.
// ACO_LIVE_CODEX_MODEL overrides the model.

func liveAdapter(t *testing.T) (*Adapter, *artifacts.RunDir) {
	t.Helper()
	if os.Getenv("ACO_LIVE_CODEX") != "1" {
		t.Skip("set ACO_LIVE_CODEX=1")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not installed")
	}
	mode := auth.Subscription
	if os.Getenv("ACO_LIVE_AUTH_MODE") == "api" {
		mode = auth.API
	}
	model := os.Getenv("ACO_LIVE_CODEX_MODEL")
	if model == "" {
		model = config.DefaultReviewerModel
	}
	dir := t.TempDir()
	red := secure.NewRedactor()
	home, _ := os.UserHomeDir()
	a := New(Options{
		Bin: bin, Mode: mode, Env: auth.NewEnv(os.LookupEnv, nil), APIHome: filepath.Join(dir, "codex-api"), UserHome: home,
		Model: model, ReasoningEffort: "medium", Redactor: red,
		InvocationTimeout: 20 * time.Minute, IdleTimeout: 10 * time.Minute,
	})
	run, err := artifacts.New(filepath.Join(dir, "aco"), red).Run("r-live-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("artifacts: %s", run.Path())
	return a, run
}

func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(full), 0o700)
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.email=a@b", "-c", "user.name=aco", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func logCalls(t *testing.T, c agent.ReviewCalls) {
	for i, inv := range c.Calls {
		t.Logf("call %d: model %s tokens in %d out %d, %s, violations %v", i+1, inv.Model, inv.Usage.InputTokens, inv.Usage.OutputTokens, inv.Usage.Duration, inv.Violations)
	}
}

// The read-only sandbox cannot create a file in the worktree.
func TestLiveReadOnly(t *testing.T) {
	a, _ := liveAdapter(t)
	res, err := a.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.ReadOnly {
		t.Fatal("the reviewer created a file in read-only mode")
	}
	t.Logf("probe ok: %+v", res)
}

// A plan with an uncovered requirement gets a "missing" coverage entry.
func TestLivePlanMissingRequirement(t *testing.T) {
	a, run := liveAdapter(t)
	repo := gitRepo(t, map[string]string{"go.mod": "module calc\n\ngo 1.22\n", "calc.go": "package calc\n"})
	req := agent.PlanReviewRequest{
		RunID: "r-live-1", Round: 1, Workspace: agent.Workspace{Dir: repo}, Output: agent.Output{Run: run, Prefix: "plan-review/round-01"},
		Requirements: "# Calculator library\n- REQ-001: Add(a, b int) int returns the sum.\n- REQ-002: Div(a, b int) (int, error) returns the quotient and an error when b is 0.\n",
		ReqIDs:       []string{"REQ-001", "REQ-002"},
		PackageFiles: []agent.PackFile{
			{Path: "IMPLEMENTATION_PLAN.md", Content: "# Implementation Plan: calculator\n\nOne phase implements Add.\n\n## Phases\n- P01 Add\n"},
			{Path: "requirements.md", Content: "- REQ-001 (functional): Add(a, b int) int returns the sum.\n- REQ-002 (functional): Div(a, b int) (int, error), error when b is 0.\n"},
			{Path: "phases/P01.md", Content: "## Phase 1: Add\n### Goal\nImplement Add.\n### Scope\n- In: calc.go Add\n### Acceptance Criteria\n- AC-01.1: Add(2, 3) returns 5 (unit test TestAdd).\n### Requirements\n- REQ-001: AC-01.1\n### Quality Gates\n- typecheck: go vet ./...\n- lint: gofmt -l .\n- unit: go test ./...\n- integration: not_applicable (library without I/O)\n- build: go build ./...\n- security: scanners: secrets\n"},
			{Path: "traceability.json", Content: `{"REQ-001":{"phases":[1],"acceptance_criteria":["AC-01.1"]}}`},
		},
		RepoOverview: "go.mod (module calc), calc.go (empty package). Language: Go. No tests yet.",
		CheckResults: "error traceability.json: REQ-002 is not covered by any phase or acceptance criterion\n",
	}
	res, err := a.ReviewPlan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	logCalls(t, res.ReviewCalls)
	var st string
	for _, c := range res.Review.RequirementCoverage {
		if c.ReqID == "REQ-002" {
			st = c.Status
		}
	}
	if st != "missing" {
		t.Errorf("REQ-002 coverage = %q, want missing: %s", st, res.Raw)
	}
}

// A seeded SQL injection is reported as a security blocker or major in
// the right file.
func TestLiveSQLInjection(t *testing.T) {
	a, run := liveAdapter(t)
	users := `package store

import (
	"database/sql"
	"fmt"
)

// User is a stored user.
type User struct {
	ID   int
	Name string
}

// FindUser returns the user with the given name.
func FindUser(db *sql.DB, name string) (*User, error) {
	q := fmt.Sprintf("SELECT id, name FROM users WHERE name = '%s'", name)
	row := db.QueryRow(q)
	var u User
	if err := row.Scan(&u.ID, &u.Name); err != nil {
		return nil, fmt.Errorf("find user %q: %w", name, err)
	}
	return &u, nil
}
`
	repo := gitRepo(t, map[string]string{"go.mod": "module users\n\ngo 1.22\n", "store/users.go": users,
		"AGENTS.md": "Go code. Tests with go test. Use parameterized queries."})
	diff := "diff --git a/store/users.go b/store/users.go\nnew file mode 100644\n--- /dev/null\n+++ b/store/users.go\n@@ -0,0 +1,23 @@\n"
	for _, l := range strings.Split(strings.TrimSuffix(users, "\n"), "\n") {
		diff += "+" + l + "\n"
	}
	req := agent.CodeReviewRequest{
		RunID: "r-live-1", Kind: agent.ReviewPhase, Phase: 1, Attempt: 1, Round: 1,
		Workspace: agent.Workspace{Dir: repo}, Output: agent.Output{Run: run, Prefix: "phases/P01/a1/reviews/r01"},
		PlanOverview: "# Plan: user store\nP01 implements FindUser over database/sql.",
		Phases:       []agent.PhaseStatus{{Number: 1, Title: "User lookup", Status: "current"}},
		PhaseSpec:    "## Phase 1: User lookup\n### Acceptance Criteria\n- AC-01.1: FindUser(db, name) returns the user with that name.\n- AC-01.2: errors are wrapped with the name.\n",
		AgentsMD:     "Go code. Tests with go test. Use parameterized queries.",
		ACIDs:        []string{"AC-01.1", "AC-01.2"}, Diff: diff,
		GateResults: "typecheck passed; lint passed; unit passed (no tests); integration not_applicable; build passed; security passed (secrets only)",
		ImplReport:  `{"summary":"Implemented FindUser; it is safe and fully tested."}`,
	}
	res, err := a.ReviewCode(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	logCalls(t, res.ReviewCalls)
	found := false
	for _, f := range res.Review.NewFindings {
		if f.Category == "security" && (f.Severity == "blocker" || f.Severity == "major") && strings.HasSuffix(filepath.ToSlash(f.File), "store/users.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("no security blocker/major in store/users.go: %s", res.Raw)
	}
	if res.Review.Verdict == "approve" {
		t.Errorf("approved an SQL injection")
	}
}
