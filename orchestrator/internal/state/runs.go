package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/events"
)

// RunMode selects the stages a run executes.
type RunMode string

// Run modes.
const (
	ModeFull     RunMode = "full"      // Stages 1-4
	ModePlanOnly RunMode = "plan_only" // Stages 1-2, stop after freeze
)

// InputKind is what the run was started from.
type InputKind string

// Input kinds.
const (
	InputRequirements InputKind = "requirements" // Stage 1 writes the plan
	InputPlan         InputKind = "plan"         // existing plan, Stage 2 first
)

// Run is one stored run.
type Run struct {
	ID           string
	RepoKey      string
	RepoPath     string
	BaseBranch   string
	RunBranch    string
	Mode         RunMode
	Input        InputKind
	Status       RunStatus
	PausedFrom   RunStatus
	ResumeAt     *time.Time
	Reason       Reason
	PlanHash     string
	CurrentPhase int
	LastEventSeq int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	EndedAt      *time.Time
}

// Snapshot returns the fields the run state machine checks.
func (r Run) Snapshot() RunSnapshot {
	return RunSnapshot{Status: r.Status, PausedFrom: r.PausedFrom, Reason: r.Reason}
}

// NewRun describes a run to create.
type NewRun struct {
	ID         string // optional; NewRunID is used when empty
	RepoPath   string
	BaseBranch string
	Mode       RunMode
	Input      InputKind
}

// NewRunID returns a fresh run ID: <yyyymmdd>-<8 hex>.
func NewRunID(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return now.UTC().Format("20060102") + "-" + hex.EncodeToString(b[:])
}

// RepoKey is the canonical identity of a repository path: absolute, with
// symbolic links resolved, cleaned, and case-folded on Windows and macOS
// (case-insensitive file systems by default). Two spellings of the same
// repository get the same key, so they share one run lock.
func RepoKey(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	abs = filepath.ToSlash(filepath.Clean(abs))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		abs = strings.ToLower(abs)
	}
	return abs, nil
}

const runCols = `id, repo_key, repo_path, base_branch, run_branch, mode, input_kind, status, paused_from, resume_at,
	reason_class, reason_stage, reason_phase, reason_detail, plan_hash, current_phase, last_event_seq,
	created_at, updated_at, ended_at`

type scanner interface{ Scan(dest ...any) error }

func scanRun(sc scanner) (Run, error) {
	var (
		r                    Run
		mode, input, status  string
		paused, class, stage string
		resumeAt, endedAt    sql.NullInt64
		created, updated     int64
	)
	err := sc.Scan(&r.ID, &r.RepoKey, &r.RepoPath, &r.BaseBranch, &r.RunBranch, &mode, &input, &status, &paused,
		&resumeAt, &class, &stage, &r.Reason.Phase, &r.Reason.Detail, &r.PlanHash, &r.CurrentPhase, &r.LastEventSeq,
		&created, &updated, &endedAt)
	if err != nil {
		return Run{}, err
	}
	r.Mode, r.Input, r.Status, r.PausedFrom = RunMode(mode), InputKind(input), RunStatus(status), RunStatus(paused)
	r.Reason.Class, r.Reason.Stage = Class(class), Stage(stage)
	r.ResumeAt, r.EndedAt = msPtr(resumeAt), msPtr(endedAt)
	r.CreatedAt, r.UpdatedAt = msTime(created), msTime(updated)
	return r, nil
}

