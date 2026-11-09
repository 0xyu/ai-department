package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/0xyu/aco/internal/events"
)

// StepStatus is the status of an intent/outcome step (§14.2).
type StepStatus string

// Step statuses.
const (
	// StepStarted: the intent is durable and the step is acting.
	StepStarted StepStatus = "started"
	// StepCompleted and StepFailed: the outcome is durable.
	StepCompleted StepStatus = "completed"
	StepFailed    StepStatus = "failed"
	// StepNeedsReconciliation: the process that wrote the intent stopped
	// (crash, kill) before writing an outcome. Resume must compare the
	// worktree with the step's checkpoint and redo or accept it.
	StepNeedsReconciliation StepStatus = "needs_reconciliation"
	// StepReconciled: resume decided what to do with an interrupted step.
	StepReconciled StepStatus = "reconciled"
)

// Resolution records how resume handled an interrupted step.
type Resolution string

// Resolutions.
const (
	ResolutionRedone    Resolution = "redone"    // reset to the checkpoint; the step runs again as a new step
	ResolutionResumed   Resolution = "resumed"   // the agent session was resumed
	ResolutionAccepted  Resolution = "accepted"  // the effect was already complete (e.g. commit found by trailer)
	ResolutionAbandoned Resolution = "abandoned" // the attempt was ended instead
)

// StepIntent is written before a step acts.
type StepIntent struct {
	RunID   string
	Phase   int // 0 for run-level steps
	Attempt int // 0 for run-level steps
	// Kind names the step, for example "implement", "gates", "review",
	// "commit" (lower case, [a-z0-9_.-]).
	Kind string
	// Key makes the step idempotent within the run: a second BeginStep with
	// the same Key returns the stored step and ErrStepExists.
	Key string
	// Owner is the run lock owner token of the acting process.
	Owner string
	// Intent is a JSON document with the step's inputs (masked before it is
	// stored). Marshal a typed struct; never pass secrets.
	Intent []byte
	// CheckpointRef is the worktree checkpoint taken before acting.
	CheckpointRef string
}

// Step is one stored step.
type Step struct {
	ID            int64
	RunID         string
	Phase         int
	Attempt       int
	Kind          string
	Key           string
	Status        StepStatus
	Owner         string
	Intent        []byte
	CheckpointRef string
	Outcome       []byte
	Error         string
	Resolution    Resolution
	StartedAt     time.Time
	EndedAt       *time.Time
}

// ErrStepExists is returned (with the stored step) by BeginStep when the key
// was used before.
var ErrStepExists = errors.New("step with this key already exists")

// ErrStepState is returned when a step is not in the status an operation
// needs (for example completing a step twice).
var ErrStepState = errors.New("step is not in the required status")

const stepCols = `id, run_id, phase, attempt, kind, idempotency_key, status, owner, intent_json, checkpoint_ref,
	outcome_json, error, resolution, started_at, ended_at`

func scanStep(sc scanner) (Step, error) {
	var (
		st              Step
		status, res     string
		intent, outcome string
		started         int64
		ended           sql.NullInt64
	)
	if err := sc.Scan(&st.ID, &st.RunID, &st.Phase, &st.Attempt, &st.Kind, &st.Key, &status, &st.Owner, &intent,
		&st.CheckpointRef, &outcome, &st.Error, &res, &started, &ended); err != nil {
		return Step{}, err
	}
	st.Status, st.Resolution = StepStatus(status), Resolution(res)
	st.Intent = []byte(intent)
	if outcome != "" {
		st.Outcome = []byte(outcome)
	}
	st.StartedAt, st.EndedAt = msTime(started), msPtr(ended)
	return st, nil
}

