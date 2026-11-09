package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The record tables store what later phases produce: plan reviews (Phase
// 8), invocations (Phases 5 and 7), gate results (Phase 6), code reviews,
// findings, commits, and reports (Phase 9). JSON documents are passed as
// bytes the caller marshalled from a typed value; they are validated and
// masked here.

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) insert(ctx context.Context, q string, args ...any) (int64, error) {
	var id int64
	err := s.write(ctx, func(tx *wtx) error {
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// PlanReview is one Stage 2 review round.
type PlanReview struct {
	RunID         string
	Round         int
	ReviewerModel string
	Verdict       string // pass | revise (the reviewer's, recorded but not trusted)
	GatePassed    bool   // the Plan Decision Gate result computed by code
	Review        []byte // plan-review.schema.json document
	Artifact      string // artifact path of the full review
	CreatedAt     time.Time
}

// AddPlanReview stores a plan review round.
func (s *Store) AddPlanReview(ctx context.Context, r PlanReview) error {
	doc, err := s.maskJSON(r.Review, "")
	if err != nil {
		return err
	}
	if doc == "" || r.Round < 1 {
		return errors.New("state: plan review needs a round >= 1 and a review document")
	}
	_, err = s.insert(ctx, `INSERT INTO plan_reviews (run_id, round, reviewer_model, verdict, gate_passed, review_json,
		artifact, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, r.RunID, r.Round, s.mask(r.ReviewerModel),
		s.mask(r.Verdict), boolInt(r.GatePassed), doc, s.mask(r.Artifact), s.nowMS())
	return err
}

// ListPlanReviews returns a run's plan reviews by round.
func (s *Store) ListPlanReviews(ctx context.Context, runID string) ([]PlanReview, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT round, reviewer_model, verdict, gate_passed, review_json, artifact,
		created_at FROM plan_reviews WHERE run_id = ? ORDER BY round`, runID)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []PlanReview
	for rows.Next() {
		r := PlanReview{RunID: runID}
		var (
			passed  int
			doc     string
			created int64
		)
		if err := rows.Scan(&r.Round, &r.ReviewerModel, &r.Verdict, &passed, &doc, &r.Artifact, &created); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		r.GatePassed, r.Review, r.CreatedAt = passed == 1, []byte(doc), msTime(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Invocation is one Claude Code or Codex CLI call.
type Invocation struct {
	ID           int64
	RunID        string
	StepID       int64  // 0 when not tied to a step
	Agent        string // claude | codex
	Role         string // planner | implementer | plan_reviewer | reviewer
	Model        string
	AuthMode     string // subscription | api
	SessionID    string
	Status       string // running | completed | failed
	Turns        int
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Transcript   string // artifact path
	Error        string
	StartedAt    time.Time
	EndedAt      *time.Time
}

// StartInvocation records an agent call as running and returns its ID.
func (s *Store) StartInvocation(ctx context.Context, in Invocation) (int64, error) {
	var step any
	if in.StepID > 0 {
		step = in.StepID
	}
	return s.insert(ctx, `INSERT INTO invocations (run_id, step_id, agent, role, model, auth_mode, session_id, status,
		started_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'running', ?)`, in.RunID, step, in.Agent, in.Role, s.mask(in.Model),
		in.AuthMode, s.mask(in.SessionID), s.nowMS())
}

// InvocationResult is the outcome of an agent call.
type InvocationResult struct {
	Failed       bool
	SessionID    string
	Turns        int
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Transcript   string
	Error        string
}

// FinishInvocation records the outcome of a running agent call.
func (s *Store) FinishInvocation(ctx context.Context, id int64, r InvocationResult) error {
	status := "completed"
	if r.Failed {
		status = "failed"
	}
	return s.write(ctx, func(tx *wtx) error {
		res, err := tx.ExecContext(ctx, `UPDATE invocations SET status = ?, session_id = CASE WHEN ? = '' THEN session_id
			ELSE ? END, turns = ?, input_tokens = ?, output_tokens = ?, cost_usd = ?, transcript = ?, error = ?,
			ended_at = ? WHERE id = ? AND status = 'running'`, status, r.SessionID, s.mask(r.SessionID), r.Turns,
			r.InputTokens, r.OutputTokens, r.CostUSD, s.mask(r.Transcript), s.mask(r.Error), s.nowMS(), id)
		if err != nil {
			return fmt.Errorf("state: %w", err)
		}
		return requireRow(res, "running invocation")
	})
}

// ListInvocations returns a run's agent calls in order.
func (s *Store) ListInvocations(ctx context.Context, runID string) ([]Invocation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(step_id, 0), agent, role, model, auth_mode, session_id,
		status, turns, input_tokens, output_tokens, cost_usd, transcript, error, started_at, ended_at
		FROM invocations WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []Invocation
	for rows.Next() {
		in := Invocation{RunID: runID}
		var (
			start int64
			end   sql.NullInt64
		)
		if err := rows.Scan(&in.ID, &in.StepID, &in.Agent, &in.Role, &in.Model, &in.AuthMode, &in.SessionID,
			&in.Status, &in.Turns, &in.InputTokens, &in.OutputTokens, &in.CostUSD, &in.Transcript, &in.Error,
			&start, &end); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		in.StartedAt, in.EndedAt = msTime(start), msPtr(end)
		out = append(out, in)
	}
	return out, rows.Err()
}

// GateResult is one quality gate execution.
type GateResult struct {
	ID          int64
	RunID       string
	Phase       int
	Attempt     int
	Round       int
	Gate        string // typecheck | lint | unit | integration | build | security
	IsBaseline  bool
	Status      string // passed | failed | not_applicable | error
	Command     string
	ExitCode    int
	Duration    time.Duration
	TreeHash    string
	Summary     []byte // JSON: counts, regressions, new findings
	LogArtifact string
	CreatedAt   time.Time
}

// AddGateResult stores a gate result.
func (s *Store) AddGateResult(ctx context.Context, g GateResult) (int64, error) {
	sum, err := s.maskJSON(g.Summary, "{}")
	if err != nil {
		return 0, err
	}
	return s.insert(ctx, `INSERT INTO gate_results (run_id, phase, attempt, round, gate, is_baseline, status, command,
		exit_code, duration_ms, tree_hash, summary_json, log_artifact, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, g.RunID, g.Phase, g.Attempt, g.Round, g.Gate,
		boolInt(g.IsBaseline), g.Status, s.mask(g.Command), g.ExitCode, g.Duration.Milliseconds(), g.TreeHash, sum,
		s.mask(g.LogArtifact), s.nowMS())
}

// ListGateResults returns a phase's gate results (phase 0: run level).
func (s *Store) ListGateResults(ctx context.Context, runID string, phase int) ([]GateResult, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, phase, attempt, round, gate, is_baseline, status, command, exit_code,
		duration_ms, tree_hash, summary_json, log_artifact, created_at FROM gate_results WHERE run_id = ? AND phase = ?
		ORDER BY id`, runID, phase)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []GateResult
	for rows.Next() {
		g := GateResult{RunID: runID}
		var (
			base        int
			dur, create int64
			sum         string
		)
		if err := rows.Scan(&g.ID, &g.Phase, &g.Attempt, &g.Round, &g.Gate, &base, &g.Status, &g.Command, &g.ExitCode,
			&dur, &g.TreeHash, &sum, &g.LogArtifact, &create); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		g.IsBaseline, g.Duration, g.Summary, g.CreatedAt = base == 1, time.Duration(dur)*time.Millisecond, []byte(sum),
			msTime(create)
		out = append(out, g)
	}
	return out, rows.Err()
}

// Review is one code review round (Stage 3) or the final review (Stage 4).
type Review struct {
	RunID         string
	Kind          string // code | final
	Phase         int
	Attempt       int
	Round         int
	ReviewerModel string
	Verdict       string
	GatePassed    bool
	TreeHash      string // tree reviewed (the commit gate compares it)
	Review        []byte // code-review.schema.json document
	Artifact      string
	CreatedAt     time.Time
}

// AddReview stores a review round.
func (s *Store) AddReview(ctx context.Context, r Review) error {
	doc, err := s.maskJSON(r.Review, "")
	if err != nil {
		return err
	}
	if doc == "" || r.Round < 1 {
		return errors.New("state: review needs a round >= 1 and a review document")
	}
	_, err = s.insert(ctx, `INSERT INTO reviews (run_id, kind, phase, attempt, round, reviewer_model, verdict,
		gate_passed, tree_hash, review_json, artifact, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.Kind, r.Phase, r.Attempt, r.Round, s.mask(r.ReviewerModel), s.mask(r.Verdict),
		boolInt(r.GatePassed), r.TreeHash, doc, s.mask(r.Artifact), s.nowMS())
	return err
}

// ListReviews returns a phase's reviews (phase 0 with kind final for Stage 4).
func (s *Store) ListReviews(ctx context.Context, runID string, phase int) ([]Review, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, attempt, round, reviewer_model, verdict, gate_passed, tree_hash,
		review_json, artifact, created_at FROM reviews WHERE run_id = ? AND phase = ? ORDER BY id`, runID, phase)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		r := Review{RunID: runID, Phase: phase}
		var (
			passed  int
			doc     string
			created int64
		)
		if err := rows.Scan(&r.Kind, &r.Attempt, &r.Round, &r.ReviewerModel, &r.Verdict, &passed, &r.TreeHash, &doc,
			&r.Artifact, &created); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		r.GatePassed, r.Review, r.CreatedAt = passed == 1, []byte(doc), msTime(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Finding is one entry of the finding ledger (plan or code).
type Finding struct {
	RunID           string
	ID              string // PF-002, F-03-002
	Scope           string // plan | code | final
	Phase           int
	Fingerprint     string // stable identity for oscillation detection
	Severity        string
	Category        string
	ResolutionClass string // fixable | deferred | spec_conflict | unsafe | environment
	Status          string // open | resolved | deferred | dispute_accepted | dispute_rejected | ...
	Title           string
	File            string
	LineStart       int
	LineEnd         int
	OpenedRound     int
	ClosedRound     int
	ReopenCount     int
	DisputeCount    int
	Data            []byte // full finding JSON (description, evidence, responses)
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// UpsertFinding inserts a finding or updates its mutable fields.
func (s *Store) UpsertFinding(ctx context.Context, f Finding) error {
	if f.ID == "" || f.Fingerprint == "" {
		return errors.New("state: finding needs an ID and a fingerprint")
	}
	data, err := s.maskJSON(f.Data, "{}")
	if err != nil {
		return err
	}
	now := s.nowMS()
	_, err = s.insert(ctx, `INSERT INTO findings (run_id, id, scope, phase, fingerprint, severity, category,
		resolution_class, status, title, file, line_start, line_end, opened_round, closed_round, reopen_count,
		dispute_count, data_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id, id) DO UPDATE SET severity = excluded.severity, category = excluded.category,
		resolution_class = excluded.resolution_class, status = excluded.status, title = excluded.title,
		file = excluded.file, line_start = excluded.line_start, line_end = excluded.line_end,
		closed_round = excluded.closed_round, reopen_count = excluded.reopen_count,
		dispute_count = excluded.dispute_count, data_json = excluded.data_json, updated_at = excluded.updated_at`,
		f.RunID, f.ID, f.Scope, f.Phase, f.Fingerprint, f.Severity, f.Category, f.ResolutionClass, f.Status,
		s.mask(f.Title), s.mask(f.File), f.LineStart, f.LineEnd, f.OpenedRound, f.ClosedRound, f.ReopenCount,
		f.DisputeCount, data, now, now)
	return err
}

// ListFindings returns a run's findings for a scope ("" = all), by ID.
func (s *Store) ListFindings(ctx context.Context, runID, scope string) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, scope, phase, fingerprint, severity, category, resolution_class,
		status, title, file, line_start, line_end, opened_round, closed_round, reopen_count, dispute_count, data_json,
		created_at, updated_at FROM findings WHERE run_id = ? AND (? = '' OR scope = ?) ORDER BY id`, runID, scope, scope)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		f := Finding{RunID: runID}
		var (
			data             string
			created, updated int64
		)
		if err := rows.Scan(&f.ID, &f.Scope, &f.Phase, &f.Fingerprint, &f.Severity, &f.Category, &f.ResolutionClass,
			&f.Status, &f.Title, &f.File, &f.LineStart, &f.LineEnd, &f.OpenedRound, &f.ClosedRound, &f.ReopenCount,
			&f.DisputeCount, &data, &created, &updated); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		f.Data, f.CreatedAt, f.UpdatedAt = []byte(data), msTime(created), msTime(updated)
		out = append(out, f)
	}
	return out, rows.Err()
}

// Commit is a commit ACO made on the run branch.
type Commit struct {
	RunID        string
	SHA          string
	Kind         string // plan_freeze | phase | final_fix | final_report
	Phase        int
	Attempt      int
	TreeHash     string
	ReviewedTree string
	Tag          string
	CreatedAt    time.Time
}

// AddCommit records a commit.
func (s *Store) AddCommit(ctx context.Context, c Commit) error {
	if c.SHA == "" || c.TreeHash == "" {
		return errors.New("state: commit needs a SHA and a tree hash")
	}
	_, err := s.insert(ctx, `INSERT INTO commits (run_id, sha, kind, phase, attempt, tree_hash, reviewed_tree, tag,
		created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.RunID, c.SHA, c.Kind, c.Phase, c.Attempt, c.TreeHash,
		c.ReviewedTree, s.mask(c.Tag), s.nowMS())
	return err
}

// ListCommits returns a run's commits in order.
func (s *Store) ListCommits(ctx context.Context, runID string) ([]Commit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha, kind, phase, attempt, tree_hash, reviewed_tree, tag, created_at
		FROM commits WHERE run_id = ? ORDER BY created_at, rowid`, runID)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []Commit
	for rows.Next() {
		c := Commit{RunID: runID}
		var created int64
		if err := rows.Scan(&c.SHA, &c.Kind, &c.Phase, &c.Attempt, &c.TreeHash, &c.ReviewedTree, &c.Tag,
			&created); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		c.CreatedAt = msTime(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Report is a written report (Phase Completion, Blocked, Failure, Final).
type Report struct {
	RunID     string
	Kind      string // phase | blocked | failure | final
	Phase     int
	Path      string // artifact or repository path
	SHA256    string
	CommitSHA string
	CreatedAt time.Time
}

// AddReport records a report; a later report of the same kind and phase
// replaces it (a re-written Blocked Report after resume).
func (s *Store) AddReport(ctx context.Context, r Report) error {
	if r.Path == "" || r.SHA256 == "" {
		return errors.New("state: report needs a path and a SHA-256")
	}
	_, err := s.insert(ctx, `INSERT INTO reports (run_id, kind, phase, path, sha256, commit_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (run_id, kind, phase) DO UPDATE SET path = excluded.path,
		sha256 = excluded.sha256, commit_sha = excluded.commit_sha, created_at = excluded.created_at`,
		r.RunID, r.Kind, r.Phase, s.mask(r.Path), r.SHA256, r.CommitSHA, s.nowMS())
	return err
}

// ListReports returns a run's reports.
func (s *Store) ListReports(ctx context.Context, runID string) ([]Report, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, phase, path, sha256, commit_sha, created_at FROM reports
		WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		r := Report{RunID: runID}
		var created int64
		if err := rows.Scan(&r.Kind, &r.Phase, &r.Path, &r.SHA256, &r.CommitSHA, &created); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
		r.CreatedAt = msTime(created)
		out = append(out, r)
	}
	return out, rows.Err()
}
