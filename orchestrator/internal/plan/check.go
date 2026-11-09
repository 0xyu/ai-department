package plan

import (
	"sort"
	"strconv"
	"strings"
)

// CheckOptions tune the phase-size heuristics (warnings only).
type CheckOptions struct {
	MaxAC        int // warn when a phase has more acceptance criteria
	MaxSpecBytes int // warn when a phase's Markdown is longer
}

// DefaultCheckOptions returns the default heuristics.
func DefaultCheckOptions() CheckOptions { return CheckOptions{MaxAC: 15, MaxSpecBytes: 24 << 10} }

// Validate returns the parse diagnostics plus the deterministic plan checks
// (§10.4), sorted.
func Validate(p *Package, opts CheckOptions) []Diagnostic {
	ds := append(append([]Diagnostic(nil), p.Diagnostics...), Check(p, opts)...)
	SortDiagnostics(ds)
	return ds
}

// Check runs the deterministic plan checks: contiguous numbering, unique
// IDs, dependency rules, required sections, the six gate entries, and full
// requirement traceability.
func Check(p *Package, opts CheckOptions) []Diagnostic {
	c := &checker{p: p, opts: opts}
	c.phases()
	c.dependencies()
	c.requirements()
	c.traceability()
	return c.out
}

type checker struct {
	p    *Package
	opts CheckOptions
	out  []Diagnostic
}

func (c *checker) add(code Code, sev Severity, loc Location, args ...string) {
	c.out = append(c.out, NewDiagnostic(code, sev, loc, args...))
}

func secLoc(ph *Phase, sec, ref string) Location {
	l := ph.Loc
	l.Section, l.Ref = sec, ref
	if line, ok := ph.sections[sec]; ok {
		l.Line = line
	}
	return l
}

func (c *checker) phases() {
	p := c.p
	if len(p.Phases) == 0 {
		c.add(CodeNoPhases, SeverityError, Location{File: c.planFile()})
		return
	}
	seen := map[int]bool{}
	numberingReported := false
	acSeen := map[string]bool{}
	for i := range p.Phases {
		ph := &p.Phases[i]
		if seen[ph.Number] {
			c.add(CodeDuplicateID, SeverityError, ph.Loc, ph.ID)
		}
		seen[ph.Number] = true
		if ph.Number != i+1 && !numberingReported {
			c.add(CodePhaseNumbering, SeverityError, ph.Loc, strconv.Itoa(i+1), strconv.Itoa(ph.Number))
			numberingReported = true
		}
		if ph.Title == "" {
			c.add(CodeMissingTitle, SeverityError, ph.Loc, ph.ID)
		}
		if strings.TrimSpace(ph.Goal) == "" {
			c.add(CodeMissingGoal, SeverityWarning, secLoc(ph, secGoal, ""), ph.ID)
		}
		if len(ph.Scope.In) == 0 && len(ph.Scope.Paths) == 0 {
			c.add(CodeMissingScope, SeverityError, secLoc(ph, secScope, ""), ph.ID)
		}
		if len(ph.AcceptanceCriteria) == 0 {
			c.add(CodeMissingAC, SeverityError, secLoc(ph, secAC, ""), ph.ID)
		}
		if len(ph.Deliverables) == 0 {
			c.add(CodeMissingDeliverables, SeverityError, secLoc(ph, secDeliverables, ""), ph.ID)
		}
		for _, ac := range ph.AcceptanceCriteria {
			if acSeen[ac.ID] {
				c.add(CodeDuplicateID, SeverityError, ac.Loc, ac.ID)
			}
			acSeen[ac.ID] = true
			if strings.TrimSpace(ac.Text) == "" {
				loc := ac.Loc
				if loc.File == "" {
					loc = secLoc(ph, secAC, ac.ID)
				}
				loc.Ref = ac.ID
				c.add(CodeEmptyAC, SeverityError, loc, ac.ID)
			}
		}
		for _, t := range GateTypes {
			g, ok := ph.Gate(t)
			loc := secLoc(ph, secGates, t)
			switch {
			case !ok:
				c.add(CodeMissingGateEntry, SeverityError, loc, ph.ID, t)
			case g.NotApplicable && g.Justification == "":
				c.add(CodeGateNAUnjustified, SeverityError, loc, ph.ID, t)
			}
			if ok && g.NotApplicable && (len(g.Commands) > 0 || len(g.Scanners) > 0) {
				c.add(CodeGateEntryConflict, SeverityError, loc, ph.ID, t)
			}
		}
		if c.opts.MaxAC > 0 && len(ph.AcceptanceCriteria) > c.opts.MaxAC {
			c.add(CodePhaseTooManyAC, SeverityWarning, secLoc(ph, secAC, ""), ph.ID, strconv.Itoa(len(ph.AcceptanceCriteria)), strconv.Itoa(c.opts.MaxAC))
		}
		if n := len(NormalizeText([]byte(ph.Raw))); c.opts.MaxSpecBytes > 0 && n > c.opts.MaxSpecBytes {
			c.add(CodePhaseSpecTooLong, SeverityWarning, ph.Loc, ph.ID, strconv.Itoa(n), strconv.Itoa(c.opts.MaxSpecBytes))
		}
	}
}

