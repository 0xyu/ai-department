package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The structured (YAML/JSON) plan form, described by schemas/plan.schema.json.
// It is also the shape of the planner's structured output in Stage 1.
type structDoc struct {
	Version      int           `yaml:"version" json:"version"`
	Title        string        `yaml:"title" json:"title"`
	Overview     string        `yaml:"overview" json:"overview"`
	Config       rawConfig     `yaml:"config" json:"config"`
	Requirements []structReq   `yaml:"requirements" json:"requirements"`
	Phases       []structPhase `yaml:"phases" json:"phases"`
	Traceability []structTrace `yaml:"traceability" json:"traceability"`
	AgentsMD     string        `yaml:"agents_md" json:"agents_md"`
}

type structReq struct {
	ID   string `yaml:"id" json:"id"`
	Kind string `yaml:"kind" json:"kind"`
	Text string `yaml:"text" json:"text"`
}

type structScope struct {
	In    []string `yaml:"in" json:"in"`
	Out   []string `yaml:"out" json:"out"`
	Paths []string `yaml:"paths" json:"paths"`
}

type structAC struct {
	ID   string `yaml:"id" json:"id"`
	Text string `yaml:"text" json:"text"`
}

type structGate struct {
	Commands      []string `yaml:"commands" json:"commands"`
	Scanners      []string `yaml:"scanners" json:"scanners"`
	NotApplicable bool     `yaml:"not_applicable" json:"not_applicable"`
	Justification string   `yaml:"justification" json:"justification"`
}

type structPhase struct {
	Number                int                    `yaml:"number" json:"number"`
	Title                 string                 `yaml:"title" json:"title"`
	Goal                  string                 `yaml:"goal" json:"goal"`
	Scope                 structScope            `yaml:"scope" json:"scope"`
	TechnicalRequirements []string               `yaml:"technical_requirements" json:"technical_requirements"`
	AcceptanceCriteria    []structAC             `yaml:"acceptance_criteria" json:"acceptance_criteria"`
	Deliverables          []string               `yaml:"deliverables" json:"deliverables"`
	DependsOn             []phaseRef             `yaml:"depends_on" json:"depends_on"`
	Requirements          []string               `yaml:"requirements" json:"requirements"`
	Gates                 map[string]*structGate `yaml:"gates" json:"gates"`
}

type structTrace struct {
	Requirement        string     `yaml:"requirement" json:"requirement"`
	Phases             []phaseRef `yaml:"phases" json:"phases"`
	AcceptanceCriteria []string   `yaml:"acceptance_criteria" json:"acceptance_criteria"`
}

// traceFile is traceability.json.
type traceFile struct {
	Version int           `json:"version"`
	Entries []structTrace `json:"entries"`
}

// phaseRef is a phase reference: 3, "3", "P03", or "Phase 3".
type phaseRef int

var phaseRefRE = regexp.MustCompile(`(?i)^(?:phase[ \t]*|p)?0*(\d{1,4})$`)

func parsePhaseRef(s string) (phaseRef, error) {
	m := phaseRefRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid phase reference %q (want 3, \"P03\", or \"Phase 3\")", clip(s))
	}
	n, _ := strconv.Atoi(m[1])
	if n <= 0 {
		return 0, fmt.Errorf("invalid phase reference %q", clip(s))
	}
	return phaseRef(n), nil
}

func (r *phaseRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: phase reference must be a number or string", n.Line)
	}
	v, err := parsePhaseRef(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*r = v
	return nil
}

func (r *phaseRef) UnmarshalJSON(b []byte) error {
	var s string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	} else {
		s = string(b)
	}
	v, err := parsePhaseRef(s)
	if err != nil {
		return err
	}
	*r = v
	return nil
}

func (r phaseRef) MarshalJSON() ([]byte, error) { return json.Marshal(PhaseID(int(r))) }

// rawConfig keeps the "config" block as YAML for the plan config layer.
type rawConfig []byte

func (c *rawConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: config must be a mapping", n.Line)
	}
	b, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	*c = b
	return nil
}

func (c *rawConfig) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if bytes.Equal(t, []byte("null")) {
		return nil
	}
	if len(t) == 0 || t[0] != '{' {
		return errors.New("config must be an object")
	}
	*c = append([]byte(nil), t...) // JSON is valid YAML for the config loader
	return nil
}

func decodeStrict(raw []byte, kind SourceKind, v any) error {
	if kind == SourceJSON {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return jsonErr(raw, err)
		}
		if _, err := dec.Token(); err != io.EOF {
			return errors.New("unexpected data after the JSON document")
		}
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("empty document")
		}
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("only one YAML document is allowed")
	}
	return nil
}

func jsonErr(raw []byte, err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Errorf("line %d: %v", 1+bytes.Count(raw[:min(int(se.Offset), len(raw))], []byte("\n")), err)
	}
	return err
}

