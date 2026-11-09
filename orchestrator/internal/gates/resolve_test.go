package gates

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/plan"
)

func lookAll(name string) (string, error) { return "/usr/bin/" + name, nil }

func lookOnly(names ...string) LookPathFunc {
	return func(n string) (string, error) {
		if slices.Contains(names, n) {
			return "/usr/bin/" + n, nil
		}
		return "", errors.New("not found")
	}
}

func fixture(name string) string { return filepath.Join("testdata", "fixtures", name) }

func runs(cs []Command) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Run)
	}
	return out
}

// AC-06.1: the Go, Node, and Python fixture repositories are auto-detected
// for all six gates, and the preflight passes when the tools exist.
func TestAutoDetectFixtures(t *testing.T) {
	cases := []struct {
		name     string
		want     map[string]string // gate -> a command it must contain
		scanners []string
	}{
		{"go", map[string]string{
			Typecheck: "go vet ./...", Lint: "gofmt -l .", Unit: "go test ./... -short -json",
			Integration: "go test -tags=integration ./... -json", Build: "go build ./...",
		}, []string{ScannerSecrets, ScannerGosec, ScannerGovulncheck}},
		{"node", map[string]string{
			Typecheck: "npx --no-install tsc --noEmit", Lint: "npm run lint", Unit: "npm test",
			Integration: "npm run test:integration", Build: "npm run build", Security: "npm audit --json",
		}, []string{ScannerSecrets, ScannerNpmAudit}},
		{"python", map[string]string{
			Typecheck: "python -m mypy .", Lint: "ruff check .", Unit: `python -m pytest -m "not integration"`,
			Integration: "python -m pytest -m integration", Build: "python -m compileall -q .",
			Security: "pip-audit -r requirements.txt -f json",
		}, []string{ScannerSecrets, ScannerPipAudit}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default().Gates
			specs, probs := Resolve(ResolveInput{Mode: ModePhase, Config: cfg, Dir: fixture(c.name), LookPath: lookAll})
			if len(probs) > 0 {
				t.Fatalf("preflight problems: %v", probs)
			}
			if len(specs) != 6 {
				t.Fatalf("%d specs", len(specs))
			}
			for i, s := range specs {
				if s.Type != Types[i] || !s.Required {
					t.Errorf("spec %d = %s required=%v", i, s.Type, s.Required)
				}
				if len(s.Commands) == 0 && s.Type != Security {
					t.Errorf("%s: no command", s.Type)
				}
				if s.Source != SourceAuto {
					t.Errorf("%s: source %q", s.Type, s.Source)
				}
				if w, ok := c.want[s.Type]; ok && !strings.Contains(strings.Join(runs(s.Commands), "\n"), w) {
					t.Errorf("%s commands %q lack %q", s.Type, runs(s.Commands), w)
				}
			}
			sec := specs[5]
			if !reflect.DeepEqual(sec.Scanners, c.scanners) {
				t.Errorf("scanners = %v, want %v", sec.Scanners, c.scanners)
			}
		})
	}
}

// Without optional tools, only core toolchain commands are proposed.
func TestAutoDetectOptionalTools(t *testing.T) {
	d := Detect(fixture("go"), lookOnly("go"))
	if got := runs(d.Commands[Lint]); len(got) != 1 || !strings.Contains(got[0], "gofmt") {
		t.Errorf("lint = %v", got)
	}
	if !reflect.DeepEqual(d.Scanners, []string{ScannerSecrets}) {
		t.Errorf("scanners = %v", d.Scanners)
	}
	d = Detect(fixture("go"), lookOnly("go", "golangci-lint", "staticcheck"))
	if got := runs(d.Commands[Lint]); len(got) != 2 || got[1] != "golangci-lint run" {
		t.Errorf("lint with golangci = %v", got)
	}
	if d := Detect(t.TempDir(), lookAll); len(d.Languages) != 0 || len(d.Commands) != 0 {
		t.Errorf("empty dir: %+v", d)
	}
}

func TestDetectMakeFallback(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Makefile", "build:\n\tcc -o x x.c\ntest: build\n\t./x --test\nlint:\n\tcheck\nVAR := 1\n")
	d := Detect(dir, nil)
	want := map[string][]string{Build: {"make build"}, Unit: {"make test"}, Lint: {"make lint"}}
	for g, w := range want {
		if !reflect.DeepEqual(runs(d.Commands[g]), w) {
			t.Errorf("%s = %v, want %v", g, runs(d.Commands[g]), w)
		}
	}
	if len(d.Commands[Integration]) != 0 {
		t.Errorf("integration = %v", d.Commands[Integration])
	}
}

func phaseWith(gates ...plan.GateEntry) *plan.Phase {
	return &plan.Phase{Number: 1, ID: "P01", Gates: gates}
}

