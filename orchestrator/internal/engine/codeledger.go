package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/0xyu/aco/internal/agent"
)

// Resolution classes (§11.7 triage).
const (
	ClassFixable      = "fixable"
	ClassDeferred     = "deferred"
	ClassSpecConflict = "spec_conflict"
	ClassUnsafe       = "unsafe"
	ClassEnvironment  = "environment"
)

// Code finding statuses in the ledger. open, partially_resolved, and
// dispute_rejected need work; resolved, dispute_accepted, and deferred are
// closed.
const (
	CodeOpen            = "open"
	CodePartial         = "partially_resolved"
	CodeResolved        = "resolved"
	CodeDisputeAccepted = "dispute_accepted"
	CodeDisputeRejected = "dispute_rejected"
	CodeDeferred        = "deferred"
)

// CodeLedgerEntry is one code finding (F-NN-NNN) and its history.
type CodeLedgerEntry struct {
	ID      string
	Attempt int
	Finding agent.CodeReviewFinding // severity may be downgraded (scope creep)
	Status  string
	// Class is the final resolution class after triage; Proposed is the
	// reviewer's.
	Class, Proposed string
	OpenedRound     int
	ClosedRound     int
	ReopenCount     int
	DisputeCount    int
	// Rejections counts disputes the reviewer rejected (dispute deadlock).
	Rejections int
	// OpenRounds counts re-reviews that found the finding still open.
	OpenRounds int
	// Downgraded is set when a late blocking finding was downgraded to
	// minor (scope-creep control, §11.7).
	Downgraded bool
	// Confirmed is the implementer's cannot_fix_* answer when the reviewer
	// accepted it: the finding cannot be fixed and triage blocks.
	Confirmed string
	Response  *agent.FindingResponse
	Note      string
	// DeferReason says why triage deferred it.
	DeferReason string
}

// Open reports whether the finding still needs work.
func (e *CodeLedgerEntry) Open() bool {
	return e.Status == CodeOpen || e.Status == CodePartial || e.Status == CodeDisputeRejected
}

// Fingerprint identifies the finding's content across rounds and attempts
// (oscillation detection): category, file, and normalized title.
func (e *CodeLedgerEntry) Fingerprint() string { return codeFingerprint(e.Finding) }

func codeFingerprint(f agent.CodeReviewFinding) string {
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	h := sha256.Sum256([]byte(norm(f.Category) + "\x00" + norm(f.File) + "\x00" + norm(f.Title)))
	return "sha256:" + hex.EncodeToString(h[:8])
}

// Location renders file:line for prompts and reports.
func (e *CodeLedgerEntry) Location() string {
	f := e.Finding
	switch {
	case f.File == "":
		return ""
	case f.LineStart > 0 && f.LineEnd > f.LineStart:
		return fmt.Sprintf("%s:%d-%d", f.File, f.LineStart, f.LineEnd)
	case f.LineStart > 0:
		return fmt.Sprintf("%s:%d", f.File, f.LineStart)
	}
	return f.File
}

// CodeLedger is the code finding ledger of one phase across its attempts.
// IDs are assigned by the engine (F-<phase>-<seq>), never by the reviewer,
// and stay unique across attempts.
type CodeLedger struct {
	Phase   int
	Attempt int // the current attempt; earlier entries are history
	Entries []*CodeLedgerEntry
}

