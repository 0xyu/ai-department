package plan

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxFileSize caps each plan file read (plans are text; anything bigger is a
// mistake or an attack).
const MaxFileSize = 8 << 20

// Options control loading.
type Options struct {
	// RepoRoot is the repository the plan belongs to, used to discover
	// AGENTS.md. When empty and the plan is a package directory at
	// <repo>/.aco/plan, the repository root is inferred.
	RepoRoot string
}

// Load reads a plan package from a directory, a Markdown file, or a YAML or
// JSON file. The returned error is only for I/O problems (path missing,
// unreadable); content problems are in Package.Diagnostics.
func Load(p string, opts Options) (*Package, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	var pkg *Package
	if fi.IsDir() {
		inferred := inferRepoRoot(p)
		if opts.RepoRoot == "" {
			opts.RepoRoot = inferred
		}
		// A package at <root>/.aco/plan must really be inside <root>: a linked
		// .aco or plan directory is refused before anything is read. The
		// check uses the root the path itself names, so an explicit --repo
		// cannot skip it; a standalone plan directory has no such root.
		for _, root := range []string{inferred, opts.RepoRoot} {
			if root == "" || !isPackageDir(p, root) {
				continue
			}
			if err := checkPackageDir(root); err != nil {
				d, ok := containmentDiag(err)
				if !ok {
					return nil, err
				}
				pkg = &Package{Source: Source{Kind: SourceDir, Name: filepath.Base(p), Path: p}, RepoRoot: opts.RepoRoot,
					Diagnostics: []Diagnostic{d}, rejected: true}
				return pkg, nil
			}
		}
		pkg, err = loadDir(p)
		if err != nil {
			return nil, err
		}
	} else {
		raw, err := readFile(p)
		if err != nil {
			return nil, err
		}
		pkg = LoadBytes(filepath.Base(p), raw)
	}
	pkg.Source.Path = p
	if opts.RepoRoot != "" {
		pkg.RepoRoot = opts.RepoRoot
		agents, err := DiscoverAgentsMD(opts.RepoRoot)
		switch {
		case errors.Is(err, ErrNotRegular):
			pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeAgentsMDNotRegular, SeverityError, Location{File: AgentsMDName}, AgentsMDName))
		case err != nil:
			return nil, err
		default:
			pkg.AgentsMD = agents
		}
	}
	return pkg, nil
}

// LoadBytes parses a single-file plan; the format is chosen by the file
// name's extension (.yaml/.yml, .json, anything else is Markdown).
func LoadBytes(name string, raw []byte) *Package {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return loadStructured(name, raw, SourceYAML)
	case ".json":
		return loadStructured(name, raw, SourceJSON)
	default:
		return loadMarkdown(name, raw)
	}
}

func inferRepoRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	if filepath.Base(abs) == "plan" && filepath.Base(filepath.Dir(abs)) == ".aco" {
		return filepath.Dir(filepath.Dir(abs))
	}
	return ""
}

func readFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFileSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", p, MaxFileSize)
	}
	return b, nil
}

// --- single-file Markdown ---

func loadMarkdown(name string, raw []byte) *Package {
	pkg := &Package{Source: Source{Kind: SourceMarkdown, Name: name}}
	pkg.Files = []File{{Path: name, Raw: raw, Hash: HashText(raw)}}
	d := parseMarkdown(raw)
	body := d.body(pkg)
	pkg.Title = d.title(body)

	phases := d.findPhases(body, len(d.lines))
	inPhase := func(idx int) bool {
		for _, pr := range phases {
			if idx >= pr.from && idx < pr.to {
				return true
			}
		}
		return false
	}
	for _, pr := range phases {
		ph, diags := d.parsePhase(pr, name)
		pkg.Phases = append(pkg.Phases, ph)
		pkg.Diagnostics = append(pkg.Diagnostics, diags...)
	}
	pkg.Requirements = d.parseRequirements(name, body, len(d.lines), inPhase)

	var ov []string
	for i := body; i < len(d.lines); i++ {
		if !inPhase(i) {
			ov = append(ov, d.lines[i].text)
		}
	}
	pkg.Overview = strings.Trim(strings.Join(ov, "\n"), "\n") + "\n"
	pkg.Traceability, pkg.TraceDerived = deriveTrace(pkg.Phases), true
	return pkg
}

// body handles front matter and returns the first body line index. Headings
// inside the front matter (YAML comments) are dropped.
func (d *mdDoc) body(pkg *Package) int {
	fm, line, start, ok := splitFrontMatter(d)
	if !ok {
		return 0
	}
	pkg.FrontMatter, pkg.FrontMatterLine = fm, line
	kept := d.headings[:0]
	for _, h := range d.headings {
		if h.idx >= start {
			kept = append(kept, h)
		}
	}
	d.headings = kept
	return start
}

func (d *mdDoc) title(from int) string {
	for _, h := range d.headings {
		if h.idx >= from && h.level == 1 {
			return stripEmphasis(h.text)
		}
	}
	return ""
}

