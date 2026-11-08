package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoad(t *testing.T, o LoadOptions) *Loaded {
	t.Helper()
	l, err := Load(o)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return l
}

func loadErrs(t *testing.T, o LoadOptions) Errors {
	t.Helper()
	_, err := Load(o)
	if err == nil {
		t.Fatal("expected error")
	}
	es, ok := err.(Errors)
	if !ok {
		t.Fatalf("error type %T", err)
	}
	return es
}

func TestDefaults(t *testing.T) {
	c := Default()
	if errs := Validate(c); len(errs) != 0 {
		t.Fatalf("defaults invalid: %v", errs)
	}
	checks := map[string][2]string{
		"implementer.model":  {c.Implementer.Model, "claude-opus-5-5"},
		"implementer.effort": {c.Implementer.Effort, "medium"},
		"planner.model":      {c.Planner.Model, "claude-opus-5-5"},
		"planner.effort":     {c.Planner.Effort, "high"},
		"reviewer.kind":      {c.Reviewer.Kind, "codex-cli"},
		"auth.mode":          {c.Auth.Mode, "subscription"},
		"auth.fallback":      {c.Auth.Fallback, "none"},
		"language.operator":  {c.Language.Operator, "zh-CN"},
		"language.documents": {c.Language.Documents, "en"},
	}
	for f, v := range checks {
		if v[0] != v[1] {
			t.Errorf("%s = %q, want %q", f, v[0], v[1])
		}
	}
	if c.Auth.Claude != "" || c.Auth.Codex != "" {
		t.Errorf("per-agent auth overrides should be empty by default")
	}
	if c.Limits.InvocationTimeout.D() != time.Hour || c.Limits.RunTimeout.D() != 72*time.Hour {
		t.Errorf("duration defaults wrong: %v %v", c.Limits.InvocationTimeout, c.Limits.RunTimeout)
	}
	if c.Limits.AgentCalls.Run != 600 || c.Limits.BudgetUSD.Run != 250 || c.Limits.MaxConcurrentRuns != 1 {
		t.Errorf("limit defaults wrong: %+v", c.Limits)
	}
	// Loading nothing yields the defaults.
	l := mustLoad(t, LoadOptions{})
	if !reflect.DeepEqual(l.Config, Default()) {
		t.Error("empty Load differs from Default()")
	}
	if _, ok := l.Origin("auth.mode"); ok {
		t.Error("auth.mode should report built-in default origin")
	}
}

func TestLayerPrecedence(t *testing.T) {
	dir := t.TempDir()
	user := writeFile(t, dir, "user.yaml", `
version: 1
planner: {effort: low}
implementer: {effort: low}
reviewer: {reasoning_effort: low}
limits: {review_rounds: 2}
gates:
  unit: {commands: ["go test ./..."]}
`)
	repoDir := filepath.Join(dir, "repo")
	if err := os.Mkdir(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repoDir, RepoConfigName, `
implementer: {effort: high}
reviewer: {reasoning_effort: medium}
limits: {review_rounds: 3}
`)
	extra := writeFile(t, dir, "extra.yaml", `
reviewer: {reasoning_effort: xhigh}
limits: {review_rounds: 4}
`)
	front := []byte("limits:\n  review_rounds: 5\nimplementer:\n  effort: max\n")

	l := mustLoad(t, LoadOptions{
		UserConfigPath:  user,
		RepoDir:         repoDir,
		ConfigFiles:     []string{extra},
		PlanFrontMatter: front,
		PlanSource:      "plan.md",
		Overrides:       []string{"limits.review_rounds=6", "language.operator=en"},
	})
	c := l.Config
	if c.Planner.Effort != "low" { // only user layer
		t.Errorf("planner.effort = %q (user layer)", c.Planner.Effort)
	}
	if c.Implementer.Effort != "max" { // user < repo < plan
		t.Errorf("implementer.effort = %q, want plan layer max", c.Implementer.Effort)
	}
	if c.Reviewer.ReasoningEffort != "xhigh" { // user < repo < file
		t.Errorf("reviewer.reasoning_effort = %q, want file layer xhigh", c.Reviewer.ReasoningEffort)
	}
	if c.Limits.ReviewRounds != 6 { // flags win
		t.Errorf("limits.review_rounds = %d, want 6 from --set", c.Limits.ReviewRounds)
	}
	if got := c.Gates.Unit.Commands; len(got) != 1 || got[0] != "go test ./..." {
		t.Errorf("gates.unit.commands = %v", got)
	}
	if c.Language.Operator != "en" {
		t.Errorf("language.operator = %q", c.Language.Operator)
	}
	wantLayers := []string{LayerDefaults, LayerUser, LayerRepo, LayerFile, LayerPlan, LayerFlags, LayerFlags}
	var gotLayers []string
	for _, li := range l.Layers {
		gotLayers = append(gotLayers, li.Name)
	}
	if !reflect.DeepEqual(gotLayers, wantLayers) {
		t.Errorf("layers = %v, want %v", gotLayers, wantLayers)
	}
	if o, ok := l.Origin("implementer.effort"); !ok || o.Name != LayerPlan {
		t.Errorf("origin of implementer.effort = %+v %v", o, ok)
	}
	if o, ok := l.Origin("planner.effort"); !ok || o.Name != LayerUser || o.Source != user {
		t.Errorf("origin of planner.effort = %+v %v", o, ok)
	}
}