func (c *checker) planFile() string {
	switch c.p.Source.Kind {
	case SourceDir:
		return PlanFileName
	default:
		return c.p.Source.Name
	}
}

func (c *checker) dependencies() {
	p := c.p
	exists := map[int]bool{}
	for _, ph := range p.Phases {
		exists[ph.Number] = true
	}
	for i := range p.Phases {
		ph := &p.Phases[i]
		for _, d := range ph.DependsOn {
			loc := secLoc(ph, secDependsOn, PhaseID(d))
			switch {
			case !exists[d]:
				c.add(CodeUnknownDependency, SeverityError, loc, ph.ID, PhaseID(d))
			case d >= ph.Number:
				c.add(CodeForwardDependency, SeverityError, loc, ph.ID, PhaseID(d))
			}
		}
	}
	for _, cyc := range p.Graph().Cycles() {
		ids := make([]string, len(cyc)+1)
		for i, n := range cyc {
			ids[i] = PhaseID(n)
		}
		ids[len(cyc)] = PhaseID(cyc[0])
		ph := p.Phase(cyc[0])
		c.add(CodeDependencyCycle, SeverityError, secLoc(ph, secDependsOn, ""), strings.Join(ids, " → "))
	}
}

func (c *checker) requirements() {
	p := c.p
	if len(p.Requirements) == 0 {
		file := c.planFile()
		if p.Source.Kind == SourceDir {
			file = RequirementsName
		}
		c.add(CodeNoRequirements, SeverityError, Location{File: file})
	}
	seen := map[string]bool{}
	for _, r := range p.Requirements {
		if seen[r.ID] {
			c.add(CodeDuplicateID, SeverityError, r.Loc, r.ID)
		}
		seen[r.ID] = true
		if r.Kind == "" {
			c.add(CodeRequirementKind, SeverityWarning, r.Loc, r.ID)
		}
	}
	for i := range p.Phases {
		ph := &p.Phases[i]
		for _, r := range ph.Requirements {
			if !seen[r] {
				c.add(CodeUnknownReference, SeverityError, secLoc(ph, secRequirements, r), ph.ID, r)
			}
		}
		for _, r := range ph.Requirements {
			for _, a := range sortedUniqueACs(ph.reqAC[r]) {
				switch owner, _ := p.AcceptanceCriterion(a); {
				case owner == nil:
					c.add(CodeUnknownReference, SeverityError, secLoc(ph, secRequirements, a), r, a)
				case owner.Number != ph.Number:
					// A phase may only claim its own criteria for a requirement.
					c.add(CodeTraceACOutsidePhases, SeverityError, secLoc(ph, secRequirements, a), r, a, owner.ID)
				}
			}
		}
	}
}

