package plan

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/0xyu/aco/internal/config"
)

// ReportFormat identifies the validate-plan JSON output.
const ReportFormat = "aco.validate-plan/v1"

// Report is the result of validating a plan (the validate-plan JSON output,
// later also returned by the HTTP API and MCP).
type Report struct {
	Format      string        `json:"format"`
	Valid       bool          `json:"valid"`
	Strict      bool          `json:"strict"`
	Source      ReportSource  `json:"source"`
	Title       string        `json:"title"`
	PlanHash    string        `json:"plan_hash"`
	Summary     ReportSummary `json:"summary"`
	Phases      []ReportPhase `json:"phases"`
	AgentsMD    *ReportFile   `json:"agents_md"`
	Lock        *LockStatus   `json:"lock"`
	Diagnostics []Diagnostic  `json:"diagnostics"`
}

// ReportSource names the input (base name only, so output is portable).
type ReportSource struct {
	Kind SourceKind `json:"kind"`
	Name string     `json:"name"`
}

// ReportSummary counts the package contents and problems.
type ReportSummary struct {
	Phases             int `json:"phases"`
	Requirements       int `json:"requirements"`
	AcceptanceCriteria int `json:"acceptance_criteria"`
	Errors             int `json:"errors"`
	Warnings           int `json:"warnings"`
}

// ReportPhase is one phase in the report.
type ReportPhase struct {
	ID                 string   `json:"id"`
	Number             int      `json:"number"`
	Title              string   `json:"title"`
	AcceptanceCriteria int      `json:"acceptance_criteria"`
	DependsOn          []string `json:"depends_on"`
	Requirements       []string `json:"requirements"`
}

// ReportFile is a hashed file.
type ReportFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// LockStatus describes the package's plan.lock.json, if any.
type LockStatus struct {
	Verified    bool   `json:"verified"`
	PlanVersion string `json:"plan_version,omitempty"`
	PlanHash    string `json:"plan_hash,omitempty"`
}

// NewReport assembles a report; ds are all diagnostics (sorted here).
func NewReport(p *Package, ds []Diagnostic, strict bool, lock *LockStatus) Report {
	SortDiagnostics(ds)
	errs, warns := Count(ds)
	r := Report{
		Format: ReportFormat, Valid: Passes(ds, strict), Strict: strict,
		Source:   ReportSource{Kind: p.Source.Kind, Name: p.Source.Name},
		Title:    p.Title,
		PlanHash: p.Hash(),
		Summary: ReportSummary{Phases: len(p.Phases), Requirements: len(p.Requirements), AcceptanceCriteria: p.ACCount(),
			Errors: errs, Warnings: warns},
		Phases:      []ReportPhase{},
		Lock:        lock,
		Diagnostics: ds,
	}
	if r.Diagnostics == nil {
		r.Diagnostics = []Diagnostic{}
	}
	for _, ph := range p.Phases {
		rp := ReportPhase{ID: ph.ID, Number: ph.Number, Title: ph.Title, AcceptanceCriteria: len(ph.AcceptanceCriteria),
			DependsOn: []string{}, Requirements: []string{}}
		for _, d := range ph.DependsOn {
			rp.DependsOn = append(rp.DependsOn, PhaseID(d))
		}
		rp.Requirements = append(rp.Requirements, ph.Requirements...)
		r.Phases = append(r.Phases, rp)
	}
	if p.AgentsMD != nil {
		r.AgentsMD = &ReportFile{Path: p.AgentsMD.Path, SHA256: p.AgentsMD.Hash}
	}
	return r
}

