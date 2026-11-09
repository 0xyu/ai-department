package codexreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/secure"
)

var update = flag.Bool("update", false, "rewrite golden files")

const testKey = "sk-proj-TESTKEYabcdefghijklmnopqrstuvwxyz0123456789"

type fixture struct {
	t        *testing.T
	dir      string
	home     string // fake user home (with ~/.codex)
	work     string
	run      *artifacts.RunDir
	apiHome  string
	extra    []string
	fixtures []string
}

func newFixture(t *testing.T, fixtures ...string) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, dir: dir, home: filepath.Join(dir, "home"), work: filepath.Join(dir, "work"),
		apiHome: filepath.Join(dir, "home", ".aco", "codex-api"), fixtures: fixtures}
	for _, d := range []string{filepath.Join(f.home, ".codex"), f.work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The user's own subscription login, which api mode must never touch.
	_ = os.WriteFile(filepath.Join(f.home, ".codex", "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"id_token":"user"}}`), 0o600)
	_ = os.WriteFile(filepath.Join(f.home, ".codex", "config.toml"), []byte("model = \"x\"\n"), 0o600)
	store := artifacts.New(filepath.Join(dir, "aco"), secure.NewRedactor())
	run, err := store.Run("r-test-1")
	if err != nil {
		t.Fatal(err)
	}
	f.run = run
	return f
}

func (f *fixture) path(name string) string { return filepath.Join(f.dir, name) }

func (f *fixture) baseEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HOME", "USERPROFILE", "CODEX_HOME":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+f.home, "USERPROFILE="+f.home,
		"OPENAI_API_KEY=sk-proj-PARENTKEYxxxxxxxxxxxxxxxxxxxx1", "ANTHROPIC_API_KEY=sk-ant-PARENTxxxxxxxxxxxxxxxx",
		"CODEX_API_KEY=parent-codex-key-xxxxxxxx", "OPENAI_BASE_URL=https://evil.example", "CLAUDE_CODE_USE_BEDROCK=1")
}