// yamlItemLines returns the line of each item of the top-level sequence key.
func yamlItemLines(raw []byte, key string) []int {
	var root yaml.Node
	if yaml.Unmarshal(raw, &root) != nil || len(root.Content) == 0 {
		return nil
	}
	m := root.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.SequenceNode {
			var out []int
			for _, it := range m.Content[i+1].Content {
				out = append(out, it.Line)
			}
			return out
		}
	}
	return nil
}

func lineAt(lines []int, i int) int {
	if i < len(lines) {
		return lines[i]
	}
	return 0
}

func loadStructured(name string, raw []byte, kind SourceKind) *Package {
	pkg := &Package{Source: Source{Kind: kind, Name: name}}
	pkg.Files = []File{{Path: name, Raw: raw, Hash: HashText(raw)}}
	var doc structDoc
	if err := decodeStrict(raw, kind, &doc); err != nil {
		pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeParseError, SeverityError, Location{File: name}, name, err.Error()))
		return pkg
	}
	if doc.Version != StructuredVersion {
		pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeUnsupportedPlanFormat, SeverityError, Location{File: name},
			name, strconv.Itoa(doc.Version), strconv.Itoa(StructuredVersion)))
	}
	var reqLines, phaseLines, traceLines []int
	if kind == SourceYAML {
		reqLines, phaseLines, traceLines = yamlItemLines(raw, "requirements"), yamlItemLines(raw, "phases"), yamlItemLines(raw, "traceability")
	}
	pkg.Title = strings.TrimSpace(doc.Title)
	pkg.Overview = string(NormalizeText([]byte(doc.Overview)))
	pkg.FrontMatter = doc.Config
	pkg.ProposedAgentsMD = doc.AgentsMD

	for i, r := range doc.Requirements {
		loc := Location{File: name, Line: lineAt(reqLines, i)}
		id, ok := canonicalReq(r.ID)
		if !ok {
			pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeInvalidID, SeverityError, loc, clip(r.ID), "requirement IDs look like REQ-001"))
			continue
		}
		loc.Ref = id
		req := Requirement{ID: id, Text: strings.TrimSpace(r.Text), Loc: loc}
		if r.Kind != "" {
			k, ok := normalizeKind(r.Kind)
			if !ok {
				pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeInvalidID, SeverityError, loc, id, "unknown kind \""+r.Kind+"\""))
			}
			req.Kind = k
		}
		pkg.Requirements = append(pkg.Requirements, req)
	}

	for i, sp := range doc.Phases {
		loc := Location{File: name, Line: lineAt(phaseLines, i)}
		if sp.Number <= 0 {
			pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeInvalidID, SeverityError, loc, "phases["+strconv.Itoa(i)+"]", "number must be ≥ 1"))
			continue
		}
		ph := Phase{
			Number: sp.Number, ID: PhaseID(sp.Number), Title: strings.TrimSpace(sp.Title), Goal: strings.TrimSpace(sp.Goal),
			Scope:                 Scope{In: trimAll(sp.Scope.In), Out: trimAll(sp.Scope.Out), Paths: trimAll(sp.Scope.Paths)},
			TechnicalRequirements: trimAll(sp.TechnicalRequirements),
			Deliverables:          trimAll(sp.Deliverables),
		}
		loc.Phase = ph.ID
		ph.Loc = loc
		for _, d := range sp.DependsOn {
			ph.DependsOn = append(ph.DependsOn, int(d))
		}
		ph.DependsOn = sortedUniqueInts(ph.DependsOn)
		for j, a := range sp.AcceptanceCriteria {
			ac := AcceptanceCriterion{ID: ACID(ph.Number, j+1), Text: strings.TrimSpace(a.Text)}
			if a.ID != "" {
				p, k, ok := parseACID(a.ID)
				aloc := loc
				aloc.Section = secAC
				switch {
				case !ok:
					pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeInvalidID, SeverityError, aloc, clip(a.ID), "acceptance criterion IDs look like AC-03.2"))
				case p != ph.Number:
					aloc.Ref = ACID(p, k)
					pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeInvalidID, SeverityError, aloc, ACID(p, k), "belongs to "+PhaseID(p)+", not "+ph.ID))
					ac.ID, ac.Explicit = ACID(p, k), true
				default:
					ac.ID, ac.Explicit = ACID(p, k), true
				}
			}
			ac.Loc = Location{File: name, Line: loc.Line, Phase: ph.ID, Section: secAC, Ref: ac.ID}
			ph.AcceptanceCriteria = append(ph.AcceptanceCriteria, ac)
		}
		for _, r := range sp.Requirements {
			id, ok := canonicalReq(r)
			if !ok {
				l := loc
				l.Section = secRequirements
				pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeInvalidID, SeverityError, l, clip(r), "requirement IDs look like REQ-001"))
				continue
			}
			ph.Requirements = appendUnique(ph.Requirements, id)
		}
		gloc := loc
		gloc.Section = secGates
		for _, t := range sortedGateKeys(sp.Gates) {
			typ, ok := canonicalGateType(t)
			l := gloc
			if !ok || typ != strings.ToLower(t) {
				pkg.Diagnostics = append(pkg.Diagnostics, NewDiagnostic(CodeUnknownGateType, SeverityError, l, ph.ID, t))
				continue
			}
			l.Ref = typ
			g := GateEntry{Type: typ}
			if sg := sp.Gates[t]; sg != nil {
				g.NotApplicable, g.Justification = sg.NotApplicable, strings.TrimSpace(sg.Justification)
				g.Commands = trimAll(sg.Commands)
				for _, s := range sg.Scanners {
					g.Scanners = append(g.Scanners, strings.ToLower(strings.TrimSpace(s)))
				}
			}
			if d := checkScanners(g, ph.ID, l); d != nil {
				pkg.Diagnostics = append(pkg.Diagnostics, *d)
				continue
			}
			ph.Gates = append(ph.Gates, g)
		}
		sortGates(ph.Gates)
		pkg.Phases = append(pkg.Phases, ph)
	}

	traces, diags := convertTraces(doc.Traceability, name, traceLines)
	pkg.Traceability = traces
	pkg.Diagnostics = append(pkg.Diagnostics, diags...)
	for i := range pkg.Phases {
		pkg.Phases[i].Raw = RenderPhase(pkg.Phases[i], pkg.Traceability)
	}
	return pkg
}

