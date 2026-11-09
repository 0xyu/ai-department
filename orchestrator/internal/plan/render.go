package plan

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RenderPhase writes a Phase Specification as Markdown in the ACO plan
// format. The Markdown loaders read it back into the same model. traces
// supplies the AC listed next to each requirement.
func RenderPhase(ph Phase, traces []Trace) string {
	var b strings.Builder
	b.WriteString("## Phase " + itoa(ph.Number) + ": " + ph.Title + "\n")
	section := func(title string) { b.WriteString("\n### " + title + "\n") }
	bullets := func(items []string) {
		for _, it := range items {
			b.WriteString(bullet(it))
		}
	}
	if ph.Goal != "" {
		section("Goal")
		b.WriteString(ph.Goal + "\n")
	}
	section("Scope")
	bullets(ph.Scope.In)
	for _, o := range ph.Scope.Out {
		b.WriteString(bullet("Out of scope: " + o))
	}
	if len(ph.Scope.Paths) > 0 {
		spans := make([]string, len(ph.Scope.Paths))
		for i, p := range ph.Scope.Paths {
			spans[i] = codeSpan(p)
		}
		b.WriteString("- Paths: " + strings.Join(spans, ", ") + "\n")
	}
	if len(ph.TechnicalRequirements) > 0 {
		section("Technical Requirements")
		bullets(ph.TechnicalRequirements)
	}
	section("Acceptance Criteria")
	for _, ac := range ph.AcceptanceCriteria {
		b.WriteString(bullet(ac.ID + ": " + ac.Text))
	}
	section("Deliverables")
	bullets(ph.Deliverables)
	section("Depends On")
	if len(ph.DependsOn) == 0 {
		b.WriteString("None\n")
	} else {
		refs := make([]string, len(ph.DependsOn))
		for i, d := range ph.DependsOn {
			refs[i] = "Phase " + itoa(d)
		}
		b.WriteString(strings.Join(refs, ", ") + "\n")
	}
	if len(ph.Requirements) > 0 {
		section("Requirements")
		for _, r := range ph.Requirements {
			b.WriteString("- " + r + reqACSuffix(ph, r, traces) + "\n")
		}
	}
	section("Quality Gates")
	for _, g := range ph.Gates {
		b.WriteString("- " + g.Type + ": " + gateSpec(g) + "\n")
	}
	return b.String()
}

// reqACSuffix lists the AC of ph that traces map to r, unless they are all
// of the phase's AC (the default when none are listed).
func reqACSuffix(ph Phase, r string, traces []Trace) string {
	own := map[string]bool{}
	for _, ac := range ph.AcceptanceCriteria {
		own[ac.ID] = true
	}
	var listed []string
	for _, t := range traces {
		if t.Requirement != r {
			continue
		}
		for _, a := range t.AcceptanceCriteria {
			if own[a] {
				listed = appendUnique(listed, a)
			}
		}
	}
	if len(listed) == 0 || len(listed) == len(own) {
		return ""
	}
	return ": " + strings.Join(sortedUniqueACs(listed), ", ")
}

func gateSpec(g GateEntry) string {
	if g.NotApplicable {
		if g.Justification == "" {
			return "not_applicable"
		}
		return "not_applicable — " + g.Justification
	}
	var parts []string
	for _, c := range g.Commands {
		parts = append(parts, codeSpan(c))
	}
	if len(g.Scanners) > 0 {
		parts = append(parts, "scanners: "+strings.Join(g.Scanners, ", "))
	}
	if len(parts) == 0 {
		return "required"
	}
	return strings.Join(parts, "; ")
}

// bullet renders a list item; continuation lines are indented under it.
func bullet(text string) string {
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] != "" {
			lines[i] = "  " + lines[i]
		}
	}
	return "- " + strings.Join(lines, "\n") + "\n"
}

// RenderRequirements writes requirements.md.
func RenderRequirements(reqs []Requirement) string {
	var b strings.Builder
	b.WriteString("# Requirements\n\n")
	for _, r := range reqs {
		kind := ""
		if r.Kind != "" {
			kind = " (" + string(r.Kind) + ")"
		}
		b.WriteString(bullet(r.ID + kind + ": " + r.Text))
	}
	return b.String()
}

// RenderPlanFile writes IMPLEMENTATION_PLAN.md: front matter, title, overview.
func RenderPlanFile(p *Package) string {
	var b strings.Builder
	if len(p.FrontMatter) > 0 {
		b.WriteString("---\n" + strings.TrimRight(string(NormalizeText(p.FrontMatter)), "\n") + "\n---\n")
	}
	ov := strings.TrimLeft(p.Overview, "\n")
	if !strings.HasPrefix(ov, "# ") {
		b.WriteString("# " + p.Title + "\n\n")
	}
	b.WriteString(ov)
	if !strings.HasSuffix(ov, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// RenderFiles returns the package-directory files for p (paths relative to
// the package directory).
func RenderFiles(p *Package) map[string][]byte {
	out := map[string][]byte{
		PlanFileName:     []byte(RenderPlanFile(p)),
		RequirementsName: []byte(RenderRequirements(p.Requirements)),
		TraceabilityName: MarshalTraceability(p.Traceability),
	}
	for _, ph := range p.Phases {
		out[PhasesDirName+"/"+ph.ID+".md"] = []byte(RenderPhase(ph, p.Traceability))
	}
	return out
}

// WriteDir writes p as a package directory (created if needed). Existing
// files with other names are left alone.
func WriteDir(dir string, p *Package) error {
	for rel, data := range RenderFiles(p) {
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(target, data); err != nil {
			return err
		}
	}
	return nil
}

// writeFileAtomic writes via a temporary file and rename.
func writeFileAtomic(target string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".tmp-"+filepath.Base(target)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, target); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }
