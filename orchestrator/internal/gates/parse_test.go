package gates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readOut(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "output", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func statusOf(tests []TestCase) map[string]TestStatus {
	m := map[string]TestStatus{}
	for _, tc := range tests {
		m[tc.ID()] = tc.Status
	}
	return m
}

func TestParseGoTest(t *testing.T) {
	p := ParseGoTest(readOut(t, "gotest.jsonl"))
	if !p.OK {
		t.Fatal("not recognized")
	}
	got := statusOf(p.Tests)
	want := map[string]TestStatus{
		"example.com/goproj.TestAdd":         TestPass,
		"example.com/goproj.TestKnownBroken": TestFail,
		"example.com/goproj.TestSkipped":     TestSkip,
		// Build failure without any failing test: a package-level failure.
		"example.com/goproj/broken": TestFail,
	}
	if len(got) != len(want) {
		t.Errorf("tests = %v", got)
	}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("%s = %q, want %q", id, got[id], st)
		}
	}
	// The package with a failing test is not listed again at package level.
	if _, ok := got["example.com/goproj"]; ok {
		t.Error("package with a failing test also listed as package failure")
	}
	if !strings.Contains(string(p.Text), "--- FAIL: TestKnownBroken") || !strings.Contains(string(p.Text), "syntax error") {
		t.Errorf("text output missing: %q", p.Text)
	}
	if ParseGoTest([]byte("ok  \texample.com/x\n")).OK {
		t.Error("plain go test output recognized as JSON")
	}
}

func TestParseJUnit(t *testing.T) {
	p := ParseJUnit(readOut(t, "pytest-junit.xml"))
	if !p.OK {
		t.Fatal("not recognized")
	}
	got := statusOf(p.Tests)
	want := map[string]TestStatus{
		"tests.test_calc.test_add": TestPass, "tests.test_calc.test_sub": TestFail,
		"tests.test_calc.test_err": TestFail, "tests.test_calc.test_skip": TestSkip,
	}
	for id, st := range want {
		if got[id] != st {
			t.Errorf("%s = %q, want %q", id, got[id], st)
		}
	}
	single := `<testsuite name="s"><testcase classname="C" name="a"/></testsuite>`
	if p := ParseJUnit([]byte(single)); !p.OK || len(p.Tests) != 1 {
		t.Errorf("single suite: %+v", p)
	}
	if ParseJUnit([]byte(`<html><body/></html>`)).OK {
		t.Error("html recognized as JUnit")
	}
}

func TestParseLintFormats(t *testing.T) {
	es := ParseESLint(readOut(t, "eslint.json"), "/repo")
	if !es.OK || len(es.Issues) != 2 {
		t.Fatalf("eslint: %+v", es)
	}
	if i := es.Issues[0]; i.Path != "src/a.js" || i.Rule != "no-unused-vars" || i.Severity != SevHigh || i.Line != 3 {
		t.Errorf("eslint error = %+v", i)
	}
	if es.Issues[1].Severity != SevLow {
		t.Errorf("eslint warning severity = %s", es.Issues[1].Severity)
	}
	gl := ParseGolangci(readOut(t, "golangci.json"), "")
	if !gl.OK || len(gl.Issues) != 1 || gl.Issues[0].Rule != "errcheck" || gl.Issues[0].Path != "internal/x/x.go" || gl.Issues[0].Severity != SevHigh {
		t.Errorf("golangci: %+v", gl)
	}
}

func TestParseSARIF(t *testing.T) {
	p := ParseSARIF(readOut(t, "gosec.sarif"), "", "")
	if !p.OK || len(p.Issues) != 3 {
		t.Fatalf("sarif: %+v", p)
	}
	want := []struct {
		rule string
		sev  Severity
		path string
	}{{"G101", SevHigh, "cmd/main.go"}, {"G104", SevMedium, "internal/x/x.go"}, {"G402", SevCritical, "net/tls.go"}}
	for i, w := range want {
		is := p.Issues[i]
		if is.Tool != "gosec" || is.Rule != w.rule || is.Severity != w.sev || is.Path != w.path {
			t.Errorf("issue %d = %+v, want %+v", i, is, w)
		}
	}
	if ParseSARIF([]byte(`{"version":"1.0","runs":[]}`), "", "").OK {
		t.Error("SARIF 1.0 accepted")
	}
}

