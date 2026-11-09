// Package fake provides scripted agent.Planner and agent.Implementer
// implementations for offline tests of the engine (§3 NFR-7). Each call
// takes the next step of the script; the requests are recorded so tests can
// assert what the engine sent.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/plan"
)

// ErrScriptExhausted is returned when a fake is called more often than scripted.
var ErrScriptExhausted = errors.New("fake: no scripted step left")

// PlanStep is one scripted planner answer.
type PlanStep struct {
	Result *agent.PlanResult
	Err    error
}

// Planner is a scripted agent.Planner.
type Planner struct {
	mu        sync.Mutex
	Steps     []PlanStep
	next      int
	Creates   []agent.PlanRequest
	Revisions []agent.PlanRevision
}

var _ agent.Planner = (*Planner)(nil)

func (p *Planner) step(ctx context.Context) (*agent.PlanResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, &agent.Error{Agent: "fake", Class: agent.ClassCancelled, Err: err}
	}
	if p.next >= len(p.Steps) {
		return nil, ErrScriptExhausted
	}
	s := p.Steps[p.next]
	p.next++
	if s.Result != nil && s.Result.SessionID == "" {
		s.Result.SessionID = "fake-plan-session"
	}
	return s.Result, s.Err
}

// Create implements agent.Planner.
func (p *Planner) Create(ctx context.Context, req agent.PlanRequest) (*agent.PlanResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Creates = append(p.Creates, req)
	return p.step(ctx)
}

// Revise implements agent.Planner.
func (p *Planner) Revise(ctx context.Context, req agent.PlanRevision) (*agent.PlanResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Revisions = append(p.Revisions, req)
	return p.step(ctx)
}

// ImplStep is one scripted implementer answer. Edit runs first, in the
// request's workspace directory, to simulate the agent's file changes.
type ImplStep struct {
	Edit   func(dir string) error
	Result *agent.ImplResult
	Err    error
}

// Implementer is a scripted agent.Implementer.
type Implementer struct {
	mu        sync.Mutex
	Steps     []ImplStep
	next      int
	Starts    []agent.ImplementRequest
	Continues []agent.ContinueRequest
	sessions  int
}

var _ agent.Implementer = (*Implementer)(nil)

func (m *Implementer) step(ctx context.Context, dir, session string) (*agent.ImplResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, &agent.Error{Agent: "fake", Class: agent.ClassCancelled, Err: err}
	}
	if m.next >= len(m.Steps) {
		return nil, ErrScriptExhausted
	}
	s := m.Steps[m.next]
	m.next++
	if s.Edit != nil {
		if err := s.Edit(dir); err != nil {
			return nil, fmt.Errorf("fake: edit: %w", err)
		}
	}
	if s.Result != nil && s.Result.SessionID == "" {
		s.Result.SessionID = session
	}
	return s.Result, s.Err
}

// Start implements agent.Implementer. Each Start gets a new session ID.
func (m *Implementer) Start(ctx context.Context, req agent.ImplementRequest) (*agent.ImplResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Starts = append(m.Starts, req)
	m.sessions++
	return m.step(ctx, req.Workspace.Dir, fmt.Sprintf("fake-session-%d", m.sessions))
}

// Continue implements agent.Implementer; the session ID is kept.
func (m *Implementer) Continue(ctx context.Context, req agent.ContinueRequest) (*agent.ImplResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Continues = append(m.Continues, req)
	return m.step(ctx, req.Workspace.Dir, req.SessionID)
}

// Remaining is the number of unused steps.
func (m *Implementer) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Steps) - m.next
}

// Report builds a schema-valid implementation report: every AC met and
// every finding fixed.
func Report(summary string, acIDs []string, findingIDs ...string) *agent.ImplReport {
	r := &agent.ImplReport{Version: 1, Summary: summary,
		FilesChanged: []agent.FileChange{}, DesignDecisions: []string{}, Tests: []agent.TestChange{},
		AcceptanceCriteria: []agent.ACAssessment{}, FindingResponses: []agent.FindingResponse{}, KnownGaps: []string{}}
	for _, id := range acIDs {
		r.AcceptanceCriteria = append(r.AcceptanceCriteria, agent.ACAssessment{ID: id, Status: "met", Evidence: "fake"})
	}
	for _, id := range findingIDs {
		r.FindingResponses = append(r.FindingResponses, agent.FindingResponse{ID: id, Response: agent.ResponseFixed, Detail: "fake fix"})
	}
	return r
}

// ImplResult wraps a report as a result.
func ImplResult(r *agent.ImplReport) *agent.ImplResult {
	raw, _ := json.Marshal(r)
	return &agent.ImplResult{Raw: raw, Report: r, Invocation: agent.Invocation{AuthMode: "subscription"}}
}

// PlanResult parses a structured plan package (JSON) as a planner result.
func PlanResult(planJSON []byte, responses ...agent.FindingResponse) *agent.PlanResult {
	return &agent.PlanResult{Raw: planJSON, Package: plan.LoadBytes("plan.json", planJSON), Responses: responses,
		Invocation: agent.Invocation{AuthMode: "subscription"}}
}
