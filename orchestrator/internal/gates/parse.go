package gates

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Parsers for machine-readable tool output. Every parser is tolerant: it
// ignores unknown fields and lines it cannot read, and reports ok only when
// it recognized the format.

// Parsed is what one command's output yielded.
type Parsed struct {
	Tests  []TestCase
	Issues []Issue
	Vulns  []Vuln
	// Text replaces the raw output in the log when the raw output is a
	// machine format meant for parsing (go test -json → test output).
	Text []byte
	OK   bool
}

func (p *Parsed) merge(q Parsed) {
	if !q.OK {
		return
	}
	p.OK = true
	p.Tests = append(p.Tests, q.Tests...)
	p.Issues = append(p.Issues, q.Issues...)
	p.Vulns = append(p.Vulns, q.Vulns...)
	if len(q.Text) > 0 {
		p.Text = append(p.Text, q.Text...)
	}
}

// --- go test -json ---------------------------------------------------------

type goTestEvent struct {
	Action     string `json:"Action"`
	Package    string `json:"Package"`
	ImportPath string `json:"ImportPath"`
	Test       string `json:"Test"`
	Output     string `json:"Output"`
}

// ParseGoTest reads `go test -json` output. Package-level failures (build
// errors, panics outside a test, TestMain failures) without a failing test
// become a TestCase with only Package set. Non-JSON lines (compiler output
// on stderr merged into stdout) are kept in Text.
func ParseGoTest(b []byte) Parsed {
	var out Parsed
	type key struct{ pkg, test string }
	status := map[key]TestStatus{}
	var order []key
	pkgFailed := map[string]bool{}
	pkgHasFailingTest := map[string]bool{}
	var text bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	events := 0
	for sc.Scan() {
		line := sc.Bytes()
		var e goTestEvent
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &e) != nil || e.Action == "" {
			text.Write(line)
			text.WriteByte('\n')
			continue
		}
		events++
		pkg := e.Package
		if pkg == "" {
			// "pkg [pkg.test]" for test binaries: the package is the first word.
			pkg, _, _ = strings.Cut(e.ImportPath, " ")
		}
		switch e.Action {
		case "output", "build-output":
			text.WriteString(e.Output)
		case "pass", "fail", "skip":
			st := TestStatus(e.Action)
			if e.Test == "" {
				if st == TestFail {
					pkgFailed[pkg] = true
				}
				continue
			}
			k := key{pkg, e.Test}
			if _, seen := status[k]; !seen {
				order = append(order, k)
			}
			status[k] = st
			if st == TestFail {
				pkgHasFailingTest[pkg] = true
			}
		case "build-fail":
			pkgFailed[pkg] = true
		}
	}
	if events == 0 {
		return Parsed{}
	}
	for _, k := range order {
		out.Tests = append(out.Tests, TestCase{Package: k.pkg, Name: k.test, Status: status[k]})
	}
	var pkgs []string
	for p := range pkgFailed {
		if !pkgHasFailingTest[p] {
			pkgs = append(pkgs, p)
		}
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		out.Tests = append(out.Tests, TestCase{Package: p, Status: TestFail})
	}
	out.Text = text.Bytes()
	out.OK = true
	return out
}

// --- JUnit XML (JUnit, pytest --junitxml, jest-junit, surefire) -------------

type junitCase struct {
	Name      string    `xml:"name,attr"`
	Classname string    `xml:"classname,attr"`
	File      string    `xml:"file,attr"`
	Failure   *struct{} `xml:"failure"`
	Error     *struct{} `xml:"error"`
	Skipped   *struct{} `xml:"skipped"`
}

type junitSuite struct {
	Name   string       `xml:"name,attr"`
	Cases  []junitCase  `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"`
}

