package gates

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Auto-detection (§11.5): commands for the six gates from the files at the
// repository root. Go, Node, Python, and Rust projects are recognized by
// their manifest; a Makefile fills gates no language detector resolved.
// Several languages in one repository contribute commands side by side.
//
// Core toolchain commands (go, npm, python, cargo) are always proposed; the
// preflight then fails with class environment if the program is missing.
// Optional tools (golangci-lint, staticcheck, gosec, govulncheck, ruff, …)
// are proposed only when LookPath finds them, so a missing optional linter
// never blocks a run.

// Detection is what auto-detection found.
type Detection struct {
	Languages []string             `json:"languages"` // go, node, python, rust, make
	Commands  map[string][]Command `json:"commands"`  // gate type -> commands
	Scanners  []string             `json:"scanners"`  // security scanners (always includes secrets)
}

// LookPathFunc reports whether a program can be found (exec.LookPath).
type LookPathFunc func(name string) (string, error)

// maxManifest bounds manifest files read during detection.
const maxManifest = 1 << 20

// maxDetectWalk bounds the files visited when looking for integration tests.
const maxDetectWalk = 20000

// Detect inspects dir and proposes gate commands.
func Detect(dir string, look LookPathFunc) *Detection {
	d := &Detection{Commands: map[string][]Command{}}
	has := func(name string) bool {
		if look == nil {
			return false
		}
		_, err := look(name)
		return err == nil
	}
	add := func(gate string, cmds ...Command) {
		for _, c := range cmds {
			if !containsCommand(d.Commands[gate], c) {
				d.Commands[gate] = append(d.Commands[gate], c)
			}
		}
	}
	scanners := map[string]bool{ScannerSecrets: true}

	if fileExists(dir, "go.mod") {
		d.Languages = append(d.Languages, "go")
		add(Typecheck, Command{Run: "go vet ./..."})
		add(Lint, Command{Run: `out=$(gofmt -l .); test -z "$out" || { echo "gofmt: files need formatting:"; echo "$out"; exit 1; }`})
		switch {
		case has("golangci-lint"):
			add(Lint, Command{Run: "golangci-lint run"})
		case has("staticcheck"):
			add(Lint, Command{Run: "staticcheck ./..."})
		}
		add(Unit, Command{Run: "go test ./... -short -json"})
		if goHasIntegrationTag(dir) {
			add(Integration, Command{Run: "go test -tags=integration ./... -json"})
		}
		add(Build, Command{Run: "go build ./..."})
		if has("govulncheck") {
			scanners[ScannerGovulncheck] = true
		}
		if has("gosec") {
			scanners[ScannerGosec] = true
		}
	}

	if pkg, ok := readPackageJSON(dir); ok {
		d.Languages = append(d.Languages, "node")
		script := func(names ...string) string {
			for _, n := range names {
				if _, ok := pkg.Scripts[n]; ok {
					return n
				}
			}
			return ""
		}
		run := func(s string) Command {
			if s == "test" {
				return Command{Run: "npm test"}
			}
			return Command{Run: "npm run " + s}
		}
		if s := script("typecheck", "type-check", "tsc"); s != "" {
			add(Typecheck, run(s))
		} else if fileExists(dir, "tsconfig.json") {
			add(Typecheck, Command{Run: "npx --no-install tsc --noEmit"})
		}
		if s := script("lint"); s != "" {
			add(Lint, run(s))
		} else if hasESLintConfig(dir, pkg) {
			add(Lint, Command{Run: "npx --no-install eslint . -f json"})
		}
		if s := script("test:unit", "unit", "test"); s != "" && !isNoTestScript(pkg.Scripts[s]) {
			add(Unit, run(s))
		}
		if s := script("test:integration", "integration", "test:e2e", "e2e"); s != "" {
			add(Integration, run(s))
		}
		if s := script("build"); s != "" {
			add(Build, run(s))
		}
		if fileExists(dir, "package-lock.json") || fileExists(dir, "npm-shrinkwrap.json") {
			scanners[ScannerNpmAudit] = true
		}
	}

	if py := detectPython(dir); py != nil {
		d.Languages = append(d.Languages, "python")
		switch {
		case py.mypy:
			add(Typecheck, Command{Run: "python -m mypy ."})
		case py.pyright || has("pyright"):
			add(Typecheck, Command{Run: "pyright"})
		case has("mypy"):
			add(Typecheck, Command{Run: "python -m mypy ."})
		}
		switch {
		case py.ruff || has("ruff"):
			add(Lint, Command{Run: "ruff check ."})
		case py.flake8 || has("flake8"):
			add(Lint, Command{Run: "python -m flake8"})
		}
		if py.integrationMarker {
			add(Unit, Command{Run: `python -m pytest -m "not integration" --junitxml={reports}/pytest-unit.xml`})
			add(Integration, Command{Run: `python -m pytest -m integration --junitxml={reports}/pytest-integration.xml`})
		} else {
			add(Unit, Command{Run: "python -m pytest --junitxml={reports}/pytest-unit.xml"})
			if py.integrationDir != "" {
				add(Integration, Command{Run: "python -m pytest " + py.integrationDir + " --junitxml={reports}/pytest-integration.xml"})
			}
		}
		add(Build, Command{Run: "python -m compileall -q ."})
		if has("pip-audit") || py.requirements {
			scanners[ScannerPipAudit] = true
		}
	}

	if fileExists(dir, "Cargo.toml") {
		d.Languages = append(d.Languages, "rust")
		add(Typecheck, Command{Run: "cargo check --all-targets"})
		add(Lint, Command{Run: "cargo clippy --all-targets -- -D warnings"})
		add(Unit, Command{Run: "cargo test --lib --bins"})
		if dirExists(dir, "tests") {
			add(Integration, Command{Run: "cargo test --tests"})
		}
		add(Build, Command{Run: "cargo build --release"})
	}

	if targets := makeTargets(dir); len(targets) > 0 {
		d.Languages = append(d.Languages, "make")
		for gate, names := range map[string][]string{
			Typecheck:   {"typecheck", "type-check", "vet", "check"},
			Lint:        {"lint"},
			Unit:        {"test-unit", "unit", "test"},
			Integration: {"test-integration", "integration", "integration-test", "e2e"},
			Build:       {"build"},
		} {
			if len(d.Commands[gate]) > 0 {
				continue
			}
			for _, n := range names {
				if targets[n] {
					add(gate, Command{Run: "make " + n})
					break
				}
			}
		}
	}

	if has("semgrep") && fileExists(dir, ".semgrep.yml") {
		scanners[ScannerSemgrep] = true
	}
	for s := range scanners {
		d.Scanners = append(d.Scanners, s)
	}
	sort.Strings(d.Scanners)
	return d
}

