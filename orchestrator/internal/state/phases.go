package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/0xyu/aco/internal/events"
)

// PhaseDef is a phase of the frozen plan, as registered for a run.
type PhaseDef struct {
	Number int
	Title  string
}

// Phase is one stored phase of a run.
type Phase struct {
	RunID      string
	Number     int
	Title      string
	Status     PhaseStatus
	Attempts   int // number of attempts started; the current one is Attempts
	BaseCommit string
	CommitSHA  string
	Reason     Reason // set while blocked (Stage is execution, Phase is Number)
	StartedAt  *time.Time
	EndedAt    *time.Time
	UpdatedAt  time.Time
}

// Attempt is one attempt of a phase.
type Attempt struct {
	Phase     int
	Number    int
	Status    string // active | exhausted | committed | blocked
	SessionID string
	StartedAt time.Time
	EndedAt   *time.Time
}

const phaseCols = `run_id, number, title, status, attempts, base_commit, commit_sha, reason_class, reason_detail,
	started_at, ended_at, updated_at`

func scanPhase(sc scanner) (Phase, error) {
	var (
		p              Phase
		status, class  string
		started, ended sql.NullInt64
		updated        int64
	)
	if err := sc.Scan(&p.RunID, &p.Number, &p.Title, &status, &p.Attempts, &p.BaseCommit, &p.CommitSHA, &class,
		&p.Reason.Detail, &started, &ended, &updated); err != nil {
		return Phase{}, err
	}
	p.Status, p.StartedAt, p.EndedAt, p.UpdatedAt = PhaseStatus(status), msPtr(started), msPtr(ended), msTime(updated)
	if class != "" {
		p.Reason.Class, p.Reason.Stage, p.Reason.Phase = Class(class), StageExecution, p.Number
	}
	return p, nil
}

// CreatePhases registers the frozen plan's phases (numbered 1..n in order)
// for a run, all pending. It can be called once per run.
func (s *Store) CreatePhases(ctx context.Context, runID string, defs []PhaseDef) error {
	if len(defs) == 0 {
		return errors.New("state: a run needs at least one phase")
	}
	for i, d := range defs {
		if d.Number != i+1 {
			return fmt.Errorf("state: phases must be numbered 1..n in order (got %d at position %d)", d.Number, i+1)
		}
	}
	return s.write(ctx, func(tx *wtx) error {
		if _, err := tx.getRun(ctx, runID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM phases WHERE run_id = ?`, runID).Scan(&n); err != nil {
			return fmt.Errorf("state: %w", err)
		}
		if n > 0 {
			return errors.New("state: phases are already registered for this run")
		}
		now := s.nowMS()
		for _, d := range defs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO phases (run_id, number, title, status, updated_at)
				VALUES (?, ?, ?, ?, ?)`, runID, d.Number, s.mask(d.Title), string(PhasePending), now); err != nil {
				return fmt.Errorf("state: %w", err)
			}
		}
		return nil
	})
}

// GetPhase returns one phase.
func (s *Store) GetPhase(ctx context.Context, runID string, number int) (Phase, error) {
	p, err := scanPhase(s.db.QueryRowContext(ctx, `SELECT `+phaseCols+` FROM phases WHERE run_id = ? AND number = ?`,
		runID, number))
	if errors.Is(err, sql.ErrNoRows) {
		return Phase{}, fmt.Errorf("state: phase %d of run %s: %w", number, runID, ErrNotFound)
	}
	return p, err
}

func (tx *wtx) getPhase(ctx context.Context, runID string, number int) (Phase, error) {
	p, err := scanPhase(tx.QueryRowContext(ctx, `SELECT `+phaseCols+` FROM phases WHERE run_id = ? AND number = ?`,
		runID, number))
	if errors.Is(err, sql.ErrNoRows) {
		return Phase{}, fmt.Errorf("state: phase %d of run %s: %w", number, runID, ErrNotFound)
	}
	return p, err
}

