package plan

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Severity of a diagnostic.
type Severity string

// Severities. Errors fail validation; warnings fail it only in strict mode.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Code identifies a kind of plan problem. Codes are stable: they are part of
// the validate-plan JSON output and of the data passed to the agents.
type Code string

// Diagnostic codes.
const (
	CodeParseError            Code = "parse_error"
	CodeMissingFile           Code = "missing_file"
	CodeNoPhases              Code = "no_phases"
	CodePhaseNumbering        Code = "phase_numbering"
	CodePhaseFileInvalid      Code = "phase_file_invalid"
	CodeDuplicateID           Code = "duplicate_id"
	CodeInvalidID             Code = "invalid_id"
	CodeDuplicateSection      Code = "duplicate_section"
	CodeMissingTitle          Code = "missing_title"
	CodeMissingGoal           Code = "missing_goal"
	CodeMissingScope          Code = "missing_scope"
	CodeMissingAC             Code = "missing_acceptance_criteria"
	CodeMissingDeliverables   Code = "missing_deliverables"
	CodeDependencyInvalid     Code = "dependency_invalid"
	CodeUnknownDependency     Code = "unknown_dependency"
	CodeForwardDependency     Code = "forward_dependency"
	CodeDependencyCycle       Code = "dependency_cycle"
	CodeMissingGateEntry      Code = "missing_gate_entry"
	CodeUnknownGateType       Code = "unknown_gate_type"
	CodeDuplicateGateEntry    Code = "duplicate_gate_entry"
	CodeGateEntryInvalid      Code = "gate_entry_invalid"
	CodeGateNAUnjustified     Code = "gate_not_applicable_unjustified"
	CodeGateEntryConflict     Code = "gate_entry_conflict"
	CodeNoRequirements        Code = "no_requirements"
	CodeRequirementKind       Code = "requirement_kind_missing"
	CodeRequirementUncovered  Code = "requirement_uncovered"
	CodeUnknownReference      Code = "unknown_reference"
	CodeTraceMismatch         Code = "trace_mismatch"
	CodePhaseTooManyAC        Code = "phase_too_many_ac"
	CodePhaseSpecTooLong      Code = "phase_spec_too_long"
	CodeFrontMatterInvalid    Code = "front_matter_invalid"
	CodeLockMismatch          Code = "lock_mismatch"
	CodeLockUnverified        Code = "lock_unverified"
	CodeAgentsMDNotRegular    Code = "agents_md_not_regular"
	CodePathNotContained      Code = "path_not_contained"
	CodeTraceACOutsidePhases  Code = "trace_ac_outside_phases"
	CodeEmptyAC               Code = "empty_acceptance_criterion"
	CodeUnsupportedPlanFormat Code = "unsupported_version"
)

// codeFormats holds the English message of each code. Arguments are always
// strings. The operator-language text lives in the i18n catalog under
// "plan.check.<code>" with the same arguments (checked by a CLI test).
var codeFormats = map[Code]string{
	CodeParseError:            "cannot parse %s: %s",
	CodeMissingFile:           "required file %s is missing",
	CodeNoPhases:              "the plan defines no phases",
	CodePhaseNumbering:        "phases must be numbered 1..N in order: expected Phase %s, found Phase %s",
	CodePhaseFileInvalid:      "phase file %s: %s",
	CodeDuplicateID:           "duplicate ID %s",
	CodeInvalidID:             "invalid ID %s: %s",
	CodeDuplicateSection:      "section \"%s\" appears more than once in %s",
	CodeMissingTitle:          "%s has no title",
	CodeMissingGoal:           "%s has no Goal",
	CodeMissingScope:          "%s has no Scope",
	CodeMissingAC:             "%s has no Acceptance Criteria",
	CodeMissingDeliverables:   "%s has no Deliverables",
	CodeDependencyInvalid:     "%s: cannot read dependency \"%s\"",
	CodeUnknownDependency:     "%s depends on %s, which does not exist",
	CodeForwardDependency:     "%s depends on %s, which is not an earlier phase",
	CodeDependencyCycle:       "dependency cycle: %s",
	CodeMissingGateEntry:      "%s has no entry for quality gate \"%s\"",
	CodeUnknownGateType:       "%s: unknown quality gate type \"%s\"",
	CodeDuplicateGateEntry:    "%s: quality gate \"%s\" is listed more than once",
	CodeGateEntryInvalid:      "%s: invalid quality gate entry: %s",
	CodeGateNAUnjustified:     "%s: quality gate \"%s\" is not_applicable without a justification",
	CodeGateEntryConflict:     "%s: quality gate \"%s\" is not_applicable but lists commands or scanners",
	CodeNoRequirements:        "the plan defines no requirements (REQ-NNN)",
	CodeRequirementKind:       "%s has no kind (functional, non_functional, or constraint)",
	CodeRequirementUncovered:  "%s is not covered by any phase and acceptance criterion",
	CodeUnknownReference:      "%s references %s, which does not exist",
	CodeTraceMismatch:         "%s and %s disagree between the Phase Specification and traceability",
	CodePhaseTooManyAC:        "%s has %s acceptance criteria (more than %s); consider splitting the phase",
	CodePhaseSpecTooLong:      "%s specification is %s bytes (more than %s); consider splitting the phase",
	CodeFrontMatterInvalid:    "front matter is not a valid ACO config: %s at %s",
	CodeLockMismatch:          "frozen file %s does not match plan.lock.json: %s",
	CodeLockUnverified:        "plan.lock.json was not verified: %s",
	CodeAgentsMDNotRegular:    "%s is not a regular file",
	CodePathNotContained:      "%s is a symbolic link, junction, or other non-regular entry; plan paths must stay inside the repository",
	CodeTraceACOutsidePhases:  "%s maps %s, but that criterion belongs to %s, which is not in its phases",
	CodeEmptyAC:               "%s has no text",
	CodeUnsupportedPlanFormat: "%s: unsupported version %s (expected %s)",
}

