package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/0xyu/aco/internal/agent"
)

// Plan finding statuses in the ledger. open and dispute_rejected need work;
// resolved and dispute_accepted are closed.
const (
	FindingOpen            = "open"
	FindingResolved        = "resolved"
	FindingDisputeAccepted = "dispute_accepted"
	FindingDisputeRejected = "dispute_rejected"
)

// PlanLedgerEntry is one plan finding (PF-NNN) and its history.
type PlanLedgerEntry struct {
	Finding      agent.PlanFinding
	Status       string
	OpenedRound  int
	ClosedRound  int
	ReopenCount  int
	DisputeCount int
	Response     *agent.FindingResponse // the planner's latest answer
	Note         string                 // the reviewer's latest note
}

// Open reports whether the finding still needs work.
func (e *PlanLedgerEntry) Open() bool {
	return e.Status == FindingOpen || e.Status == FindingDisputeRejected
}

// Fingerprint is a stable identity of the finding's content.
func (e *PlanLedgerEntry) Fingerprint() string {
	f := e.Finding
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	h := sha256.Sum256([]byte(norm(f.Dimension) + "\x00" + norm(f.Location) + "\x00" + norm(f.Title)))
	return "sha256:" + hex.EncodeToString(h[:8])
}

// LedgerChange is one status change the engine reports as an event.
type LedgerChange struct {
	ID   string
	Kind string // opened | resolved | reopened | dispute_accepted | dispute_rejected
}

// PlanLedger is the plan finding ledger of a run (§10.4). IDs are
// assigned by the engine, never by the reviewer.
type PlanLedger struct {
	Entries []*PlanLedgerEntry
}

// Get returns the entry with id, or nil.
func (l *PlanLedger) Get(id string) *PlanLedgerEntry {
	for _, e := range l.Entries {
		if e.Finding.ID == id {
			return e
		}
	}
	return nil
}

// OpenEntries returns the findings that still need work, in ID order.
func (l *PlanLedger) OpenEntries() []*PlanLedgerEntry {
	var out []*PlanLedgerEntry
	for _, e := range l.Entries {
		if e.Open() {
			out = append(out, e)
		}
	}
	return out
}

// Previous is what a re-review needs a status for: every open finding with
// the planner's answer.
func (l *PlanLedger) Previous() []agent.PlanFindingRecord {
	var out []agent.PlanFindingRecord
	for _, e := range l.OpenEntries() {
		out = append(out, agent.PlanFindingRecord{Finding: e.Finding, Response: e.Response})
	}
	return out
}

// Apply records a review of round: statuses of previous findings, then the
// new findings with fresh IDs. It returns the changes in order.
func (l *PlanLedger) Apply(round int, rev *agent.PlanReview) []LedgerChange {
	var ch []LedgerChange
	for _, ps := range rev.PreviousFindings {
		e := l.Get(ps.ID)
		if e == nil {
			continue // the parser rejects unknown IDs; be safe anyway
		}
		e.Note = ps.Note
		switch ps.Status {
		case "resolved":
			e.Status, e.ClosedRound = FindingResolved, round
			ch = append(ch, LedgerChange{e.Finding.ID, "resolved"})
		case "dispute_accepted":
			e.Status, e.ClosedRound = FindingDisputeAccepted, round
			ch = append(ch, LedgerChange{e.Finding.ID, "dispute_accepted"})
		case "dispute_rejected":
			e.Status = FindingDisputeRejected
			ch = append(ch, LedgerChange{e.Finding.ID, "dispute_rejected"})
		case "unresolved":
			if e.Response != nil && e.Response.Response == agent.ResponseFixed {
				// The planner said fixed, the reviewer disagrees.
				e.ReopenCount++
				ch = append(ch, LedgerChange{e.Finding.ID, "reopened"})
			}
			e.Status = FindingOpen
		}
	}
	for _, nf := range rev.NewFindings {
		e := &PlanLedgerEntry{
			Finding: agent.PlanFinding{
				ID: fmt.Sprintf("PF-%03d", len(l.Entries)+1), Severity: nf.Severity, Dimension: nf.Dimension,
				Location: nf.Location, Title: nf.Title, Description: nf.Description, RequiredChange: nf.RequiredChange,
			},
			Status: FindingOpen, OpenedRound: round,
		}
		l.Entries = append(l.Entries, e)
		ch = append(ch, LedgerChange{e.Finding.ID, "opened"})
	}
	return ch
}

// SetResponses records the planner's answers to the open findings. Answers
// to unknown or closed findings are ignored. It returns the IDs disputed.
func (l *PlanLedger) SetResponses(rs []agent.FindingResponse) []string {
	var disputed []string
	for _, r := range rs {
		e := l.Get(r.ID)
		if e == nil || !e.Open() {
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

// GateFindings maps the ledger for the Plan Decision Gate.
func (l *PlanLedger) GateFindings() []PlanGateFinding {
	out := make([]PlanGateFinding, 0, len(l.Entries))
	for _, e := range l.Entries {
		out = append(out, PlanGateFinding{ID: e.Finding.ID, Severity: e.Finding.Severity, Open: e.Open()})
	}
	return out
}
