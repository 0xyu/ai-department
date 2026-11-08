package i18n

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// declaredKeys parses keys.go and returns every Key constant's value.
func declaredKeys(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "keys.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				lit := vs.Values[i].(*ast.BasicLit)
				v, _ := strconv.Unquote(lit.Value)
				keys[n.Name] = v
			}
		}
	}
	return keys
}

func TestCatalogComplete(t *testing.T) {
	keys := declaredKeys(t)
	seen := map[string]string{}
	for name, v := range keys {
		if other, dup := seen[v]; dup {
			t.Errorf("key value %q used by %s and %s", v, name, other)
		}
		seen[v] = name
		if !Has(Key(v)) {
			t.Errorf("key %s (%q) has no catalog entry", name, v)
		}
	}
	for k := range catalog {
		if _, ok := seen[string(k)]; !ok {
			t.Errorf("catalog entry %q has no constant in keys.go", k)
		}
	}
}

var verbRE = regexp.MustCompile(`%[-+# 0]*\d*(\.\d+)?[a-zA-Z%]`)

func verbs(s string) []string {
	var out []string
	for _, v := range verbRE.FindAllString(s, -1) {
		if v != "%%" {
			out = append(out, v[len(v)-1:])
		}
	}
	return out
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func TestCatalogEntries(t *testing.T) {
	for k, m := range catalog {
		if strings.TrimSpace(m.zh) == "" || strings.TrimSpace(m.en) == "" {
			t.Errorf("%s: empty translation", k)
		}
		if strings.Join(verbs(m.zh), "") != strings.Join(verbs(m.en), "") {
			t.Errorf("%s: format verbs differ: zh %v en %v", k, verbs(m.zh), verbs(m.en))
		}
		if hasHan(m.en) {
			t.Errorf("%s: en text contains Chinese: %q", k, m.en)
		}
	}
}

func TestPrinterLanguages(t *testing.T) {
	var zh, en bytes.Buffer
	NewPrinter(&zh, ZhCN).Say(CLIUnknownCommand, "frob")
	NewPrinter(&en, EN).Say(CLIUnknownCommand, "frob")
	if zh.String() != "未知命令：frob\n" || en.String() != "Unknown command: frob\n" {
		t.Errorf("zh=%q en=%q", zh.String(), en.String())
	}
	var def bytes.Buffer
	NewPrinter(&def, "fr").Say(CLIUsageHeader)
	if def.String() != "用法：\n" {
		t.Errorf("unsupported language should fall back to zh-CN, got %q", def.String())
	}
	if got := T(EN, DoctorAuthMode, "Claude Code", M(AuthModeAPI), M(AuthSourceEnv, "ACO_AUTH_MODE")); got !=
		"Claude Code auth mode: API key (api) (from environment variable ACO_AUTH_MODE)" {
		t.Errorf("nested Msg: %q", got)
	}
	if got := T(EN, Key("no.such.key")); got != "!(no.such.key)" {
		t.Errorf("missing key: %q", got)
	}
}

func TestParseLang(t *testing.T) {
	if l, ok := ParseLang("zh-cn"); !ok || l != ZhCN {
		t.Error("zh-cn")
	}
	if _, ok := ParseLang("de"); ok {
		t.Error("de should be unsupported")
	}
}

// TestNoOperatorOutputBypassesCatalog fails if any non-test Go file outside
// this package writes to stdout/stderr directly, or passes a string literal
// instead of a catalog Key to the printer.
func TestNoOperatorOutputBypassesCatalog(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	self, _ := filepath.Abs(".")
	violations, err := findBypasses(root, self)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Errorf("operator output bypasses the i18n catalog: %s", v)
	}
}

// The checker itself must catch each forbidden pattern.
func TestBypassCheckerCatchesViolations(t *testing.T) {
	dir := t.TempDir()
	bad := `package bad

import (
	"fmt"
	"io"
	"os"
)

func f(p interface{ Say(string, ...any) }) {
	fmt.Println("hi")
	fmt.Fprintf(os.Stderr, "x")
	io.WriteString(os.Stdout, "y")
	os.Stdout.WriteString("z")
	println("w")
	p.Say("literal")
}
`
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := findBypasses(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 6 {
		t.Errorf("want 6 violations, got %d: %v", len(v), v)
	}
}

func findBypasses(root, skip string) ([]string, error) {
	forbiddenFmt := map[string]bool{
		"Print": true, "Printf": true, "Println": true,
		"Fprint": true, "Fprintf": true, "Fprintln": true,
	}
	keyFuncs := map[string]bool{"Say": true, "T": true, "M": true}
	var violations []string
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path == skip || name == "testdata" || name == ".git" || name == "bin" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pos := fset.Position(call.Pos())
			where := rel + ":" + strconv.Itoa(pos.Line)
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "print" || fn.Name == "println" {
					violations = append(violations, where+": builtin "+fn.Name)
				}
			case *ast.SelectorExpr:
				if x, ok := fn.X.(*ast.Ident); ok {
					switch {
					case x.Name == "fmt" && forbiddenFmt[fn.Sel.Name]:
						violations = append(violations, where+": fmt."+fn.Sel.Name)
					case x.Name == "io" && fn.Sel.Name == "WriteString":
						violations = append(violations, where+": io.WriteString")
					}
				}
				if inner, ok := fn.X.(*ast.SelectorExpr); ok {
					if pkg, ok := inner.X.(*ast.Ident); ok && pkg.Name == "os" && (inner.Sel.Name == "Stdout" || inner.Sel.Name == "Stderr") {
						violations = append(violations, where+": os."+inner.Sel.Name+"."+fn.Sel.Name)
					}
				}
				if keyFuncs[fn.Sel.Name] && len(call.Args) > 0 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						violations = append(violations, where+": string literal passed to "+fn.Sel.Name)
					}
				}
			}
			return true
		})
		return nil
	})
	return violations, err
}
