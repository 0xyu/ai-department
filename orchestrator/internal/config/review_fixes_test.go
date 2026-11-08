package config

import (
	"strings"
	"testing"
)

const fakeKey = "sk-ant-api03-FAKEFAKEFAKEFAKE0000"

// noLeak fails when any part of any error carries the fake secret.
func noLeak(t *testing.T, es Errors) {
	t.Helper()
	for _, e := range es {
		for _, s := range []string{e.Source, e.Field, e.Value, e.Allowed, e.Error()} {
			if strings.Contains(s, "FAKEFAKE") {
				t.Errorf("secret in error: %+v", e)
			}
		}
	}
}

func TestOverrideErrorsNeverEchoTheValue(t *testing.T) {
	cases := []struct {
		arg, field, source string
		code               ErrCode
	}{
		{"auth.api_key=" + fakeKey, "auth.api_key", "--set auth.api_key", ErrSecretField},
		{"reviewer.token=" + fakeKey, "reviewer.token", "--set reviewer.token", ErrSecretField},
		{fakeKey, "", "--set #1", ErrInvalidOverride},
		{"=" + fakeKey, "", "--set #1", ErrInvalidOverride},
		{"a..b=" + fakeKey, "", "--set #1", ErrInvalidOverride},
		{"planner.model=[" + fakeKey, "planner.model", "--set planner.model", ErrParse},
		{"planner.model=" + fakeKey + "\n---\nx: 1", "planner.model", "--set planner.model", ErrParse},
	}
	for _, tc := range cases {
		es := loadErrs(t, LoadOptions{Overrides: []string{tc.arg}})
		noLeak(t, es)
		e := es[0]
		if e.Code != tc.code || e.Field != tc.field || e.Source != tc.source || e.Layer != LayerFlags {
			t.Errorf("%q: got %+v", tc.arg, e)
		}
	}
	// Position is counted among all --set flags.
	es := loadErrs(t, LoadOptions{Overrides: []string{"git.push=true", fakeKey}})
	if es[0].Source != "--set #2" {
		t.Errorf("ordinal: %+v", es[0])
	}
}

func TestOverrideSourceIsKeyPath(t *testing.T) {
	l := mustLoad(t, LoadOptions{Overrides: []string{"planner.effort=max"}})
	if o, ok := l.Origin("planner.effort"); !ok || o.Source != "--set planner.effort" {
		t.Errorf("origin %+v", o)
	}
}

func TestSingleYAMLDocument(t *testing.T) {
	bad := []struct {
		yaml string
		line int
	}{
		{"planner: {effort: max}\n---\nplanner: {effort: low}\n", 3},
		{"planner: {effort: max}\n---\n", 2},
		{"planner: {effort: max}\n---\n[unclosed\n", 4},
		{"planner: {effort: max}\n---\nkey: [x\n", 4},
	}
	for _, tc := range bad {
		dir := t.TempDir()
		path := writeFile(t, dir, RepoConfigName, tc.yaml)
		es := loadErrs(t, LoadOptions{RepoDir: dir})
		if es[0].Code != ErrParse || es[0].Layer != LayerRepo || es[0].Source != path || es[0].Line == 0 {
			t.Errorf("%q: got %+v", tc.yaml, es[0])
		}
		// --config files share the parser.
		es = loadErrs(t, LoadOptions{ConfigFiles: []string{path}})
		if es[0].Code != ErrParse || es[0].Layer != LayerFile {
			t.Errorf("--config %q: got %+v", tc.yaml, es[0])
		}
		// So does the plan front matter.
		es = loadErrs(t, LoadOptions{PlanFrontMatter: []byte(tc.yaml), PlanSource: "plan.md"})
		if es[0].Code != ErrParse || es[0].Layer != LayerPlan || es[0].Source != "plan.md" {
			t.Errorf("plan %q: got %+v", tc.yaml, es[0])
		}
	}
	if es := loadErrs(t, LoadOptions{Overrides: []string{"planner.effort=max\n---\nlow"}}); es[0].Code != ErrParse {
		t.Errorf("override with two documents: %+v", es[0])
	}

	// Valid single documents, with or without markers, and empty input still load.
	for _, ok := range []string{"", "# only a comment\n", "---\n", "---\nplanner: {effort: max}\n", "planner: {effort: max}\n...\n"} {
		dir := t.TempDir()
		writeFile(t, dir, RepoConfigName, ok)
		mustLoad(t, LoadOptions{RepoDir: dir})
	}
	// Layer precedence is unchanged.
	dir := t.TempDir()
	writeFile(t, dir, RepoConfigName, "---\nplanner: {effort: low}\n")
	l := mustLoad(t, LoadOptions{RepoDir: dir, Overrides: []string{"planner.effort=max"}})
	if l.Config.Planner.Effort != "max" {
		t.Errorf("precedence: %s", l.Config.Planner.Effort)
	}
}

func TestNonFiniteFloatsRejected(t *testing.T) {
	fields := []struct{ yamlPath, field string }{
		{"pricing: {codex: {m1: {input_per_mtok: %s}}}", "pricing.codex.m1.input_per_mtok"},
		{"pricing: {codex: {m1: {output_per_mtok: %s}}}", "pricing.codex.m1.output_per_mtok"},
		{"limits: {budget_usd: {run: %s}}", "limits.budget_usd.run"},
		{"limits: {budget_usd: {planning: %s}}", "limits.budget_usd.planning"},
		{"limits: {budget_usd: {phase: %s}}", "limits.budget_usd.phase"},
		{"limits: {budget_usd: {final: %s}}", "limits.budget_usd.final"},
		{"limits: {budget_usd: {soft_ratio: %s}}", "limits.budget_usd.soft_ratio"},
		{"limits: {agent_calls: {soft_ratio: %s}}", "limits.agent_calls.soft_ratio"},
	}
	for _, v := range []string{".nan", ".NaN", ".inf", "+.inf", "-.inf", ".Inf"} {
		for _, f := range fields {
			dir := t.TempDir()
			writeFile(t, dir, RepoConfigName, strings.Replace(f.yamlPath, "%s", v, 1)+"\n")
			es := loadErrs(t, LoadOptions{RepoDir: dir})
			if !es.Has(f.field, ErrOutOfRange) {
				t.Fatalf("%s=%s: got %v", f.field, v, es)
			}
			for _, e := range es {
				if e.Field == f.field && (e.Layer != LayerRepo || e.Line != 1) {
					t.Errorf("%s=%s not attributed to repo line 1: %+v", f.field, v, e)
				}
			}
			es = loadErrs(t, LoadOptions{Overrides: []string{f.field + "=" + v}})
			// A map key (pricing.codex.<model>) is user-written, so that
			// override is named by position, not by its key.
			wantSrc := "--set " + f.field
			if strings.HasPrefix(f.field, "pricing.codex.") {
				wantSrc = "--set #1"
			}
			if !es.Has(f.field, ErrOutOfRange) || es[0].Layer != LayerFlags || es[0].Source != wantSrc || es[0].Line != 0 {
				t.Errorf("--set %s=%s: got %v", f.field, v, es)
			}
		}
	}
	// Finite boundary values are still accepted.
	mustLoad(t, LoadOptions{Overrides: []string{
		"limits.budget_usd.run=0", "limits.budget_usd.soft_ratio=1", "limits.agent_calls.soft_ratio=0.01",
		"pricing.codex.m1.input_per_mtok=0", "pricing.codex.m1.output_per_mtok=1e6",
	}})
}