func (f *fixture) adapter(mode auth.Mode, mut ...func(*Options)) *Adapter {
	td, _ := filepath.Abs("testdata")
	binArgs := []string{"-test.run=^TestFakeCodex$", "--", "--fake-codex", "testdata=" + td,
		"fixtures=" + strings.Join(f.fixtures, ","), "counter=" + f.path("counter"),
		"dumpargs=" + f.path("args"), "dumpenv=" + f.path("env"), "dumpstdin=" + f.path("stdin"),
		"artifacts=" + f.run.Path(), "loginlog=" + f.path("logins")}
	binArgs = append(append(binArgs, f.extra...), "--end-fake")
	env := auth.NewEnv(func(k string) (string, bool) {
		if k == auth.EnvOpenAIKey && mode == auth.API {
			return testKey, true
		}
		return "", false
	}, nil)
	o := Options{
		Bin: os.Args[0], BinArgs: binArgs, Mode: mode, Env: env, BaseEnv: f.baseEnv(),
		APIHome: f.apiHome, UserHome: f.home, Model: "gpt-6.1-sol", ReasoningEffort: "high",
		IdleTimeout: 60 * time.Second, InvocationTimeout: 2 * time.Minute, KillGrace: 5 * time.Second,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	for _, m := range mut {
		m(&o)
	}
	return New(o)
}

func (f *fixture) codeReq() agent.CodeReviewRequest {
	return agent.CodeReviewRequest{
		RunID: "r1", Kind: agent.ReviewPhase, Phase: 1, Attempt: 1, Round: 1,
		Workspace: agent.Workspace{Dir: f.work}, Output: agent.Output{Run: f.run, Prefix: "phases/P01/a1/reviews/r01"},
		PlanOverview: "# Plan\nA calculator.", PhaseSpec: "## Phase 1: Calculator\n- AC-01.1 Add works\n- AC-01.2 Sub works",
		AgentsMD: "Run go test.", ACIDs: []string{"AC-01.1", "AC-01.2"},
		Diff:        sampleDiff,
		GateResults: "unit: passed", ImplReport: `{"summary":"done"}`,
	}
}

func (f *fixture) planReq() agent.PlanReviewRequest {
	return agent.PlanReviewRequest{
		RunID: "r1", Round: 1, Workspace: agent.Workspace{Dir: f.work},
		Output:       agent.Output{Run: f.run, Prefix: "plan-review/round-01"},
		Requirements: "REQ-001 add\nREQ-002 subtract", ReqIDs: []string{"REQ-001", "REQ-002"},
		PackageFiles: []agent.PackFile{{Path: "IMPLEMENTATION_PLAN.md", Content: "# Plan"}},
		CheckResults: "all checks passed",
	}
}

func (f *fixture) read(name string) string {
	b, _ := os.ReadFile(f.path(name))
	return string(b)
}

const sampleDiff = `diff --git a/calc.go b/calc.go
new file mode 100644
--- /dev/null
+++ b/calc.go
@@ -0,0 +1,4 @@
+package calc
+
+func Add(a, b int) int { return a + b }
+func Sub(a, b int) int { return a - b }
`

func classOf(t *testing.T, err error) agent.ErrorClass {
	t.Helper()
	var ae *agent.Error
	if !errors.As(err, &ae) {
		t.Fatalf("not an agent error: %v", err)
	}
	return ae.Class
}

func TestValidCodeReview(t *testing.T) {
	f := newFixture(t, "code_valid.jsonl")
	res, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Review.Verdict != "approve" || len(res.Review.AcceptanceCriteria) != 2 || len(res.Inconsistent) != 0 {
		t.Fatalf("review: %+v inconsistent %v", res.Review, res.Inconsistent)
	}
	if res.SessionID != "01a1bef3-0000-7000-8000-000000000001" || res.Usage.InputTokens != 1200 || res.Usage.OutputTokens != 800 ||
		res.Usage.CacheReadTokens != 300 || res.Usage.ReasoningTokens != 40 || res.Usage.Turns != 1 {
		t.Errorf("usage/session: %+v %s", res.Usage, res.SessionID)
	}
	if res.Priced || res.Usage.CostUSD != 0 {
		t.Error("subscription mode without a price must not have a cost")
	}
	// Artifacts: context pack, JSONL stream, final message, stderr, accepted review, schema.
	for _, rel := range []string{res.PromptRef, res.TranscriptRef, res.OutputRef, res.StderrRef, res.ReviewRef, "schemas/code-review.schema.json"} {
		if _, err := f.run.Read(rel); err != nil {
			t.Errorf("artifact %s: %v", rel, err)
		}
	}
	if res.ContextRef != "phases/P01/a1/reviews/r01/call-01/context.md" {
		t.Errorf("context ref %s", res.ContextRef)
	}
	sch, _ := f.run.Read("schemas/code-review.schema.json")
	if strings.Contains(string(sch), "$schema") {
		t.Error("the schema file for --output-schema keeps $schema")
	}
	// The pack went to stdin; the commands in the stream stayed inside the roots.
	stdin := f.read("stdin-0")
	for _, want := range []string{"# Instructions", "AC-01.1, AC-01.2", "UNVERIFIED CLAIMS", "func Add", "## Regression data", "added calc.go Add (go)"} {
		if !strings.Contains(stdin, want) {
			t.Errorf("stdin lacks %q", want)
		}
	}
	if len(res.Violations) != 0 {
		t.Errorf("violations: %+v", res.Violations)
	}
}

// AC-07.2: the built command always has the read-only, ephemeral, schema
// flags and the model; never a write-capable or bypass flag.
func TestCommandFlags(t *testing.T) {
	f := newFixture(t, "code_valid.jsonl")
	if _, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq()); err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal([]byte(f.read("args-0")), &args); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--sandbox read-only", "--ephemeral", "--json", "-m gpt-6.1-sol", `model_reasoning_effort="high"`, "--ignore-user-config", "--ignore-rules", "--disable hooks"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args lack %q: %s", want, joined)
		}
	}
	if !slices.Contains(args, "--output-schema") || args[len(args)-1] != "-" {
		t.Errorf("args: %v", args)
	}
	for _, bad := range []string{"workspace-write", "danger-full-access", "--dangerously-bypass-approvals-and-sandbox", "--add-dir", "--worktree"} {
		if strings.Contains(joined, bad) {
			t.Errorf("args contain %s", bad)
		}
	}
}

