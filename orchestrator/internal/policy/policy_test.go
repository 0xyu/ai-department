package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
)

func strictConfig() Config {
	pc := config.Default().Policy
	pc.ScopeEnforcement = "block"
	return FromConfig(pc, []string{"internal/gitops/**", "docs/git.md"}, secure.NewRedactor())
}

func added(lines ...string) []Line {
	out := make([]Line, len(lines))
	for i, l := range lines {
		out[i] = Line{No: i + 1, Text: l}
	}
	return out
}

// testFile is a test change inside scope, so that tests_missing stays quiet.
var testFile = File{Status: gitops.Modified, Path: "internal/gitops/x_test.go", Added: added("func TestX(t *testing.T) {}")}

func codes(r Result) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Code+":"+f.Path)
	}
	return out
}

func requireFinding(t *testing.T, r Result, code, path string, blocking bool) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.Code == code && f.Path == path {
			if f.Blocking != blocking {
				t.Fatalf("%s %s: blocking=%v, want %v", code, path, f.Blocking, blocking)
			}
			return f
		}
	}
	t.Fatalf("no %s finding for %s in %v", code, path, codes(r))
	return Finding{}
}

func TestCleanDiffPasses(t *testing.T) {
	r := Check(strictConfig(), []File{
		{Status: gitops.Modified, Path: "internal/gitops/x.go", Size: 100, Added: added("func X() {}")},
		testFile,
	})
	if len(r.Findings) != 0 || r.Rejected() {
		t.Fatalf("unexpected findings %v", codes(r))
	}
}

func TestRejectedDiffs(t *testing.T) {
	cfg := strictConfig()
	cases := []struct {
		name string
		file File
		code string
		path string
	}{
		{"protected plan", File{Status: gitops.Modified, Path: ".aco/plan/phases/P01.md"}, CodeProtectedPath, ".aco/plan/phases/P01.md"},
		{"protected AGENTS.md", File{Status: gitops.Modified, Path: "AGENTS.md"}, CodeProtectedPath, "AGENTS.md"},
		{"protected case variant", File{Status: gitops.Added, Path: ".ACO/plan/x.md"}, CodeProtectedPath, ".ACO/plan/x.md"},
		{"protected CI", File{Status: gitops.Added, Path: ".github/workflows/ci.yml"}, CodeProtectedPath, ".github/workflows/ci.yml"},
		{"protected rename source", File{Status: gitops.Renamed, OldPath: "AGENTS.md", Path: "internal/gitops/agents.md"}, CodeProtectedPath, "AGENTS.md"},
		{"outside scope", File{Status: gitops.Added, Path: "internal/other/y.go"}, CodeScopeViolation, "internal/other/y.go"},
		{"10 MB file", File{Status: gitops.Added, Path: "internal/gitops/big.txt", Size: 10 << 20}, CodeFileTooLarge, "internal/gitops/big.txt"},
		{"fake AWS key", File{Status: gitops.Added, Path: "internal/gitops/cfg.go", Added: added(`const k = "AKIAIOSFODNN7EXAMPLE"`)}, CodeSecret, "internal/gitops/cfg.go"},
		{"private key", File{Status: gitops.Added, Path: "internal/gitops/k.txt", Added: added("-----BEGIN RSA PRIVATE KEY-----")}, CodeSecret, "internal/gitops/k.txt"},
		{"key file", File{Status: gitops.Added, Path: "internal/gitops/id_rsa"}, CodeSecretFile, "internal/gitops/id_rsa"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Check(cfg, []File{c.file, testFile})
			if !r.Rejected() {
				t.Fatalf("not rejected: %v", codes(r))
			}
			f := requireFinding(t, r, c.code, c.path, true)
			if f.Message == "" {
				t.Fatal("finding without a reason")
			}
		})
	}
}

