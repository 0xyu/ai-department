package plan

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/0xyu/aco/internal/config"
)

var (
	phaseHeadingRE = regexp.MustCompile(`(?i)^phase[ \t]+0*(\d{1,4})(?:[ \t]*[:：.\-–—]+[ \t]*(.*))?$`)
	reqTokenRE     = regexp.MustCompile(`(?i)\bREQ-\d{1,6}\b`)
	acTokenRE      = regexp.MustCompile(`(?i)\bAC-\d{1,4}\.\d{1,4}\b`)
	acPrefixRE     = regexp.MustCompile(`(?is)^[*_` + "`" + `]*(AC-\d{1,4}\.\d{1,4})[*_` + "`" + `]*[ \t]*(?:[:：.)\-–—][ \t]*)?(.*)$`)
	reqPrefixRE    = regexp.MustCompile(`(?is)^[*_` + "`" + `]*(REQ-\d{1,6})[*_` + "`" + `]*[ \t]*(.*)$`)
	depTokenRE     = regexp.MustCompile(`(?i)^(?:phase[ \t]*|p)?0*(\d{1,4})\b`)
	gateLabelRE    = regexp.MustCompile(`(?s)^[*_` + "`" + `]*([A-Za-z][A-Za-z _-]*?)[*_` + "`" + `]*[ \t]*[:：][ \t]*(.*)$`)
	naRE           = regexp.MustCompile(`(?i)^(?:not[ _-]applicable|n/a)\b`)
	depSplitRE     = regexp.MustCompile(`[,;\n]|\band\b`)
)

// Section names inside a Phase Specification.
const (
	secGoal         = "goal"
	secScope        = "scope"
	secTechnical    = "technical_requirements"
	secAC           = "acceptance_criteria"
	secDeliverables = "deliverables"
	secDependsOn    = "depends_on"
	secGates        = "quality_gates"
	secRequirements = "requirements"
)

func sectionKey(title string) string {
	t := strings.ToLower(strings.TrimSpace(stripEmphasis(title)))
	t = strings.TrimRight(t, ":： ")
	t = strings.Join(strings.Fields(t), " ")
	switch t {
	case "goal", "goals", "objective":
		return secGoal
	case "scope":
		return secScope
	case "technical requirements", "technical requirement":
		return secTechnical
	case "acceptance criteria", "acceptance criterion":
		return secAC
	case "deliverables", "deliverable":
		return secDeliverables
	case "depends on", "dependencies", "dependency":
		return secDependsOn
	case "quality gates", "quality gate", "gates":
		return secGates
	case "requirements", "requirements covered", "covered requirements":
		return secRequirements
	}
	return ""
}

// phaseHeading reports whether h opens a Phase Specification.
func phaseHeading(h heading) (num int, title string, ok bool) {
	m := phaseHeadingRE.FindStringSubmatch(h.text)
	if m == nil {
		return 0, "", false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, "", false
	}
	return n, strings.TrimSpace(stripEmphasis(m[2])), true
}

type phaseRange struct {
	h        heading
	num      int
	title    string
	from, to int // line range of the section, heading included
}

// findPhases returns the top-level Phase Specification sections in lines
// [from, to). A phase heading nested inside another phase section is not a
// separate phase.
func (d *mdDoc) findPhases(from, to int) []phaseRange {
	var out []phaseRange
	next := from
	for _, h := range d.headings {
		if h.idx < next || h.idx >= to {
			continue
		}
		num, title, ok := phaseHeading(h)
		if !ok {
			continue
		}
		end := d.sectionEnd(h, to)
		out = append(out, phaseRange{h: h, num: num, title: title, from: h.idx, to: end})
		next = end
	}
	return out
}