// ParseJUnit reads a JUnit XML report (<testsuites> or <testsuite> root).
func ParseJUnit(b []byte) Parsed {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	var root junitSuite
	var start xml.StartElement
	for {
		tok, err := dec.Token()
		if err != nil {
			return Parsed{}
		}
		if se, ok := tok.(xml.StartElement); ok {
			start = se
			break
		}
	}
	if start.Name.Local != "testsuites" && start.Name.Local != "testsuite" {
		return Parsed{}
	}
	if err := dec.DecodeElement(&root, &start); err != nil {
		return Parsed{}
	}
	var out Parsed
	var walk func(s junitSuite)
	walk = func(s junitSuite) {
		for _, c := range s.Cases {
			pkg := c.Classname
			if pkg == "" {
				pkg = s.Name
			}
			st := TestPass
			switch {
			case c.Failure != nil || c.Error != nil:
				st = TestFail
			case c.Skipped != nil:
				st = TestSkip
			}
			out.Tests = append(out.Tests, TestCase{Package: pkg, Name: c.Name, Status: st})
		}
		for _, sub := range s.Suites {
			walk(sub)
		}
	}
	walk(root)
	out.OK = true
	return out
}

// --- ESLint JSON (-f json) -------------------------------------------------

type eslintFile struct {
	FilePath string `json:"filePath"`
	Messages []struct {
		RuleID   *string `json:"ruleId"`
		Severity int     `json:"severity"` // 1 warning, 2 error
		Message  string  `json:"message"`
		Line     int     `json:"line"`
	} `json:"messages"`
}

// ParseESLint reads ESLint's JSON formatter output. Errors are high, warnings
// low.
func ParseESLint(b []byte, dir string) Parsed {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '[' || !bytes.Contains(b, []byte(`"filePath"`)) {
		return Parsed{}
	}
	var files []eslintFile
	if json.Unmarshal(b, &files) != nil {
		return Parsed{}
	}
	out := Parsed{OK: true}
	for _, f := range files {
		for _, m := range f.Messages {
			sev := SevLow
			if m.Severity >= 2 {
				sev = SevHigh
			}
			rule := ""
			if m.RuleID != nil {
				rule = *m.RuleID
			}
			out.Issues = append(out.Issues, Issue{Tool: "eslint", Rule: rule, Path: relPath(dir, f.FilePath),
				Line: m.Line, Severity: sev, Message: m.Message})
		}
	}
	return out
}

// --- golangci-lint JSON ----------------------------------------------------

// ParseGolangci reads golangci-lint's JSON output ({"Issues": […]}).
func ParseGolangci(b []byte, dir string) Parsed {
	var doc struct {
		Issues *[]struct {
			FromLinter string `json:"FromLinter"`
			Text       string `json:"Text"`
			Severity   string `json:"Severity"`
			Pos        struct {
				Filename string `json:"Filename"`
				Line     int    `json:"Line"`
			} `json:"Pos"`
		} `json:"Issues"`
	}
	if json.Unmarshal(bytes.TrimSpace(b), &doc) != nil || doc.Issues == nil {
		return Parsed{}
	}
	out := Parsed{OK: true}
	for _, i := range *doc.Issues {
		sev := SevHigh
		if s := normSeverity(i.Severity); s != "" {
			sev = s
		}
		out.Issues = append(out.Issues, Issue{Tool: "golangci-lint", Rule: i.FromLinter, Path: relPath(dir, i.Pos.Filename),
			Line: i.Pos.Line, Severity: sev, Message: i.Text})
	}
	return out
}

// --- SARIF 2.1.0 (gosec, semgrep, trivy, CodeQL, …) ------------------------