func TestBuildArgsGolden(t *testing.T) {
	caps := recordedCaps(t)
	args, err := BuildArgs(caps, CommandSpec{Model: "gpt-6.1-sol", ReasoningEffort: "high", Dir: abs("/work"),
		SchemaPath: abs("/runs/r1/schemas/code-review.schema.json"), LastMessagePath: abs("/runs/r1/last.json"), WindowsSandbox: "unelevated"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range args {
		args[i] = filepath.ToSlash(strings.TrimPrefix(args[i], filepath.VolumeName(args[i])))
	}
	got, _ := json.MarshalIndent(args, "", "  ")
	golden := filepath.Join("testdata", "args-review.golden.json")
	if *update {
		_ = os.WriteFile(golden, append(got, '\n'), 0o600)
	}
	want, _ := os.ReadFile(golden)
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
		t.Errorf("args differ from %s (run with -update):\n%s", golden, got)
	}
}

func abs(p string) string {
	a, _ := filepath.Abs(filepath.FromSlash(p))
	return a
}

func recordedCaps(t *testing.T) *Capabilities {
	t.Helper()
	v, _ := os.ReadFile("testdata/codex-version-0.162.0.txt")
	h, _ := os.ReadFile("testdata/codex-exec-help-0.162.0.txt")
	fl, _ := os.ReadFile("testdata/codex-features-0.162.0.txt")
	c := ParseCapabilities(string(v), string(h), string(fl))
	if c.Version != "0.162.0" || len(c.Missing()) != 0 {
		t.Fatalf("recorded caps: %s missing %v", c.Version, c.Missing())
	}
	return c
}

func TestCheckArgsRefusesWriteAndBypass(t *testing.T) {
	base := []string{"exec", "--ephemeral", "--output-schema", "/s.json", "-"}
	for _, extra := range [][]string{
		{"--dangerously-bypass-approvals-and-sandbox"}, {"--sandbox", "workspace-write"}, {"--sandbox", "danger-full-access"},
		{"-s", "workspace-write"}, {"--sandbox=workspace-write"}, {"--full-auto"}, {"--yolo"}, {"--add-dir", "/tmp"},
		{"--worktree"}, {"-c", `sandbox_mode="workspace-write"`}, {"-c", "approval_policy=never"},
		{"-c", "sandbox_workspace_write.network_access=true"}, {"-c", `mcp_servers.x.command="sh"`}, {"--approve-for-me"},
		{"--dangerously-bypass-hook-trust"}, {"--sandbox"},
	} {
		if err := CheckArgs(append(append([]string{}, base...), extra...)); err == nil {
			t.Errorf("accepted %v", extra)
		}
	}
	if err := CheckArgs(append(base, "--sandbox", "read-only")); err != nil {
		t.Errorf("read-only refused: %v", err)
	}
	if err := CheckArgs([]string{"exec", "--sandbox", "read-only", "-"}); err == nil {
		t.Error("a command without --ephemeral and --output-schema was accepted")
	}
}

// AC-07.3: schema-invalid, then valid.
func TestSchemaInvalidThenValid(t *testing.T) {
	f := newFixture(t, "code_schema_invalid.jsonl", "code_valid.jsonl")
	res, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Calls) != 2 || res.FallbackUsed {
		t.Fatalf("calls %d fallback %v", len(res.Calls), res.FallbackUsed)
	}
	second := f.read("stdin-1")
	if !strings.HasPrefix(second, "# Correction") || !strings.Contains(second, "test_assessment") || !strings.Contains(second, "# Instructions") {
		t.Errorf("the retry does not carry the validation errors and the full pack:\n%.600s", second)
	}
	if res.Total.InputTokens != 2400 {
		t.Errorf("total usage of both calls: %+v", res.Total)
	}
}

func TestSchemaInvalidExhaustedThenFallback(t *testing.T) {
	f := newFixture(t, "code_schema_invalid.jsonl", "code_schema_invalid.jsonl", "code_schema_invalid.jsonl", "code_valid.jsonl")
	res, err := f.adapter(auth.Subscription, func(o *Options) { o.FallbackModel = "gpt-6.1-sol-mini" }).ReviewCode(context.Background(), f.codeReq())
	if err != nil {
		t.Fatal(err)
	}
	if !res.FallbackUsed || len(res.Calls) != 4 {
		t.Fatalf("fallback %v calls %d", res.FallbackUsed, len(res.Calls))
	}
	var args []string
	_ = json.Unmarshal([]byte(f.read("args-3")), &args)
	if !strings.Contains(strings.Join(args, " "), "-m gpt-6.1-sol-mini") {
		t.Errorf("fallback call args: %v", args)
	}

	// Without a fallback model: ClassSchemaInvalid after 1 + 2 retries, all calls kept.
	g := newFixture(t, "code_schema_invalid.jsonl")
	_, err = g.adapter(auth.Subscription).ReviewCode(context.Background(), g.codeReq())
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Class != agent.ClassSchemaInvalid || len(ae.Calls) != 3 || ae.Class.Retryable() {
		t.Fatalf("exhausted: %v (calls %d)", err, len(ae.Calls))
	}
}