func TestSecretFindingsNeverContainTheSecret(t *testing.T) {
	red := secure.NewRedactor(secure.NewSecret(securetest.FakeOpaque))
	cfg := strictConfig()
	cfg.Redactor = red
	var lines []string
	for _, s := range []string{securetest.FakeAnthropic, securetest.FakeOpenAI, securetest.FakeOpaque, "AKIAIOSFODNN7EXAMPLE"} {
		lines = append(lines, `x := "`+s+`"`)
	}
	r := Check(cfg, []File{{Status: gitops.Added, Path: "internal/gitops/s.go", Added: added(lines...)}, testFile})
	rules := map[string]bool{}
	for _, f := range r.Findings {
		if f.Code == CodeSecret {
			rules[f.Rule] = true
		}
	}
	for _, want := range []string{"anthropic_key", "openai_key", "registered_secret", "aws_access_key"} {
		if !rules[want] {
			t.Errorf("rule %s did not fire; got %v", want, rules)
		}
	}
	b, _ := json.Marshal(r)
	var text strings.Builder
	for _, f := range r.Findings {
		text.WriteString(f.String() + "\n")
	}
	securetest.AssertNoLeak(t, "policy findings", string(b)+text.String(), securetest.FakeAnthropic, securetest.FakeOpenAI, securetest.FakeOpaque, "AKIAIOSFODNN7EXAMPLE")
}

func TestGenericSecretIsFlaggedNotBlocking(t *testing.T) {
	r := Check(strictConfig(), []File{{Status: gitops.Added, Path: "internal/gitops/c.go", Added: added(
		`password = "hunter2hunter2hunter2"`,
		`api_key: "your-api-key-goes-here-123"`,
	)}, testFile})
	n := 0
	for _, f := range r.Findings {
		if f.Code == CodeSecret {
			n++
			if f.Blocking || f.Rule != "generic_assignment" || f.Line != 1 {
				t.Fatalf("finding %+v", f)
			}
		}
	}
	if n != 1 {
		t.Fatalf("got %d generic findings: %v", n, codes(r))
	}
}

func TestAntiGaming(t *testing.T) {
	cases := []struct {
		line, code, rule string
	}{
		{`	t.Skip("flaky")`, CodeTestSkipped, "go_skip"},
		{`@pytest.mark.skip(reason="later")`, CodeTestSkipped, "pytest_skip"},
		{`it.skip("works", () => {})`, CodeTestSkipped, "js_skip"},
		{`describe.only("x", () => {})`, CodeTestSkipped, "js_only"},
		{`/* eslint-disable no-console */`, CodeSuppression, "eslint_disable"},
		{`exec.Command(x) // #nosec G204`, CodeSuppression, "nosec"},
		{`x = f()  # noqa: E501`, CodeSuppression, "noqa"},
		{`// @ts-ignore`, CodeSuppression, "ts_ignore"},
	}
	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			r := Check(strictConfig(), []File{{Status: gitops.Modified, Path: "internal/gitops/x_test.go", Added: added(c.line)}})
			f := requireFinding(t, r, c.code, "internal/gitops/x_test.go", true)
			if f.Rule != c.rule || f.Line != 1 {
				t.Fatalf("finding %+v", f)
			}
		})
	}
	// Clean lines do not fire.
	for _, ok := range []string{`t.Run("skip", func(t *testing.T) {})`, `skipWhitespace(x)`, `// explain why tests matter`} {
		if r := Check(strictConfig(), []File{{Status: gitops.Modified, Path: "internal/gitops/x_test.go", Added: added(ok)}}); len(r.Findings) != 0 {
			t.Errorf("%q: %v", ok, codes(r))
		}
	}
	// warn mode reports without blocking.
	cfg := strictConfig()
	cfg.AntiGaming = "warn"
	r := Check(cfg, []File{{Status: gitops.Modified, Path: "internal/gitops/x_test.go", Added: added("t.Skip()")}})
	requireFinding(t, r, CodeTestSkipped, "internal/gitops/x_test.go", false)
}

