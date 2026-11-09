// Package apidiff parses unified Git diffs and extracts changed public APIs
// and exports for Go, TypeScript/JavaScript, and Python (§11.6 regression
// data). The extraction is a line-based heuristic over the diff, not a
// parser of the languages: it sees declarations whose signature line was
// added, removed, or changed, which is what a reviewer needs to look for
// broken callers.
package apidiff

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// File is one file of a diff.
type File struct {
	OldPath, NewPath string // "/dev/null" for added / deleted files
	Binary           bool
	Hunks            []Hunk
	Header           string // the "diff --git" header lines up to the first hunk
}

// Path is the file's current path (the old one for deletions).
func (f *File) Path() string {
	if f.NewPath != "" && f.NewPath != "/dev/null" {
		return f.NewPath
	}
	return f.OldPath
}

// Hunk is one "@@" section.
type Hunk struct {
	Header             string // "@@ -1,3 +1,4 @@ func x"
	OldStart, NewStart int
	Lines              []string // with their " ", "+", "-" prefix
}

// Text is the hunk with its header.
func (h *Hunk) Text() string { return h.Header + "\n" + strings.Join(h.Lines, "\n") + "\n" }

// Added and Removed count the hunk's changed lines.
func (h *Hunk) Added() (n int) {
	for _, l := range h.Lines {
		if strings.HasPrefix(l, "+") {
			n++
		}
	}
	return n
}

// Removed counts removed lines.
func (h *Hunk) Removed() (n int) {
	for _, l := range h.Lines {
		if strings.HasPrefix(l, "-") {
			n++
		}
	}
	return n
}

var hunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

func unquote(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, `"`) && strings.HasSuffix(p, `"`) && len(p) > 1 {
		p = p[1 : len(p)-1]
	}
	for _, pre := range []string{"a/", "b/"} {
		if strings.HasPrefix(p, pre) {
			return p[len(pre):]
		}
	}
	return p
}

// Parse splits a unified diff (git diff output) into files and hunks.
func Parse(diff string) []File {
	var files []File
	var cur *File
	var hunk *Hunk
	inHeader := false
	for _, l := range strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			files = append(files, File{Header: l})
			cur, hunk, inHeader = &files[len(files)-1], nil, true
			rest := strings.TrimPrefix(l, "diff --git ")
			if i := strings.Index(rest, " b/"); i >= 0 {
				cur.OldPath, cur.NewPath = unquote(rest[:i]), unquote(rest[i+1:])
			}
		case cur == nil:
			continue
		case inHeader && strings.HasPrefix(l, "--- "):
			cur.OldPath = unquote(strings.TrimPrefix(l, "--- "))
			cur.Header += "\n" + l
		case inHeader && strings.HasPrefix(l, "+++ "):
			cur.NewPath = unquote(strings.TrimPrefix(l, "+++ "))
			cur.Header += "\n" + l
		case strings.HasPrefix(l, "@@"):
			inHeader = false
			h := Hunk{Header: l}
			if m := hunkRE.FindStringSubmatch(l); m != nil {
				fmt.Sscan(m[1], &h.OldStart)
				fmt.Sscan(m[2], &h.NewStart)
			}
			cur.Hunks = append(cur.Hunks, h)
			hunk = &cur.Hunks[len(cur.Hunks)-1]
		case inHeader:
			if strings.HasPrefix(l, "Binary files") || l == "GIT binary patch" {
				cur.Binary = true
			}
			cur.Header += "\n" + l
		case hunk != nil && (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") || strings.HasPrefix(l, `\`)):
			hunk.Lines = append(hunk.Lines, l)
		}
	}
	return files
}

// Change is one changed public declaration.
type Change struct {
	Lang   string // go | ts | py
	File   string
	Symbol string
	Kind   string // added | removed | changed
	Old    string // the old signature line (removed / changed)
	New    string // the new signature line (added / changed)
}

type rule struct {
	lang  string
	match func(file string) bool
	decl  func(line string) (symbol string, ok bool)
}