func TestInconsistentVerdict(t *testing.T) {
	f := newFixture(t, "code_inconsistent.jsonl")
	res, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Review.Verdict != "approve" || len(res.Inconsistent) < 2 {
		t.Fatalf("inconsistent: %v", res.Inconsistent)
	}
}

func TestUsageLimitWithResetTime(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	f := newFixture(t, "usage_limit.jsonl")
	_, err := f.adapter(auth.Subscription, func(o *Options) { o.Now = func() time.Time { return now } }).ReviewCode(context.Background(), f.codeReq())
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Class != agent.ClassSubscriptionLimit {
		t.Fatalf("got %v", err)
	}
	if want := now.Add(2*time.Hour + 13*time.Minute); !ae.ResetAt.Equal(want) {
		t.Errorf("reset %v, want %v", ae.ResetAt, want)
	}
	if n := f.read("counter"); n != "1" {
		t.Errorf("a usage limit must not be retried by the adapter (calls %s)", n)
	}
}

func TestLoggedOutIsAuth(t *testing.T) {
	f := newFixture(t, "logged_out.jsonl")
	_, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq())
	if c := classOf(t, err); c != agent.ClassAuth {
		t.Fatalf("class %s: %v", c, err)
	}
}

// AC-07.4: a read of ~/.ssh/id_rsa is a policy violation; the review is
// discarded, retried once, and discarded again.
func TestReadOutsideWorktreeIsPolicyViolation(t *testing.T) {
	f := newFixture(t, "read_ssh.jsonl")
	_, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq())
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Class != agent.ClassPolicy {
		t.Fatalf("got %v", err)
	}
	if len(ae.Calls) != 2 {
		t.Errorf("policy violation must be retried exactly once: %d calls", len(ae.Calls))
	}
	v := ae.Invocation.Violations
	if len(v) == 0 || v[0].Kind != "read" || !strings.Contains(v[0].Path, ".ssh/id_rsa") {
		t.Errorf("violations: %+v", v)
	}
	// Discarded: no accepted review was written.
	if _, err := f.run.Read("phases/P01/a1/reviews/r01/review.json"); err == nil {
		t.Error("a discarded review was stored as accepted")
	}

	// Discard and retry once: the second, clean review is accepted.
	g := newFixture(t, "read_ssh.jsonl", "code_valid.jsonl")
	res, err := g.adapter(auth.Subscription).ReviewCode(context.Background(), g.codeReq())
	if err != nil || len(res.Calls) != 2 || len(res.Calls[0].Violations) == 0 {
		t.Fatalf("retry after violation: %v", err)
	}
}

func TestRecordedSkillReadAndFileChangeAreViolations(t *testing.T) {
	for _, fx := range []string{"recorded_skill_read.jsonl", "file_change.jsonl"} {
		f := newFixture(t, fx)
		_, err := f.adapter(auth.Subscription, func(o *Options) { o.UserHome = "C:/Users/LocalUser" }).ReviewCode(context.Background(), f.codeReq())
		if c := classOf(t, err); c != agent.ClassPolicy {
			t.Errorf("%s: class %s", fx, c)
		}
	}
}

func TestTransientRetriedThenValid(t *testing.T) {
	f := newFixture(t, "transient.jsonl", "transient.jsonl", "code_valid.jsonl")
	var slept []time.Duration
	res, err := f.adapter(auth.Subscription, func(o *Options) {
		o.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	}).ReviewCode(context.Background(), f.codeReq())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Calls) != 3 || len(slept) != 2 {
		t.Fatalf("calls %d sleeps %v", len(res.Calls), slept)
	}
}

