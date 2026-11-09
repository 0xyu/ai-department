package gates

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xyu/aco/internal/secure"
	"github.com/0xyu/aco/internal/secure/securetest"
)

// The test binary doubles as a fake tool: `<test binary> -test.run=^TestHelperProcess$ -- <mode>`.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("ACO_GATES_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	mode := args[0]
	out := os.Stdout
	switch mode {
	case "gotest": // args[1]: file of go test -json lines to print; exit 1 if any test failed
		b, _ := os.ReadFile(args[1])
		out.Write(b)
		if bytes.Contains(b, []byte(`"Action":"fail"`)) {
			os.Exit(1)
		}
	case "cat": // args[1]: file; args[2]: exit code
		b, _ := os.ReadFile(args[1])
		out.Write(b)
		var code int
		fmt.Sscan(args[2], &code)
		os.Exit(code)
	case "hang":
		fmt.Println("starting")
		time.Sleep(10 * time.Minute)
	case "bigfail": // 2 MB of noise with one failure in the middle
		line := strings.Repeat("x", 99) + "\n"
		for i := 0; i < 10000; i++ {
			out.WriteString(line)
		}
		fmt.Fprintln(out, "--- FAIL: TestTheRealProblem (0.01s)")
		fmt.Fprintln(out, "    real_test.go:42: expected 1, got 2")
		for i := 0; i < 10000; i++ {
			out.WriteString(line)
		}
		fmt.Fprintln(out, "FAIL\texample.com/x\t0.5s")
		os.Exit(1)
	case "write": // writes args[2] to the file args[1]
		os.WriteFile(args[1], []byte(args[2]), 0o644)
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
	case "leak": // prints the secret in file args[1] to stdout and stderr, fails
		b, _ := os.ReadFile(args[1])
		fmt.Println("token=" + string(b))
		fmt.Fprintln(os.Stderr, "error: auth with "+string(b)+" failed")
		os.Exit(1)
	case "lint": // args[1]: file of error lines; exit 1 if non-empty
		b, _ := os.ReadFile(args[1])
		out.Write(b)
		if len(bytes.TrimSpace(b)) > 0 {
			os.Exit(1)
		}
	case "report": // writes args[1] into $ACO_GATE_REPORTS/args[2], exit args[3]
		b, _ := os.ReadFile(args[1])
		os.WriteFile(filepath.Join(os.Getenv("ACO_GATE_REPORTS"), args[2]), b, 0o600)
		var code int
		fmt.Sscan(args[3], &code)
		os.Exit(code)
	}
	os.Exit(0)
}

// helper returns a gate command line that runs the helper in mode.
func helper(mode string, args ...string) string {
	parts := []string{quoteShell(os.Args[0]), "-test.run=^TestHelperProcess$", "--", mode}
	for _, a := range args {
		parts = append(parts, quoteShell(a))
	}
	return strings.Join(parts, " ")
}