type sarifDoc struct {
	Version string `json:"version"`
	Runs    []struct {
		Tool struct {
			Driver struct {
				Name  string      `json:"name"`
				Rules []sarifRule `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`
		Results []struct {
			RuleID    string                     `json:"ruleId"`
			RuleIndex *int                       `json:"ruleIndex"`
			Level     string                     `json:"level"`
			Message   struct{ Text string }      `json:"message"`
			Props     map[string]json.RawMessage `json:"properties"`
			Locations []struct {
				Physical struct {
					Artifact struct {
						URI string `json:"uri"`
					} `json:"artifactLocation"`
					Region struct {
						StartLine int `json:"startLine"`
					} `json:"region"`
				} `json:"physicalLocation"`
			} `json:"locations"`
		} `json:"results"`
	} `json:"runs"`
}

type sarifRule struct {
	ID      string                     `json:"id"`
	Props   map[string]json.RawMessage `json:"properties"`
	Default struct {
		Level string `json:"level"`
	} `json:"defaultConfiguration"`
}

// ParseSARIF reads a SARIF log. Severity comes from a security-severity
// score (CVSS: ≥9 critical, ≥7 high, ≥4 medium), a severity property, or
// the result level (error high, warning medium, note low), in that order.
func ParseSARIF(b []byte, dir, fallbackTool string) Parsed {
	var doc sarifDoc
	if json.Unmarshal(b, &doc) != nil || !strings.HasPrefix(doc.Version, "2.") || doc.Runs == nil {
		return Parsed{}
	}
	out := Parsed{OK: true}
	for _, run := range doc.Runs {
		tool := strings.ToLower(run.Tool.Driver.Name)
		if tool == "" {
			tool = fallbackTool
		}
		rules := map[string]sarifRule{}
		for _, r := range run.Tool.Driver.Rules {
			rules[r.ID] = r
		}
		for _, res := range run.Results {
			rule, ok := rules[res.RuleID]
			if !ok && res.RuleIndex != nil && *res.RuleIndex >= 0 && *res.RuleIndex < len(run.Tool.Driver.Rules) {
				rule = run.Tool.Driver.Rules[*res.RuleIndex]
			}
			sev := sarifSeverity(res.Props)
			if sev == "" {
				sev = sarifSeverity(rule.Props)
			}
			if sev == "" {
				level := res.Level
				if level == "" {
					level = rule.Default.Level
				}
				switch level {
				case "error", "":
					sev = SevHigh
				case "warning":
					sev = SevMedium
				default:
					sev = SevLow
				}
			}
			is := Issue{Tool: tool, Rule: res.RuleID, Severity: sev, Message: res.Message.Text}
			if len(res.Locations) > 0 {
				is.Path = relPath(dir, sarifURI(res.Locations[0].Physical.Artifact.URI))
				is.Line = res.Locations[0].Physical.Region.StartLine
			}
			out.Issues = append(out.Issues, is)
		}
	}
	return out
}

func sarifURI(u string) string {
	if strings.HasPrefix(u, "file:") {
		if p, err := url.Parse(u); err == nil {
			s := p.Path
			if len(s) > 2 && s[0] == '/' && s[2] == ':' { // file:///C:/x
				s = s[1:]
			}
			return s
		}
	}
	if s, err := url.PathUnescape(u); err == nil {
		return s
	}
	return u
}

func sarifSeverity(props map[string]json.RawMessage) Severity {
	if props == nil {
		return ""
	}
	if raw, ok := props["security-severity"]; ok {
		var s string
		var f float64
		if json.Unmarshal(raw, &s) == nil {
			f, _ = strconv.ParseFloat(strings.TrimSpace(s), 64)
		} else {
			_ = json.Unmarshal(raw, &f)
		}
		switch {
		case f >= 9:
			return SevCritical
		case f >= 7:
			return SevHigh
		case f >= 4:
			return SevMedium
		case f > 0:
			return SevLow
		}
	}
	for _, k := range []string{"severity", "issue_severity", "problem.severity"} {
		if raw, ok := props[k]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				if sev := normSeverity(s); sev != "" {
					return sev
				}
			}
		}
	}
	return ""
}

// normSeverity maps tool severity words onto Severity ("" if unknown).
func normSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return SevCritical
	case "high", "error", "severe":
		return SevHigh
	case "medium", "moderate", "warning":
		return SevMedium
	case "low", "info", "note", "informational", "minor":
		return SevLow
	}
	return ""
}

// --- govulncheck -format json ----------------------------------------------

