package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
)

var update = flag.Bool("update", false, "rewrite golden files")

func testCaps(t *testing.T) *Capabilities {
	t.Helper()
	v, err := os.ReadFile("testdata/claude-version-2.1.311.txt")
	if err != nil {
		t.Fatal(err)
	}
	h, err := os.ReadFile("testdata/claude-help-2.1.311.txt")
	if err != nil {
		t.Fatal(err)
	}
	return ParseCapabilities(string(v), string(h))
}

const validReport = `{"version":1,"summary":"Implemented the store.","files_changed":[{"path":"internal/store/store.go","change":"added","reason":"new store"}],"design_decisions":["in-memory map"],"tests":[{"path":"internal/store/store_test.go","change":"added","covers":["AC-01.1"]}],"acceptance_criteria":[{"id":"AC-01.1","status":"met","evidence":"TestCreate"}],"finding_responses":[{"id":"F-001","response":"fixed","detail":"added a nil check"}],"known_gaps":[]}`

func planJSON(t *testing.T, extra string) string {
	t.Helper()
	b, err := os.ReadFile("../../plan/testdata/valid/plan.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if extra != "" {
		s = strings.TrimSuffix(s, "}") + "," + extra + "}"
	}
	return s
}

type harness struct {
	t   *testing.T
	tmp string
	ws  string
	run *artifacts.RunDir
	red *secure.Redactor
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "worktree")
	if err := os.MkdirAll(filepath.Join(ws, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	red := secure.NewRedactor()
	run, err := artifacts.New(filepath.Join(tmp, "aco"), red).Run("r-test-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "aco-bin"), []byte("hook"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, tmp: tmp, ws: ws, run: run, red: red}
}

// fixture writes a stream fixture with the test-side placeholders filled.
func (h *harness) fixture(name string, subs map[string]string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "stream", name+".jsonl"))
	if err != nil {
		h.t.Fatal(err)
	}
	s := string(b)
	defaults := map[string]string{"{{SOURCE}}": "none", "{{REPORT}}": validReport}
	for k, v := range subs {
		defaults[k] = v
	}
	for k, v := range defaults {
		s = strings.ReplaceAll(s, k, v)
	}
	p := filepath.Join(h.tmp, name+"-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".jsonl")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) file(name string) string { return filepath.Join(h.tmp, name) }

func (h *harness) adapter(mutate func(*Options), fake ...string) *Adapter {
	h.t.Helper()
	bin, err := os.Executable()
	if err != nil {
		h.t.Fatal(err)
	}
	prefix := append([]string{"-test.run=^TestFakeClaude$", "--", "--fake-claude"}, fake...)
	prefix = append(prefix, "--end-fake")
	o := Options{
		Bin: bin, BinArgs: prefix, HookBin: filepath.Join(h.tmp, "aco-bin"), Caps: testCaps(h.t),
		Mode: auth.Subscription, Env: auth.NewEnv(nil, nil), BaseEnv: []string{"PATH=" + os.Getenv("PATH")},
		Redactor: h.red, MaxTurns: 200, InvocationTimeout: time.Minute, IdleTimeout: 30 * time.Second, KillGrace: 5 * time.Second,
		ExtraAllowedBash: []string{"go "},
	}
	if mutate != nil {
		mutate(&o)
	}
	return New(o)
}

func (h *harness) ws0() agent.Workspace {
	return agent.Workspace{Dir: h.ws, ProtectedPaths: config.Default().Policy.ProtectedPaths, ScopePaths: []string{"internal/**"},
		ScopeEnforcement: "flag", Network: "allowlist"}
}

func (h *harness) implReq(prefix string) agent.ImplementRequest {
	return agent.ImplementRequest{RunID: "r-test-1", Kind: agent.KindPhase, Phase: 1, PhaseTitle: "Store", Attempt: 1,
		Workspace: h.ws0(), Output: agent.Output{Run: h.run, Prefix: prefix}, PhaseSpec: "## Phase 1: Store",
		ACIDs: []string{"AC-01.1"}, BudgetScopes: []agent.Scope{agent.ScopeRun, agent.ScopePhase}}
}

func readArgs(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var a []string
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func containsSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func TestParseCapabilities(t *testing.T) {
	c := testCaps(t)
	if c.Version != "2.1.311" {
		t.Errorf("version %q", c.Version)
	}
	if m := c.Missing(); len(m) > 0 {
		t.Errorf("2.1.311 lacks %v", m)
	}
	for _, f := range []string{"effort", "json-schema", "max-turns", "max-budget-usd", "permission-prompts"} {
		if !c.Has(f) {
			t.Errorf("missing %s", f)
		}
	}
	if c.EffortVia() != EffortFlag {
		t.Error("effort should use the flag")
	}
	old := ParseCapabilities("1.0.0 (Claude Code)", "--print --model")
	if old.Has("max-turns") || len(old.Missing()) == 0 || old.EffortVia() != EffortSettings {
		t.Errorf("old CLI parsed wrong: %+v", old)
	}
}

func goldenArgs(t *testing.T, name string, got []string) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden.json")
	b, _ := json.MarshalIndent(got, "", "  ")
	b = append(b, '\n')
	if *update {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(want) != string(b) {
		t.Errorf("%s: args differ from golden file:\n%s", name, b)
	}
}

// AC-05.2: golden command lines of both profiles.
func TestBuildArgsGolden(t *testing.T) {
	caps := testCaps(t)
	base := CommandSpec{SessionID: "00000000-0000-4000-8000-000000000001", SettingsPath: "/aco/runs/r1/inv/settings.json",
		Schema: []byte(`{ "type": "object" }`), SystemPrompt: "RULES", MaxTurns: 200, Network: "allowlist"}

	impl := base
	impl.Profile, impl.Model, impl.Effort, impl.AllowedBash = ProfileImplementer, "claude-opus-5-5", "medium", []string{"go ", "make "}
	ia, err := BuildArgs(caps, impl)
	if err != nil {
		t.Fatal(err)
	}
	goldenArgs(t, "args-implementer", ia)
	if !containsSeq(ia, "--model", "claude-opus-5-5", "--effort", "medium") {
		t.Error("implementer: want --model claude-opus-5-5 --effort medium")
	}
	if !containsSeq(ia, "--json-schema", `{"type":"object"}`) || !containsSeq(ia, "--max-turns", "200") {
		t.Error("implementer: schema or max turns missing")
	}

	pl := base
	pl.Profile, pl.Model, pl.Effort = ProfilePlanner, "claude-opus-5-5", "high"
	pa, err := BuildArgs(caps, pl)
	if err != nil {
		t.Fatal(err)
	}
	goldenArgs(t, "args-planner", pa)
	if !containsSeq(pa, "--effort", "high") || !containsSeq(pa, "--tools", "Read,Glob,Grep") || !containsSeq(pa, "--allowedTools", "Read,Glob,Grep") {
		t.Error("planner: want --effort high and only read tools")
	}
	dis := argAfter(pa, "--disallowedTools")
	for _, w := range []string{"Write", "Edit", "Bash", "NotebookEdit"} {
		if !strings.Contains(dis, w) {
			t.Errorf("planner: %s not disallowed", w)
		}
		if strings.Contains(argAfter(pa, "--tools"), w) || strings.Contains(argAfter(pa, "--allowedTools"), w) {
			t.Errorf("planner: write tool %s available", w)
		}
	}
	if argAfter(pa, "--permission-mode") != "dontAsk" || argAfter(ia, "--permission-mode") != "acceptEdits" {
		t.Error("permission modes")
	}

	res := impl
	res.Resume = true
	ra, _ := BuildArgs(caps, res)
	if !containsSeq(ra, "--resume", impl.SessionID) || slices.Contains(ra, "--session-id") {
		t.Error("resume must use --resume and no --session-id")
	}
	if _, err := BuildArgs(ParseCapabilities("2.0.0", "--print"), impl); err == nil {
		t.Error("missing flags accepted")
	}
}

func TestEffortViaSettingsAndStrictEffort(t *testing.T) {
	h := newHarness(t)
	noEffort := ParseCapabilities("2.1.0", strings.ReplaceAll(mustRead(t, "testdata/claude-help-2.1.311.txt"), "--effort", "--xffort"))
	args := h.file("args.json")
	a := h.adapter(func(o *Options) { o.Caps = noEffort; o.StrictEffort = false; o.ImplementerEffort = "medium" },
		"fixture="+h.fixture("success_impl", nil), "dumpargs="+args)
	if _, err := a.Implementer().Start(context.Background(), h.implReq("p1/a")); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(readArgs(t, args), "--effort") {
		t.Error("--effort passed to a CLI without it")
	}
	s, _ := h.run.Read("p1/a/settings.json")
	if !strings.Contains(string(s), `"effortLevel": "medium"`) {
		t.Errorf("effort not in settings:\n%s", s)
	}
	strict := h.adapter(func(o *Options) { o.Caps = noEffort; o.StrictEffort = true }, "fixture="+h.fixture("success_impl", nil))
	if _, err := strict.Implementer().Start(context.Background(), h.implReq("p1/b")); err == nil || !strings.Contains(err.Error(), "effort") {
		t.Errorf("strict_effort: want error, got %v", err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// AC-05.1: recorded fixtures map to the right result and error class.
func TestStreamFixtures(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		fixture string
		mode    auth.Mode
		exit    string
		class   agent.ErrorClass // "" = success
		check   func(t *testing.T, e *agent.Error)
	}{
		{fixture: "success_impl", mode: auth.Subscription},
		{fixture: "max_turns", mode: auth.Subscription, class: agent.ClassMaxTurns},
		{fixture: "error_overloaded", mode: auth.Subscription, exit: "1", class: agent.ClassTransient},
		{fixture: "usage_limit", mode: auth.Subscription, exit: "1", class: agent.ClassSubscriptionLimit, check: func(t *testing.T, e *agent.Error) {
			if e.ResetAt.Unix() != 1794197400 {
				t.Errorf("reset time from the rate-limit event: %v", e.ResetAt)
			}
		}},
		{fixture: "usage_limit_legacy", mode: auth.Subscription, exit: "1", class: agent.ClassSubscriptionLimit, check: func(t *testing.T, e *agent.Error) {
			if e.ResetAt.Unix() != 1794197400 {
				t.Errorf("legacy reset time: %v", e.ResetAt)
			}
		}},
		{fixture: "auth_invalid", mode: auth.API, exit: "1", class: agent.ClassAuth},
		{fixture: "quota", mode: auth.API, exit: "1", class: agent.ClassAPIQuota},
		{fixture: "rate_limit_api", mode: auth.API, exit: "1", class: agent.ClassAPIRateLimit, check: func(t *testing.T, e *agent.Error) {
			if e.RetryAfter != 30*time.Second {
				t.Errorf("retry after: %v", e.RetryAfter)
			}
		}},
		{fixture: "no_result", mode: auth.Subscription, exit: "1", class: agent.ClassCrash},
		{fixture: "no_result", mode: auth.Subscription, class: agent.ClassCrash},
	}
	for _, tc := range cases {
		t.Run(tc.fixture+"/"+string(tc.mode)+"/"+tc.exit, func(t *testing.T) {
			h := newHarness(t)
			src := "none"
			if tc.mode == auth.API {
				src = "ANTHROPIC_API_KEY"
			}
			fake := []string{"fixture=" + h.fixture(tc.fixture, map[string]string{"{{SOURCE}}": src})}
			if tc.exit != "" {
				fake = append(fake, "exit="+tc.exit)
			}
			a := h.adapter(func(o *Options) {
				o.Mode = tc.mode
				o.Now = func() time.Time { return now }
				if tc.mode == auth.API {
					o.Env = auth.NewEnv(func(k string) (string, bool) {
						return "sk-ant-api03-testkey-0000", k == auth.EnvAnthropicKey
					}, nil)
				}
			}, fake...)
			res, err := a.Implementer().Start(context.Background(), h.implReq("inv/x"))
			if tc.class == "" {
				if err != nil {
					t.Fatalf("want success, got %v", err)
				}
				if res.Report == nil || res.SessionID == "" || res.Usage.Turns != 3 || res.Usage.CostUSD != 0.4213 || res.Usage.OutputTokens != 3400 {
					t.Errorf("result not parsed: %+v", res.Invocation)
				}
				if res.AuthSource != "none" || res.CLIVersion != "2.1.311" || res.Model != "claude-opus-5-5" {
					t.Errorf("init not parsed: %+v", res.Invocation)
				}
				return
			}
			var ae *agent.Error
			if !errors.As(err, &ae) || ae.Class != tc.class {
				t.Fatalf("want class %s, got %v", tc.class, err)
			}
			if tc.check != nil {
				tc.check(t, ae)
			}
			if ae.Invocation.TranscriptRef == "" {
				t.Error("transcript ref missing on error")
			}
		})
	}
}

// AC-05.3: subscription mode hides the parent's API key from Claude, and an
// init message reporting API-key auth stops the adapter with class auth.
func TestSubscriptionMode(t *testing.T) {
	h := newHarness(t)
	parent := []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=" + securetest.FakeAnthropic,
		"OPENAI_API_KEY=" + securetest.FakeOpenAI, "ACO_API_TOKEN=aco-token-abcdefgh1234", "ANTHROPIC_BASE_URL=https://x.example"}
	envDump := h.file("env.txt")
	a := h.adapter(func(o *Options) {
		o.BaseEnv = parent
		o.Env = auth.NewEnv(func(k string) (string, bool) {
			for _, kv := range parent {
				if n, v, _ := strings.Cut(kv, "="); n == k {
					return v, true
				}
			}
			return "", false
		}, nil)
	}, "fixture="+h.fixture("success_impl", nil), "dumpenv="+envDump)
	if _, err := a.Implementer().Start(context.Background(), h.implReq("sub/ok")); err != nil {
		t.Fatal(err)
	}
	env := mustRead(t, envDump)
	for _, bad := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ACO_API_TOKEN", "ANTHROPIC_BASE_URL", securetest.FakeAnthropic, securetest.FakeOpenAI} {
		if strings.Contains(env, bad) {
			t.Errorf("child environment contains %s", bad)
		}
	}

	bad := h.adapter(func(o *Options) { o.BaseEnv = parent },
		"fixture="+h.fixture("success_impl", map[string]string{"{{SOURCE}}": "ANTHROPIC_API_KEY"}), "hang=1")
	start := time.Now()
	_, err := bad.Implementer().Start(context.Background(), h.implReq("sub/bad"))
	if agent.ClassOf(err) != agent.ClassAuth {
		t.Fatalf("want auth, got %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("auth mismatch should stop the CLI at once, took %v", time.Since(start))
	}
}

// AC-05.4: api mode passes ANTHROPIC_API_KEY and no OpenAI variables,
// rejects subscription auth, and charges total_cost_usd to the budget.
func TestAPIMode(t *testing.T) {
	h := newHarness(t)
	ef := auth.NewEnvFile(h.file(".env"), map[string]string{auth.EnvAnthropicKey: securetest.FakeAnthropic})
	h.red.AddSecret(ef.AllSecrets()...)
	parent := []string{"PATH=" + os.Getenv("PATH"), "OPENAI_API_KEY=" + securetest.FakeOpenAI, "CODEX_API_KEY=codex-abcdefgh1234"}
	budget := agent.NewBudget(config.BudgetLimits{Run: 1.0, Phase: 0.5, SoftRatio: 0.8})
	envDump, args := h.file("env.txt"), h.file("args.json")
	a := h.adapter(func(o *Options) {
		o.Mode, o.BaseEnv, o.Env, o.Budget = auth.API, parent, auth.NewEnv(nil, ef), budget
	}, "fixture="+h.fixture("success_impl", map[string]string{"{{SOURCE}}": "ANTHROPIC_API_KEY"}), "dumpenv="+envDump, "dumpargs="+args)
	res, err := a.Implementer().Start(context.Background(), h.implReq("api/ok"))
	if err != nil {
		t.Fatal(err)
	}
	env := mustRead(t, envDump)
	if !strings.Contains(env, "ANTHROPIC_API_KEY="+securetest.FakeAnthropic) {
		t.Error("api key not passed")
	}
	for _, bad := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", securetest.FakeOpenAI} {
		if strings.Contains(env, bad) {
			t.Errorf("child environment contains %s", bad)
		}
	}
	if !res.Charged || res.Usage.CostUSD != 0.4213 || budget.Spent(agent.ScopePhase) != 0.4213 || budget.Spent(agent.ScopeRun) != 0.4213 {
		t.Errorf("cost not charged: charged=%v cost=%v phase=%v", res.Charged, res.Usage.CostUSD, budget.Spent(agent.ScopePhase))
	}
	if got := argAfter(readArgs(t, args), "--max-budget-usd"); got != "0.50" {
		t.Errorf("--max-budget-usd = %q, want the remaining phase budget 0.50", got)
	}
	// A second call crosses the phase budget; a third is refused before starting.
	if res2, err := a.Implementer().Start(context.Background(), h.implReq("api/ok2")); err != nil || !slices.Contains(res2.Budget.Exceeded, agent.ScopePhase) {
		t.Fatalf("second call: err=%v budget=%+v", err, res2.Budget)
	}
	if _, err := a.Implementer().Start(context.Background(), h.implReq("api/refused")); agent.ClassOf(err) != agent.ClassBudget {
		t.Errorf("want budget_exceeded before the call, got %v", err)
	}
	// The key never reaches artifacts.
	for _, f := range []string{"api/ok/transcript.jsonl", "api/ok/prompt.md", "api/ok/settings.json"} {
		if b, _ := h.run.Read(f); strings.Contains(string(b), securetest.FakeAnthropic) {
			t.Errorf("%s contains the key", f)
		}
	}

	sub := h.adapter(func(o *Options) { o.Mode, o.Env = auth.API, auth.NewEnv(nil, ef) },
		"fixture="+h.fixture("success_impl", map[string]string{"{{SOURCE}}": "none"}))
	if _, err := sub.Implementer().Start(context.Background(), h.implReq("api/sub")); agent.ClassOf(err) != agent.ClassAuth {
		t.Errorf("subscription auth in api mode: want auth, got %v", err)
	}
	nokey := h.adapter(func(o *Options) { o.Mode = auth.API }, "fixture="+h.fixture("success_impl", nil))
	if _, err := nokey.Implementer().Start(context.Background(), h.implReq("api/nokey")); agent.ClassOf(err) != agent.ClassAuth {
		t.Errorf("api mode without key: want auth, got %v", err)
	}
}

// AC-05.6: cancelling terminates Claude and its children within 5 s.
func TestCancelKillsProcessTree(t *testing.T) {
	h := newHarness(t)
	pidFile := h.file("grandchild.pid")
	a := h.adapter(nil, "fixture="+h.fixture("init_only", nil), "spawn="+pidFile, "hang=1")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := a.Implementer().Start(ctx, h.implReq("cancel/x"))
		errc <- err
	}()
	var gc int
	deadline := time.Now().Add(20 * time.Second)
	for gc == 0 && time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			gc, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gc == 0 {
		t.Fatal("fake CLI did not start its child")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-errc:
		if agent.ClassOf(err) != agent.ClassCancelled {
			t.Errorf("want cancelled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return within 5 s of cancellation")
	}
	for processAlive(gc) {
		if time.Since(start) > 5*time.Second {
			t.Fatalf("grandchild %d still alive %v after cancel", gc, time.Since(start))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIdleTimeoutAndTurnLimit(t *testing.T) {
	h := newHarness(t)
	a := h.adapter(func(o *Options) { o.IdleTimeout = 300 * time.Millisecond }, "fixture="+h.fixture("init_only", nil), "hang=1")
	_, err := a.Implementer().Start(context.Background(), h.implReq("idle/x"))
	if agent.ClassOf(err) != agent.ClassTimeout {
		t.Fatalf("idle: want timeout, got %v", err)
	}
	noMax := testCaps(t)
	delete(noMax.Flags, "max-turns")
	b := h.adapter(func(o *Options) { o.Caps = noMax; o.MaxTurns = 2 }, "fixture="+h.fixture("many_turns", nil), "hang=1")
	_, err = b.Implementer().Start(context.Background(), h.implReq("turns/x"))
	if agent.ClassOf(err) != agent.ClassMaxTurns {
		t.Fatalf("turns: want max_turns, got %v", err)
	}
	c := h.adapter(func(o *Options) { o.InvocationTimeout = 400 * time.Millisecond }, "fixture="+h.fixture("init_only", nil), "hang=1")
	if _, err := c.Implementer().Start(context.Background(), h.implReq("total/x")); agent.ClassOf(err) != agent.ClassTimeout {
		t.Fatalf("total: want timeout, got %v", err)
	}
}

func TestPlannerCreateAndRevise(t *testing.T) {
	h := newHarness(t)
	args, stdin := h.file("args.json"), h.file("stdin.txt")
	a := h.adapter(nil, "fixture="+h.fixture("success_plan", map[string]string{"{{PLAN}}": planJSON(t, "")}), "dumpargs="+args, "dumpstdin="+stdin)
	req := agent.PlanRequest{RunID: "r-test-1", Workspace: h.ws0(), Output: agent.Output{Run: h.run, Prefix: "planning/r1"},
		Requirements: "Build a todo service. Token: " + securetest.FakeAnthropic, RepoOverview: "go.mod"}
	h.red.AddSecret(secure.NewSecret(securetest.FakeAnthropic))
	res, err := a.Planner().Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Package == nil || len(res.Package.Phases) != 3 || len(res.Package.Requirements) != 4 {
		t.Fatalf("package not parsed: %+v", res.Package)
	}
	for _, d := range res.Package.Diagnostics {
		t.Errorf("unexpected diagnostic %s: %s", d.Code, d.Message)
	}
	got := readArgs(t, args)
	if !containsSeq(got, "--effort", "high") || argAfter(got, "--tools") != "Read,Glob,Grep" || argAfter(got, "--session-id") != res.SessionID {
		t.Errorf("planner args: %v", got)
	}
	if s := mustRead(t, stdin); strings.Contains(s, securetest.FakeAnthropic) || !strings.Contains(s, "Build a todo service") {
		t.Errorf("prompt on stdin not masked or missing:\n%s", s)
	}

	rev := h.adapter(nil, "fixture="+h.fixture("success_plan", map[string]string{"{{PLAN}}": planJSON(t,
		`"finding_responses":[{"id":"PF-001","response":"fixed","detail":"split phase 2"}]`)}), "dumpargs="+args)
	rr, err := rev.Planner().Revise(context.Background(), agent.PlanRevision{RunID: "r-test-1", SessionID: res.SessionID,
		Workspace: h.ws0(), Output: agent.Output{Run: h.run, Prefix: "planning/r2"}, Round: 2,
		Findings: []agent.PlanFinding{{ID: "PF-001", Severity: "major", Title: "phase 2 too big"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rr.Responses) != 1 || rr.Responses[0].Response != "fixed" || len(rr.Package.Diagnostics) != 0 {
		t.Errorf("revision: %+v %v", rr.Responses, rr.Package.Diagnostics)
	}
	if ra := readArgs(t, args); argAfter(ra, "--resume") != res.SessionID || !strings.Contains(argAfter(ra, "--json-schema"), "finding_responses") {
		t.Error("revise must resume the planning session with the revision schema")
	}
	_, err = rev.Planner().Revise(context.Background(), agent.PlanRevision{RunID: "r-test-1", SessionID: res.SessionID,
		Workspace: h.ws0(), Output: agent.Output{Run: h.run, Prefix: "planning/r3"},
		Findings: []agent.PlanFinding{{ID: "PF-001"}, {ID: "PF-002"}}})
	if agent.ClassOf(err) != agent.ClassIncomplete || !strings.Contains(err.Error(), "PF-002") {
		t.Errorf("unanswered finding: want incomplete naming PF-002, got %v", err)
	}

	writer := h.adapter(nil, "fixture="+h.fixture("planner_write_tool", nil), "hang=1")
	if _, err := writer.Planner().Create(context.Background(), req); agent.ClassOf(err) != agent.ClassCrash || !strings.Contains(err.Error(), "write tool") {
		t.Errorf("planner with a write tool: got %v", err)
	}
	broken := h.adapter(nil, "fixture="+h.fixture("success_plan", map[string]string{"{{PLAN}}": `{"version":1,"title":"x","unknown":1}`}))
	if _, err := broken.Planner().Create(context.Background(), req); agent.ClassOf(err) != agent.ClassIncomplete {
		t.Errorf("schema-invalid package: want incomplete, got %v", err)
	}
}

func TestImplementerContinue(t *testing.T) {
	h := newHarness(t)
	args := h.file("args.json")
	a := h.adapter(nil, "fixture="+h.fixture("success_impl", nil), "dumpargs="+args, "deny=protected_path")
	res, err := a.Implementer().Start(context.Background(), h.implReq("impl/s"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GuardDenials) != 1 || res.GuardDenials[0].Rule != "protected_path" {
		t.Errorf("guard denials not collected: %+v", res.GuardDenials)
	}
	creq := agent.ContinueRequest{RunID: "r-test-1", SessionID: res.SessionID, Kind: agent.ContinueReviewFix, Phase: 1,
		Workspace: h.ws0(), Output: agent.Output{Run: h.run, Prefix: "impl/c"}, ACIDs: []string{"AC-01.1"},
		Findings: []agent.CodeFinding{{ID: "F-001", Severity: "major", Title: "nil map"}}}
	cres, err := a.Implementer().Continue(context.Background(), creq)
	if err != nil {
		t.Fatal(err)
	}
	if cres.Report.FindingResponses[0].ID != "F-001" || argAfter(readArgs(t, args), "--resume") != res.SessionID {
		t.Error("continue did not resume the session")
	}
	creq.Findings = append(creq.Findings, agent.CodeFinding{ID: "F-002"})
	creq.Output.Prefix = "impl/c2"
	cres, err = a.Implementer().Continue(context.Background(), creq)
	if agent.ClassOf(err) != agent.ClassIncomplete || cres == nil || cres.Report == nil {
		t.Errorf("unanswered finding: want incomplete with the report, got %v", err)
	}
	bad := h.adapter(nil, "fixture="+h.fixture("success_impl", map[string]string{"{{REPORT}}": `{"version":1}`}))
	if _, err := bad.Implementer().Start(context.Background(), h.implReq("impl/bad")); agent.ClassOf(err) != agent.ClassIncomplete {
		t.Errorf("invalid report: want incomplete, got %v", err)
	}
}

func TestSettingsAndHookCommand(t *testing.T) {
	cmd := HookCommand(`C:\Program Files\aco\aco.exe`, "r-1", `C:\Users\a b\.aco\runs\r-1\guard.json`)
	want := `"C:/Program Files/aco/aco.exe" hook pre-tool-use --run "r-1" --policy "C:/Users/a b/.aco/runs/r-1/guard.json"`
	if cmd != want {
		t.Errorf("hook command:\n got %s\nwant %s", cmd, want)
	}
	if c := HookCommand("/x/$(rm)/aco", "r", "/p`x`"); !strings.Contains(c, `\$(rm)`) || !strings.Contains(c, "\\`x\\`") {
		t.Errorf("shell metacharacters not escaped: %s", c)
	}
	b, err := Settings(cmd, []string{"WebFetch"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var s settingsFile
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Hooks["PreToolUse"][0].Matcher != "*" || s.Hooks["PreToolUse"][0].Hooks[0].Command != cmd || s.EffortLevel != "" || s.Permissions.Deny[0] != "WebFetch" {
		t.Errorf("settings: %s", b)
	}
}

// Tools ran but no guard hook responded (settings ignored), or the hook
// exited with neither allow nor block: both stop the invocation.
func TestGuardHookMustRun(t *testing.T) {
	h := newHarness(t)
	for _, fx := range []string{"hook_missing", "hook_failed"} {
		a := h.adapter(nil, "fixture="+h.fixture(fx, nil), "hang=1")
		_, err := a.Implementer().Start(context.Background(), h.implReq("guard/"+fx))
		if agent.ClassOf(err) != agent.ClassCrash || !strings.Contains(err.Error(), "guard hook") {
			t.Errorf("%s: want crash about the guard hook, got %v", fx, err)
		}
	}
}

func TestMissingHookBinaryRefuses(t *testing.T) {
	h := newHarness(t)
	a := h.adapter(func(o *Options) { o.HookBin = h.file("no-such-aco") }, "fixture="+h.fixture("success_impl", nil))
	if _, err := a.Implementer().Start(context.Background(), h.implReq("nohook/x")); err == nil || !strings.Contains(err.Error(), "guard hook") {
		t.Fatalf("want refusal without the hook binary, got %v", err)
	}
}

func TestDetectWithFakeCLI(t *testing.T) {
	bin, _ := os.Executable()
	v, _ := filepath.Abs("testdata/claude-version-2.1.311.txt")
	hf, _ := filepath.Abs("testdata/claude-help-2.1.311.txt")
	c, err := Detect(context.Background(), bin, []string{"-test.run=^TestFakeClaude$", "--", "--fake-claude", "version=" + v, "help=" + hf, "--end-fake"},
		[]string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != "2.1.311" || len(c.Missing()) != 0 {
		t.Errorf("detected %+v", c)
	}
}
