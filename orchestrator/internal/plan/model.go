// Package plan holds the plan package model (§10.1 of the implementation
// plan), its loaders, the deterministic plan checks (§10.4), AGENTS.md
// discovery, and the freeze manifest (plan.lock.json, §10.5).
//
// A plan package can be read from three forms, which all produce the same
// model (except raw text and source locations):
//
//   - a package directory (.aco/plan/: IMPLEMENTATION_PLAN.md, requirements.md,
//     traceability.json, phases/PNN.md);
//   - a single Markdown plan (requirements, overview, and "## Phase N: Title"
//     sections in one file);
//   - a single YAML or JSON document (schemas/plan.schema.json), the form
//     Stage 1 returns as structured output.
//
// Loaders never fail on content: problems become Diagnostics so they can be
// passed to the reviewer and the planner. Only I/O errors are returned as
// errors.
package plan

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/0xyu/aco/internal/config"
)

// Package-directory layout (relative to the repository root).
const (
	PackageDir        = ".aco/plan"
	PlanFileName      = "IMPLEMENTATION_PLAN.md"
	RequirementsName  = "requirements.md"
	TraceabilityName  = "traceability.json"
	PhasesDirName     = "phases"
	PlanReviewName    = "plan-review.md"
	LockFileName      = "plan.lock.json"
	AgentsMDName      = "AGENTS.md"
	StructuredVersion = 1 // "version" of the YAML/JSON plan form and traceability.json
)

// GateTypes lists the six quality gate types in execution order.
var GateTypes = config.GateTypes

// SourceKind is the form a package was read from.
type SourceKind string

// Source kinds.
const (
	SourceDir      SourceKind = "dir"
	SourceMarkdown SourceKind = "markdown"
	SourceYAML     SourceKind = "yaml"
	SourceJSON     SourceKind = "json"
)

// Source describes where a package came from.
type Source struct {
	Kind SourceKind
	Path string // path as loaded (file or directory)
	Name string // base name, used in diagnostics
}

// File is one source file of a package, kept byte-for-byte.
type File struct {
	Path string // slash-separated, relative to the package root (or the file name)
	Raw  []byte // exact bytes read
	Hash string // HashText(Raw): "sha256:<hex>" of the normalized text
}

// RequirementKind classifies a requirement.
type RequirementKind string

// Requirement kinds.
const (
	KindFunctional    RequirementKind = "functional"
	KindNonFunctional RequirementKind = "non_functional"
	KindConstraint    RequirementKind = "constraint"
)

// Requirement is one numbered requirement (REQ-NNN).
type Requirement struct {
	ID   string // canonical "REQ-001"
	Kind RequirementKind
	Text string
	Loc  Location
}

// Scope is a phase's scope.
type Scope struct {
	In    []string // in-scope items
	Out   []string // explicitly out of scope
	Paths []string // scope.paths globs (slash-separated)
}

// AcceptanceCriterion is one testable criterion of a phase.
type AcceptanceCriterion struct {
	ID       string // canonical "AC-03.2"
	Text     string
	Explicit bool // ID was written in the source (else numbered by position)
	Loc      Location
}

// GateEntry is one phase's declaration for one quality gate type.
type GateEntry struct {
	Type          string // one of GateTypes
	NotApplicable bool
	Justification string   // required when NotApplicable
	Commands      []string // empty: resolved from config / auto-detect later
	Scanners      []string // security gate only
}

// Phase is one Phase Specification.
type Phase struct {
	Number                int
	ID                    string // "P03"
	Title                 string
	Goal                  string
	Scope                 Scope
	TechnicalRequirements []string
	AcceptanceCriteria    []AcceptanceCriterion
	Deliverables          []string
	DependsOn             []int    // sorted, unique
	Requirements          []string // REQ IDs this phase covers, in source order
	Gates                 []GateEntry
	// Raw is the phase's Markdown exactly as written in the source (for
	// prompts). For YAML/JSON sources it is the rendered Markdown.
	Raw string
	Loc Location

	sections map[string]int      // section name -> line (for diagnostics)
	reqAC    map[string][]string // REQ -> AC listed next to it in the phase (Markdown only)
}

// Gate returns the phase's entry for gate type t.
func (p *Phase) Gate(t string) (GateEntry, bool) {
	for _, g := range p.Gates {
		if g.Type == t {
			return g, true
		}
	}
	return GateEntry{}, false
}

// Trace maps one requirement to the phases and acceptance criteria that
// cover it (traceability.json).
type Trace struct {
	Requirement        string
	Phases             []int
	AcceptanceCriteria []string
	Loc                Location
}

// Package is a parsed plan package.
type Package struct {
	Source   Source
	Title    string
	Overview string // plan text without phase sections or front matter (normalized line endings)

	// FrontMatter is the YAML config block of the plan (the "plan" config
	// layer); FrontMatterLine is the source line its first byte is on.
	FrontMatter     []byte
	FrontMatterLine int

	Requirements []Requirement
	Phases       []Phase // in source order
	Traceability []Trace
	TraceDerived bool // traceability was derived from the phases' Requirements sections

	Files            []File // every source file read, sorted by Path
	RepoRoot         string // "" when unknown
	AgentsMD         *File  // discovered AGENTS.md (Path relative to RepoRoot), nil if none
	ProposedAgentsMD string // AGENTS.md text proposed by the planner (structured form only)

	// Diagnostics are the problems found while parsing. Validate adds the
	// deterministic checks.
	Diagnostics []Diagnostic

	// rejected is set when loading refused the package before reading it
	// (a linked .aco or .aco/plan); later checks then do no I/O through it.
	rejected bool
}