// parsePhase parses one Phase Specification section.
func (d *mdDoc) parsePhase(pr phaseRange, file string) (Phase, []Diagnostic) {
	ph := Phase{
		Number: pr.num, ID: PhaseID(pr.num), Title: pr.title,
		Loc:      Location{File: file, Line: d.lines[pr.from].num, Phase: PhaseID(pr.num)},
		sections: map[string]int{},
		reqAC:    map[string][]string{},
	}
	ph.Raw = d.raw(pr.from, d.trimEnd(pr.from+1, pr.to))
	var diags []Diagnostic
	loc := func(sec string, line int, ref string) Location {
		return Location{File: file, Line: line, Phase: ph.ID, Section: sec, Ref: ref}
	}

	// Sub-sections are the headings one level below the phase heading (the
	// first deeper heading sets the level, so "# Phase" + "### Goal" works).
	subLevel := 0
	for _, h := range d.headings {
		if h.idx > pr.from && h.idx < pr.to && h.level > pr.h.level {
			subLevel = h.level
			break
		}
	}
	for _, h := range d.headings {
		if subLevel == 0 || h.idx <= pr.from || h.idx >= pr.to || h.level != subLevel {
			continue
		}
		key := sectionKey(h.text)
		if key == "" {
			continue // extra sections are kept in Raw only
		}
		line := d.lines[h.idx].num
		if _, dup := ph.sections[key]; dup {
			diags = append(diags, NewDiagnostic(CodeDuplicateSection, SeverityError, loc(key, line, ""), h.text, ph.ID))
			continue
		}
		ph.sections[key] = line
		from := h.idx + 1
		to := d.trimEnd(from, d.sectionEnd(h, pr.to))
		switch key {
		case secGoal:
			ph.Goal = d.text(from, to)
		case secScope:
			ph.Scope = d.parseScope(from, to, subLevel)
		case secTechnical:
			ph.TechnicalRequirements = itemTexts(d.entries(from, to))
		case secDeliverables:
			ph.Deliverables = itemTexts(d.entries(from, to))
		case secAC:
			for i, it := range d.entries(from, to) {
				ac, diag := parseAC(ph.Number, i+1, it.text, loc(secAC, it.line, ""))
				ac.Loc = loc(secAC, it.line, ac.ID)
				ph.AcceptanceCriteria = append(ph.AcceptanceCriteria, ac)
				if diag != nil {
					diags = append(diags, *diag)
				}
			}
		case secDependsOn:
			deps, bad := parseDeps(d.text(from, to))
			ph.DependsOn = deps
			for _, b := range bad {
				diags = append(diags, NewDiagnostic(CodeDependencyInvalid, SeverityError, loc(key, line, ""), ph.ID, b))
			}
		case secRequirements:
			for _, it := range d.entries(from, to) {
				reqs, acs := refsIn(it.text)
				if len(reqs) == 0 {
					diags = append(diags, NewDiagnostic(CodeInvalidID, SeverityError, loc(key, it.line, ""), it.text, "no REQ-NNN ID in this entry"))
					continue
				}
				// Grouped entries ("- REQ-004, REQ-001: AC-01.2") give the
				// listed AC to every listed REQ; each is then checked against
				// this phase. An entry without AC keeps the all-AC fallback.
				for _, r := range reqs {
					ph.Requirements = appendUnique(ph.Requirements, r)
					if len(acs) > 0 {
						ph.reqAC[r] = append(ph.reqAC[r], acs...)
					}
				}
			}
		case secGates:
			for _, it := range d.entries(from, to) {
				g, diag := parseGateLine(it.text, ph.ID, loc(secGates, it.line, ""))
				if diag != nil {
					diags = append(diags, *diag)
					continue
				}
				if _, dup := ph.Gate(g.Type); dup {
					l := loc(secGates, it.line, g.Type)
					diags = append(diags, NewDiagnostic(CodeDuplicateGateEntry, SeverityError, l, ph.ID, g.Type))
					continue
				}
				ph.Gates = append(ph.Gates, g)
			}
		}
	}
	sortGates(ph.Gates)
	return ph, diags
}