func newRunner(t *testing.T, mod func(*Options)) *Runner {
	t.Helper()
	opts := Options{Dir: t.TempDir(), ExtraEnv: []string{"ACO_GATES_HELPER=1"}}
	if mod != nil {
		mod(&opts)
	}
	r, err := New(opts)
	if err != nil {
		t.Skipf("no runner: %v", err)
	}
	return r
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func spec(gate string, runs ...string) Spec {
	s := Spec{Type: gate, Required: true, Timeout: time.Minute, Source: SourceConfig}
	for _, r := range runs {
		s.Commands = append(s.Commands, Command{Run: r})
	}
	return s
}

func goEvents(pkg string, results map[string]string) string {
	var b strings.Builder
	for name, st := range results {
		fmt.Fprintf(&b, `{"Action":"run","Package":%q,"Test":%q}`+"\n", pkg, name)
		if st == "fail" {
			fmt.Fprintf(&b, `{"Action":"output","Package":%q,"Test":%q,"Output":"--- FAIL: %s (0.00s)\n"}`+"\n", pkg, name, name)
		}
		fmt.Fprintf(&b, `{"Action":%q,"Package":%q,"Test":%q}`+"\n", st, pkg, name)
	}
	return b.String()
}

// AC-06.2: a pre-existing failing test does not fail the gate when nothing
// new fails; breaking a previously passing test fails it and lists the
// regression by name.
func TestBaselineRegression(t *testing.T) {
	r := newRunner(t, nil)
	dir := t.TempDir()
	baseFile := writeFile(t, dir, "base.jsonl", goEvents("example.com/p", map[string]string{"TestAdd": "pass", "TestKnownBroken": "fail"}))
	sameFile := writeFile(t, dir, "same.jsonl", goEvents("example.com/p", map[string]string{"TestAdd": "pass", "TestKnownBroken": "fail", "TestNew": "pass"}))
	regrFile := writeFile(t, dir, "regr.jsonl", goEvents("example.com/p", map[string]string{"TestAdd": "fail", "TestKnownBroken": "fail"}))
	ctx := context.Background()

	baseRound, err := r.Run(ctx, []Spec{spec(Unit, helper("gotest", baseFile)+" -json")}, RunOptions{Baseline: true})
	if err != nil {
		t.Fatal(err)
	}
	if g := baseRound.Gate(Unit); g.Status != Failed {
		t.Fatalf("baseline round records the failure as it is: %s", g.Status)
	}
	base := NewBaseline(baseRound, strings.Repeat("a", 40), time.Now())

	same, err := r.Run(ctx, []Spec{spec(Unit, helper("gotest", sameFile)+" -json")}, RunOptions{Compare: base})
	if err != nil {
		t.Fatal(err)
	}
	g := same.Gate(Unit)
	if g.Status != Passed || !same.Passed() {
		t.Fatalf("pre-existing failure only: status %s (%s)", g.Status, g.Summary)
	}
	if c := g.Comparison; len(c.PreexistingFailures) != 1 || c.PreexistingFailures[0] != "example.com/p.TestKnownBroken" {
		t.Errorf("preexisting = %v", c.PreexistingFailures)
	}

	regr, err := r.Run(ctx, []Spec{spec(Unit, helper("gotest", regrFile)+" -json")}, RunOptions{Compare: base})
	if err != nil {
		t.Fatal(err)
	}
	g = regr.Gate(Unit)
	if g.Status != Failed || g.Class != ClassQuality {
		t.Fatalf("regression: %s/%s", g.Status, g.Class)
	}
	if c := g.Comparison; len(c.Regressions) != 1 || c.Regressions[0] != "example.com/p.TestAdd" {
		t.Errorf("regressions = %v", c.Regressions)
	}
	if !strings.Contains(g.Summary, "1 regression") {
		t.Errorf("summary = %q", g.Summary)
	}
	if !strings.Contains(g.Commands[0].Log, "--- FAIL: TestAdd") || strings.Contains(g.Commands[0].Log, `"Action"`) {
		t.Errorf("go test log should be the test output, got %q", g.Commands[0].Log)
	}

	// No baseline: every failure is new.
	nob, _ := r.Run(ctx, []Spec{spec(Unit, helper("gotest", baseFile)+" -json")}, RunOptions{})
	if nob.Gate(Unit).Status != Failed {
		t.Errorf("without baseline the pre-existing failure fails: %s", nob.Gate(Unit).Status)
	}
}

// AC-06.3: a new vulnerable dependency at or above high fails the security
// gate; a pre-existing one is reported but does not block.
func TestSecurityVulnBaseline(t *testing.T) {
	r := newRunner(t, nil)
	baseOut, _ := filepath.Abs(filepath.Join("testdata", "output", "npm-audit-base.json"))
	newOut, _ := filepath.Abs(filepath.Join("testdata", "output", "npm-audit-new.json"))
	sec := func(file string) Spec {
		s := spec(Security)
		s.Scanners = []string{ScannerNpmAudit}
		// npm audit exits 1 when it finds anything.
		s.Commands = []Command{{Run: helper("cat", file, "1"), Scanner: ScannerNpmAudit}}
		return s
	}
	ctx := context.Background()
	b, err := r.Run(ctx, []Spec{sec(baseOut)}, RunOptions{Baseline: true})
	if err != nil {
		t.Fatal(err)
	}
	base := NewBaseline(b, strings.Repeat("b", 40), time.Now())

	pre, _ := r.Run(ctx, []Spec{sec(baseOut)}, RunOptions{Compare: base})
	g := pre.Gate(Security)
	if g.Status != Passed {
		t.Fatalf("pre-existing critical vuln blocks: %s %s", g.Status, g.Summary)
	}
	if len(g.Comparison.PreexistingVulns) != 1 || g.Comparison.PreexistingVulns[0].Package != "minimist" {
		t.Errorf("pre-existing not reported: %+v", g.Comparison)
	}

	added, _ := r.Run(ctx, []Spec{sec(newOut)}, RunOptions{Compare: base})
	g = added.Gate(Security)
	if g.Status != Failed {
		t.Fatalf("new high vuln: %s", g.Status)
	}
	c := g.Comparison
	if len(c.BlockingVulns) != 1 || c.BlockingVulns[0].Package != "lodash" {
		t.Errorf("blocking = %+v", c.BlockingVulns)
	}
	if len(c.NewVulns) != 2 { // lodash (high) and debug (moderate, below threshold)
		t.Errorf("new = %+v", c.NewVulns)
	}

	// At fail_severity critical, the new high vuln is reported only.
	rc := newRunner(t, func(o *Options) { o.FailSeverity = SevCritical })
	crit, _ := rc.Run(ctx, []Spec{sec(newOut)}, RunOptions{Compare: base})
	if g := crit.Gate(Security); g.Status != Passed || len(g.Comparison.NewVulns) != 2 {
		t.Errorf("critical threshold: %s %+v", g.Status, g.Comparison)
	}
}

// AC-06.4: a hanging command is killed at its timeout; "command not found"
// is classified environment.
func TestTimeoutAndEnvironment(t *testing.T) {
	r := newRunner(t, nil)
	s := spec(Unit, helper("hang"))
	s.Timeout = 500 * time.Millisecond
	start := time.Now()
	round, err := r.Run(context.Background(), []Spec{s}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 15*time.Second {
		t.Errorf("hang not killed promptly: %v", el)
	}
	g := round.Gate(Unit)
	if g.Status != Timeout || g.Class != ClassTimeout || g.Commands[0].Status != Timeout {
		t.Errorf("hang: %s/%s", g.Status, g.Class)
	}
	if !strings.Contains(g.Commands[0].Log, "starting") {
		t.Errorf("output before the kill lost: %q", g.Commands[0].Log)
	}

	round, err = r.Run(context.Background(), []Spec{spec(Build, "aco-definitely-missing-tool-xyz --version")}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	g = round.Gate(Build)
	if g.Status != Failed || g.Class != ClassEnvironment || g.Commands[0].Reason != "command not found" {
		t.Errorf("missing command: %s/%s %q exit=%d log=%q", g.Status, g.Class, g.Commands[0].Reason, g.Commands[0].ExitCode, g.Commands[0].Log)
	}
}

// AC-06.5: logs over 1 MB are trimmed and keep the first failure.
func TestLargeLogTrimmed(t *testing.T) {
	r := newRunner(t, nil)
	round, err := r.Run(context.Background(), []Spec{spec(Unit, helper("bigfail"))}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := round.Gate(Unit).Commands[0]
	if c.LogBytes < 2_000_000 || !c.Truncated || len(c.Log) > MaxLogBytes {
		t.Fatalf("log bytes %d, kept %d, truncated %v", c.LogBytes, len(c.Log), c.Truncated)
	}
	for _, want := range []string{"--- FAIL: TestTheRealProblem", "real_test.go:42: expected 1, got 2", "FAIL\texample.com/x", "bytes trimmed"} {
		if !strings.Contains(c.Log, want) {
			t.Errorf("trimmed log lacks %q", want)
		}
	}
}

func TestTrim(t *testing.T) {
	small := []byte("ok\n")
	if out, cut := Trim(small, 100); cut || !bytes.Equal(out, small) {
		t.Error("small log changed")
	}
	var b bytes.Buffer
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "line %d of noise\n", i)
	}
	b.WriteString("main.go:12:5: undefined: foo\n")
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "more %d\n", i)
	}
	out, cut := Trim(b.Bytes(), 8192)
	if !cut || len(out) > 8192 || !bytes.Contains(out, []byte("main.go:12:5: undefined: foo")) || !bytes.Contains(out, []byte("more 4999")) {
		t.Errorf("trim: cut=%v len=%d\n%s", cut, len(out), out)
	}
	if !bytes.HasPrefix(out, []byte("line 0 of noise")) {
		t.Error("head lost")
	}
}