func containsCommand(cs []Command, c Command) bool {
	for _, x := range cs {
		if x == c {
			return true
		}
	}
	return false
}

// regularFile reports whether name under dir is a regular file (symlinks are
// not followed).
func fileExists(dir, name string) bool {
	fi, err := os.Lstat(filepath.Join(dir, name))
	return err == nil && fi.Mode().IsRegular()
}

func dirExists(dir, name string) bool {
	fi, err := os.Lstat(filepath.Join(dir, name))
	return err == nil && fi.IsDir()
}

func readSmall(dir, name string) ([]byte, bool) {
	p := filepath.Join(dir, name)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxManifest {
		return nil, false
	}
	b, err := os.ReadFile(p) // #nosec G304 -- a fixed manifest name in the worktree
	return b, err == nil
}

type packageJSON struct {
	Scripts      map[string]string `json:"scripts"`
	ESLintConfig json.RawMessage   `json:"eslintConfig"`
}

func readPackageJSON(dir string) (*packageJSON, bool) {
	b, ok := readSmall(dir, "package.json")
	if !ok {
		return nil, false
	}
	var p packageJSON
	if json.Unmarshal(b, &p) != nil {
		return nil, false
	}
	if p.Scripts == nil {
		p.Scripts = map[string]string{}
	}
	return &p, true
}