func itemTexts(items []item) []string {
	var out []string
	for _, it := range items {
		if it.text != "" {
			out = append(out, it.text)
		}
	}
	return out
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// cutLabel strips a leading "Label:" (case-insensitive, optional emphasis).
func cutLabel(s string, labels ...string) (string, bool) {
	t := strings.TrimLeft(strings.TrimSpace(s), "*_")
	for _, lb := range labels {
		if len(t) < len(lb) || !strings.EqualFold(t[:len(lb)], lb) {
			continue
		}
		r := strings.TrimLeft(t[len(lb):], "*_ \t")
		for _, colon := range []string{":", "："} {
			if after, ok := strings.CutPrefix(r, colon); ok {
				return strings.TrimSpace(strings.TrimLeft(after, "*_ \t")), true
			}
		}
	}
	return s, false
}

var (
	pathLabels = []string{"scope paths", "scope.paths", "paths", "path"}
	outLabels  = []string{"out of scope", "out-of-scope", "out"}
	inLabels   = []string{"in scope", "in-scope"}
)

// parseScope reads a Scope section: plain items are in scope; "Out of
// scope:" items and items under an "Out of scope" sub-heading are out of
// scope; "Paths:" items (or a "Paths" sub-heading) list scope.paths globs.
func (d *mdDoc) parseScope(from, to, level int) Scope {
	var sc Scope
	mode := "in"
	segStart := from
	flush := func(end int) {
		for _, it := range d.entries(segStart, end) {
			text := it.text
			if rest, ok := cutLabel(text, pathLabels...); ok {
				sc.Paths = append(sc.Paths, parseGlobs(rest)...)
				continue
			}
			if rest, ok := cutLabel(text, outLabels...); ok {
				if rest != "" {
					sc.Out = append(sc.Out, rest)
				}
				continue
			}
			if rest, ok := cutLabel(text, inLabels...); ok {
				text = rest
			}
			if text == "" {
				continue
			}
			switch mode {
			case "out":
				sc.Out = append(sc.Out, text)
			case "paths":
				sc.Paths = append(sc.Paths, parseGlobs(text)...)
			default:
				sc.In = append(sc.In, text)
			}
		}
	}
	for _, h := range d.headings {
		if h.idx < from || h.idx >= to || h.level <= level {
			continue
		}
		flush(h.idx)
		segStart = h.idx + 1
		switch t := strings.ToLower(strings.TrimSpace(stripEmphasis(h.text))); {
		case strings.HasPrefix(t, "out"):
			mode = "out"
		case strings.Contains(t, "path"):
			mode = "paths"
		default:
			mode = "in"
		}
	}
	flush(to)
	return sc
}

// parseGlobs reads "`a/**`, `b`" or "a/**, b".
func parseGlobs(s string) []string {
	spans, rest := codeSpans(s)
	if len(spans) == 0 {
		spans = strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' })
	}
	var out []string
	for _, g := range spans {
		g = strings.TrimSpace(g)
		if g != "" {
			out = append(out, g)
		}
	}
	return out
}

// parseAC reads one acceptance criterion; index is its 1-based position.
func parseAC(phase, index int, text string, loc Location) (AcceptanceCriterion, *Diagnostic) {
	ac := AcceptanceCriterion{ID: ACID(phase, index), Text: text}
	m := acPrefixRE.FindStringSubmatch(text)
	if m == nil {
		return ac, nil
	}
	p, i, ok := parseACID(m[1])
	if !ok {
		return ac, nil
	}
	ac.ID, ac.Text, ac.Explicit = ACID(p, i), strings.TrimSpace(m[2]), true
	if p != phase {
		loc.Ref = ac.ID
		d := NewDiagnostic(CodeInvalidID, SeverityError, loc, ac.ID, "belongs to "+PhaseID(p)+", not "+PhaseID(phase))
		return ac, &d
	}
	return ac, nil
}

// parseDeps reads a Depends On section ("Phase 2, Phase 4", "P02", "None").
func parseDeps(text string) (deps []int, bad []string) {
	t := strings.ToLower(strings.TrimSpace(strings.Trim(stripEmphasis(text), ".")))
	switch t {
	case "", "none", "n/a", "-", "—", "–", "nothing", "no dependencies":
		return nil, nil
	}
	tokens := depSplitRE.Split(text, -1)
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if m := listRE.FindStringSubmatch(tok); m != nil {
			tok = strings.TrimSpace(m[4])
		}
		tok = strings.TrimSpace(stripEmphasis(tok))
		if tok == "" {
			continue
		}
		m := depTokenRE.FindStringSubmatch(tok)
		if m == nil {
			bad = append(bad, tok)
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n <= 0 {
			bad = append(bad, tok)
			continue
		}
		deps = append(deps, n)
	}
	return sortedUniqueInts(deps), bad
}

// refsIn returns the canonical REQ and AC IDs mentioned in s.
func refsIn(s string) (reqs, acs []string) {
	for _, m := range reqTokenRE.FindAllString(s, -1) {
		if id, ok := canonicalReq(m); ok {
			reqs = appendUnique(reqs, id)
		}
	}
	for _, m := range acTokenRE.FindAllString(s, -1) {
		if p, i, ok := parseACID(m); ok {
			acs = appendUnique(acs, ACID(p, i))
		}
	}
	return reqs, acs
}

var gateAliases = map[string]string{
	"typecheck": "typecheck", "typechecking": "typecheck", "types": "typecheck",
	"lint": "lint", "linting": "lint", "linter": "lint",
	"unit": "unit", "unittest": "unit", "unittests": "unit",
	"integration": "integration", "integrationtest": "integration", "integrationtests": "integration",
	"build":    "build",
	"security": "security", "securityscan": "security", "securityscanning": "security",
}

func canonicalGateType(s string) (string, bool) {
	k := strings.ToLower(s)
	k = strings.NewReplacer(" ", "", "-", "", "_", "").Replace(k)
	t, ok := gateAliases[k]
	return t, ok
}

// parseGateLine reads "typecheck: `go vet ./...`", "unit: required",
// "integration: not_applicable — <justification>", or
// "security: scanners: secrets, gosec; `extra command`".
func parseGateLine(text, phaseID string, loc Location) (GateEntry, *Diagnostic) {
	// Join continuation lines but keep the payload's own spacing: commands
	// such as `sh -c 'echo "a   b"'` must reach the gate runner unchanged.
	// Only the label and the control keywords are normalized.
	flat := strings.ReplaceAll(text, "\n", " ")
	m := gateLabelRE.FindStringSubmatch(flat)
	if m == nil {
		d := NewDiagnostic(CodeGateEntryInvalid, SeverityError, loc, phaseID,
			"expected \"<gate>: <commands | required | not_applicable — justification>\"")
		return GateEntry{}, &d
	}
	label := strings.Join(strings.Fields(m[1]), " ")
	typ, ok := canonicalGateType(label)
	if !ok {
		d := NewDiagnostic(CodeUnknownGateType, SeverityError, loc, phaseID, label)
		return GateEntry{}, &d
	}
	loc.Ref = typ
	g := GateEntry{Type: typ}
	spec := strings.TrimSpace(m[2])
	if na := naRE.FindString(spec); na != "" {
		g.NotApplicable = true
		rest := strings.TrimLeft(strings.TrimSpace(spec[len(na):]), ":：-–—,; \t")
		if inner, ok := strings.CutPrefix(rest, "("); ok && strings.HasSuffix(inner, ")") {
			rest = strings.TrimSuffix(inner, ")")
		}
		g.Justification = strings.TrimSpace(rest)
		return g, nil
	}
	spans, rest := codeSpans(spec)
	g.Commands = spans
	for _, part := range strings.Split(rest, ";") {
		part = strings.TrimSpace(part)
		if v, ok := cutLabel(part, "scanners", "scanner"); ok {
			for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
				g.Scanners = append(g.Scanners, strings.ToLower(s))
			}
			continue
		}
		if v, ok := cutLabel(part, "commands", "command"); ok {
			part = v
		}
		p := strings.Trim(part, " \t,;")
		switch strings.ToLower(strings.Join(strings.Fields(p), " ")) {
		case "", "required", "default", "config", "auto", "auto-detect", "auto_detect":
			continue
		}
		if len(spans) == 0 {
			g.Commands = append(g.Commands, p)
		}
	}
	if d := checkScanners(g, phaseID, loc); d != nil {
		return GateEntry{}, d
	}
	return g, nil
}