// deriveTrace builds traceability from the phases' Requirements sections:
// a requirement maps to the AC listed next to it, or to all of the phase's
// AC when none are listed.
func deriveTrace(phases []Phase) []Trace {
	byReq := map[string]*Trace{}
	var order []string
	for _, ph := range phases {
		for _, r := range ph.Requirements {
			t := byReq[r]
			if t == nil {
				t = &Trace{Requirement: r, Loc: Location{File: ph.Loc.File, Line: ph.sections[secRequirements], Phase: ph.ID, Section: secRequirements, Ref: r}}
				byReq[r] = t
				order = append(order, r)
			}
			t.Phases = append(t.Phases, ph.Number)
			if acs := ph.reqAC[r]; len(acs) > 0 {
				// Only the phase's own criteria; others are reported by Check.
				for _, a := range acs {
					if phaseOwnsAC(&ph, a) {
						t.AcceptanceCriteria = append(t.AcceptanceCriteria, a)
					}
				}
			} else {
				for _, ac := range ph.AcceptanceCriteria {
					t.AcceptanceCriteria = append(t.AcceptanceCriteria, ac.ID)
				}
			}
		}
	}
	var out []Trace
	for _, r := range order {
		t := *byReq[r]
		t.Phases = sortedUniqueInts(t.Phases)
		t.AcceptanceCriteria = sortedUniqueACs(t.AcceptanceCriteria)
		out = append(out, t)
	}
	sortTraces(out)
	return out
}

func phaseOwnsAC(ph *Phase, id string) bool {
	for _, ac := range ph.AcceptanceCriteria {
		if ac.ID == id {
			return true
		}
	}
	return false
}

// --- package directory ---

var phaseFileRE = regexp.MustCompile(`(?i)^p0*(\d{1,4})\.md$`)

func loadDir(dir string) (*Package, error) {
	pkg := &Package{Source: Source{Kind: SourceDir, Name: filepath.Base(dir)}}
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if e.Type() == fs.ModeDir {
			return nil
		}
		if rel == LockFileName {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil // not a plan file; CheckLock checks it
		}
		if e.Type() != 0 {
			// Links, junctions, and other reparse points are never followed.
			pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodePathNotContained, SeverityError, Location{File: rel, Ref: rel}, rel))
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := readRegular(p, rel)
		if err != nil {
			return err
		}
		files[rel] = b
		pkg.Files = append(pkg.Files, File{Path: rel, Raw: b, Hash: HashText(b)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(pkg.Files, func(i, j int) bool { return pkg.Files[i].Path < pkg.Files[j].Path })
	missing := func(name string) {
		pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeMissingFile, SeverityError, Location{File: name}, name))
	}

	if raw, ok := files[PlanFileName]; ok {
		d := parseMarkdown(raw)
		body := d.body(pkg)
		pkg.Title = d.title(body)
		pkg.Overview = string(NormalizeText([]byte(d.raw(body, len(d.lines)))))
	} else {
		missing(PlanFileName)
	}

	if raw, ok := files[RequirementsName]; ok {
		d := parseMarkdown(raw)
		pkg.Requirements = d.parseRequirements(RequirementsName, 0, len(d.lines), nil)
	} else {
		missing(RequirementsName)
	}

	type phaseFile struct {
		num  int
		name string
	}
	var pfs []phaseFile
	for rel := range files {
		dirPart, base := path.Split(rel)
		if dirPart != PhasesDirName+"/" {
			continue
		}
		m := phaseFileRE.FindStringSubmatch(base)
		if m == nil {
			pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodePhaseFileInvalid, SeverityError, Location{File: rel}, rel, "name must be PNN.md"))
			continue
		}
		n, _ := strconv.Atoi(m[1])
		pfs = append(pfs, phaseFile{num: n, name: rel})
	}
	sort.Slice(pfs, func(i, j int) bool {
		if pfs[i].num != pfs[j].num {
			return pfs[i].num < pfs[j].num
		}
		return pfs[i].name < pfs[j].name
	})
	for _, pf := range pfs {
		d := parseMarkdown(files[pf.name])
		ranges := d.findPhases(0, len(d.lines))
		bad := func(reason string) {
			pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodePhaseFileInvalid, SeverityError, Location{File: pf.name}, pf.name, reason))
		}
		switch {
		case len(ranges) == 0:
			bad("no \"Phase N: Title\" heading")
			continue
		case len(ranges) > 1:
			bad("more than one phase heading")
		}
		if ranges[0].num != pf.num {
			bad(fmt.Sprintf("file is for %s but its heading is %s", PhaseID(pf.num), PhaseID(ranges[0].num)))
		}
		ph, diags := d.parsePhase(ranges[0], pf.name)
		pkg.Phases = append(pkg.Phases, ph)
		pkg.Diagnostics = append(pkg.Diagnostics, diags...)
	}

	if raw, ok := files[TraceabilityName]; ok {
		traces, diags := parseTraceabilityJSON(raw)
		pkg.Traceability = traces
		pkg.Diagnostics = append(pkg.Diagnostics, diags...)
	} else {
		missing(TraceabilityName)
	}
	return pkg, nil
}
