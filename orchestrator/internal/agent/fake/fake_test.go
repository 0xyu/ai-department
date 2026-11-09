package fake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xyu/aco/internal/agent"
)

func TestScriptedImplementer(t *testing.T) {
	dir := t.TempDir()
	m := &Implementer{Steps: []ImplStep{
		{Edit: func(d string) error { return os.WriteFile(filepath.Join(d, "a.go"), []byte("package a\n"), 0o644) },
			Result: ImplResult(Report("first", []string{"AC-01.1"}))},
		{Result: ImplResult(Report("fix", []string{"AC-01.1"}, "F-001"))},
		{Err: &agent.Error{Class: agent.ClassMaxTurns}},
	}}
	ws := agent.Workspace{Dir: dir}
	r1, err := m.Start(context.Background(), agent.ImplementRequest{Phase: 1, Workspace: ws})
	if err != nil || r1.SessionID != "fake-session-1" {
		t.Fatalf("start: %v %+v", err, r1)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.go")); err != nil {
		t.Error("edit not applied")
	}
	r2, err := m.Continue(context.Background(), agent.ContinueRequest{SessionID: r1.SessionID, Workspace: ws})
	if err != nil || r2.SessionID != r1.SessionID || r2.Report.FindingResponses[0].ID != "F-001" {
		t.Fatalf("continue: %v %+v", err, r2)
	}
	if _, err := m.Continue(context.Background(), agent.ContinueRequest{SessionID: "x", Workspace: ws}); agent.ClassOf(err) != agent.ClassMaxTurns {
		t.Errorf("scripted error: %v", err)
	}
	if _, err := m.Start(context.Background(), agent.ImplementRequest{Workspace: ws}); !errors.Is(err, ErrScriptExhausted) {
		t.Errorf("exhausted: %v", err)
	}
	if len(m.Starts) != 2 || len(m.Continues) != 2 || m.Remaining() != 0 {
		t.Errorf("calls not recorded: %d starts, %d continues", len(m.Starts), len(m.Continues))
	}
	// The report helper produces schema-valid reports.
	raw, _ := json.Marshal(Report("x", []string{"AC-02.1"}, "F-9"))
	if _, probs := agent.ParseImplReport(raw); len(probs) > 0 {
		t.Errorf("fake report invalid: %v", probs)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Steps = append(m.Steps, ImplStep{})
	if _, err := m.Start(ctx, agent.ImplementRequest{}); agent.ClassOf(err) != agent.ClassCancelled {
		t.Errorf("cancelled context: %v", err)
	}
}

func TestScriptedPlanner(t *testing.T) {
	p := &Planner{Steps: []PlanStep{
		{Result: PlanResult([]byte(`{"version":1,"title":"T","requirements":[],"phases":[],"traceability":[]}`))},
		{Result: PlanResult([]byte(`{}`), agent.FindingResponse{ID: "PF-1", Response: "fixed", Detail: "d"})},
	}}
	r, err := p.Create(context.Background(), agent.PlanRequest{Requirements: "x"})
	if err != nil || r.SessionID != "fake-plan-session" || r.Package == nil || r.Package.Title != "T" {
		t.Fatalf("create: %v %+v", err, r)
	}
	r2, err := p.Revise(context.Background(), agent.PlanRevision{SessionID: r.SessionID, Round: 2})
	if err != nil || len(r2.Responses) != 1 || len(p.Revisions) != 1 || p.Revisions[0].Round != 2 {
		t.Fatalf("revise: %v", err)
	}
	if _, err := p.Revise(context.Background(), agent.PlanRevision{}); !errors.Is(err, ErrScriptExhausted) {
		t.Error("want exhausted")
	}
}