func checkScanners(g GateEntry, phaseID string, loc Location) *Diagnostic {
	if len(g.Scanners) == 0 {
		return nil
	}
	if g.Type != "security" {
		d := NewDiagnostic(CodeGateEntryInvalid, SeverityError, loc, phaseID, "only the security gate has scanners")
		return &d
	}
	for _, s := range g.Scanners {
		known := false
		for _, k := range config.SecurityScanners {
			known = known || s == k
		}
		if !known {
			d := NewDiagnostic(CodeGateEntryInvalid, SeverityError, loc, phaseID,
				"unknown scanner \""+s+"\" (known: "+strings.Join(config.SecurityScanners, ", ")+")")
			return &d
		}
	}
	return nil
}

func sortGates(gs []GateEntry) {
	rank := func(t string) int {
		for i, g := range GateTypes {
			if g == t {
				return i
			}
		}
		return len(GateTypes)
	}
	sort.SliceStable(gs, func(i, j int) bool { return rank(gs[i].Type) < rank(gs[j].Type) })
}

// --- requirements ---

func normalizeKind(s string) (RequirementKind, bool) {
	k := strings.ToLower(strings.TrimSpace(s))
	k = strings.NewReplacer("-", "", "_", "", " ", "").Replace(k)
	switch k {
	case "functional", "fr", "func":
		return KindFunctional, true
	case "nonfunctional", "nfr":
		return KindNonFunctional, true
	case "constraint", "constraints":
		return KindConstraint, true
	}
	return "", false
}