func TestDeletedTests(t *testing.T) {
	for _, p := range []string{"internal/gitops/a_test.go", "internal/gitops/tests/test_a.py", "internal/gitops/web/a.spec.ts", "internal/gitops/__tests__/a.js"} {
		r := Check(strictConfig(), []File{{Status: gitops.Deleted, Path: p}, testFile})
		requireFinding(t, r, CodeTestDeleted, p, true)
	}
	// A test renamed into a non-test name counts as deleted; a test moved to
	// another test name does not.
	r := Check(strictConfig(), []File{{Status: gitops.Renamed, OldPath: "internal/gitops/a_test.go", Path: "internal/gitops/a_old.txt"}, testFile})
	requireFinding(t, r, CodeTestDeleted, "internal/gitops/a_test.go", true)
	r = Check(strictConfig(), []File{{Status: gitops.Renamed, OldPath: "internal/gitops/a_test.go", Path: "internal/gitops/b_test.go"}})
	if r.Has(CodeTestDeleted) {
		t.Fatal("test rename flagged as deletion")
	}
}

func TestTestsMissing(t *testing.T) {
	cfg := strictConfig()
	r := Check(cfg, []File{{Status: gitops.Modified, Path: "internal/gitops/x.go", Added: added("func Y() {}")}})
	requireFinding(t, r, CodeTestsMissing, "internal/gitops/x.go", true)
	cfg.TestsMissing = "warn"
	r = Check(cfg, []File{{Status: gitops.Modified, Path: "internal/gitops/x.go"}})
	requireFinding(t, r, CodeTestsMissing, "internal/gitops/x.go", false)
	// Docs-only and testdata changes need no tests; testdata counts as a test change.
	if r := Check(strictConfig(), []File{{Status: gitops.Modified, Path: "docs/git.md"}}); r.Has(CodeTestsMissing) {
		t.Fatal("docs change flagged")
	}
	if r := Check(strictConfig(), []File{{Status: gitops.Modified, Path: "internal/gitops/x.go"}, {Status: gitops.Added, Path: "internal/gitops/testdata/case.json"}}); r.Has(CodeTestsMissing) {
		t.Fatal("testdata change not counted")
	}
}

func TestScopeFlagModeAndBinary(t *testing.T) {
	pc := config.Default().Policy // scope_enforcement: flag, binary_files: warn
	cfg := FromConfig(pc, []string{"src/**"}, nil)
	r := Check(cfg, []File{{Status: gitops.Added, Path: "other/a.png", Binary: true, Size: 10}, {Status: gitops.Added, Path: "src/a_test.go"}})
	requireFinding(t, r, CodeScopeViolation, "other/a.png", false)
	requireFinding(t, r, CodeBinaryFile, "other/a.png", false)
	if r.Rejected() {
		t.Fatal("flag/warn findings rejected the diff")
	}
	pc.BinaryFiles = "fail"
	r = Check(FromConfig(pc, nil, nil), []File{{Status: gitops.Added, Path: "a.png", Binary: true}})
	requireFinding(t, r, CodeBinaryFile, "a.png", true)
	// No scope paths: no scope check.
	if r := Check(FromConfig(config.Default().Policy, nil, nil), []File{{Status: gitops.Added, Path: "anything/x_test.go"}}); r.Has(CodeScopeViolation) {
		t.Fatal("scope check without scope paths")
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, path  string
		fold, want bool
	}{
		{".aco/**", ".aco/plan/P01.md", false, true},
		{".aco/**", "x/.aco/plan", false, false},
		{"AGENTS.md", "AGENTS.md", false, true},
		{"AGENTS.md", "sub/AGENTS.md", false, true},
		{"AGENTS.md", "agents.md", true, true},
		{"AGENTS.md", "agents.md", false, false},
		{"**/*.pem", "a/b/c.pem", false, true},
		{"**/*.pem", "c.pem", false, true},
		{".env*", ".env.local", false, true},
		{".env*", "config/.env", false, true},
		{"internal/gitops", "internal/gitops/x.go", false, true},
		{"internal/gitops/**", "internal/gitopsx/x.go", false, false},
		{"src/*.go", "src/a.go", false, true},
		{"src/*.go", "src/sub/a.go", false, false},
		{"src/**/a.go", "src/a.go", false, true},
		{"src/**/a.go", "src/x/y/a.go", false, true},
		{"/docs/*.md", "docs/git.md", false, true},
		{"build/", "build/out/x", false, true},
		{"", "x", false, false},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.path, c.fold); got != c.want {
			t.Errorf("Match(%q, %q, %v) = %v", c.pat, c.path, c.fold, got)
		}
	}
}