// Results are redacted; gate commands never see agent or other secrets.
func TestRedactionAndEnvironment(t *testing.T) {
	red := secure.NewRedactor(secure.NewSecret(securetest.FakeOpaque))
	r := newRunner(t, func(o *Options) {
		o.Redactor = red
		o.BaseEnv = []string{"PATH=" + os.Getenv("PATH"), "SYSTEMROOT=" + os.Getenv("SYSTEMROOT"),
			"ANTHROPIC_API_KEY=" + securetest.FakeAnthropic, "OPENAI_API_KEY=" + securetest.FakeOpenAI,
			"ACO_API_TOKEN=aco-token-abcdefgh12345678", "DB_PASSWORD=pw-abcdefgh12345678"}
	})
	secretFile := writeFile(t, t.TempDir(), "secret.txt", securetest.FakeOpaque)
	round, err := r.Run(context.Background(), []Spec{spec(Unit, helper("env")), spec(Build, helper("leak", secretFile))}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	envLog := round.Gate(Unit).Commands[0].Log
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ACO_API_TOKEN", "DB_PASSWORD"} {
		if strings.Contains(envLog, k+"=") {
			t.Errorf("%s reached the gate command", k)
		}
	}
	if !strings.Contains(envLog, "CI=true") || !strings.Contains(envLog, "ACO_GATE_REPORTS=") {
		t.Errorf("gate env = %q", envLog)
	}
	leak := round.Gate(Build)
	securetest.AssertNoLeak(t, "gate log", leak.Commands[0].Log+strings.Join(leak.Commands[0].Signatures, "\n"), securetest.FakeOpaque)
	if !strings.Contains(leak.Commands[0].Log, secure.Mask) {
		t.Errorf("mask missing: %q", leak.Commands[0].Log)
	}
}

