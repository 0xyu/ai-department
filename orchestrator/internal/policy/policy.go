// Package policy is the diff policy (part 1, §11.4 and §16): it checks a
// phase diff for protected paths, paths outside the phase scope, oversized
// and binary files, secrets, anti-gaming edits (deleted or skipped tests,
// suppressed lint and security findings), and missing tests. Results are
// structured findings; the engine decides what to do with them.
//
// Findings never contain the offending text, only the path, line, and rule,
// so a detected secret is not copied into reports, prompts, or events.
package policy

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/0xyu/aco/internal/config"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/secure"
)

// Finding codes.
const (
	CodeProtectedPath  = "protected_path"
	CodeScopeViolation = "scope_violation"
	CodeFileTooLarge   = "file_too_large"
	CodeBinaryFile     = "binary_file"
	CodeSecret         = "secret_detected"
	CodeSecretFile     = "secret_file"
	CodeTestDeleted    = "test_deleted"
	CodeTestSkipped    = "test_skipped"
	CodeSuppression    = "lint_suppressed"
	CodeTestsMissing   = "tests_missing"
	CodeScanIncomplete = "scan_incomplete"
)

// MaxScanBytes is the largest blob whose content Collect reads. A larger
// changed file is not scanned and gets a blocking scan_incomplete finding.
const MaxScanBytes = 64 << 20

// Config is the policy for one phase diff.
type Config struct {
	ProtectedPaths   []string
	ScopePaths       []string // phase scope.paths; empty means no scope check
	ScopeEnforcement string   // flag | block
	TestsMissing     string   // warn | fail
	AntiGaming       string   // warn | fail
	BinaryFiles      string   // warn | fail
	MaxFileBytes     int64
	// Redactor flags registered secret values (from the environment) that
	// match no pattern.
	Redactor *secure.Redactor
}

// FromConfig builds the policy for a phase from the run config and the
// phase's scope.paths.
func FromConfig(pc config.PolicyConfig, scopePaths []string, red *secure.Redactor) Config {
	return Config{
		ProtectedPaths:   append([]string(nil), pc.ProtectedPaths...),
		ScopePaths:       append([]string(nil), scopePaths...),
		ScopeEnforcement: pc.ScopeEnforcement,
		TestsMissing:     pc.TestsMissing,
		AntiGaming:       pc.AntiGaming,
		BinaryFiles:      pc.BinaryFiles,
		MaxFileBytes:     int64(pc.MaxFileBytes),
		Redactor:         red,
	}
}

// File is one changed file of a diff.
type File struct {
	Status  string // gitops.Added, Modified, Deleted, Renamed, Copied, TypeChg
	Path    string
	OldPath string
	Size    int64
	Binary  bool
	Added   []Line
	Removed []Line
	// Unscanned is set when the content could not be read in full (too
	// large); Check then reports a blocking scan_incomplete finding.
	Unscanned bool
}

func (f File) paths() []string {
	if f.OldPath != "" && f.OldPath != f.Path {
		return []string{f.OldPath, f.Path}
	}
	return []string{f.Path}
}