func (c *checker) traceability() {
	p := c.p
	reqKnown := map[string]bool{}
	for _, r := range p.Requirements {
		reqKnown[r.ID] = true
	}
	covered := map[string]bool{}
	mapped := map[string]map[int]bool{} // REQ -> phases (traceability)
	seenEntry := map[string]bool{}
	for _, t := range p.Traceability {
		loc := t.Loc
		if loc.File == "" {
			loc.File = TraceabilityName
		}
		loc.Ref = t.Requirement
		if seenEntry[t.Requirement] && !p.TraceDerived {
			c.add(CodeDuplicateID, SeverityError, loc, t.Requirement)
		}
		seenEntry[t.Requirement] = true
		if !reqKnown[t.Requirement] {
			c.add(CodeUnknownReference, SeverityError, loc, "traceability", t.Requirement)
		}
		if mapped[t.Requirement] == nil {
			mapped[t.Requirement] = map[int]bool{}
		}
		phaseOK := false
		for _, n := range t.Phases {
			if p.Phase(n) == nil {
				c.add(CodeUnknownReference, SeverityError, loc, t.Requirement, PhaseID(n))
				continue
			}
			mapped[t.Requirement][n] = true
			phaseOK = true
		}
		// Coverage needs at least one valid pair: an existing, non-empty AC
		// whose own phase is in this entry's phases.
		pairOK := false
		for _, a := range t.AcceptanceCriteria {
			owner, ac := p.AcceptanceCriterion(a)
			if owner == nil {
				c.add(CodeUnknownReference, SeverityError, loc, t.Requirement, a)
				continue
			}
			if !containsInt(t.Phases, owner.Number) {
				if !p.TraceDerived { // derived traces only hold the phase's own AC; the phase check reports the rest
					c.add(CodeTraceACOutsidePhases, SeverityError, loc, t.Requirement, a, owner.ID)
				}
				continue
			}
			if strings.TrimSpace(ac.Text) != "" {
				pairOK = true
			}
		}
		if phaseOK && pairOK {
			covered[t.Requirement] = true
		}
	}
	for _, r := range p.Requirements {
		if !covered[r.ID] {
			c.add(CodeRequirementUncovered, SeverityError, r.Loc, r.ID)
		}
	}
	if p.TraceDerived {
		return
	}
	// Explicit traceability must agree with the phases' own lists.
	for i := range p.Phases {
		ph := &p.Phases[i]
		for _, r := range ph.Requirements {
			if reqKnown[r] && !mapped[r][ph.Number] {
				c.add(CodeTraceMismatch, SeverityWarning, secLoc(ph, secRequirements, r), r, ph.ID)
			}
		}
	}
	reqs := make([]string, 0, len(mapped))
	for r := range mapped {
		reqs = append(reqs, r)
	}
	sort.Slice(reqs, func(i, j int) bool { return reqLess(reqs[i], reqs[j]) })
	for _, r := range reqs {
		for _, n := range sortedKeys(mapped[r]) {
			ph := p.Phase(n)
			if len(ph.Requirements) > 0 && !containsString(ph.Requirements, r) {
				c.add(CodeTraceMismatch, SeverityWarning, secLoc(ph, secRequirements, r), r, ph.ID)
			}
		}
	}
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// --- dependency graph ---

// Graph is the phase dependency graph (edges point from a phase to the
// phases it depends on; unknown phases are left out).
type Graph struct {
	nodes []int
	deps  map[int][]int
}

// Graph builds the dependency graph of p.
func (p *Package) Graph() *Graph {
	g := &Graph{deps: map[int][]int{}}
	exists := map[int]bool{}
	for _, ph := range p.Phases {
		if !exists[ph.Number] {
			g.nodes = append(g.nodes, ph.Number)
		}
		exists[ph.Number] = true
	}
	sort.Ints(g.nodes)
	for _, ph := range p.Phases {
		for _, d := range ph.DependsOn {
			if exists[d] {
				g.deps[ph.Number] = append(g.deps[ph.Number], d)
			}
		}
		g.deps[ph.Number] = sortedUniqueInts(g.deps[ph.Number])
	}
	return g
}

// Dependencies returns the direct dependencies of phase n.
func (g *Graph) Dependencies(n int) []int { return cloneInts(g.deps[n]) }

// Ancestors returns every phase n depends on, directly or transitively, sorted.
func (g *Graph) Ancestors(n int) []int {
	seen := map[int]bool{}
	stack := append([]int(nil), g.deps[n]...)
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[x] || x == n {
			continue
		}
		seen[x] = true
		stack = append(stack, g.deps[x]...)
	}
	return sortedKeys(seen)
}

// Cycles returns each dependency cycle once, rotated to start at its lowest
// phase number, in order.
func (g *Graph) Cycles() [][]int {
	const (
		white = iota
		grey
		black
	)
	color := map[int]int{}
	var stack []int
	found := map[string][]int{}
	var visit func(n int)
	visit = func(n int) {
		color[n] = grey
		stack = append(stack, n)
		for _, d := range g.deps[n] {
			switch color[d] {
			case white:
				visit(d)
			case grey:
				// Cycle: from d's position in the stack to the top.
				i := len(stack) - 1
				for i >= 0 && stack[i] != d {
					i--
				}
				cyc := rotateMin(append([]int(nil), stack[i:]...))
				key := ""
				for _, x := range cyc {
					key += strconv.Itoa(x) + ","
				}
				found[key] = cyc
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}
	for _, n := range g.nodes {
		if color[n] == white {
			visit(n)
		}
	}
	out := make([][]int, 0, len(found))
	for _, c := range found {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	return out
}

func rotateMin(c []int) []int {
	m := 0
	for i, v := range c {
		if v < c[m] {
			m = i
		}
	}
	return append(c[m:], c[:m]...)
}