// Phase Specification → config → auto-detection.
func TestResolvePrecedence(t *testing.T) {
	cfg := config.Default().Gates
	cfg.Unit.Commands = []string{"go test ./internal/... -json"}
	ph := phaseWith(
		plan.GateEntry{Type: Typecheck, Commands: []string{"go vet ./internal/gates/..."}},
		plan.GateEntry{Type: Integration, NotApplicable: true, Justification: "no services in this phase"},
		plan.GateEntry{Type: Security, Scanners: []string{"secrets", "gosec"}},
	)
	specs, probs := Resolve(ResolveInput{Phase: ph, Config: cfg, Dir: fixture("go"), LookPath: lookAll})
	if len(probs) > 0 {
		t.Fatal(probs)
	}
	by := map[string]Spec{}
	for _, s := range specs {
		by[s.Type] = s
	}
	if s := by[Typecheck]; s.Source != SourcePhase || runs(s.Commands)[0] != "go vet ./internal/gates/..." {
		t.Errorf("typecheck = %+v", s)
	}
	if s := by[Unit]; s.Source != SourceConfig || runs(s.Commands)[0] != "go test ./internal/... -json" {
		t.Errorf("unit = %+v", s)
	}
	if s := by[Build]; s.Source != SourceAuto {
		t.Errorf("build = %+v", s)
	}
	if s := by[Integration]; s.Required || s.Justification != "no services in this phase" {
		t.Errorf("integration = %+v", s)
	}
	if s := by[Security]; !reflect.DeepEqual(s.Scanners, []string{"secrets", "gosec"}) || len(s.Commands) != 1 || s.Commands[0].Scanner != ScannerGosec {
		t.Errorf("security = %+v", s)
	}
	if by[Unit].Timeout != DefaultTimeout {
		t.Errorf("timeout = %v", by[Unit].Timeout)
	}
}

// A required gate with no command, an unjustified not_applicable, and a
// missing program each fail the preflight.
func TestResolvePreflightProblems(t *testing.T) {
	cfg := config.Default().Gates
	cfg.AutoDetect = false
	cfg.Build.Commands = []string{"definitely-missing-tool build", "go build ./..."}
	ph := phaseWith(plan.GateEntry{Type: Lint, NotApplicable: true})
	_, probs := Resolve(ResolveInput{Phase: ph, Config: cfg, Dir: t.TempDir(), LookPath: lookOnly("go")})
	codes := map[string]string{}
	for _, p := range probs {
		codes[p.Gate+"/"+p.Code] = p.Program
	}
	for _, want := range []string{
		Typecheck + "/" + ProblemUnresolved, Unit + "/" + ProblemUnresolved, Integration + "/" + ProblemUnresolved,
		Lint + "/" + ProblemUnjustified, Build + "/" + ProblemCommandNotFound,
	} {
		if _, ok := codes[want]; !ok {
			t.Errorf("missing problem %s in %v", want, probs)
		}
	}
	if codes[Build+"/"+ProblemCommandNotFound] != "definitely-missing-tool" {
		t.Errorf("program = %q", codes[Build+"/"+ProblemCommandNotFound])
	}
	for _, p := range probs {
		if p.Code == ProblemCommandNotFound && p.Class != ClassEnvironment {
			t.Errorf("command_not_found class = %s", p.Class)
		}
		if p.Gate == Security {
			t.Errorf("security gate always resolves (built-in secrets): %v", p)
		}
	}
	cfg.Security.Scanners = []string{"nope"}
	_, probs = Resolve(ResolveInput{Config: cfg, Dir: t.TempDir()})
	found := false
	for _, p := range probs {
		found = found || p.Code == ProblemUnknownScanner
	}
	if !found {
		t.Error("unknown scanner not reported")
	}
}

// Stage 4 adds every phase's declared commands to the whole-project ones.
func TestResolveFullMode(t *testing.T) {
	cfg := config.Default().Gates
	p1 := phaseWith(plan.GateEntry{Type: Unit, Commands: []string{"go test ./a/... -json"}},
		plan.GateEntry{Type: Integration, NotApplicable: true, Justification: "none"})
	p2 := &plan.Phase{Number: 2, ID: "P02", Gates: []plan.GateEntry{{Type: Unit, Commands: []string{"go test ./... -short -json"}},
		{Type: Integration, NotApplicable: true, Justification: "still none"}}}
	specs, _ := Resolve(ResolveInput{Mode: ModeFull, Phases: []*plan.Phase{p1, p2}, Config: cfg, Dir: t.TempDir(), LookPath: lookAll})
	by := map[string]Spec{}
	for _, s := range specs {
		by[s.Type] = s
	}
	if got := runs(by[Unit].Commands); !reflect.DeepEqual(got, []string{"go test ./a/... -json", "go test ./... -short -json"}) {
		t.Errorf("unit = %v", got)
	}
	if s := by[Integration]; s.Required || !strings.Contains(s.Justification, "P01: none") || !strings.Contains(s.Justification, "P02: still none") {
		t.Errorf("integration = %+v", s)
	}
}

func TestPrograms(t *testing.T) {
	cases := map[string][]string{
		"go vet ./...":                          {"go"},
		`test -z "$(gofmt -l .)"`:               {"gofmt"},
		"CGO_ENABLED=0 go build ./...":          {"go"},
		"npm ci && npm test | tee out.txt":      {"npm", "tee"},
		`out=$(gofmt -l .); test -z "$out"`:     {"gofmt"},
		"cd sub; make lint || exit 1":           {"make"},
		`python -m pytest -m "not integration"`: {"python"},
		"$TOOL run":                             nil,
	}
	for in, want := range cases {
		if got := Programs(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Programs(%q) = %v, want %v", in, got, want)
		}
	}
}