// kindFromHeading guesses a requirement kind from a heading's text.
func kindFromHeading(t string) RequirementKind {
	l := strings.ToLower(t)
	switch {
	case strings.Contains(l, "non-functional"), strings.Contains(l, "nonfunctional"), strings.Contains(l, "non functional"):
		return KindNonFunctional
	case strings.Contains(l, "constraint"):
		return KindConstraint
	case strings.Contains(l, "functional"):
		return KindFunctional
	}
	return ""
}

// parseRequirements extracts REQ-NNN definitions from lines [from, to):
// list items and table rows whose first token is a REQ ID. skip reports
// lines to ignore (phase sections in a single-file plan).
func (d *mdDoc) parseRequirements(file string, from, to int, skip func(int) bool) []Requirement {
	var out []Requirement
	type ctx struct {
		level int
		kind  RequirementKind
	}
	var stack []ctx
	kindNow := func() RequirementKind {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind != "" {
				return stack[i].kind
			}
		}
		return ""
	}
	// Segments start at each heading in range.
	starts := []int{from}
	for _, h := range d.headings {
		if h.idx > from && h.idx < to {
			starts = append(starts, h.idx)
		}
	}
	for si, s := range starts {
		e := to
		if si+1 < len(starts) {
			e = starts[si+1]
		}
		if h, ok := d.headingAt(s); ok {
			for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, ctx{level: h.level, kind: kindFromHeading(h.text)})
		}
		if skip != nil && skip(s) {
			continue
		}
		kind := kindNow()
		items, _ := d.blocks(s, e)
		for _, it := range items {
			if r, ok := parseReqItem(it.text, kind); ok {
				r.Loc = Location{File: file, Line: it.line, Ref: r.ID}
				out = append(out, r)
			}
		}
		for i := s; i < e; i++ {
			if d.inFence[i] {
				continue
			}
			if r, ok := parseReqRow(d.lines[i].text, kind); ok {
				r.Loc = Location{File: file, Line: d.lines[i].num, Ref: r.ID}
				out = append(out, r)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Loc.Line < out[j].Loc.Line })
	return out
}

func parseReqItem(text string, ctxKind RequirementKind) (Requirement, bool) {
	m := reqPrefixRE.FindStringSubmatch(text)
	if m == nil {
		return Requirement{}, false
	}
	id, ok := canonicalReq(m[1])
	if !ok {
		return Requirement{}, false
	}
	r := Requirement{ID: id, Kind: ctxKind}
	rest := strings.TrimSpace(m[2])
	if len(rest) > 0 && (rest[0] == '(' || rest[0] == '[') {
		closeCh := ")"
		if rest[0] == '[' {
			closeCh = "]"
		}
		if end := strings.Index(rest, closeCh); end > 0 {
			if k, ok := normalizeKind(rest[1:end]); ok {
				r.Kind = k
				rest = strings.TrimSpace(rest[end+1:])
			}
		}
	}
	rest = strings.TrimLeft(rest, "*_")
	rest = strings.TrimLeft(rest, " \t")
	for _, sep := range []string{":", "：", "—", "–", "-", "."} {
		if after, ok := strings.CutPrefix(rest, sep); ok {
			rest = after
			break
		}
	}
	r.Text = strings.TrimSpace(rest)
	return r, true
}

func parseReqRow(line string, ctxKind RequirementKind) (Requirement, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "|") {
		return Requirement{}, false
	}
	cells := strings.Split(strings.Trim(t, "|"), "|")
	if len(cells) < 2 {
		return Requirement{}, false
	}
	id, ok := canonicalReq(stripEmphasis(cells[0]))
	if !ok {
		return Requirement{}, false
	}
	r := Requirement{ID: id, Kind: ctxKind}
	for _, c := range cells[1:] {
		c = strings.TrimSpace(c)
		if k, ok := normalizeKind(stripEmphasis(c)); ok && r.Text == "" {
			r.Kind = k
			continue
		}
		if r.Text == "" && c != "" {
			r.Text = c
		}
	}
	return r, true
}