// Codes returns every diagnostic code with its English message format.
func Codes() map[Code]string {
	out := make(map[Code]string, len(codeFormats))
	for k, v := range codeFormats {
		out[k] = v
	}
	return out
}

// Location points at the source of a diagnostic.
type Location struct {
	File    string `json:"file,omitempty"`    // slash-separated, relative to the package root
	Line    int    `json:"line,omitempty"`    // 1-based, 0 if unknown
	Phase   string `json:"phase,omitempty"`   // "P03"
	Section string `json:"section,omitempty"` // e.g. "acceptance_criteria"
	Ref     string `json:"ref,omitempty"`     // ID the problem is about (AC-03.2, REQ-004, typecheck)
}

// String renders "file:line#section".
func (l Location) String() string {
	var b strings.Builder
	b.WriteString(l.File)
	if l.Line > 0 {
		b.WriteString(":" + strconv.Itoa(l.Line))
	}
	if l.Section != "" {
		b.WriteString("#" + strings.ReplaceAll(l.Section, "_", "-"))
	}
	return b.String()
}

// Diagnostic is one structured plan problem.
type Diagnostic struct {
	Code     Code     `json:"code"`
	Severity Severity `json:"severity"`
	Location Location `json:"location"`
	Message  string   `json:"message"` // English (documents language)
	Args     []string `json:"-"`       // message arguments, for the operator-language rendering
}

const maxArgLen = 120

// NewDiagnostic builds a diagnostic; args fill the code's message.
func NewDiagnostic(code Code, sev Severity, loc Location, args ...string) Diagnostic {
	for i, a := range args {
		args[i] = clip(a)
	}
	format, ok := codeFormats[code]
	if !ok {
		format = string(code)
	}
	vals := make([]any, len(args))
	for i, a := range args {
		vals[i] = a
	}
	return Diagnostic{Code: code, Severity: sev, Location: loc, Message: fmt.Sprintf(format, vals...), Args: args}
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxArgLen {
		return string(r[:maxArgLen-1]) + "…"
	}
	return s
}

// SortDiagnostics orders diagnostics by file, line, phase, code, message.
func SortDiagnostics(ds []Diagnostic) {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.Location.File != b.Location.File {
			return a.Location.File < b.Location.File
		}
		if a.Location.Line != b.Location.Line {
			return a.Location.Line < b.Location.Line
		}
		if a.Location.Phase != b.Location.Phase {
			return a.Location.Phase < b.Location.Phase
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
}

// Count returns the number of errors and warnings.
func Count(ds []Diagnostic) (errs, warns int) {
	for _, d := range ds {
		if d.Severity == SeverityError {
			errs++
		} else {
			warns++
		}
	}
	return errs, warns
}

// Passes reports whether ds allow the plan: no errors, and in strict mode
// no warnings either.
func Passes(ds []Diagnostic, strict bool) bool {
	e, w := Count(ds)
	return e == 0 && (!strict || w == 0)
}

// --- small sorting helpers shared by loaders and checks ---

func sortedUniqueInts(in []int) []int {
	if len(in) == 0 {
		return nil
	}
	out := append([]int(nil), in...)
	sort.Ints(out)
	j := 0
	for i, v := range out {
		if i == 0 || v != out[j-1] {
			out[j] = v
			j++
		}
	}
	return out[:j]
}

func acLess(a, b string) bool {
	pa, ia, oka := parseACID(a)
	pb, ib, okb := parseACID(b)
	if oka && okb {
		if pa != pb {
			return pa < pb
		}
		return ia < ib
	}
	return a < b
}

func sortedUniqueACs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return acLess(out[i], out[j]) })
	j := 0
	for i, v := range out {
		if i == 0 || v != out[j-1] {
			out[j] = v
			j++
		}
	}
	return out[:j]
}

func reqLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func sortTraces(ts []Trace) {
	sort.SliceStable(ts, func(i, j int) bool { return reqLess(ts[i].Requirement, ts[j].Requirement) })
}
