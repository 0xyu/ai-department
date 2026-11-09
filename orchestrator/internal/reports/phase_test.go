package reports

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/0xyu/aco/schemas"
)

func sample() *PhaseReport {
	return &PhaseReport{
		RunID: "20261109-abcd", Phase: 1, Title: "Operation 1", Goal: "Implement operation 1.", PlanVersion: "v1",
		PlanHash: "sha256:" + strings.Repeat("a", 64), BaseCommit: strings.Repeat("b", 40), ReviewedTree: strings.Repeat("c", 40),
		Attempt: 1, Attempts: 1, Sessions: []Session{{Attempt: 1, SessionID: "s-1"}},
		ImplementerModel: "claude-opus-5-5", ReviewerModel: "gpt-6.1-sol",
		AcceptanceCriteria: []AC{{ID: "AC-01.1", Text: "Adds | numbers", Status: "met", Evidence: "TestAdd", Tests: []string{"calc/calc_test.go"}}},
		Requirements:       []string{"REQ-001"},
		Files:              []FileChange{{Path: "calc/calc.go", Status: "modified", Added: 3, Deleted: 1}},
		DiffStat:           " calc/calc.go | 4 +++-\n",
		Gates: []Gate{{Gate: "unit", Status: "passed", Required: true, Source: "phase", Commands: []string{"go test ./..."},
			DurationMS: 1200, Summary: "unit: passed", Baseline: BaselineDelta{HasBaseline: true, FixedTests: 1}}},
		RegressionsSeen: []string{"pkg.TestX"},
		Reviews:         []ReviewRound{{Attempt: 1, Round: 1, Verdict: "approve", GatePassed: true, TreeHash: "t"}},
		Findings: []Finding{{ID: "F-01-001", Attempt: 1, Severity: "minor", Category: "tests", ResolutionClass: "deferred",
			Status: "deferred", Title: "More cases"}},
		Deferred:       []Deferred{{ID: "F-01-001", Severity: "minor", Category: "tests", Title: "More cases", Reason: "severity minor"}},
		Implementation: Implementation{Summary: "Implemented addition.", Decisions: []string{"One package."}},
		Usage: Usage{Agents: []AgentUsage{{Agent: "claude", Role: "implementer", Model: "claude-opus-5-5", AuthMode: "subscription",
			Calls: 1, Turns: 12, InputTokens: 100, OutputTokens: 50, CostUSD: 0.5}}, DurationMS: 60000},
		GeneratedAt: "2026-11-09T12:00:00Z",
	}
}

func TestPhaseReportValidatesAndRoundTrips(t *testing.T) {
	r := sample()
	r.Digest = BuildDigest(r)
	raw, err := r.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if probs := ValidatePhaseJSON(raw); len(probs) > 0 {
		t.Fatalf("schema: %v", probs)
	}
	back, err := ParsePhase(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, r) {
		t.Fatalf("round trip differs:\n%+v\n%+v", back, r)
	}
	// An empty report (nil lists) still validates after normalization.
	empty := &PhaseReport{Phase: 2}
	raw, _ = empty.JSON()
	if probs := ValidatePhaseJSON(raw); len(probs) > 0 {
		t.Fatalf("empty report: %v", probs)
	}
}

