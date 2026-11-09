//go:build integration

package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/state"
)

// A whole run through the store, the bus, and the artifact store: a
// subscriber that joins at the start sees exactly the stored event log, and
// the run's artifacts sit in its run directory.
func TestRunLifecycleEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newStore(t, "", state.Options{})
	bus := events.NewBus(s)
	arts := artifacts.New(t.TempDir(), nil)
	repo := t.TempDir()

	run, err := s.CreateRun(ctx, state.NewRun{RepoPath: repo, Mode: state.ModeFull, Input: state.InputRequirements})
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := bus.Subscribe(ctx, run.ID, 0)
	lock, _, err := s.AcquireLock(ctx, repo, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := arts.Run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	to := func(st state.RunStatus) { must(s.TransitionRun(ctx, run.ID, st, state.TransitionOptions{})) }
	ph := func(n int, st state.PhaseStatus) { must(s.TransitionPhase(ctx, run.ID, n, st, state.Reason{})) }

	to(state.RunPreflight)
	to(state.RunPlanning)
	must(dir.Write(artifacts.Planning(1, "plan.json"), []byte(`{"phases":2}`)))
	must(bus.Publish(ctx, events.Event{RunID: run.ID, Type: events.PlanCreated, Stage: "planning"}))
	to(state.RunPlanReview)
	must(nil, s.CreatePhases(ctx, run.ID, []state.PhaseDef{{Number: 1, Title: "A"}, {Number: 2, Title: "B"}}))
	must(bus.Publish(ctx, events.Event{RunID: run.ID, Type: events.PlanFrozen, Stage: "plan_review"}))
	to(state.RunExecuting)
	for n := 1; n <= 2; n++ {
		ph(n, state.PhaseInitializing)
		ph(n, state.PhaseImplementing)
		st, err := s.BeginStep(ctx, state.StepIntent{RunID: run.ID, Phase: n, Attempt: 1, Kind: "implement",
			Key: artifacts.Attempt(n, 1) + "/implement", Owner: lock.Owner()})
		if err != nil {
			t.Fatal(err)
		}
		must(dir.Write(artifacts.StepFile(n, 1, st.ID, "implement", "prompt.md"), []byte("implement")))
		must(s.CompleteStep(ctx, st.ID, nil))
		for _, p := range []state.PhaseStatus{state.PhaseQualityGates, state.PhaseReviewing, state.PhaseCommitGate,
			state.PhaseCommitted} {
			ph(n, p)
		}
	}
	to(state.RunFinalVerification)
	to(state.RunCompleted)
	must(nil, lock.Release(ctx))

	stored, err := s.EventsAfter(ctx, run.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := collect(t, sub, len(stored))
	for i := range stored {
		a, _ := json.Marshal(stored[i])
		b, _ := json.Marshal(got[i])
		if string(a) != string(b) {
			t.Fatalf("event %d differs:\n stored %s\n   live %s", i, a, b)
		}
	}
	if last := got[len(got)-1]; last.Type != events.RunCompleted {
		t.Errorf("last event %s", last.Type)
	}
	if _, err := dir.Read(artifacts.Planning(1, "plan.json")); err != nil {
		t.Error(err)
	}
}
