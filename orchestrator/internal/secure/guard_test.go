package secure_test

import (
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
func base64URL(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

// Guard rules for non-test code. Paths are slash-separated, relative to the
// module root. Extend an allowlist only with a review of why that code needs
// the raw capability.
var (
	// Packages that may call Secret.Reveal (later: the agent adapters that
	// pass API keys to the child process environment).
	revealAllowed = []string{
		"internal/secure/",
		// Hands an API key to the matching agent CLI's environment (§16.5).
		"internal/auth/childenv.go",
	}
	// Only the logging package builds slog loggers and handlers; everything
	// else receives a *slog.Logger that already masks.
	slogAllowed = []string{"internal/logging/"}
	// Files that may read the process environment directly.
	envAllowed = []string{
		"internal/cli/cli.go", "internal/doctor/doctor.go",
		// Builds the git child environment with secret variables removed.
		"internal/gitops/exec.go",
		// Builds agent CLI child environments from an allowlist (§16.5).
		"internal/auth/childenv.go",
	}
)

var slogForbidden = map[string]bool{
	"New": true, "NewTextHandler": true, "NewJSONHandler": true, "SetDefault": true, "Default": true,
	"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true, "LogAttrs": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
}

var envForbidden = map[string]map[string]bool{
	"os":      {"Getenv": true, "LookupEnv": true, "Environ": true, "ExpandEnv": true},
	"syscall": {"Getenv": true, "Environ": true},
}

// governedImports are the packages whose calls the guard checks by selector
// (pkg.Func). A dot import would make those calls bare identifiers the guard
// cannot see, so it is rejected for these packages in every file.
var governedImports = map[string]bool{"log": true, "log/slog": true, "os": true, "syscall": true}

func allowed(rel string, list []string) bool {
	for _, a := range list {
		if strings.HasPrefix(rel, a) {
			return true
		}
	}
	return false
}

// violations reports every guard violation in one parsed file.
func violations(fset *token.FileSet, f *ast.File, rel string) []string {
	var out []string
	add := func(n ast.Node, msg string) {
		out = append(out, rel+":"+strconv.Itoa(fset.Position(n.Pos()).Line)+": "+msg)
	}
	imports := map[string]string{} // local name -> import path
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if im.Name != nil {
			name = im.Name.Name
		}
		imports[name] = p
		if name == "." && governedImports[p] {
			add(im, `dot import of "`+p+`" hides calls from the security guard`)
		}
		switch {
		case p == "log":
			add(im, `stdlib "log" bypasses the masking logger`)
		case strings.HasSuffix(p, "/internal/secure/securetest") && !strings.HasPrefix(rel, "internal/secure/securetest/"):
			add(im, "securetest is for tests only")
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Reveal" && !allowed(rel, revealAllowed) {
			add(sel, "Secret.Reveal outside the allowlist")
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch imports[id.Name] {
		case "log/slog":
			if slogForbidden[sel.Sel.Name] && !allowed(rel, slogAllowed) {
				add(sel, "slog."+sel.Sel.Name+" outside internal/logging")
			}
		case "os", "syscall":
			if envForbidden[imports[id.Name]][sel.Sel.Name] && !allowed(rel, envAllowed) {
				add(sel, imports[id.Name]+"."+sel.Sel.Name+" outside the env allowlist")
			}
		}
		return true
	})
	return out
}

func moduleRoot(t *testing.T) string {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSecurityGuards(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && path != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		for _, v := range violations(fset, f, rel) {
			t.Error(v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 20 {
		t.Fatalf("scanned only %d files; wrong root %s?", files, root)
	}
}

// Dot imports of governed packages are rejected everywhere, so their calls
// cannot slip past the selector checks as bare identifiers.
func TestSecurityGuardsRejectDotImports(t *testing.T) {
	src := `package x
import (
	. "os"
	. "log/slog"
	. "strings"
)
func f() {
	_ = Getenv("K")
	Info("x")
	_ = TrimSpace(" ")
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"internal/x/x.go", "internal/logging/x.go", "internal/cli/cli.go"} {
		got := violations(fset, f, rel)
		all := strings.Join(got, "\n")
		if len(got) != 2 || !strings.Contains(all, `"os"`) || !strings.Contains(all, `"log/slog"`) {
			t.Errorf("%s: want 2 dot-import violations (os, log/slog), got %d:\n%s", rel, len(got), all)
		}
	}
}

// The checker itself must catch each kind of violation.
func TestSecurityGuardsSelfTest(t *testing.T) {
	src := `package x
import (
	"log"
	"log/slog"
	lg "log/slog"
	"os"
	"github.com/0xyu/aco/internal/secure/securetest"
)
func f(s interface{ Reveal() string }) {
	_ = s.Reveal()
	slog.Info("x")
	_ = lg.New(nil)
	_ = os.Getenv("K")
	_, _ = os.LookupEnv("K")
	_ = os.Environ()
	log.Print(securetest.FakeAnthropic)
	_ = slog.String("ok", "allowed")
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := violations(fset, f, "internal/x/x.go")
	if len(got) != 8 {
		t.Errorf("want 8 violations, got %d:\n%s", len(got), strings.Join(got, "\n"))
	}
	if v := violations(fset, f, "internal/logging/x.go"); len(v) != 6 {
		t.Errorf("logging may use slog: got %d:\n%s", len(v), strings.Join(v, "\n"))
	}
}