var (
	goFunc   = regexp.MustCompile(`^func\s+(?:\(\s*[A-Za-z_]\w*\s+\*?\s*([A-Za-z_]\w*)(?:\[[^\]]*\])?\s*\)\s*)?([A-Za-z_]\w*)\s*[\[(]`)
	goType   = regexp.MustCompile(`^type\s+([A-Za-z_]\w*)\b`)
	goVar    = regexp.MustCompile(`^(?:var|const)\s+([A-Za-z_]\w*)\b`)
	tsExport = regexp.MustCompile(`^export\s+(?:declare\s+)?(?:default\s+)?(?:abstract\s+)?(?:async\s+)?(function\*?|class|const|let|var|interface|type|enum|namespace)\s+([A-Za-z_$][\w$]*)`)
	tsNamed  = regexp.MustCompile(`^export\s*\{([^}]*)\}`)
	pyDecl   = regexp.MustCompile(`^(?:async\s+)?(def|class)\s+([A-Za-z]\w*)`)
	pyAll    = regexp.MustCompile(`^__all__\s*[+]?=`)
)

func exportedGo(name string) bool { return name != "" && name[0] >= 'A' && name[0] <= 'Z' }

var rules = []rule{
	{lang: "go", match: func(f string) bool { return strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") },
		decl: func(l string) (string, bool) {
			if m := goFunc.FindStringSubmatch(l); m != nil {
				if !exportedGo(m[2]) || (m[1] != "" && !exportedGo(m[1])) {
					return "", false
				}
				if m[1] != "" {
					return m[1] + "." + m[2], true
				}
				return m[2], true
			}
			for _, re := range []*regexp.Regexp{goType, goVar} {
				if m := re.FindStringSubmatch(l); m != nil && exportedGo(m[1]) {
					return m[1], true
				}
			}
			return "", false
		}},
	{lang: "ts", match: func(f string) bool {
		switch path.Ext(f) {
		case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
			return !strings.Contains(f, ".test.") && !strings.Contains(f, ".spec.")
		}
		return false
	}, decl: func(l string) (string, bool) {
		if m := tsExport.FindStringSubmatch(l); m != nil {
			return m[2], true
		}
		if m := tsNamed.FindStringSubmatch(l); m != nil {
			return "{" + strings.Join(strings.Fields(m[1]), " ") + "}", true
		}
		return "", false
	}},
	{lang: "py", match: func(f string) bool {
		b := path.Base(f)
		return strings.HasSuffix(f, ".py") && !strings.HasPrefix(b, "test_") && !strings.HasSuffix(b, "_test.py") && !strings.Contains(f, "/tests/")
	}, decl: func(l string) (string, bool) {
		if m := pyDecl.FindStringSubmatch(l); m != nil && !strings.HasPrefix(m[2], "_") {
			return m[2], true
		}
		if pyAll.MatchString(l) {
			return "__all__", true
		}
		return "", false
	}},
}

// Extract returns the changed public declarations of a diff, sorted by
// file and symbol.
func Extract(files []File) []Change {
	var out []Change
	for _, f := range files {
		p := f.Path()
		var r *rule
		for i := range rules {
			if rules[i].match(p) {
				r = &rules[i]
				break
			}
		}
		if r == nil || f.Binary {
			continue
		}
		oldDecl, newDecl := map[string]string{}, map[string]string{}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if len(l) == 0 || (l[0] != '+' && l[0] != '-') {
					continue
				}
				body := l[1:]
				sym, ok := r.decl(body)
				if !ok {
					continue
				}
				sig := strings.TrimSpace(body)
				if l[0] == '+' {
					newDecl[sym] = sig
				} else {
					oldDecl[sym] = sig
				}
			}
		}
		syms := map[string]bool{}
		for s := range oldDecl {
			syms[s] = true
		}
		for s := range newDecl {
			syms[s] = true
		}
		for s := range syms {
			o, okO := oldDecl[s]
			n, okN := newDecl[s]
			c := Change{Lang: r.lang, File: p, Symbol: s, Old: o, New: n}
			switch {
			case okO && okN && o == n:
				continue // moved, not changed
			case okO && okN:
				c.Kind = "changed"
			case okN:
				c.Kind = "added"
			default:
				c.Kind = "removed"
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}

// Render lists changes for a context pack ("(none)" when empty).
func Render(cs []Change) string {
	if len(cs) == 0 {
		return "(none detected)"
	}
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(fmt.Sprintf("- %s %s %s (%s)\n", c.Kind, c.File, c.Symbol, c.Lang))
		if c.Old != "" {
			b.WriteString(fmt.Sprintf("    old: %s\n", c.Old))
		}
		if c.New != "" {
			b.WriteString(fmt.Sprintf("    new: %s\n", c.New))
		}
	}
	return b.String()
}