func TestAPIErrors(t *testing.T) {
	f := newFixture(t, "api_rate_limit.jsonl")
	_, err := f.adapter(auth.API, func(o *Options) { o.TransientRetries = 1 }).ReviewCode(context.Background(), f.codeReq())
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Class != agent.ClassAPIRateLimit || ae.RetryAfter != 20*time.Second || len(ae.Calls) != 2 {
		t.Fatalf("rate limit: %v", err)
	}
	g := newFixture(t, "api_quota.jsonl")
	_, err = g.adapter(auth.API).ReviewCode(context.Background(), g.codeReq())
	if c := classOf(t, err); c != agent.ClassAPIQuota {
		t.Fatalf("quota: %s", c)
	}
}

func TestEffortIgnoredAndModelNotFound(t *testing.T) {
	f := newFixture(t, "effort_ignored.jsonl")
	_, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq())
	if c := classOf(t, err); c != agent.ClassCrash || !strings.Contains(err.Error(), "model_reasoning_effort") {
		t.Fatalf("effort ignored: %v", err)
	}
	g := newFixture(t, "model_not_found.jsonl")
	_, err = g.adapter(auth.Subscription).ReviewCode(context.Background(), g.codeReq())
	if c := classOf(t, err); c != agent.ClassCrash || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("model not found: %v", err)
	}
}

func TestPlanReviews(t *testing.T) {
	f := newFixture(t, "plan_valid.jsonl")
	res, err := f.adapter(auth.Subscription).ReviewPlan(context.Background(), f.planReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Review.Verdict != "pass" || len(res.Inconsistent) != 0 {
		t.Fatalf("plan review: %+v", res.Review)
	}
	stdin := f.read("stdin-0")
	for _, want := range []string{"# Task: review the implementation plan", "REQ-001, REQ-002", "IMPLEMENTATION_PLAN.md", "all checks passed"} {
		if !strings.Contains(stdin, want) {
			t.Errorf("plan pack lacks %q", want)
		}
	}
	g := newFixture(t, "plan_missing.jsonl")
	res, err = g.adapter(auth.Subscription).ReviewPlan(context.Background(), g.planReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Review.RequirementCoverage[1].Status != "missing" || res.Review.Verdict != "revise" {
		t.Errorf("missing coverage: %+v", res.Review.RequirementCoverage)
	}
	// A coverage list that skips a requirement is schema-invalid for this plan.
	h := newFixture(t, "plan_valid.jsonl")
	req := h.planReq()
	req.ReqIDs = append(req.ReqIDs, "REQ-003")
	_, err = h.adapter(auth.Subscription).ReviewPlan(context.Background(), req)
	if c := classOf(t, err); c != agent.ClassSchemaInvalid {
		t.Errorf("missing REQ-003: %s", c)
	}
}

func readEnv(s string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[strings.ToUpper(k)] = v
		}
	}
	return m
}

func TestSubscriptionEnv(t *testing.T) {
	f := newFixture(t, "code_valid.jsonl")
	if _, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq()); err != nil {
		t.Fatal(err)
	}
	env := readEnv(f.read("env-0"))
	for _, v := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CODEX_HOME"} {
		if _, ok := env[v]; ok {
			t.Errorf("subscription env has %s", v)
		}
	}
}

func hashTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		s := sha256.Sum256(b)
		out[p] = hex.EncodeToString(s[:])
		return nil
	})
	return out
}

// AC-07.6: api mode logs the dedicated CODEX_HOME in with the key on
// stdin, never changes ~/.codex, and never writes the key to artifacts.
func TestAPIModeDedicatedHome(t *testing.T) {
	f := newFixture(t, "code_valid.jsonl", "code_valid.jsonl")
	before := hashTree(t, filepath.Join(f.home, ".codex"))
	a := f.adapter(auth.API, func(o *Options) {
		o.Pricing = map[string]config.Price{"gpt-6.1-sol": {InputPerMTok: 2, OutputPerMTok: 10}}
		o.Budget = agent.NewBudget(config.BudgetLimits{Run: 100, Phase: 10, SoftRatio: 0.8})
	})
	req := f.codeReq()
	req.BudgetScopes = []agent.Scope{agent.ScopeRun, agent.ScopePhase}
	res, err := a.ReviewCode(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// A second review reuses the login.
	req.Output.Prefix = "phases/P01/a1/reviews/r02"
	if _, err := a.ReviewCode(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if logins := strings.Fields(f.read("logins")); len(logins) != 1 || logins[0] != f.apiHome {
		t.Errorf("logins: %v (want one into %s)", logins, f.apiHome)
	}
	env := readEnv(f.read("env-0"))
	if env["CODEX_HOME"] != f.apiHome {
		t.Errorf("CODEX_HOME = %q", env["CODEX_HOME"])
	}
	for _, v := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_BASE_URL"} {
		if _, ok := env[v]; ok {
			t.Errorf("api env has %s", v)
		}
	}
	after := hashTree(t, filepath.Join(f.home, ".codex"))
	if len(before) != len(after) {
		t.Errorf("~/.codex files changed: %d -> %d", len(before), len(after))
	}
	for p, h := range before {
		if after[p] != h {
			t.Errorf("~/.codex file changed: %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(f.apiHome, "state_5.sqlite")); err != nil {
		t.Error("the CLI did not run with the dedicated home")
	}
	// Cost from tokens × pricing: 1200 in × 2/M + 800 out × 10/M.
	if !res.Priced || res.Usage.CostUSD < 0.0103 || res.Usage.CostUSD > 0.0105 || !res.Charged {
		t.Errorf("cost: %+v priced %v", res.Usage, res.Priced)
	}
	// The key is in no artifact.
	_ = filepath.WalkDir(f.run.Path(), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), testKey) || strings.Contains(string(b), testKey[10:30]) {
				t.Errorf("key leaked into %s", p)
			}
		}
		return nil
	})
}