// CheckFrontMatter validates the plan's front matter (or structured
// "config" block) as an ACO config layer.
func CheckFrontMatter(p *Package) []Diagnostic {
	if len(p.FrontMatter) == 0 {
		return nil
	}
	file := p.Source.Name
	if p.Source.Kind == SourceDir {
		file = PlanFileName
	}
	_, err := config.Load(config.LoadOptions{PlanFrontMatter: p.FrontMatter, PlanSource: file})
	var ce config.Errors
	if err == nil || !errors.As(err, &ce) {
		return nil
	}
	var out []Diagnostic
	for _, e := range ce {
		line := 0
		if e.Line > 0 && p.FrontMatterLine > 0 {
			line = p.FrontMatterLine + e.Line - 1
		}
		field := config.DisplayPath(e.Field)
		if field == "" {
			field = "(document)"
		}
		out = append(out, NewDiagnostic(CodeFrontMatterInvalid, SeverityError, Location{File: file, Line: line, Section: "front_matter"},
			string(e.Code), field))
	}
	return out
}

// CheckLock verifies a package directory's plan.lock.json against the files
// under the repository root. It returns nil status when there is no lock.
func CheckLock(p *Package) (*LockStatus, []Diagnostic) {
	if p.Source.Kind != SourceDir || p.rejected {
		// A rejected package already carries its path_not_contained
		// diagnostic; nothing is opened through the rejected component.
		return nil, nil
	}
	lockFile := filepath.Join(p.Source.Path, LockFileName)
	rel := LockFileName
	// For a source at <root>/.aco/plan, check every component of the
	// manifest path before opening it, whatever --repo says.
	for _, root := range []string{inferRepoRoot(p.Source.Path), p.RepoRoot} {
		if root == "" || !isPackageDir(p.Source.Path, root) {
			continue
		}
		rel = PackageDir + "/" + LockFileName
		if _, err := checkManifest(root); err != nil {
			if d, ok := containmentDiag(err); ok {
				return &LockStatus{}, []Diagnostic{d}
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return &LockStatus{}, []Diagnostic{NewDiagnostic(CodeLockUnverified, SeverityError, Location{File: LockFileName}, err.Error())}
			}
		}
	}
	// A linked manifest is refused before it is read.
	raw, err := readRegular(lockFile, rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if d, ok := containmentDiag(err); ok {
		return &LockStatus{}, []Diagnostic{d}
	}
	loc := Location{File: LockFileName}
	if err != nil {
		return &LockStatus{}, []Diagnostic{NewDiagnostic(CodeLockUnverified, SeverityError, loc, err.Error())}
	}
	l, err := ParseLock(raw)
	if err != nil {
		return &LockStatus{}, []Diagnostic{NewDiagnostic(CodeLockMismatch, SeverityError, loc, LockFileName, err.Error())}
	}
	st := &LockStatus{PlanVersion: l.PlanVersion, PlanHash: l.PlanHash}
	if p.RepoRoot == "" || !samePath(p.Source.Path, filepath.Join(p.RepoRoot, filepath.FromSlash(PackageDir))) {
		return st, []Diagnostic{NewDiagnostic(CodeLockUnverified, SeverityWarning, loc,
			"the plan directory is not <repo>/"+PackageDir+"; pass the repository with --repo")}
	}
	err = VerifyLock(p.RepoRoot, l)
	var me MismatchError
	if d, ok := containmentDiag(err); ok {
		return st, []Diagnostic{d}
	}
	switch {
	case err == nil:
		st.Verified = true
		return st, nil
	case errors.As(err, &me):
		var ds []Diagnostic
		for _, m := range me {
			ds = append(ds, NewDiagnostic(CodeLockMismatch, SeverityError, Location{File: m.Path, Ref: m.Path}, m.Path, m.Problem))
		}
		return st, ds
	default:
		return st, []Diagnostic{NewDiagnostic(CodeLockUnverified, SeverityError, loc, err.Error())}
	}
}

func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	if aa == bb {
		return true
	}
	fa, err1 := os.Stat(aa)
	fb, err2 := os.Stat(bb)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

// ValidateFull runs every check that needs no agent: parse diagnostics, the
// deterministic plan checks, front matter, and the freeze manifest.
func ValidateFull(p *Package, opts CheckOptions, strict bool) Report {
	ds := Validate(p, opts)
	ds = append(ds, CheckFrontMatter(p)...)
	lock, lds := CheckLock(p)
	ds = append(ds, lds...)
	return NewReport(p, ds, strict, lock)
}