// ParseGovulncheck reads govulncheck's JSON message stream. A vulnerability
// counts when a finding's trace reaches a function (the code calls it), as
// govulncheck's own text output does. The Go vulnerability database has no
// severity, so every called vulnerability is high (fail closed).
func ParseGovulncheck(b []byte) Parsed {
	dec := json.NewDecoder(bytes.NewReader(b))
	type osv struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	}
	type frame struct {
		Module   string `json:"module"`
		Version  string `json:"version"`
		Package  string `json:"package"`
		Function string `json:"function"`
	}
	type msg struct {
		Config  *json.RawMessage `json:"config"`
		OSV     *osv             `json:"osv"`
		Finding *struct {
			OSV   string  `json:"osv"`
			Trace []frame `json:"trace"`
		} `json:"finding"`
	}
	summaries := map[string]string{}
	called := map[string]frame{}
	var ids []string
	recognized := false
	for {
		var m msg
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return Parsed{}
		}
		if m.Config != nil || m.OSV != nil || m.Finding != nil {
			recognized = true
		}
		if m.OSV != nil {
			summaries[m.OSV.ID] = m.OSV.Summary
		}
		if f := m.Finding; f != nil && len(f.Trace) > 0 && f.Trace[0].Function != "" {
			if _, ok := called[f.OSV]; !ok {
				ids = append(ids, f.OSV)
			}
			called[f.OSV] = f.Trace[0]
		}
	}
	if !recognized {
		return Parsed{}
	}
	out := Parsed{OK: true}
	for _, id := range ids {
		fr := called[id]
		out.Vulns = append(out.Vulns, Vuln{Tool: ScannerGovulncheck, ID: id, Package: fr.Module, Version: fr.Version,
			Severity: SevHigh, Summary: summaries[id]})
	}
	return out
}

// --- npm audit --json ------------------------------------------------------