// Each layer kind overrides the one below it on the same field.
func TestEachLayerOverridesPrevious(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	_ = os.Mkdir(repoDir, 0o700)
	user := writeFile(t, dir, "user.yaml", "planner: {effort: low}\n")
	steps := []struct {
		name string
		opts func(o *LoadOptions)
		want string
	}{
		{"defaults", func(o *LoadOptions) {}, "high"},
		{"user", func(o *LoadOptions) { o.UserConfigPath = user }, "low"},
		{"repo", func(o *LoadOptions) {
			writeFile(t, repoDir, RepoConfigName, "planner: {effort: medium}\n")
			o.RepoDir = repoDir
		}, "medium"},
		{"file", func(o *LoadOptions) {
			o.ConfigFiles = []string{writeFile(t, dir, "f.yaml", "planner: {effort: max}\n")}
		}, "max"},
		{"plan", func(o *LoadOptions) { o.PlanFrontMatter = []byte("planner: {effort: low}\n") }, "low"},
		{"flags", func(o *LoadOptions) { o.Overrides = []string{"planner.effort=high"} }, "high"},
	}
	var o LoadOptions
	for _, s := range steps {
		s.opts(&o)
		if got := mustLoad(t, o).Config.Planner.Effort; got != s.want {
			t.Errorf("after %s layer: planner.effort = %q, want %q", s.name, got, s.want)
		}
	}
}

func TestInvalidValuesNameTheField(t *testing.T) {
	cases := []struct {
		yaml  string
		field string
		code  ErrCode
	}{
		{"planner: {effort: extreme}", "planner.effort", ErrInvalidValue},
		{"implementer: {effort: ultra}", "implementer.effort", ErrInvalidValue},
		{"auth: {mode: free}", "auth.mode", ErrInvalidValue},
		{"auth: {claude: apikey}", "auth.claude", ErrInvalidValue},
		{"auth: {codex: chatgpt}", "auth.codex", ErrInvalidValue},
		{"auth: {fallback: subscription}", "auth.fallback", ErrInvalidValue},
		{"language: {operator: fr}", "language.operator", ErrInvalidValue},
		{"policy: {scope_enforcement: maybe}", "policy.scope_enforcement", ErrInvalidValue},
		{"sandbox: {mode: vm}", "sandbox.mode", ErrInvalidValue},
		{"gates: {security: {fail_severity: severe}}", "gates.security.fail_severity", ErrInvalidValue},
		{"gates: {security: {scanners: [secrets, bogus]}}", "gates.security.scanners[1]", ErrInvalidValue},
		{"limits: {review_rounds: 0}", "limits.review_rounds", ErrOutOfRange},
		{"limits: {agent_calls: {soft_ratio: 1.5}}", "limits.agent_calls.soft_ratio", ErrOutOfRange},
		{"limits: {review_rounds: many}", "limits.review_rounds", ErrInvalidType},
		{"git: {push: yes please}", "git.push", ErrInvalidType},
		{"limits: {phase_timeout: soon}", "limits.phase_timeout", ErrInvalidDuration},
		{"planner: {model: ''}", "planner.model", ErrRequired},
		{"version: 2", "version", ErrUnsupportedVersion},
		{"planner: {colour: red}", "planner.colour", ErrUnknownField},
		{"surprise: true", "surprise", ErrUnknownField},
		{"gates: {e2e: {commands: [x]}}", "gates.e2e", ErrUnknownGateType},
		{"pricing: {codex: {m1: {input_per_mtok: -1}}}", "pricing.codex.m1.input_per_mtok", ErrOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, RepoConfigName, tc.yaml+"\n")
			es := loadErrs(t, LoadOptions{RepoDir: dir})
			if !es.Has(tc.field, tc.code) {
				t.Fatalf("want %s/%s, got %v", tc.field, tc.code, es)
			}
			for _, e := range es {
				if e.Field == tc.field {
					if e.Layer != LayerRepo || !strings.HasSuffix(e.Source, RepoConfigName) || e.Line != 1 {
						t.Errorf("error not attributed to repo file line 1: %+v", e)
					}
				}
			}
		})
	}
}

func TestAllErrorsInALayerAreReported(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, RepoConfigName, "api_key: x\nplanner: {effort: extreme}\n")
	es := loadErrs(t, LoadOptions{RepoDir: dir})
	if !es.Has("api_key", ErrSecretField) || !es.Has("planner.effort", ErrInvalidValue) {
		t.Fatalf("got %v", es)
	}
}