func sortedGateKeys(m map[string]*structGate) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		for i, g := range GateTypes {
			if g == k {
				return i
			}
		}
		return len(GateTypes)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && (rank(keys[j]) < rank(keys[j-1]) || rank(keys[j]) == rank(keys[j-1]) && keys[j] < keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func trimAll(in []string) []string {
	var out []string
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func convertTraces(in []structTrace, file string, lines []int) ([]Trace, []Diagnostic) {
	var out []Trace
	var diags []Diagnostic
	for i, st := range in {
		loc := Location{File: file, Line: lineAt(lines, i)}
		id, ok := canonicalReq(st.Requirement)
		if !ok {
			diags = append(diags, NewDiagnostic(CodeInvalidID, SeverityError, loc, clip(st.Requirement), "requirement IDs look like REQ-001"))
			continue
		}
		loc.Ref = id
		t := Trace{Requirement: id, Loc: loc}
		for _, p := range st.Phases {
			t.Phases = append(t.Phases, int(p))
		}
		for _, a := range st.AcceptanceCriteria {
			p, k, ok := parseACID(a)
			if !ok {
				diags = append(diags, NewDiagnostic(CodeInvalidID, SeverityError, loc, clip(a), "acceptance criterion IDs look like AC-03.2"))
				continue
			}
			t.AcceptanceCriteria = append(t.AcceptanceCriteria, ACID(p, k))
		}
		t.Phases = sortedUniqueInts(t.Phases)
		t.AcceptanceCriteria = sortedUniqueACs(t.AcceptanceCriteria)
		out = append(out, t)
	}
	return out, diags
}

func parseTraceabilityJSON(raw []byte) ([]Trace, []Diagnostic) {
	var tf traceFile
	if err := decodeStrict(raw, SourceJSON, &tf); err != nil {
		return nil, []Diagnostic{NewDiagnostic(CodeParseError, SeverityError, Location{File: TraceabilityName}, TraceabilityName, err.Error())}
	}
	var diags []Diagnostic
	if tf.Version != StructuredVersion {
		diags = append(diags, NewDiagnostic(CodeUnsupportedPlanFormat, SeverityError, Location{File: TraceabilityName},
			TraceabilityName, strconv.Itoa(tf.Version), strconv.Itoa(StructuredVersion)))
	}
	traces, more := convertTraces(tf.Entries, TraceabilityName, nil)
	return traces, append(diags, more...)
}

// MarshalTraceability renders traceability.json (deterministic, LF, trailing newline).
func MarshalTraceability(ts []Trace) []byte {
	tf := traceFile{Version: StructuredVersion, Entries: []structTrace{}}
	for _, t := range canonicalTrace(ts) {
		st := structTrace{Requirement: t.Requirement, Phases: []phaseRef{}, AcceptanceCriteria: []string{}}
		for _, p := range t.Phases {
			st.Phases = append(st.Phases, phaseRef(p))
		}
		st.AcceptanceCriteria = append(st.AcceptanceCriteria, t.AcceptanceCriteria...)
		tf.Entries = append(tf.Entries, st)
	}
	b, _ := json.MarshalIndent(tf, "", "  ")
	return append(b, '\n')
}
