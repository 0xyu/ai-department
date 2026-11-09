package engine

import (
	_ "embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/plan"
)

// PlanFormatGuide is the ACO plan-format guide given to the planner (§10.1).
//
//go:embed planformat.md
var PlanFormatGuide string

// Overview limits keep the repository overview a summary: the agents can
// read the repository themselves.
const (
	maxOverviewFiles = 300
	maxOverviewTests = 60
	maxOverviewWalk  = 20000
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true, "__pycache__": true,
	"target": true, "dist": true, "build": true, ".idea": true, ".vscode": true,
}

func isTestFile(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	switch {
	case strings.HasSuffix(base, "_test.go"), strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.HasSuffix(base, "_test.py"), strings.Contains(base, ".test."), strings.Contains(base, ".spec."):
		return true
	}
	for _, seg := range strings.Split(strings.ToLower(filepath.ToSlash(filepath.Dir(rel))), "/") {
		if seg == "tests" || seg == "test" || seg == "__tests__" {
			return true
		}
	}
	return false
}

// RepoOverview summarizes a repository for the planner and the plan
// reviewer (§10.1, §10.3): detected languages, a bounded file list, and the
// existing tests. Symbolic links are listed but never followed.
func RepoOverview(dir string, look gates.LookPathFunc) string {
	var files, tests []string
	total := 0
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if e.IsDir() {
			if rel != "." && (skipDirs[e.Name()] || rel == plan.PackageDir) {
				return filepath.SkipDir
			}
			return nil
		}
		total++
		if total > maxOverviewWalk {
			return filepath.SkipAll
		}
		if len(files) < maxOverviewFiles {
			files = append(files, rel)
		}
		if isTestFile(rel) && len(tests) < maxOverviewTests {
			tests = append(tests, rel)
		}
		return nil
	})
	sort.Strings(files)
	sort.Strings(tests)
	d := gates.Detect(dir, look)
	var b strings.Builder
	langs := "none detected"
	if len(d.Languages) > 0 {
		langs = strings.Join(d.Languages, ", ")
	}
	b.WriteString(fmt.Sprintf("Languages: %s\n", langs))
	b.WriteString(fmt.Sprintf("Files: %d", total))
	if total > maxOverviewWalk {
		b.WriteString(fmt.Sprintf("+ (walk stopped)"))
	}
	b.WriteString("\n\n## Tree")
	if total > len(files) {
		b.WriteString(fmt.Sprintf(" (first %d files)", len(files)))
	}
	b.WriteString("\n")
	for _, f := range files {
		b.WriteString(f + "\n")
	}
	b.WriteString("\n## Existing tests\n")
	if len(tests) == 0 {
		b.WriteString("(none found)\n")
	}
	for _, t := range tests {
		b.WriteString(t + "\n")
	}
	return b.String()
}

// GateCatalogue lists what ACO can run for each gate type in this
// repository (§10.1): the configured commands, else the auto-detected ones,
// and the available security scanners.
func GateCatalogue(dir string, cfg *config.Config, look gates.LookPathFunc) string {
	var d *gates.Detection
	if cfg.Gates.AutoDetect {
		d = gates.Detect(dir, look)
	}
	configured := map[string][]string{
		"typecheck": cfg.Gates.Typecheck.Commands, "lint": cfg.Gates.Lint.Commands, "unit": cfg.Gates.Unit.Commands,
		"integration": cfg.Gates.Integration.Commands, "build": cfg.Gates.Build.Commands,
		"security": cfg.Gates.Security.Commands,
	}
	var b strings.Builder
	for _, g := range config.GateTypes {
		b.WriteString(fmt.Sprintf("%s:", g))
		switch {
		case len(configured[g]) > 0:
			b.WriteString(" (configured)\n")
			for _, c := range configured[g] {
				b.WriteString("  - " + c + "\n")
			}
		case d != nil && len(d.Commands[g]) > 0:
			b.WriteString(" (auto-detected)\n")
			for _, c := range d.Commands[g] {
				line := c.Run
				if c.Scanner != "" {
					line += "   [scanner " + c.Scanner + "]"
				}
				b.WriteString("  - " + line + "\n")
			}
		default:
			b.WriteString(" nothing detected; declare commands or not_applicable with a justification\n")
		}
	}
	scanners := append([]string{}, cfg.Gates.Security.Scanners...)
	if d != nil {
		for _, s := range d.Scanners {
			if !contains(scanners, s) {
				scanners = append(scanners, s)
			}
		}
	}
	sort.Strings(scanners)
	b.WriteString(fmt.Sprintf("security scanners available: %s\n", strings.Join(scanners, ", ")))
	return b.String()
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// RenderChecks renders deterministic plan check results for the planner
// and the reviewer (English, one problem per line).
func RenderChecks(ds []plan.Diagnostic) string {
	errs, warns := plan.Count(ds)
	if len(ds) == 0 {
		return "PASS: all deterministic plan checks passed (0 errors, 0 warnings).\n"
	}
	var b strings.Builder
	verdict := "PASS"
	if errs > 0 {
		verdict = "FAIL"
	}
	b.WriteString(fmt.Sprintf("%s: %d errors, %d warnings.\n", verdict, errs, warns))
	for _, d := range ds {
		loc := d.Location.String()
		if d.Location.Ref != "" {
			loc += " [" + d.Location.Ref + "]"
		}
		b.WriteString(fmt.Sprintf("- %s %s at %s: %s\n", d.Severity, d.Code, loc, d.Message))
	}
	return b.String()
}