func TestAPIModeRefusesUserCodexHome(t *testing.T) {
	f := newFixture(t, "code_valid.jsonl")
	_, err := f.adapter(auth.API, func(o *Options) { o.APIHome = filepath.Join(f.home, ".codex") }).ReviewCode(context.Background(), f.codeReq())
	if c := classOf(t, err); c != agent.ClassAuth {
		t.Fatalf("class %s", c)
	}
}

// §16.5: the login is checked before every review; a login of the other
// mode stops before any exec.
func TestLoginMismatchStopsBeforeExec(t *testing.T) {
	f := newFixture(t, "code_valid.jsonl")
	f.extra = []string{"status=Logged in using an API key - sk-proj-***12345"}
	_, err := f.adapter(auth.Subscription).ReviewCode(context.Background(), f.codeReq())
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Class != agent.ClassAuth {
		t.Fatalf("got %v", err)
	}
	var me *auth.CodexLoginMismatchError
	if !errors.As(err, &me) || me.Got != auth.CodexLoginAPIKey {
		t.Errorf("mismatch error: %v", err)
	}
	if _, err := os.Stat(f.path("args-0")); err == nil {
		t.Error("exec ran despite the login mismatch")
	}
	g := newFixture(t, "code_valid.jsonl")
	g.extra = []string{"status=Not logged in"}
	if _, err := g.adapter(auth.Subscription).ReviewCode(context.Background(), g.codeReq()); classOf(t, err) != agent.ClassAuth {
		t.Error("logged out must be auth")
	}
}

func TestIdleTimeoutKillsReviewer(t *testing.T) {
	f := newFixture(t, "")
	f.extra = []string{"hang=1"}
	start := time.Now()
	_, err := f.adapter(auth.Subscription, func(o *Options) { o.IdleTimeout = 2 * time.Second; o.TransientRetries = 1 }).ReviewCode(context.Background(), f.codeReq())
	if c := classOf(t, err); c != agent.ClassTimeout {
		t.Fatalf("class %s", c)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestBudgetExhaustedRefusesCall(t *testing.T) {
	f := newFixture(t, "code_valid.jsonl")
	b := agent.NewBudget(config.BudgetLimits{Phase: 1, SoftRatio: 0.8})
	b.Charge(1, agent.ScopePhase)
	req := f.codeReq()
	req.BudgetScopes = []agent.Scope{agent.ScopePhase}
	_, err := f.adapter(auth.API, func(o *Options) {
		o.Budget = b
		o.Pricing = map[string]config.Price{"gpt-6.1-sol": {InputPerMTok: 1, OutputPerMTok: 1}}
	}).ReviewCode(context.Background(), req)
	if c := classOf(t, err); c != agent.ClassBudget {
		t.Fatalf("class %s", c)
	}
}

func TestNoModelConfigured(t *testing.T) {
	f := newFixture(t, "code_valid.jsonl")
	_, err := f.adapter(auth.Subscription, func(o *Options) { o.Model = "" }).ReviewCode(context.Background(), f.codeReq())
	if !errors.Is(err, ErrNoModel) {
		t.Fatalf("got %v", err)
	}
}