func TestParsePhaseRejectsInvalid(t *testing.T) {
	r := sample()
	raw, _ := r.JSON()
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for name, mut := range map[string]func(map[string]any){
		"missing digest":  func(m map[string]any) { delete(m, "digest") },
		"unknown field":   func(m map[string]any) { m["commit_sha"] = "x" },
		"bad format":      func(m map[string]any) { m["format"] = "aco.phase-report/v2" },
		"bad ac status":   func(m map[string]any) { m["acceptance_criteria"].([]any)[0].(map[string]any)["status"] = "done" },
		"null list":       func(m map[string]any) { m["files"] = nil },
		"string as phase": func(m map[string]any) { m["phase"] = "1" },
	} {
		c := map[string]any{}
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &c)
		mut(c)
		b, _ = json.Marshal(c)
		if _, err := ParsePhase(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every Go field is in the schema and every schema property is a Go field,
// with all properties required (strict structured documents).
func TestPhaseSchemaMatchesType(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(schemas.PhaseReport, &root); err != nil {
		t.Fatal(err)
	}
	var check func(path string, node map[string]any, typ reflect.Type)
	check = func(path string, node map[string]any, typ reflect.Type) {
		for typ.Kind() == reflect.Slice || typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		props, _ := node["properties"].(map[string]any)
		var req []string
		for _, r := range node["required"].([]any) {
			req = append(req, r.(string))
		}
		var fields []string
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			fields = append(fields, name)
			p, ok := props[name].(map[string]any)
			if !ok {
				t.Errorf("%s.%s: not in the schema", path, name)
				continue
			}
			if p["type"] == "array" {
				if items, ok := p["items"].(map[string]any); ok {
					check(path+"."+name+"[]", items, f.Type)
				}
			} else {
				check(path+"."+name, p, f.Type)
			}
		}
		sort.Strings(fields)
		sort.Strings(req)
		if !slices.Equal(fields, req) {
			t.Errorf("%s: fields %v, required %v", path, fields, req)
		}
		if node["additionalProperties"] != false {
			t.Errorf("%s: additionalProperties must be false", path)
		}
	}
	check("$", root, reflect.TypeOf(PhaseReport{}))
}

func TestReadPhase(t *testing.T) {
	dir := t.TempDir()
	r := sample()
	raw, _ := r.JSON()
	_, js := PhasePaths(1)
	p := filepath.Join(dir, filepath.FromSlash(js))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPhase(dir, 1); err != nil || got.Title != r.Title {
		t.Fatalf("ReadPhase: %v", err)
	}
	if _, err := ReadPhase(dir, 2); err == nil {
		t.Fatal("missing report accepted")
	}
	// A report for another phase under this phase's name is refused.
	_, js2 := PhasePaths(2)
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(js2)), raw, 0o644)
	if _, err := ReadPhase(dir, 2); err == nil {
		t.Fatal("phase mismatch accepted")
	}
}

func TestReadPhaseRefusesLinkedDir(t *testing.T) {
	target := t.TempDir()
	r := sample()
	raw, _ := r.JSON()
	if err := os.MkdirAll(filepath.Join(target, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(target, "reports", "phase-01.json"), raw, 0o644)
	repo := t.TempDir()
	link := filepath.Join(repo, ".aco")
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on Windows")
		}
		t.Fatal(err)
	}
	if _, err := ReadPhase(repo, 1); err == nil {
		t.Fatal("a report behind a linked .aco was read")
	}
}

func TestDigestBounded(t *testing.T) {
	r := sample()
	for i := 0; i < 500; i++ {
		r.Files = append(r.Files, FileChange{Path: strings.Repeat("d/", 20) + "f.go", Status: "added"})
		r.Implementation.Decisions = append(r.Implementation.Decisions, strings.Repeat("decision ", 100))
	}
	d := BuildDigest(r)
	if len(d) > MaxDigestChars || !strings.Contains(d, "Phase 01") || !strings.Contains(d, "more") {
		t.Fatalf("digest %d bytes", len(d))
	}
	if s := clip("ab€cd", 5); s != "ab…" {
		t.Fatalf("clip %q", s)
	}
}

func TestMarkdownHasEverySection(t *testing.T) {
	r := sample()
	r.Digest = BuildDigest(r)
	md := RenderPhaseMarkdown(r)
	for _, s := range []string{"# Phase 01: Operation 1", "## Acceptance criteria", `Adds \| numbers`, "## Requirements covered",
		"## Files changed", "## Quality gates", "pkg.TestX", "## Security", "## Review history", "### Findings",
		"## Deferred items", "F-01-001", "## Implementation summary", "## Usage", "estimate, not charged", "## Digest"} {
		if !strings.Contains(md, s) {
			t.Errorf("markdown lacks %q", s)
		}
	}
}