// ParseNpmAudit reads `npm audit --json` (report version 2, npm ≥ 7, and the
// npm 6 "advisories" format). Each advisory is one Vuln on the package it
// affects; entries that are vulnerable only through a dependency are not
// counted again.
func ParseNpmAudit(b []byte) Parsed {
	var doc struct {
		AuditReportVersion int `json:"auditReportVersion"`
		Vulnerabilities    map[string]struct {
			Name     string            `json:"name"`
			Severity string            `json:"severity"`
			Range    string            `json:"range"`
			Via      []json.RawMessage `json:"via"`
		} `json:"vulnerabilities"`
		Advisories map[string]struct {
			ID         int    `json:"id"`
			Module     string `json:"module_name"`
			Severity   string `json:"severity"`
			Title      string `json:"title"`
			GHSA       string `json:"github_advisory_id"`
			URL        string `json:"url"`
			Vulnerable string `json:"vulnerable_versions"`
		} `json:"advisories"`
	}
	if json.Unmarshal(bytes.TrimSpace(b), &doc) != nil || (doc.Vulnerabilities == nil && doc.Advisories == nil) {
		return Parsed{}
	}
	out := Parsed{OK: true}
	seen := map[string]bool{}
	add := func(v Vuln) {
		if !seen[v.Key()] {
			seen[v.Key()] = true
			out.Vulns = append(out.Vulns, v)
		}
	}
	names := make([]string, 0, len(doc.Vulnerabilities))
	for n := range doc.Vulnerabilities {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := doc.Vulnerabilities[n]
		for _, raw := range e.Via {
			var adv struct {
				Source any    `json:"source"`
				Name   string `json:"name"`
				Title  string `json:"title"`
				URL    string `json:"url"`
				Sev    string `json:"severity"`
				Range  string `json:"range"`
			}
			if json.Unmarshal(raw, &adv) != nil || adv.Name == "" {
				continue // a string: vulnerable through another package
			}
			id := path.Base(adv.URL)
			if adv.URL == "" || id == "." || id == "/" {
				id = "npm-" + strings.Trim(string(mustJSON(adv.Source)), `"`)
			}
			sev := normSeverity(adv.Sev)
			if sev == "" {
				sev = SevHigh
			}
			add(Vuln{Tool: ScannerNpmAudit, ID: id, Package: adv.Name, Version: adv.Range, Severity: sev, Summary: adv.Title})
		}
	}
	keys := make([]string, 0, len(doc.Advisories))
	for k := range doc.Advisories {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := doc.Advisories[k]
		id := a.GHSA
		if id == "" {
			id = "npm-" + strconv.Itoa(a.ID)
		}
		sev := normSeverity(a.Severity)
		if sev == "" {
			sev = SevHigh
		}
		add(Vuln{Tool: ScannerNpmAudit, ID: id, Package: a.Module, Version: a.Vulnerable, Severity: sev, Summary: a.Title})
	}
	return out
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// --- pip-audit -f json -----------------------------------------------------

// ParsePipAudit reads pip-audit's JSON output (an object with
// "dependencies", or a bare array in older versions). pip-audit reports no
// severity, so every vulnerability is high (fail closed).
func ParsePipAudit(b []byte) Parsed {
	type dep struct {
		Name  string `json:"name"`
		Ver   string `json:"version"`
		Vulns []struct {
			ID          string   `json:"id"`
			Aliases     []string `json:"aliases"`
			Description string   `json:"description"`
		} `json:"vulns"`
	}
	b = bytes.TrimSpace(b)
	var deps []dep
	if len(b) > 0 && b[0] == '[' {
		if !bytes.Contains(b, []byte(`"vulns"`)) || json.Unmarshal(b, &deps) != nil {
			return Parsed{}
		}
	} else {
		var doc struct {
			Dependencies *[]dep `json:"dependencies"`
		}
		if json.Unmarshal(b, &doc) != nil || doc.Dependencies == nil {
			return Parsed{}
		}
		deps = *doc.Dependencies
	}
	out := Parsed{OK: true}
	for _, d := range deps {
		for _, v := range d.Vulns {
			out.Vulns = append(out.Vulns, Vuln{Tool: ScannerPipAudit, ID: v.ID, Package: strings.ToLower(d.Name),
				Version: d.Ver, Severity: SevHigh, Summary: firstLine(v.Description)})
		}
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// --- dispatch ----------------------------------------------------------------

// parseScanner parses a scanner command's stdout and report files.
func parseScanner(name string, stdout []byte, reports map[string][]byte, dir string) Parsed {
	var p Parsed
	switch name {
	case ScannerGovulncheck:
		p.merge(ParseGovulncheck(stdout))
	case ScannerNpmAudit:
		p.merge(ParseNpmAudit(stdout))
	}
	for _, n := range sortedKeys(reports) {
		b := reports[n]
		switch name {
		case ScannerGosec, ScannerSemgrep, ScannerTrivy:
			p.merge(ParseSARIF(b, dir, name))
		case ScannerPipAudit:
			p.merge(ParsePipAudit(b))
		default:
			p.merge(sniff(n, b, dir))
		}
	}
	if !p.OK && name == ScannerPipAudit {
		p.merge(ParsePipAudit(stdout))
	}
	return p
}

// parseOutput sniffs a regular command's stdout and report files.
func parseOutput(run string, stdout []byte, reports map[string][]byte, dir string) Parsed {
	var p Parsed
	if strings.Contains(run, "-json") || looksLikeGoTestJSON(stdout) {
		p.merge(ParseGoTest(stdout))
	}
	if !p.OK {
		p.merge(sniff("stdout.json", stdout, dir))
	}
	for _, n := range sortedKeys(reports) {
		p.merge(sniff(n, reports[n], dir))
	}
	return p
}

func looksLikeGoTestJSON(b []byte) bool {
	t := bytes.TrimSpace(b)
	return bytes.HasPrefix(t, []byte(`{"Time":`)) || bytes.HasPrefix(t, []byte(`{"ImportPath":`)) || bytes.HasPrefix(t, []byte(`{"Action":`))
}

func sniff(name string, b []byte, dir string) Parsed {
	t := bytes.TrimSpace(b)
	if len(t) == 0 {
		return Parsed{}
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case ext == ".xml" || t[0] == '<':
		return ParseJUnit(t)
	case ext == ".sarif" || bytes.Contains(t[:min(len(t), 4096)], []byte(`"runs"`)) && bytes.Contains(t[:min(len(t), 4096)], []byte(`"version"`)):
		if p := ParseSARIF(t, dir, ""); p.OK {
			return p
		}
	}
	for _, f := range []func() Parsed{
		func() Parsed { return ParseESLint(t, dir) },
		func() Parsed { return ParseGolangci(t, dir) },
		func() Parsed { return ParseNpmAudit(t) },
		func() Parsed { return ParsePipAudit(t) },
	} {
		if p := f(); p.OK {
			return p
		}
	}
	return Parsed{}
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// relPath makes p relative to dir (slash-separated) when it is inside it.
func relPath(dir, p string) string {
	if p == "" {
		return ""
	}
	p = filepath.ToSlash(p)
	if dir != "" {
		d := strings.TrimSuffix(filepath.ToSlash(dir), "/") + "/"
		if len(p) > len(d) && strings.EqualFold(p[:len(d)], d) {
			p = p[len(d):]
		}
	}
	return strings.TrimPrefix(p, "./")
}