func TestSecretFieldsRejected(t *testing.T) {
	for _, y := range []string{
		"api_key: sk-ant-abcdefghijklmnop",
		"reviewer: {api_key: x}",
		"auth: {anthropic_api_key: x}",
		"auth: {openai_api_key: x}",
		"auth: {token: x}",
		"implementer: {apiKey: x}",
	} {
		dir := t.TempDir()
		writeFile(t, dir, RepoConfigName, y+"\n")
		es := loadErrs(t, LoadOptions{RepoDir: dir})
		found := false
		for _, e := range es {
			if e.Code == ErrSecretField {
				found = true
				if e.Value != "" {
					t.Errorf("secret error must not carry the value: %+v", e)
				}
			}
		}
		if !found {
			t.Errorf("%q: want secret_field error, got %v", y, es)
		}
	}
}

// The schema itself must never grow a credential field.
func TestSchemaHasNoSecretFields(t *testing.T) {
	var walk func(t2 reflect.Type, path string)
	walk = func(t2 reflect.Type, path string) {
		switch t2.Kind() {
		case reflect.Struct:
			if t2 == durationType {
				return
			}
			for name, ft := range yamlFields(t2) {
				p := join(path, name)
				if secretKeyRE.MatchString(name) {
					t.Errorf("config field %s looks like a secret", p)
				}
				walk(ft, p)
			}
		case reflect.Map, reflect.Slice:
			walk(t2.Elem(), path)
		}
	}
	walk(reflect.TypeOf(Config{}), "")
}

func TestAllEffortsAccepted(t *testing.T) {
	for _, e := range Efforts {
		l := mustLoad(t, LoadOptions{Overrides: []string{"implementer.effort=" + e, "planner.effort=" + e}})
		if l.Config.Implementer.Effort != e || l.Config.Planner.Effort != e {
			t.Errorf("effort %s not applied", e)
		}
	}
}

func TestOverrides(t *testing.T) {
	l := mustLoad(t, LoadOptions{Overrides: []string{
		"git.push=true",
		"limits.idle_timeout=5m",
		"gates.lint.commands=[golangci-lint run, go vet ./...]",
		"pricing.codex.gpt-x.input_per_mtok=1.25",
	}})
	c := l.Config
	if !c.Git.Push || c.Limits.IdleTimeout.D() != 5*time.Minute || len(c.Gates.Lint.Commands) != 2 {
		t.Errorf("overrides not applied: %+v %+v %+v", c.Git, c.Limits.IdleTimeout, c.Gates.Lint)
	}
	if l2 := mustLoad(t, LoadOptions{Overrides: []string{"policy.protected_paths=[only/**]"}}); len(l2.Config.Policy.ProtectedPaths) != 1 {
		t.Errorf("lists must be replaced, got %v", l2.Config.Policy.ProtectedPaths)
	}
	if c.Pricing.Codex["gpt-x"].InputPerMTok != 1.25 {
		t.Errorf("pricing override: %+v", c.Pricing.Codex)
	}
	for _, bad := range []string{"no-equals", ".x=1", "a..b=1"} {
		es := loadErrs(t, LoadOptions{Overrides: []string{bad}})
		if es[0].Code != ErrInvalidOverride {
			t.Errorf("%q: got %v", bad, es)
		}
	}
	if es := loadErrs(t, LoadOptions{Overrides: []string{"auth.api_key=x"}}); es[0].Code != ErrSecretField {
		t.Errorf("secret via --set: %v", es)
	}
}

func TestParseAndFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, RepoConfigName, "planner: [unclosed\n")
	if es := loadErrs(t, LoadOptions{RepoDir: dir}); es[0].Code != ErrParse {
		t.Errorf("got %v", es)
	}
	es := loadErrs(t, LoadOptions{ConfigFiles: []string{filepath.Join(dir, "missing.yaml")}})
	if es[0].Code != ErrFileNotFound {
		t.Errorf("got %v", es)
	}
	// Missing optional layers are skipped.
	mustLoad(t, LoadOptions{UserConfigPath: filepath.Join(dir, "nope.yaml"), RepoDir: filepath.Join(dir, "nope")})
}

func TestCRLFConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, RepoConfigName, "version: 1\r\nplanner:\r\n  effort: max\r\n")
	if got := mustLoad(t, LoadOptions{RepoDir: dir}).Config.Planner.Effort; got != "max" {
		t.Errorf("effort = %q", got)
	}
}

func TestExtractFrontMatter(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"---\nplanner:\n  effort: max\n---\n# Plan\n", "planner:\n  effort: max\n", true},
		{"---\r\na: 1\r\n---\r\nbody", "a: 1\n", true},
		{"---\n---\nbody", "", true},
		{"# No front matter\n", "", false},
		{"---\nunterminated\n", "", false},
	}
	for _, tc := range cases {
		got, ok := ExtractFrontMatter([]byte(tc.in))
		if ok != tc.ok || string(got) != tc.want {
			t.Errorf("ExtractFrontMatter(%q) = %q, %v", tc.in, got, ok)
		}
	}
}