// Unstructured failures are compared by normalized failure lines.
func TestUnstructuredBaseline(t *testing.T) {
	r := newRunner(t, nil)
	dir := t.TempDir()
	baseF := writeFile(t, dir, "base.txt", "pkg/a.go:10:2: error: unused variable x\n")
	movedF := writeFile(t, dir, "moved.txt", "pkg/a.go:14:2: error: unused variable x\n")
	newF := writeFile(t, dir, "new.txt", "pkg/a.go:14:2: error: unused variable x\npkg/b.go:3:1: error: missing return\n")
	ctx := context.Background()
	b, _ := r.Run(ctx, []Spec{spec(Lint, helper("lint", baseF))}, RunOptions{Baseline: true})
	base := NewBaseline(b, strings.Repeat("c", 40), time.Now())
	// The helper path differs per file, so compare by a fixed command line:
	// rewrite the baseline command to the one used below.
	run := func(file string) *Result {
		s := spec(Lint, helper("lint", file))
		base.Results[Lint].Commands[0].Command.Run = s.Commands[0].Run
		round, err := r.Run(ctx, []Spec{s}, RunOptions{Compare: base})
		if err != nil {
			t.Fatal(err)
		}
		return round.Gate(Lint)
	}
	if g := run(movedF); g.Status != Passed {
		t.Errorf("same error on another line: %s %s", g.Status, g.Summary)
	}
	if g := run(newF); g.Status != Failed || len(g.Comparison.NewFailures) != 1 {
		t.Errorf("new error: %s %+v", g.Status, g.Comparison)
	}
}