func TestParseVulnFormats(t *testing.T) {
	gv := ParseGovulncheck(readOut(t, "govulncheck.json"))
	if !gv.OK || len(gv.Vulns) != 1 {
		t.Fatalf("govulncheck: %+v", gv)
	}
	if v := gv.Vulns[0]; v.ID != "GO-2023-1571" || v.Package != "golang.org/x/net" || v.Version != "v0.6.0" || v.Severity != SevHigh {
		t.Errorf("govulncheck vuln = %+v", v)
	}

	base := ParseNpmAudit(readOut(t, "npm-audit-base.json"))
	if !base.OK || len(base.Vulns) != 1 {
		t.Fatalf("npm audit base: %+v", base)
	}
	if v := base.Vulns[0]; v.ID != "GHSA-xvch-5gv4-984h" || v.Package != "minimist" || v.Severity != SevCritical {
		t.Errorf("npm vuln = %+v", v)
	}
	newer := ParseNpmAudit(readOut(t, "npm-audit-new.json"))
	sev := map[string]Severity{}
	for _, v := range newer.Vulns {
		sev[v.Package] = v.Severity
	}
	if len(newer.Vulns) != 3 || sev["lodash"] != SevHigh || sev["debug"] != SevMedium {
		t.Errorf("npm audit new: %+v", newer.Vulns)
	}
	v6 := `{"advisories":{"118":{"id":118,"module_name":"minimatch","severity":"high","title":"ReDoS","github_advisory_id":"GHSA-hxm2-r34f-qmc5"}}}`
	if p := ParseNpmAudit([]byte(v6)); !p.OK || len(p.Vulns) != 1 || p.Vulns[0].ID != "GHSA-hxm2-r34f-qmc5" {
		t.Errorf("npm 6: %+v", p)
	}

	pa := ParsePipAudit(readOut(t, "pip-audit.json"))
	if !pa.OK || len(pa.Vulns) != 1 {
		t.Fatalf("pip-audit: %+v", pa)
	}
	if v := pa.Vulns[0]; v.ID != "GHSA-9wx4-h78v-vm56" || v.Package != "requests" || v.Severity != SevHigh || strings.Contains(v.Summary, "\n") {
		t.Errorf("pip-audit vuln = %+v", v)
	}
	if ParsePipAudit([]byte(`[1,2]`)).OK {
		t.Error("bare array without vulns accepted")
	}
}

// Report files left in {reports} are sniffed by name and content.
func TestSniffReports(t *testing.T) {
	p := parseOutput("python -m pytest --junitxml={reports}/u.xml", []byte("== 1 failed ==\n"),
		map[string][]byte{"u.xml": readOut(t, "pytest-junit.xml")}, "")
	if !p.OK || len(p.Tests) != 4 {
		t.Errorf("junit report: %+v", p)
	}
	p = parseOutput("npx eslint . -f json", readOut(t, "eslint.json"), nil, "/repo")
	if !p.OK || len(p.Issues) != 2 {
		t.Errorf("eslint stdout: %+v", p)
	}
	p = parseScanner(ScannerGosec, nil, map[string][]byte{"gosec.sarif": readOut(t, "gosec.sarif")}, "")
	if !p.OK || len(p.Issues) != 3 {
		t.Errorf("gosec report: %+v", p)
	}
	p = parseScanner(ScannerPipAudit, nil, map[string][]byte{"pip-audit.json": readOut(t, "pip-audit.json")}, "")
	if !p.OK || len(p.Vulns) != 1 {
		t.Errorf("pip-audit report: %+v", p)
	}
	if p := parseOutput("make build", []byte("all good\n"), nil, ""); p.OK {
		t.Errorf("plain output parsed: %+v", p)
	}
}