// Finding is one policy result. Blocking findings reject the diff; the others
// are reported for review.
type Finding struct {
	Code     string `json:"code"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Rule     string `json:"rule,omitempty"`
	Blocking bool   `json:"blocking"`
	Message  string `json:"message"`
}

func (f Finding) String() string {
	loc := f.Path
	if f.Line > 0 {
		loc += fmt.Sprintf(":%d", f.Line)
	}
	return fmt.Sprintf("%s %s: %s", f.Code, loc, f.Message)
}

// Result holds every finding of a check, sorted by path, line, and code.
type Result struct {
	Findings []Finding `json:"findings"`
}

// Rejected reports whether any finding is blocking.
func (r Result) Rejected() bool {
	for _, f := range r.Findings {
		if f.Blocking {
			return true
		}
	}
	return false
}

// Blocking returns the blocking findings.
func (r Result) Blocking() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Blocking {
			out = append(out, f)
		}
	}
	return out
}

// Has reports whether a finding with code exists.
func (r Result) Has(code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// Check applies the policy to a diff.
func Check(cfg Config, files []File) Result {
	var out []Finding
	add := func(f Finding) { out = append(out, f) }
	fail := func(mode string) bool { return mode == "fail" }

	sourceChanged, testChanged := []string{}, false
	for _, f := range files {
		for _, p := range f.paths() {
			if MatchAny(cfg.ProtectedPaths, p, true) {
				add(Finding{Code: CodeProtectedPath, Path: p, Blocking: true, Message: "path is protected and may not be changed by the implementer"})
			}
			if len(cfg.ScopePaths) > 0 && !MatchAny(cfg.ScopePaths, p, false) {
				add(Finding{Code: CodeScopeViolation, Path: p, Blocking: cfg.ScopeEnforcement == "block", Message: "path is outside the phase scope.paths"})
			}
		}
		if f.Status != gitops.Deleted {
			if cfg.MaxFileBytes > 0 && f.Size > cfg.MaxFileBytes {
				add(Finding{Code: CodeFileTooLarge, Path: f.Path, Blocking: true,
					Message: fmt.Sprintf("file is %d bytes; the limit is %d", f.Size, cfg.MaxFileBytes)})
			}
			if f.Binary {
				add(Finding{Code: CodeBinaryFile, Path: f.Path, Blocking: fail(cfg.BinaryFiles), Message: "binary file in the phase diff"})
			}
			if rule := secretFileRule(f.Path); rule != "" {
				add(Finding{Code: CodeSecretFile, Path: f.Path, Rule: rule, Blocking: true, Message: "file name indicates credentials or a private key"})
			}
		}
		if f.Unscanned {
			add(Finding{Code: CodeScanIncomplete, Path: f.Path, Blocking: true,
				Message: fmt.Sprintf("file content was not scanned for secrets and anti-gaming edits (larger than %d bytes)", MaxScanBytes)})
		}
		for _, l := range f.Added {
			for _, rule := range scanSecrets(l.Text, cfg.Redactor) {
				add(Finding{Code: CodeSecret, Path: f.Path, Line: l.No, Rule: rule.name, Blocking: rule.blocking, Message: "added line contains " + rule.desc})
			}
			for _, rule := range scanGaming(l.Text) {
				add(Finding{Code: rule.code, Path: f.Path, Line: l.No, Rule: rule.name, Blocking: fail(cfg.AntiGaming), Message: rule.desc})
			}
		}

		// Test bookkeeping.
		switch {
		case f.Status == gitops.Deleted && IsTestFile(f.Path),
			f.Status == gitops.Renamed && IsTestFile(f.OldPath) && !IsTestFile(f.Path):
			p := f.Path
			if f.Status == gitops.Renamed {
				p = f.OldPath
			}
			add(Finding{Code: CodeTestDeleted, Path: p, Rule: "deleted_test_file", Blocking: fail(cfg.AntiGaming), Message: "test file deleted"})
		}
		if f.Status != gitops.Deleted && IsTestRelated(f.Path) {
			testChanged = true
		}
		if f.Status != gitops.Deleted && IsSource(f.Path) && !IsTestRelated(f.Path) {
			sourceChanged = append(sourceChanged, f.Path)
		}
	}
	if len(sourceChanged) > 0 && !testChanged {
		add(Finding{Code: CodeTestsMissing, Path: sourceChanged[0], Blocking: fail(cfg.TestsMissing),
			Message: fmt.Sprintf("%d source file(s) changed without any added or changed test", len(sourceChanged))})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Code < b.Code
	})
	return Result{Findings: out}
}

// Collect reads the diff between two tree-ishes from Git and returns it as
// policy input. The changed paths come from diff-tree (rename detection on);
// added and removed lines come from comparing the raw old and new blob
// contents with DiffLines, so .gitattributes (for example "-diff" or a diff
// driver marked binary) and diff presentation settings cannot hide a line
// from the secret and anti-gaming checks. Binary is set when git reports
// the file binary or the new content contains a NUL byte; it only drives
// the binary_files check. Any read error fails the whole collection.
func Collect(ctx context.Context, repo *gitops.Repo, from, to string) ([]File, error) {
	changes, err := repo.Changes(ctx, from, to)
	if err != nil {
		return nil, err
	}
	files := make([]File, 0, len(changes))
	for _, c := range changes {
		f := File{Status: c.Status, Path: c.Path, OldPath: c.OldPath, Size: c.Size, Binary: c.Binary}
		if c.Status != gitops.Deleted && !gitops.IsSubmodule(c.NewMode) {
			if c.Size > MaxScanBytes || c.OldSize > MaxScanBytes {
				f.Unscanned = true
			} else {
				var oldContent []byte
				if c.Status != gitops.Added && !gitops.IsSubmodule(c.OldMode) {
					if oldContent, err = repo.ReadBlob(ctx, c.OldBlob); err != nil {
						return nil, err
					}
				}
				newContent, err := repo.ReadBlob(ctx, c.NewBlob)
				if err != nil {
					return nil, err
				}
				if bytes.IndexByte(newContent, 0) >= 0 {
					f.Binary = true
				}
				f.Added, f.Removed = DiffLines(oldContent, newContent)
			}
		}
		files = append(files, f)
	}
	return files, nil
}

// CheckDiff is Collect followed by Check.
func CheckDiff(ctx context.Context, repo *gitops.Repo, from, to string, cfg Config) (Result, error) {
	files, err := Collect(ctx, repo, from, to)
	if err != nil {
		return Result{}, err
	}
	return Check(cfg, files), nil
}

var sourceExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true,
	".java": true, ".kt": true, ".kts": true, ".scala": true, ".rs": true, ".rb": true, ".php": true, ".cs": true,
	".swift": true, ".m": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".cxx": true,
	".vue": true, ".svelte": true, ".ex": true, ".exs": true, ".dart": true, ".lua": true, ".sh": true,
}

// IsSource reports whether p is a source code file (by extension).
func IsSource(p string) bool { return sourceExts[strings.ToLower(path.Ext(p))] }

var testNamePatterns = []string{
	"*_test.go", "test_*.py", "*_test.py", "*_tests.py", "conftest.py",
	"*.test.js", "*.test.jsx", "*.test.ts", "*.test.tsx", "*.test.mjs", "*.test.cjs",
	"*.spec.js", "*.spec.jsx", "*.spec.ts", "*.spec.tsx", "*.spec.mjs", "*.spec.cjs",
	"*Test.java", "*Tests.java", "*Test.kt", "*Tests.kt", "*_spec.rb", "*_test.rb", "*Tests.cs", "*Test.cs",
	"*_test.rs", "*_test.exs", "*Tests.swift", "*Test.php",
}

var testDirs = map[string]bool{"test": true, "tests": true, "__tests__": true, "spec": true, "specs": true}

// IsTestFile reports whether p is a test file: by name, or a source file in a
// test directory.
func IsTestFile(p string) bool {
	base := path.Base(p)
	for _, pat := range testNamePatterns {
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	if !IsSource(p) {
		return false
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if testDirs[strings.ToLower(seg)] {
			return true
		}
	}
	return false
}

// IsTestRelated reports whether p is a test file or test data (testdata/,
// fixtures/, snapshots).
func IsTestRelated(p string) bool {
	if IsTestFile(p) {
		return true
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		switch strings.ToLower(seg) {
		case "testdata", "fixtures", "__snapshots__", "__fixtures__":
			return true
		}
	}
	return strings.HasSuffix(p, ".snap") || strings.HasSuffix(p, ".golden")
}