// isNoTestScript recognizes npm init's placeholder test script.
func isNoTestScript(s string) bool { return strings.Contains(s, "no test specified") }

func hasESLintConfig(dir string, pkg *packageJSON) bool {
	if len(pkg.ESLintConfig) > 0 {
		return true
	}
	for _, n := range []string{"eslint.config.js", "eslint.config.mjs", "eslint.config.cjs", "eslint.config.ts",
		".eslintrc", ".eslintrc.js", ".eslintrc.cjs", ".eslintrc.json", ".eslintrc.yml", ".eslintrc.yaml"} {
		if fileExists(dir, n) {
			return true
		}
	}
	return false
}

type pythonProject struct {
	mypy, pyright, ruff, flake8 bool
	integrationMarker           bool
	integrationDir              string
	requirements                bool
}

var pytestIntegrationMarker = regexp.MustCompile(`(?m)^\s*["']?integration\b`)

func detectPython(dir string) *pythonProject {
	py := &pythonProject{}
	found := false
	if b, ok := readSmall(dir, "pyproject.toml"); ok {
		found = true
		s := string(b)
		py.mypy = strings.Contains(s, "[tool.mypy]")
		py.pyright = strings.Contains(s, "[tool.pyright]")
		py.ruff = strings.Contains(s, "[tool.ruff")
		if i := strings.Index(s, "markers"); i >= 0 && strings.Contains(s, "[tool.pytest") {
			py.integrationMarker = pytestIntegrationMarker.MatchString(s[i:])
		}
	}
	for _, n := range []string{"setup.py", "setup.cfg", "requirements.txt"} {
		if fileExists(dir, n) {
			found = true
		}
	}
	if !found {
		return nil
	}
	py.requirements = fileExists(dir, "requirements.txt")
	if b, ok := readSmall(dir, "pytest.ini"); ok && pytestIntegrationMarker.Match(b) {
		py.integrationMarker = true
	}
	if b, ok := readSmall(dir, "setup.cfg"); ok {
		s := string(b)
		py.mypy = py.mypy || strings.Contains(s, "[mypy]")
		py.flake8 = strings.Contains(s, "[flake8]")
	}
	py.mypy = py.mypy || fileExists(dir, "mypy.ini")
	py.pyright = py.pyright || fileExists(dir, "pyrightconfig.json")
	py.ruff = py.ruff || fileExists(dir, "ruff.toml") || fileExists(dir, ".ruff.toml")
	py.flake8 = py.flake8 || fileExists(dir, ".flake8")
	for _, d := range []string{"tests/integration", "test/integration", "integration_tests"} {
		if dirExists(dir, filepath.FromSlash(d)) {
			py.integrationDir = d
			break
		}
	}
	return py
}

var goIntegrationTag = regexp.MustCompile(`(?m)^//go:build\b.*\bintegration\b`)

// goHasIntegrationTag reports whether any _test.go file has an integration
// build constraint.
func goHasIntegrationTag(dir string) bool {
	found, visited := false, 0
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		if e.IsDir() {
			n := e.Name()
			if p != dir && (strings.HasPrefix(n, ".") || n == "vendor" || n == "node_modules" || n == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if visited++; visited > maxDetectWalk {
			return filepath.SkipAll
		}
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), "_test.go") {
			return nil
		}
		f, err := os.Open(p) // #nosec G304 -- walking the worktree
		if err != nil {
			return nil
		}
		defer f.Close()
		head := make([]byte, 4096)
		n, _ := f.Read(head)
		if goIntegrationTag.Match(head[:n]) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

var makeTargetRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_.-]*)\s*:([^=]|$)`)

func makeTargets(dir string) map[string]bool {
	var b []byte
	for _, n := range []string{"Makefile", "makefile", "GNUmakefile"} {
		if x, ok := readSmall(dir, n); ok {
			b = x
			break
		}
	}
	if b == nil {
		return nil
	}
	out := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), maxManifest)
	for sc.Scan() {
		if m := makeTargetRE.FindStringSubmatch(sc.Text()); m != nil {
			out[m[1]] = true
		}
	}
	return out
}