// Get returns the entry with id, or nil.
func (l *CodeLedger) Get(id string) *CodeLedgerEntry {
	for _, e := range l.Entries {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// Current returns the entries of the current attempt.
func (l *CodeLedger) Current() []*CodeLedgerEntry {
	var out []*CodeLedgerEntry
	for _, e := range l.Entries {
		if e.Attempt == l.Attempt {
			out = append(out, e)
		}
	}
	return out
}

// OpenEntries returns the current attempt's findings that need work.
func (l *CodeLedger) OpenEntries() []*CodeLedgerEntry {
	var out []*CodeLedgerEntry
	for _, e := range l.Current() {
		if e.Open() {
			out = append(out, e)
		}
	}
	return out
}

// ForReview is what a re-review needs a status for: every finding of the
// current attempt except deferred ones, open and closed (a closed finding
// the reviewer finds unresolved is reopened).
func (l *CodeLedger) ForReview() []agent.LedgerEntry {
	var out []agent.LedgerEntry
	for _, e := range l.Current() {
		if e.Status == CodeDeferred {
			continue
		}
		le := agent.LedgerEntry{ID: e.ID, Severity: e.Finding.Severity, Category: e.Finding.Category, Location: e.Location(),
			Title: e.Finding.Title, RequiredChange: e.Finding.RequiredChange, State: "closed"}
		if e.Open() {
			le.State = "open"
		}
		if e.Response != nil {
			le.Response, le.Detail = e.Response.Response, e.Response.Detail
		}
		out = append(out, le)
	}
	return out
}

// ChangedLines maps a file to the line numbers (new side) changed in the
// latest round, for scope-creep control.
type ChangedLines map[string][]int

// touches reports whether a finding points at a changed line (or at a
// changed file when it names no line).
func (c ChangedLines) touches(f agent.CodeReviewFinding) bool {
	lines, ok := c[f.File]
	if !ok {
		return false
	}
	if f.LineStart <= 0 {
		return true
	}
	end := f.LineEnd
	if end < f.LineStart {
		end = f.LineStart
	}
	for _, n := range lines {
		if n >= f.LineStart && n <= end {
			return true
		}
	}
	return false
}

// ScopeCreep reports whether a new finding raised in round is downgraded
// (§11.7): after limits.new_blocker_cutoff_round, a new blocking finding
// must point to lines changed in the latest round or be a security or
// regression issue; otherwise it becomes minor and deferred. cutoff <= 0
// disables the rule.
func ScopeCreep(f agent.CodeReviewFinding, round, cutoff int, changed ChangedLines) bool {
	if cutoff <= 0 || round <= cutoff {
		return false
	}
	if f.Severity != "blocker" && f.Severity != "major" {
		return false
	}
	if f.Category == "security" || f.Category == "regression" {
		return false
	}
	return !changed.touches(f)
}

// Apply records a review of round: the statuses of earlier findings, then
// the new findings (a new finding that matches a closed one of this
// attempt reopens it). New blocking findings after the cutoff round that
// do not touch changed lines are downgraded (ScopeCreep).
func (l *CodeLedger) Apply(round, cutoff int, rev *agent.CodeReview, changed ChangedLines) []LedgerChange {
	var ch []LedgerChange
	for _, ps := range rev.PreviousFindings {
		e := l.Get(ps.ID)
		if e == nil || e.Attempt != l.Attempt {
			continue // the parser rejects unknown IDs; be safe anyway
		}
		e.Note = ps.Note
		resp := ""
		if e.Response != nil {
			resp = e.Response.Response
		}
		wasOpen := e.Open()
		switch ps.Status {
		case "resolved":
			if wasOpen {
				e.Status, e.ClosedRound = CodeResolved, round
				ch = append(ch, LedgerChange{e.ID, "resolved"})
			}
		case "dispute_accepted":
			if resp == agent.ResponseCannotFixWithinSpec || resp == agent.ResponseCannotFixSafely {
				// The reviewer agrees it cannot be fixed: triage blocks.
				e.Confirmed, e.Status = resp, CodeOpen
				ch = append(ch, LedgerChange{e.ID, "confirmed"})
				continue
			}
			if wasOpen {
				e.Status, e.ClosedRound = CodeDisputeAccepted, round
				ch = append(ch, LedgerChange{e.ID, "dispute_accepted"})
			}
		case "dispute_rejected":
			e.Status, e.ClosedRound = CodeDisputeRejected, 0
			e.Rejections++
			e.OpenRounds++
			ch = append(ch, LedgerChange{e.ID, "dispute_rejected"})
		case "partially_resolved":
			e.Status, e.ClosedRound = CodePartial, 0
			e.OpenRounds++
		case "unresolved":
			if !wasOpen || resp == agent.ResponseFixed {
				// Closed and back, or "fixed" and the reviewer disagrees.
				e.ReopenCount++
				ch = append(ch, LedgerChange{e.ID, "reopened"})
			}
			e.Status, e.ClosedRound = CodeOpen, 0
			e.OpenRounds++
		}
	}
	for _, nf := range rev.NewFindings {
		fp := codeFingerprint(nf)
		var match *CodeLedgerEntry
		for _, e := range l.Current() {
			if e.Fingerprint() == fp {
				match = e
				break
			}
		}
		if match != nil {
			ch = append(ch, l.refresh(match, nf, cutoff, changed)...)
			continue
		}
		e := &CodeLedgerEntry{
			ID: fmt.Sprintf("F-%02d-%03d", l.Phase, len(l.Entries)+1), Attempt: l.Attempt, Finding: nf,
			Status: CodeOpen, Proposed: nf.ResolutionClass, OpenedRound: round,
		}
		l.Entries = append(l.Entries, e)
		ch = append(ch, LedgerChange{e.ID, "opened"})
		if ScopeCreep(nf, round, cutoff, changed) {
			e.Downgraded = true
			e.Finding.Severity = "minor"
		}
	}
	return ch
}

// refresh records a new finding that has the fingerprint of an existing one
// of this attempt. The entry keeps its ID and history (opened round, reopen,
// dispute, and open-round counts, the implementer's response), but takes the
// reviewer's current severity, proposed class, and details, so triage judges
// the finding as it is now: an earlier deferral or downgrade never hides a
// finding that is blocking today. The scope-creep downgrade is decided again
// for the current severity; it only applies to a finding first raised after
// the cutoff round. A closed entry (resolved, accepted dispute, deferred) is
// reopened.
func (l *CodeLedger) refresh(e *CodeLedgerEntry, nf agent.CodeReviewFinding, cutoff int, changed ChangedLines) []LedgerChange {
	var ch []LedgerChange
	f := e.Finding
	f.Severity, f.ResolutionClass = nf.Severity, nf.ResolutionClass
	f.Description, f.Evidence, f.RequiredChange = nf.Description, nf.Evidence, nf.RequiredChange
	f.LineStart, f.LineEnd = nf.LineStart, nf.LineEnd
	e.Finding, e.Proposed = f, nf.ResolutionClass
	e.Downgraded = ScopeCreep(nf, e.OpenedRound, cutoff, changed)
	if e.Downgraded {
		e.Finding.Severity = "minor"
	}
	if !e.Open() {
		e.ReopenCount++
		e.Status, e.ClosedRound, e.DeferReason = CodeOpen, 0, ""
		ch = append(ch, LedgerChange{e.ID, "reopened"})
	}
	return ch
}

// SetResponses records the implementer's answers to open findings and
// returns the IDs it disputed. Answers to unknown or closed findings are
// ignored.
func (l *CodeLedger) SetResponses(rs []agent.FindingResponse) []string {
	var disputed []string
	for _, r := range rs {
		e := l.Get(r.ID)
		if e == nil || e.Attempt != l.Attempt || !e.Open() {
			continue
		}
		r := r
		e.Response = &r
		if r.Response == agent.ResponseDisputed {
			e.DisputeCount++
			disputed = append(disputed, r.ID)
		}
	}
	return disputed
}

// MaxReopen returns the highest reopen count and the most rejected
// dispute count of the current attempt.
func (l *CodeLedger) MaxCounts() (reopen, rejections int) {
	for _, e := range l.Current() {
		reopen = max(reopen, e.ReopenCount)
		rejections = max(rejections, e.Rejections)
	}
	return reopen, rejections
}

// GateFindings maps the current attempt for the Review Decision Gate.
func (l *CodeLedger) GateFindings() []ReviewGateFinding {
	var out []ReviewGateFinding
	for _, e := range l.Current() {
		out = append(out, ReviewGateFinding{ID: e.ID, Severity: e.Finding.Severity, Category: e.Finding.Category,
			Class: e.Class, Open: e.Open(), Deferred: e.Status == CodeDeferred})
	}
	return out
}

// --- triage -------------------------------------------------------------------

// TriageResult is the outcome of triage over the open findings.
type TriageResult struct {
	// Blocked is the class that blocks the phase ("" when none), by
	// priority unsafe > spec_conflict > environment; BlockedIDs are the
	// findings with that class.
	Blocked    string
	BlockedIDs []string
	Fixable    []string // sent to the implementer
	Deferred   []string // newly deferred in this triage
}

// TriageRules are the inputs of the override rules.
type TriageRules struct {
	// ProtectedPaths: a finding whose file or required change touches one
	// of them is unsafe.
	ProtectedPaths []string
	// MatchPath matches a protected-path glob (policy.MatchAny).
	MatchPath func(patterns []string, path string) bool
	// SecurityRounds: a security finding still open after this many
	// re-reviews is unsafe (default 2).
	SecurityRounds int
}

// ClassifyFinding is the triage rule for one open finding (§11.7): the
// reviewer proposes a class, the override rules decide.
func ClassifyFinding(e *CodeLedgerEntry, rules TriageRules) (class, reason string) {
	f := e.Finding
	secRounds := rules.SecurityRounds
	if secRounds <= 0 {
		secRounds = 2
	}
	switch {
	case e.Confirmed == agent.ResponseCannotFixSafely:
		return ClassUnsafe, "the implementer reported cannot_fix_safely and the reviewer confirmed"
	case e.Confirmed == agent.ResponseCannotFixWithinSpec:
		return ClassSpecConflict, "the implementer reported cannot_fix_within_spec and the reviewer confirmed"
	case e.Downgraded:
		return ClassDeferred, "new blocking finding after the cutoff round outside the latest changes (scope-creep control)"
	case f.Category == "out_of_scope":
		return ClassDeferred, "out of the phase scope"
	case f.Severity == "minor" || f.Severity == "nit":
		return ClassDeferred, "severity " + f.Severity
	case touchesProtected(f, rules):
		return ClassUnsafe, "the required change touches a protected path"
	case f.Category == "security" && e.OpenRounds >= secRounds:
		return ClassUnsafe, fmt.Sprintf("security finding unresolved after %d rounds", e.OpenRounds)
	}
	switch e.Proposed {
	case ClassSpecConflict, ClassUnsafe, ClassEnvironment:
		return e.Proposed, "proposed by the reviewer"
	case ClassDeferred:
		// Only out-of-scope, minor/nit, and downgraded findings may be
		// deferred (handled above): an in-scope blocker or major, security
		// included, stays work for this phase whatever the reviewer says.
		return ClassFixable, "the reviewer proposed deferred, overridden: in-scope " + f.Severity
	}
	return ClassFixable, ""
}

// pathTokenChars ends a path token in free text.
const pathTokenChars = " \t\r\n`'\"()[]{}<>,;"

func touchesProtected(f agent.CodeReviewFinding, rules TriageRules) bool {
	if rules.MatchPath == nil || len(rules.ProtectedPaths) == 0 {
		return false
	}
	if f.File != "" && rules.MatchPath(rules.ProtectedPaths, f.File) {
		return true
	}
	for _, tok := range strings.FieldsFunc(f.RequiredChange, func(r rune) bool { return strings.ContainsRune(pathTokenChars, r) }) {
		tok = strings.TrimRight(strings.TrimPrefix(tok, "./"), ".:")
		if tok == "" || !(strings.Contains(tok, "/") || strings.Contains(tok, ".")) {
			continue
		}
		if rules.MatchPath(rules.ProtectedPaths, tok) {
			return true
		}
	}
	return false
}

var blockedPriority = map[string]int{ClassUnsafe: 3, ClassSpecConflict: 2, ClassEnvironment: 1}

// Triage classifies every open finding of the current attempt, defers the
// deferred ones (status deferred), and reports what blocks and what goes
// to the implementer.
func (l *CodeLedger) Triage(round int, rules TriageRules) TriageResult {
	var res TriageResult
	for _, e := range l.OpenEntries() {
		class, reason := ClassifyFinding(e, rules)
		e.Class = class
		switch class {
		case ClassDeferred:
			e.Status, e.ClosedRound, e.DeferReason = CodeDeferred, round, reason
			res.Deferred = append(res.Deferred, e.ID)
		case ClassFixable:
			res.Fixable = append(res.Fixable, e.ID)
		default:
			switch p := blockedPriority[class]; {
			case p > blockedPriority[res.Blocked]:
				res.Blocked, res.BlockedIDs = class, []string{e.ID}
			case p == blockedPriority[res.Blocked]:
				res.BlockedIDs = append(res.BlockedIDs, e.ID)
			}
		}
	}
	return res
}

var severityRank = map[string]int{"blocker": 0, "major": 1, "minor": 2, "nit": 3}

func categoryRank(c string) int {
	switch c {
	case "security":
		return 0
	case "regression":
		return 1
	}
	return 2
}

// FixOrder sorts findings for the implementer: blockers, then majors; within
// a severity security, then regression, then the rest; then by ID.
func FixOrder(es []*CodeLedgerEntry) []*CodeLedgerEntry {
	out := append([]*CodeLedgerEntry(nil), es...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if sa, sb := severityRank[a.Finding.Severity], severityRank[b.Finding.Severity]; sa != sb {
			return sa < sb
		}
		if ca, cb := categoryRank(a.Finding.Category), categoryRank(b.Finding.Category); ca != cb {
			return ca < cb
		}
		return a.ID < b.ID
	})
	return out
}