// CreateRun stores a new run in status created and emits run.created.
func (s *Store) CreateRun(ctx context.Context, n NewRun) (Run, error) {
	if n.ID == "" {
		n.ID = NewRunID(s.now())
	}
	if err := checkRunID(n.ID); err != nil {
		return Run{}, err
	}
	if n.Mode != ModeFull && n.Mode != ModePlanOnly {
		return Run{}, fmt.Errorf("state: unknown run mode %q", n.Mode)
	}
	if n.Input != InputRequirements && n.Input != InputPlan {
		return Run{}, fmt.Errorf("state: unknown input kind %q", n.Input)
	}
	key, err := RepoKey(n.RepoPath)
	if err != nil {
		return Run{}, fmt.Errorf("state: repository path: %w", err)
	}
	var out Run
	err = s.write(ctx, func(tx *wtx) error {
		now := s.nowMS()
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs (id, repo_key, repo_path, base_branch, mode, input_kind, status,
			created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, n.ID, key, s.mask(n.RepoPath), s.mask(n.BaseBranch),
			string(n.Mode), string(n.Input), string(RunCreated), now, now); err != nil {
			return fmt.Errorf("state: create run: %w", err)
		}
		if err := tx.emit(ctx, n.ID, events.RunCreated, "", 0,
			events.Str("mode", string(n.Mode)), events.Str("input", string(n.Input))); err != nil {
			return err
		}
		out, err = tx.getRun(ctx, n.ID)
		return err
	})
	return out, err
}

// GetRun returns a run.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("state: run %s: %w", id, ErrNotFound)
	}
	return r, err
}

func (tx *wtx) getRun(ctx context.Context, id string) (Run, error) {
	r, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("state: run %s: %w", id, ErrNotFound)
	}
	return r, err
}