// Fail-fast stops a round at the first failing gate; baseline rounds run all.
func TestFailFastAndNotApplicable(t *testing.T) {
	r := newRunner(t, nil)
	dir := t.TempDir()
	failF := writeFile(t, dir, "f.txt", "x.go:1:1: error: boom\n")
	specs := []Spec{
		spec(Typecheck, helper("lint", failF)),
		{Type: Lint, Required: false, Justification: "docs only"},
		spec(Unit, helper("env")),
	}
	round, err := r.Run(context.Background(), specs, RunOptions{FailFast: true})
	if err != nil {
		t.Fatal(err)
	}
	if round.Passed() || round.FirstFailure().Gate != Typecheck {
		t.Fatalf("first failure = %+v", round.FirstFailure())
	}
	if g := round.Gate(Unit); g.Status != Skipped {
		t.Errorf("unit after failure = %s", g.Status)
	}
	base, _ := r.Run(context.Background(), specs, RunOptions{FailFast: true, Baseline: true})
	if g := base.Gate(Unit); g.Status != Passed {
		t.Errorf("baseline round skipped a gate: %s", g.Status)
	}
	if g := base.Gate(Lint); g.Status != NotApplicable || !strings.Contains(g.Summary, "docs only") {
		t.Errorf("n/a = %+v", g)
	}
	recs, err := StateRecords(round, "run-1", 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Gate != Typecheck || recs[0].Status != "failed" || recs[1].Status != "not_applicable" {
		t.Errorf("records = %+v", recs)
	}
}

// Scanner report files written to {reports} are parsed; a scanner whose
// output cannot be read is an error, not a pass.
func TestScannerReportsDir(t *testing.T) {
	r := newRunner(t, nil)
	sarif, _ := filepath.Abs(filepath.Join("testdata", "output", "gosec.sarif"))
	s := spec(Security)
	s.Commands = []Command{{Run: helper("report", sarif, "gosec.sarif", "1"), Scanner: ScannerGosec}}
	round, _ := r.Run(context.Background(), []Spec{s}, RunOptions{})
	g := round.Gate(Security)
	// G101 (high) and G402 (critical) block at high; G104 (medium) does not.
	if g.Status != Failed || len(g.Comparison.BlockingIssues) != 2 || len(g.Comparison.NewIssues) != 3 {
		t.Errorf("gosec: %s %+v", g.Status, g.Comparison)
	}
	bad := spec(Security)
	bad.Commands = []Command{{Run: helper("cat", sarif+".missing", "2"), Scanner: ScannerGosec}}
	round, _ = r.Run(context.Background(), []Spec{bad}, RunOptions{})
	if g := round.Gate(Security); g.Status != Errored || g.Class != ClassInternal {
		t.Errorf("unreadable scanner: %s/%s", g.Status, g.Class)
	}
}

func TestBaselineStoreAndCompact(t *testing.T) {
	r := newRunner(t, nil)
	round, _ := r.Run(context.Background(), []Spec{spec(Unit, helper("bigfail"))}, RunOptions{Baseline: true})
	b := NewBaseline(round, strings.Repeat("d", 40), time.Now())
	if l := len(b.Results[Unit].Commands[0].Log); l > 16<<10 {
		t.Errorf("baseline log not compacted: %d", l)
	}
	c := round.Gate(Unit).Compact(4096)
	if len(c.Commands[0].Log) > 4096 || len(round.Gate(Unit).Commands[0].Log) <= 4096 {
		t.Error("compact must bound the copy, not the original")
	}
}