// Rejected reports whether loading refused the package without reading it.
func (p *Package) Rejected() bool { return p.rejected }

// Phase returns the phase with number n, or nil.
func (p *Package) Phase(n int) *Phase {
	for i := range p.Phases {
		if p.Phases[i].Number == n {
			return &p.Phases[i]
		}
	}
	return nil
}

// Requirement returns the requirement with the given ID, or nil.
func (p *Package) Requirement(id string) *Requirement {
	for i := range p.Requirements {
		if p.Requirements[i].ID == id {
			return &p.Requirements[i]
		}
	}
	return nil
}

// AcceptanceCriterion returns the criterion with the given ID and its phase.
func (p *Package) AcceptanceCriterion(id string) (*Phase, *AcceptanceCriterion) {
	for i := range p.Phases {
		ph := &p.Phases[i]
		for j := range ph.AcceptanceCriteria {
			if ph.AcceptanceCriteria[j].ID == id {
				return ph, &ph.AcceptanceCriteria[j]
			}
		}
	}
	return nil, nil
}

// ACCount is the total number of acceptance criteria.
func (p *Package) ACCount() int {
	n := 0
	for _, ph := range p.Phases {
		n += len(ph.AcceptanceCriteria)
	}
	return n
}

// Hash identifies the package's content: the hash of its source files
// (normalized line endings, so it is the same on every OS).
func (p *Package) Hash() string {
	files := make([]LockedFile, len(p.Files))
	for i, f := range p.Files {
		files[i] = LockedFile{Path: f.Path, SHA256: f.Hash}
	}
	return combinedHash(files)
}

// Canonical returns a copy without raw text, source locations, or other
// form-specific data, with traceability in canonical order. Two sources that
// describe the same plan have equal canonical models.
func (p *Package) Canonical() *Package {
	c := &Package{Title: p.Title}
	for _, r := range p.Requirements {
		r.Loc = Location{}
		c.Requirements = append(c.Requirements, r)
	}
	for _, ph := range p.Phases {
		q := ph
		q.Raw, q.Loc, q.sections, q.reqAC = "", Location{}, nil, nil
		q.Scope = Scope{In: cloneStrings(ph.Scope.In), Out: cloneStrings(ph.Scope.Out), Paths: cloneStrings(ph.Scope.Paths)}
		q.AcceptanceCriteria = nil
		for _, ac := range ph.AcceptanceCriteria {
			ac.Loc, ac.Explicit = Location{}, false
			q.AcceptanceCriteria = append(q.AcceptanceCriteria, ac)
		}
		q.TechnicalRequirements = cloneStrings(ph.TechnicalRequirements)
		q.Deliverables = cloneStrings(ph.Deliverables)
		q.DependsOn = cloneInts(ph.DependsOn)
		q.Requirements = cloneStrings(ph.Requirements)
		q.Gates = nil
		for _, g := range ph.Gates {
			g.Commands, g.Scanners = cloneStrings(g.Commands), cloneStrings(g.Scanners)
			q.Gates = append(q.Gates, g)
		}
		c.Phases = append(c.Phases, q)
	}
	c.Traceability = canonicalTrace(p.Traceability)
	return c
}

func canonicalTrace(in []Trace) []Trace {
	var out []Trace
	for _, t := range in {
		if len(t.Phases) == 0 && len(t.AcceptanceCriteria) == 0 {
			continue
		}
		out = append(out, Trace{
			Requirement:        t.Requirement,
			Phases:             sortedUniqueInts(t.Phases),
			AcceptanceCriteria: sortedUniqueACs(t.AcceptanceCriteria),
		})
	}
	sortTraces(out)
	return out
}

// PhaseID formats a phase number as "P03".
func PhaseID(n int) string { return fmt.Sprintf("P%02d", n) }

// ACID formats an acceptance criterion ID as "AC-03.2".
func ACID(phase, index int) string { return fmt.Sprintf("AC-%02d.%d", phase, index) }

// ReqID formats a requirement ID as "REQ-001".
func ReqID(n int) string { return fmt.Sprintf("REQ-%03d", n) }

// parseACID splits a canonical or loose "AC-3.02" into numbers.
func parseACID(s string) (phase, index int, ok bool) {
	rest, found := strings.CutPrefix(strings.ToUpper(strings.TrimSpace(s)), "AC-")
	if !found {
		return 0, 0, false
	}
	a, b, found := strings.Cut(rest, ".")
	if !found {
		return 0, 0, false
	}
	pa, e1 := strconv.Atoi(a)
	ib, e2 := strconv.Atoi(b)
	if e1 != nil || e2 != nil || pa <= 0 || ib <= 0 || !allDigits(a) || !allDigits(b) {
		return 0, 0, false
	}
	return pa, ib, true
}

// canonicalReq turns "req-1" / "REQ-001" into "REQ-001".
func canonicalReq(s string) (string, bool) {
	rest, found := strings.CutPrefix(strings.ToUpper(strings.TrimSpace(s)), "REQ-")
	if !found || !allDigits(rest) {
		return "", false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return "", false
	}
	return ReqID(n), true
}

func allDigits(s string) bool {
	if s == "" || len(s) > 6 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func cloneStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

func cloneInts(s []int) []int {
	if len(s) == 0 {
		return nil
	}
	return append([]int(nil), s...)
}