// ListRuns returns runs, newest first; repoPath "" lists every repository.
func (s *Store) ListRuns(ctx context.Context, repoPath string) ([]Run, error) {
	q, args := `SELECT `+runCols+` FROM runs ORDER BY created_at DESC, id DESC`, []any{}
	if repoPath != "" {
		key, err := RepoKey(repoPath)
		if err != nil {
			return nil, err
		}
		q, args = `SELECT `+runCols+` FROM runs WHERE repo_key = ? ORDER BY created_at DESC, id DESC`, []any{key}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TransitionOptions carries data for a run transition.
type TransitionOptions struct {
	Reason   Reason    // required for blocked and failed
	ResumeAt time.Time // optional, for paused (subscription limit reset)
}

// TransitionRun moves a run to `to` through the run state machine, records
// stage entries, and emits the transition's events, all in one transaction.
func (s *Store) TransitionRun(ctx context.Context, id string, to RunStatus, opt TransitionOptions) (Run, error) {
	var out Run
	err := s.write(ctx, func(tx *wtx) error {
		cur, err := tx.getRun(ctx, id)
		if err != nil {
			return err
		}
		if err := CheckRunTransition(cur.Snapshot(), to, opt.Reason); err != nil {
			return err
		}
		if !opt.ResumeAt.IsZero() && to != RunPaused {
			return errors.New("state: resume_at is only set when pausing")
		}
		out, err = tx.applyRun(ctx, cur, to, opt, false)
		return err
	})
	return out, err
}

// ResumeRun restarts a blocked or failed run (aco resume). to "" resumes at
// the stage named in the stored reason.
func (s *Store) ResumeRun(ctx context.Context, id string, to RunStatus) (Run, error) {
	var out Run
	err := s.write(ctx, func(tx *wtx) error {
		cur, err := tx.getRun(ctx, id)
		if err != nil {
			return err
		}
		if to == "" {
			if to, err = RunResumeTarget(cur.Snapshot()); err != nil {
				return err
			}
		}
		if err := CheckRunResume(cur.Snapshot(), to); err != nil {
			return err
		}
		out, err = tx.applyRun(ctx, cur, to, TransitionOptions{}, true)
		return err
	})
	return out, err
}

func (tx *wtx) applyRun(ctx context.Context, cur Run, to RunStatus, opt TransitionOptions, resumed bool) (Run, error) {
	s, now := tx.s, tx.s.nowMS()
	pausedFrom := ""
	var resumeAt any
	if to == RunPaused {
		pausedFrom = string(cur.Status)
		if !opt.ResumeAt.IsZero() {
			resumeAt = opt.ResumeAt.UnixMilli()
		}
	}
	var ended any
	if to.Terminal() {
		ended = now
	}
	r := opt.Reason
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, paused_from = ?, resume_at = ?, reason_class = ?,
		reason_stage = ?, reason_phase = ?, reason_detail = ?, updated_at = ?, ended_at = ? WHERE id = ?`,
		string(to), pausedFrom, resumeAt, string(r.Class), string(r.Stage), r.Phase, s.mask(r.Detail), now, ended,
		cur.ID); err != nil {
		return Run{}, fmt.Errorf("state: %w", err)
	}
	if err := tx.recordStages(ctx, cur, to, resumed, now); err != nil {
		return Run{}, err
	}
	for _, em := range RunTransitionEvents(cur.Status, to, resumed) {
		payload := events.Payload{events.Str("from", string(cur.Status)), events.Str("to", string(to))}
		if em.Type == events.RunBlocked || em.Type == events.RunFailed {
			payload = append(payload, events.Str("class", string(r.Class)), events.Str("reason_stage", string(r.Stage)),
				events.Int("reason_phase", int64(r.Phase)), events.Str("detail", r.Detail))
		}
		if em.Type == events.RunPaused && !opt.ResumeAt.IsZero() {
			payload = append(payload, events.Str("resume_at", opt.ResumeAt.UTC().Format(time.RFC3339)))
		}
		if err := tx.emit(ctx, cur.ID, em.Type, em.Stage, r.Phase, payload...); err != nil {
			return Run{}, err
		}
	}
	return tx.getRun(ctx, cur.ID)
}

// recordStages keeps one stages row per entry into a stage status.
func (tx *wtx) recordStages(ctx context.Context, cur Run, to RunStatus, resumed bool, now int64) error {
	from := cur.Status
	if from == RunPaused {
		if to != RunFailed && to != RunCancelled {
			return nil // resuming continues the open stage entry
		}
		from = cur.PausedFrom
	}
	if to == RunPaused {
		return nil
	}
	if fs := StageOf(from); fs != "" && !resumed {
		status := "completed"
		switch to {
		case RunBlocked, RunFailed, RunCancelled:
			status = string(to)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE stages SET status = ?, ended_at = ? WHERE run_id = ? AND stage = ?
			AND status = 'active'`, status, now, cur.ID, string(fs)); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	if ts := StageOf(to); ts != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO stages (run_id, stage, entry, status, started_at)
			SELECT ?, ?, COALESCE(MAX(entry), 0) + 1, 'active', ? FROM stages WHERE run_id = ? AND stage = ?`,
			cur.ID, string(ts), now, cur.ID, string(ts)); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	return nil
}

// StageEntry is one entry into a stage.
type StageEntry struct {
	Stage     Stage
	Entry     int
	Status    string // active | completed | blocked | failed | cancelled
	StartedAt time.Time
	EndedAt   *time.Time
}

// ListStages returns a run's stage entries in order.
func (s *Store) ListStages(ctx context.Context, runID string) ([]StageEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stage, entry, status, started_at, ended_at FROM stages
		WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []StageEntry
	for rows.Next() {
		var (
			e     StageEntry
			stage string
			start int64
			end   sql.NullInt64
		)
		if err := rows.Scan(&stage, &e.Entry, &e.Status, &start, &end); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		e.Stage, e.StartedAt, e.EndedAt = Stage(stage), msTime(start), msPtr(end)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RunInfo updates descriptive run fields; nil leaves a field unchanged.
type RunInfo struct {
	RunBranch *string
	PlanHash  *string
}

// UpdateRunInfo sets the run branch or plan hash.
func (s *Store) UpdateRunInfo(ctx context.Context, id string, info RunInfo) (Run, error) {
	var out Run
	err := s.write(ctx, func(tx *wtx) error {
		cur, err := tx.getRun(ctx, id)
		if err != nil {
			return err
		}
		if info.RunBranch != nil {
			cur.RunBranch = s.mask(*info.RunBranch)
		}
		if info.PlanHash != nil {
			cur.PlanHash = s.mask(*info.PlanHash)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET run_branch = ?, plan_hash = ?, updated_at = ? WHERE id = ?`,
			cur.RunBranch, cur.PlanHash, s.nowMS(), id); err != nil {
			return fmt.Errorf("state: %w", err)
		}
		out, err = tx.getRun(ctx, id)
		return err
	})
	return out, err
}