func (tx *wtx) getStep(ctx context.Context, id int64) (Step, error) {
	st, err := scanStep(tx.QueryRowContext(ctx, `SELECT `+stepCols+` FROM steps WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, fmt.Errorf("state: step %d: %w", id, ErrNotFound)
	}
	return st, err
}

// GetStep returns a step.
func (s *Store) GetStep(ctx context.Context, id int64) (Step, error) {
	st, err := scanStep(s.db.QueryRowContext(ctx, `SELECT `+stepCols+` FROM steps WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Step{}, fmt.Errorf("state: step %d: %w", id, ErrNotFound)
	}
	return st, err
}

// BeginStep durably records a step's intent and emits step.started. Act only
// after it returns nil.
func (s *Store) BeginStep(ctx context.Context, in StepIntent) (Step, error) {
	if !nameRE.MatchString(in.Kind) {
		return Step{}, errors.New("state: step kind must match [a-z][a-z0-9_.-]*")
	}
	if !keyRE.MatchString(in.Key) {
		return Step{}, errors.New("state: step key must be 1-200 characters of [A-Za-z0-9_.:/-]")
	}
	if in.Owner == "" {
		return Step{}, errors.New("state: step owner is required (the run lock owner)")
	}
	if in.Phase < 0 || in.Attempt < 0 || (in.Phase == 0) != (in.Attempt == 0) {
		return Step{}, errors.New("state: phase and attempt are both 0 (run level) or both positive")
	}
	intent, err := s.maskJSON(in.Intent, "{}")
	if err != nil {
		return Step{}, err
	}
	var out Step
	err = s.write(ctx, func(tx *wtx) error {
		if _, err := tx.getRun(ctx, in.RunID); err != nil {
			return err
		}
		existing, err := scanStep(tx.QueryRowContext(ctx, `SELECT `+stepCols+` FROM steps WHERE run_id = ?
			AND idempotency_key = ?`, in.RunID, in.Key))
		if err == nil {
			out = existing
			return ErrStepExists
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("state: %w", err)
		}
		if in.Phase > 0 {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE run_id = ? AND phase = ? AND number = ?`,
				in.RunID, in.Phase, in.Attempt).Scan(&n); err != nil {
				return fmt.Errorf("state: %w", err)
			}
			if n == 0 {
				return fmt.Errorf("state: attempt %d of phase %d: %w", in.Attempt, in.Phase, ErrNotFound)
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO steps (run_id, phase, attempt, kind, idempotency_key, status, owner,
			intent_json, checkpoint_ref, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, in.RunID, in.Phase, in.Attempt,
			in.Kind, in.Key, string(StepStarted), in.Owner, intent, s.mask(in.CheckpointRef), s.nowMS())
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		if out, err = tx.getStep(ctx, id); err != nil {
			return err
		}
		return tx.emitStep(ctx, out, events.StepStarted)
	})
	return out, err
}

func (tx *wtx) emitStep(ctx context.Context, st Step, t events.Type) error {
	stage := Stage("")
	if st.Phase > 0 {
		stage = StageExecution
	}
	payload := events.Payload{events.Int("step_id", st.ID), events.Str("kind", st.Kind), events.Str("key", st.Key),
		events.Int("attempt", int64(st.Attempt)), events.Str("status", string(st.Status))}
	if st.Resolution != "" {
		payload = append(payload, events.Str("resolution", string(st.Resolution)))
	}
	return tx.emit(ctx, st.RunID, t, stage, st.Phase, payload...)
}

// finish moves a step from one of `from` to `to` and emits step.completed.
func (s *Store) finish(ctx context.Context, id int64, from []StepStatus, to StepStatus, outcome []byte, errText string,
	res Resolution) (Step, error) {
	out, err := s.maskJSON(outcome, "")
	if err != nil {
		return Step{}, err
	}
	var st Step
	err = s.write(ctx, func(tx *wtx) error {
		cur, err := tx.getStep(ctx, id)
		if err != nil {
			return err
		}
		if !contains(from, cur.Status) {
			return fmt.Errorf("state: step %d is %s: %w", id, cur.Status, ErrStepState)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE steps SET status = ?, outcome_json = ?, error = ?, resolution = ?,
			ended_at = ? WHERE id = ?`, string(to), out, s.mask(errText), string(res), s.nowMS(), id); err != nil {
			return fmt.Errorf("state: %w", err)
		}
		if st, err = tx.getStep(ctx, id); err != nil {
			return err
		}
		return tx.emitStep(ctx, st, events.StepCompleted)
	})
	return st, err
}

// CompleteStep durably records a started step's outcome.
func (s *Store) CompleteStep(ctx context.Context, id int64, outcome []byte) (Step, error) {
	return s.finish(ctx, id, []StepStatus{StepStarted}, StepCompleted, outcome, "", "")
}

// FailStep records that a started step failed (errText is masked).
func (s *Store) FailStep(ctx context.Context, id int64, errText string, outcome []byte) (Step, error) {
	return s.finish(ctx, id, []StepStatus{StepStarted}, StepFailed, outcome, errText, "")
}

// ResolveStep records how resume handled an interrupted step.
func (s *Store) ResolveStep(ctx context.Context, id int64, res Resolution, outcome []byte) (Step, error) {
	switch res {
	case ResolutionRedone, ResolutionResumed, ResolutionAccepted, ResolutionAbandoned:
	default:
		return Step{}, fmt.Errorf("state: unknown resolution %q", res)
	}
	return s.finish(ctx, id, []StepStatus{StepNeedsReconciliation}, StepReconciled, outcome, "", res)
}

// MarkInterrupted marks every started step of runID that belongs to another
// owner as needs_reconciliation and returns them. Call it right after taking
// the run lock (the previous owner is gone: its lock was released or stale),
// before acting on the run.
func (s *Store) MarkInterrupted(ctx context.Context, runID, currentOwner string) ([]Step, error) {
	var out []Step
	err := s.write(ctx, func(tx *wtx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+stepCols+` FROM steps WHERE run_id = ? AND status = ? AND owner <> ?
			ORDER BY id`, runID, string(StepStarted), currentOwner)
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		var found []Step
		for rows.Next() {
			st, err := scanStep(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("state: %w", err)
			}
			found = append(found, st)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("state: %w", err)
		}
		for _, st := range found {
			if _, err := tx.ExecContext(ctx, `UPDATE steps SET status = ? WHERE id = ?`,
				string(StepNeedsReconciliation), st.ID); err != nil {
				return fmt.Errorf("state: %w", err)
			}
			st.Status = StepNeedsReconciliation
			out = append(out, st)
		}
		return nil
	})
	return out, err
}

// ListSteps returns a run's steps in order; no statuses means all.
func (s *Store) ListSteps(ctx context.Context, runID string, statuses ...StepStatus) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+stepCols+` FROM steps WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		if len(statuses) == 0 || contains(statuses, st.Status) {
			out = append(out, st)
		}
	}
	return out, rows.Err()
}