// ListPhases returns a run's phases in order.
func (s *Store) ListPhases(ctx context.Context, runID string) ([]Phase, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+phaseCols+` FROM phases WHERE run_id = ? ORDER BY number`, runID)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []Phase
	for rows.Next() {
		p, err := scanPhase(rows)
		if err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TransitionPhase moves a phase through the phase state machine. The run
// must be executing. Entering initializing starts a new attempt; reason is
// required exactly for blocked (its Stage and Phase are filled in).
func (s *Store) TransitionPhase(ctx context.Context, runID string, number int, to PhaseStatus, reason Reason) (Phase, error) {
	if to == PhaseBlocked {
		reason.Stage, reason.Phase = StageExecution, number
	}
	var out Phase
	err := s.write(ctx, func(tx *wtx) error {
		cur, err := tx.phaseForChange(ctx, runID, number)
		if err != nil {
			return err
		}
		if err := CheckPhaseTransition(cur.Status, to, reason); err != nil {
			return err
		}
		out, err = tx.applyPhase(ctx, cur, to, reason)
		return err
	})
	return out, err
}

// ResumePhase restarts a blocked phase in a new attempt (aco resume
// --retry). The run must already be executing again (ResumeRun).
func (s *Store) ResumePhase(ctx context.Context, runID string, number int) (Phase, error) {
	var out Phase
	err := s.write(ctx, func(tx *wtx) error {
		cur, err := tx.phaseForChange(ctx, runID, number)
		if err != nil {
			return err
		}
		if err := CheckPhaseResume(cur.Status); err != nil {
			return err
		}
		out, err = tx.applyPhase(ctx, cur, PhaseInitializing, Reason{})
		return err
	})
	return out, err
}

func (tx *wtx) phaseForChange(ctx context.Context, runID string, number int) (Phase, error) {
	run, err := tx.getRun(ctx, runID)
	if err != nil {
		return Phase{}, err
	}
	if run.Status != RunExecuting {
		return Phase{}, &TransitionError{Machine: "phase", From: "run " + string(run.Status), To: "phase change",
			Why: "phases change only while the run is executing"}
	}
	return tx.getPhase(ctx, runID, number)
}

func (tx *wtx) applyPhase(ctx context.Context, cur Phase, to PhaseStatus, reason Reason) (Phase, error) {
	s, now := tx.s, tx.s.nowMS()
	attempts := cur.Attempts
	endAttempt := ""
	switch to {
	case PhaseInitializing:
		endAttempt = "exhausted" // a still-active previous attempt (restart)
	case PhaseAttemptExhausted:
		endAttempt = "exhausted"
	case PhaseCommitted:
		endAttempt = "committed"
	case PhaseBlocked:
		endAttempt = "blocked"
	}
	if endAttempt != "" && attempts > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE attempts SET status = ?, ended_at = ? WHERE run_id = ? AND phase = ?
			AND number = ? AND status = 'active'`, endAttempt, now, cur.RunID, cur.Number, attempts); err != nil {
			return Phase{}, fmt.Errorf("state: %w", err)
		}
	}
	if to == PhaseInitializing {
		attempts++
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts (run_id, phase, number, status, started_at)
			VALUES (?, ?, ?, 'active', ?)`, cur.RunID, cur.Number, attempts, now); err != nil {
			return Phase{}, fmt.Errorf("state: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET current_phase = ?, updated_at = ? WHERE id = ?`,
			cur.Number, now, cur.RunID); err != nil {
			return Phase{}, fmt.Errorf("state: %w", err)
		}
	}
	var started, ended any = nil, nil
	if cur.StartedAt != nil {
		started = cur.StartedAt.UnixMilli()
	} else if to == PhaseInitializing {
		started = now
	}
	if to.Terminal() {
		ended = now
	}
	if _, err := tx.ExecContext(ctx, `UPDATE phases SET status = ?, attempts = ?, reason_class = ?, reason_detail = ?,
		started_at = ?, ended_at = ?, updated_at = ? WHERE run_id = ? AND number = ?`, string(to), attempts,
		string(reason.Class), s.mask(reason.Detail), started, ended, now, cur.RunID, cur.Number); err != nil {
		return Phase{}, fmt.Errorf("state: %w", err)
	}
	for _, t := range PhaseTransitionEvents(cur.Status, to) {
		payload := events.Payload{events.Str("from", string(cur.Status)), events.Str("to", string(to)),
			events.Int("attempt", int64(attempts))}
		if t == events.PhaseBlocked || t == events.PhaseInitFailed {
			payload = append(payload, events.Str("class", string(reason.Class)), events.Str("detail", reason.Detail))
		}
		if err := tx.emit(ctx, cur.RunID, t, StageExecution, cur.Number, payload...); err != nil {
			return Phase{}, err
		}
	}
	return tx.getPhase(ctx, cur.RunID, cur.Number)
}

// SetPhaseCommits records the phase's base commit and, once committed, its
// commit SHA ("" leaves a value unchanged).
func (s *Store) SetPhaseCommits(ctx context.Context, runID string, number int, base, commit string) error {
	return s.write(ctx, func(tx *wtx) error {
		res, err := tx.ExecContext(ctx, `UPDATE phases SET base_commit = CASE WHEN ? = '' THEN base_commit ELSE ? END,
			commit_sha = CASE WHEN ? = '' THEN commit_sha ELSE ? END, updated_at = ? WHERE run_id = ? AND number = ?`,
			base, base, commit, commit, s.nowMS(), runID, number)
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		return requireRow(res, "phase")
	})
}

// SetAttemptSession records the Claude session ID of an attempt.
func (s *Store) SetAttemptSession(ctx context.Context, runID string, phase, attempt int, sessionID string) error {
	return s.write(ctx, func(tx *wtx) error {
		res, err := tx.ExecContext(ctx, `UPDATE attempts SET session_id = ? WHERE run_id = ? AND phase = ? AND number = ?`,
			s.mask(sessionID), runID, phase, attempt)
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		return requireRow(res, "attempt")
	})
}

// ListAttempts returns the attempts of a phase in order.
func (s *Store) ListAttempts(ctx context.Context, runID string, phase int) ([]Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT phase, number, status, session_id, started_at, ended_at FROM attempts
		WHERE run_id = ? AND phase = ? ORDER BY number`, runID, phase)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var (
			a     Attempt
			start int64
			end   sql.NullInt64
		)
		if err := rows.Scan(&a.Phase, &a.Number, &a.Status, &a.SessionID, &start, &end); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		a.StartedAt, a.EndedAt = msTime(start), msPtr(end)
		out = append(out, a)
	}
	return out, rows.Err()
}

func requireRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("state: %s: %w", what, ErrNotFound)
	}
	return nil
}
