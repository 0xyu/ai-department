package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/events"
	"github.com/0xyu/aco/internal/gates"
	"github.com/0xyu/aco/internal/gitops"
	"github.com/0xyu/aco/internal/policy"
	"github.com/0xyu/aco/internal/reports"
	"github.com/0xyu/aco/internal/state"
)

// errWorktreeDrift: the Git helper refused to stage because the worktree
// no longer matches the reviewed tree (ErrTreeMismatch,
// ErrUnreviewedChange) or a report file no longer holds the generated
// bytes (ErrExtraMismatch). Nothing was committed; the phase goes back
// through gates and review on the new candidate.
var errWorktreeDrift = errors.New("engine: the worktree changed after the commit gate")

// beforeStageHook runs right before the reviewed paths are staged (tests
// inject worktree drift there).
var beforeStageHook func()

// reportSnapshot is a report file's content before this commit attempt
// wrote it, so a failed attempt can put it back, and what the attempt
// wrote, so it only touches a file it still owns.
type reportSnapshot struct {
	rel    string
	data   []byte
	exists bool
	// wrote and mode are the bytes and file mode this attempt left; owned
	// is false until the attempt has written the file.
	wrote []byte
	mode  os.FileMode
	owned bool
}

func snapshotReport(wt, rel string) (reportSnapshot, error) {
	data, err := os.ReadFile(filepath.Join(wt, filepath.FromSlash(rel)))
	switch {
	case err == nil:
		return reportSnapshot{rel: rel, data: data, exists: true}, nil
	case errors.Is(err, os.ErrNotExist):
		return reportSnapshot{rel: rel}, nil
	}
	return reportSnapshot{}, err
}

// written records what this attempt wrote to the file.
func (s *reportSnapshot) written(wt string, data []byte) error {
	fi, err := os.Lstat(filepath.Join(wt, filepath.FromSlash(s.rel)))
	if err != nil {
		return err
	}
	s.wrote, s.mode, s.owned = data, fi.Mode(), true
	return nil
}

// stillOwned reports whether the file still holds exactly what this
// attempt wrote (same bytes, same mode): only then is it ours to discard.
func (s reportSnapshot) stillOwned(wt string) bool {
	if !s.owned {
		return false
	}
	target := filepath.Join(wt, filepath.FromSlash(s.rel))
	fi, err := os.Lstat(target)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode() != s.mode {
		return false
	}
	data, err := os.ReadFile(target)
	return err == nil && bytes.Equal(data, s.wrote)
}

// restore discards what this attempt wrote: the old content comes back,
// or the file is removed when there was none. A file someone else changed
// or removed since the attempt wrote it is left as it is.
func (s reportSnapshot) restore(wt string) error {
	if !s.stillOwned(wt) {
		return nil
	}
	target := filepath.Join(wt, filepath.FromSlash(s.rel))
	if s.exists {
		return os.WriteFile(target, s.data, 0o644)
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// commit writes the Phase Completion Report and commits exactly the
// reviewed paths plus the two report files (§11.8 steps 5–9). When the Git
// helper finds that the worktree drifted from the reviewed tree before
// the branch moved, it discards the report files it wrote and returns
// errWorktreeDrift.
func (p *phaseRun) commit(ctx context.Context, tree string, paths []string, pol policy.Result) error {
	repo, wt := p.d.Worktree, p.wt()
	key := gitops.PhaseAttemptKey(p.in.RunID, p.in.Phase, p.attempt)
	// §14.2: never commit the same attempt twice.
	if dup, err := repo.FindCommitByTrailer(ctx, "HEAD", "ACO-Phase-Attempt", key); err != nil {
		return err
	} else if dup != "" {
		return fmt.Errorf("engine: phase %d attempt %d is already committed as %s", p.in.Phase, p.attempt, dup)
	}
	rep, err := p.buildReport(ctx, tree, pol)
	if err != nil {
		return err
	}
	jsonBytes, err := rep.JSON()
	if err != nil {
		return err
	}
	jsonBytes = []byte(p.d.Redactor.String(string(jsonBytes)))
	if probs := reports.ValidatePhaseJSON(jsonBytes); len(probs) > 0 {
		return fmt.Errorf("engine: the phase report does not match its schema: %s", strings.Join(probs, "; "))
	}
	md := []byte(p.d.Redactor.String(reports.RenderPhaseMarkdown(rep)))
	mdRel, jsonRel := reports.PhasePaths(p.in.Phase)
	files := []struct {
		rel  string
		data []byte
	}{{mdRel, md}, {jsonRel, jsonBytes}}
	snaps := make([]reportSnapshot, len(files))
	for i, f := range files {
		if snaps[i], err = snapshotReport(wt, f.rel); err != nil {
			return err
		}
	}
	for i, f := range files {
		if err := writeReportFile(wt, f.rel, f.data); err != nil {
			return err
		}
		if err := snaps[i].written(wt, f.data); err != nil {
			return err
		}
	}
	if err := p.emit(ctx, events.ReportWritten, events.Str("kind", "phase"), events.Str("path", jsonRel)); err != nil {
		return err
	}

	rev := p.att.lastReview.Review
	var acRows []gitops.ACRow
	for _, a := range rep.AcceptanceCriteria {
		acRows = append(acRows, gitops.ACRow{ID: a.ID, Status: a.Status, Text: a.Text})
	}
	msg := gitops.PhaseMessage(gitops.PhaseInfo{RunID: p.in.RunID, Phase: p.in.Phase, Title: p.spec.Title, Attempt: p.attempt,
		PlanHash: p.lock.PlanHash, ReviewedTree: tree, ReviewRounds: p.att.reviewRound, ReviewerModel: rep.ReviewerModel,
		Summary: firstNonEmpty(rep.Implementation.Summary, rev.Summary), AC: acRows})
	intent, _ := json.Marshal(map[string]any{"reviewed_tree": tree, "paths": paths, "attempt_key": key,
		"reports": []string{mdRel, jsonRel}})
	p.att.seq++
	p.att.commitTries++
	stepKey := fmt.Sprintf("P%02d/a%d/commit", p.in.Phase, p.attempt)
	if p.att.commitTries > 1 {
		stepKey += fmt.Sprintf("-%d", p.att.commitTries)
	}
	st, err := p.d.Store.BeginStep(ctx, state.StepIntent{RunID: p.in.RunID, Phase: p.in.Phase, Attempt: p.attempt, Kind: "commit",
		Key: stepKey, Owner: p.d.Owner, Intent: intent})
	if err != nil {
		return err
	}
	wctx := context.WithoutCancel(ctx)
	head, err := repo.Head(wctx)
	if err != nil {
		_, _ = p.d.Store.FailStep(wctx, st.ID, err.Error(), nil)
		return err
	}
	if beforeStageHook != nil {
		beforeStageHook()
	}
	// The commit is bound to the reviewed tree, to the exact report bytes
	// (the files are re-read when staged), and to the phase's validated
	// base as its parent (the ref compare-and-swap alone would accept an
	// external commit made before staging).
	res, err := repo.Commit(wctx, gitops.CommitSpec{Paths: paths, ExpectedTree: tree, Extra: []string{mdRel, jsonRel},
		ExtraData: map[string][]byte{mdRel: md, jsonRel: jsonBytes}, ExpectedHead: p.base, Message: msg})
	if err != nil {
		return p.commitFailed(wctx, st.ID, head, snaps, err)
	}
	tag := ""
	if p.d.Config.Git.Tag {
		tag = gitops.PhaseTag(p.in.RunID, p.in.Phase)
		if err := repo.Tag(wctx, tag, res.Commit, fmt.Sprintf("ACO phase %02d: %s", p.in.Phase, p.spec.Title)); err != nil {
			return err
		}
	}
	if p.d.Config.Git.Notes {
		note, _ := json.Marshal(map[string]any{"kind": "phase", "run_id": p.in.RunID, "phase": p.in.Phase, "attempt": p.attempt,
			"commit": res.Commit, "tree": res.Tree, "reviewed_tree": tree, "plan_hash": p.lock.PlanHash, "tag": tag,
			"report": json.RawMessage(jsonBytes)})
		if err := repo.AddNote(wctx, res.Commit, note); err != nil {
			return err
		}
	}
	if err := p.d.Store.AddCommit(wctx, state.Commit{RunID: p.in.RunID, SHA: res.Commit, Kind: "phase", Phase: p.in.Phase,
		Attempt: p.attempt, TreeHash: res.Tree, ReviewedTree: tree, Tag: tag}); err != nil {
		return err
	}
	if err := p.d.Store.SetPhaseCommits(wctx, p.in.RunID, p.in.Phase, "", res.Commit); err != nil {
		return err
	}
	sum := sha256.Sum256(jsonBytes)
	if err := p.d.Store.AddReport(wctx, state.Report{RunID: p.in.RunID, Kind: "phase", Phase: p.in.Phase, Path: jsonRel,
		SHA256: hex.EncodeToString(sum[:]), CommitSHA: res.Commit}); err != nil {
		return err
	}
	// §11.8 step 9: the gate results are the next phase's baseline.
	if _, err := p.baselineStore().Save(gates.NewBaseline(p.att.lastRound, res.Commit, p.d.Now())); err != nil {
		return err
	}
	if err := p.emit(wctx, events.GitCommit, events.Str("kind", "phase"), events.Str("sha", res.Commit), events.Str("tag", tag),
		events.Str("reviewed_tree", tree)); err != nil {
		return err
	}
	outcome, _ := json.Marshal(map[string]any{"commit": res.Commit, "tree": res.Tree, "tag": tag})
	if _, err := p.d.Store.CompleteStep(wctx, st.ID, outcome); err != nil {
		return err
	}
	if err := p.toPhase(wctx, state.PhaseCommitted); err != nil {
		return err
	}
	p.out.Status, p.out.RunStatus = state.PhaseCommitted, state.RunExecuting
	p.out.Commit, p.out.Tag, p.out.ReviewedTree, p.out.Report = res.Commit, tag, tree, rep
	p.out.ReviewRounds = p.att.reviewRound
	return stopError{}
}

// commitFailed finishes the commit step of a failed CommitReviewed. Drift
// of the worktree from the reviewed tree, found before the branch moved,
// is retryable: the step records it, the report files of this attempt are
// discarded (other worktree edits stay), and errWorktreeDrift sends the
// phase back through gates and review. Anything else (the branch moved,
// Git failed) fails the run.
func (p *phaseRun) commitFailed(ctx context.Context, stepID int64, head string, snaps []reportSnapshot, cerr error) error {
	drift := errors.Is(cerr, gitops.ErrTreeMismatch) || errors.Is(cerr, gitops.ErrUnreviewedChange) ||
		errors.Is(cerr, gitops.ErrExtraMismatch)
	if drift {
		if now, err := p.d.Worktree.Head(ctx); err != nil || now != head {
			drift = false // the branch moved after all: not worktree drift
		}
	}
	if !drift {
		_, _ = p.d.Store.FailStep(ctx, stepID, cerr.Error(), nil)
		return fmt.Errorf("engine: phase commit: %w", cerr)
	}
	outcome, _ := json.Marshal(map[string]any{"committed": false, "retry": "worktree_drift", "head": head})
	if _, err := p.d.Store.FailStep(ctx, stepID, cerr.Error(), outcome); err != nil {
		return err
	}
	wt := p.wt()
	for _, s := range snaps {
		if err := s.restore(wt); err != nil {
			return err
		}
	}
	return errWorktreeDrift
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// writeReportFile writes .aco/reports/<file> in the worktree, refusing a
// linked .aco or .aco/reports.
func writeReportFile(wt, rel string, data []byte) error {
	dir := filepath.Join(wt, filepath.FromSlash(reports.Dir))
	for _, d := range []string{filepath.Join(wt, ".aco"), dir} {
		if err := realDir(d); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(wt, filepath.FromSlash(rel))
	if fi, err := os.Lstat(target); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("engine: %s is not a regular file", rel)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(target, data, 0o644)
}

var changeStatus = map[string]string{
	gitops.Added: "added", gitops.Modified: "modified", gitops.Deleted: "deleted", gitops.Renamed: "renamed",
	gitops.Copied: "copied", gitops.TypeChg: "type_changed",
}

// buildReport assembles the Phase Completion Report (§11.9).
func (p *phaseRun) buildReport(ctx context.Context, tree string, pol policy.Result) (*reports.PhaseReport, error) {
	repo := p.d.Worktree
	rev := p.att.lastReview.Review
	model := firstNonEmpty(p.att.lastReview.Model, p.d.Config.Reviewer.Model)
	r := &reports.PhaseReport{
		Format: reports.PhaseFormat, RunID: p.in.RunID, Phase: p.in.Phase, Title: p.spec.Title, Goal: p.spec.Goal,
		PlanVersion: p.lock.PlanVersion, PlanHash: p.lock.PlanHash, BaseCommit: p.base, ReviewedTree: tree,
		Attempt: p.attempt, Attempts: p.attempt, Sessions: p.sessions, ImplementerModel: p.d.Config.Implementer.Model,
		ReviewerModel: model, Requirements: p.spec.Requirements, RegressionsSeen: p.seenRegr,
		GeneratedAt: p.d.Now().UTC().Format(time.RFC3339),
	}
	// Acceptance criteria: the final review's status and evidence.
	evidence := map[string][2]string{}
	for _, a := range rev.AcceptanceCriteria {
		evidence[a.ID] = [2]string{a.Status, a.Evidence}
	}
	tests := map[string][]string{}
	if rep := p.att.lastReport; rep != nil {
		for _, t := range rep.Tests {
			for _, id := range t.Covers {
				tests[id] = append(tests[id], t.Path)
			}
		}
	}
	for _, ac := range p.spec.AcceptanceCriteria {
		e := evidence[ac.ID]
		r.AcceptanceCriteria = append(r.AcceptanceCriteria, reports.AC{ID: ac.ID, Text: ac.Text, Status: e[0], Evidence: e[1],
			Tests: tests[ac.ID]})
	}
	// Files.
	changes, err := repo.Changes(ctx, p.base, tree)
	if err != nil {
		return nil, err
	}
	var diffPaths []string
	for _, c := range changes {
		fc := reports.FileChange{Path: c.Path, Status: changeStatus[c.Status], Added: c.Added, Deleted: c.Deleted, Binary: c.Binary}
		if c.OldPath != c.Path {
			fc.OldPath = c.OldPath
		}
		if fc.Status == "" {
			fc.Status = "modified"
		}
		r.Files = append(r.Files, fc)
		diffPaths = append(diffPaths, c.Paths()...)
	}
	if r.DiffStat, err = repo.DiffStat(ctx, p.base, tree); err != nil {
		return nil, err
	}
	// Gates.
	for _, g := range p.att.lastRound.Results {
		rg := reports.Gate{Gate: g.Gate, Status: string(g.Status), Required: g.Required, Source: string(g.Source),
			DurationMS: g.Duration.Milliseconds(), Summary: g.Summary}
		for _, c := range g.Commands {
			rg.Commands = append(rg.Commands, c.Command.Run)
		}
		if c := g.Comparison; c != nil {
			rg.Baseline = reports.BaselineDelta{HasBaseline: c.HasBaseline, NewFailingTests: len(c.NewFailingTests),
				Regressions: len(c.Regressions), FixedTests: len(c.FixedTests), PreexistingFailures: len(c.PreexistingFailures),
				NewIssues: len(c.NewIssues), PreexistingIssues: c.PreexistingIssues, NewVulns: len(c.NewVulns),
				PreexistingVulns: len(c.PreexistingVulns)}
			r.Regressions = append(r.Regressions, c.Regressions...)
		}
		r.Gates = append(r.Gates, rg)
	}
	// Security.
	for _, s := range p.specs {
		if s.Type == gates.Security {
			r.Security.Scanners = append(r.Security.Scanners, s.Scanners...)
		}
	}
	if g := p.att.lastRound.Gate(gates.Security); g != nil && g.Comparison != nil {
		for _, i := range g.Comparison.NewIssues {
			r.Security.NewFindings = append(r.Security.NewFindings, fmt.Sprintf("%s %s %s:%d %s", i.Severity, i.Tool, i.Path, i.Line, i.Rule))
		}
		for _, v := range g.Comparison.NewVulns {
			r.Security.NewFindings = append(r.Security.NewFindings, fmt.Sprintf("%s %s %s", v.Severity, v.ID, v.Package))
		}
		for _, v := range g.Comparison.PreexistingVulns {
			r.Security.Preexisting = append(r.Security.Preexisting, fmt.Sprintf("%s %s %s", v.Severity, v.ID, v.Package))
		}
		if n := g.Comparison.PreexistingIssues; n > 0 {
			r.Security.Preexisting = append(r.Security.Preexisting, fmt.Sprintf("%d pre-existing issue(s)", n))
		}
	}
	for _, f := range pol.Findings {
		r.Security.Policy = append(r.Security.Policy, reports.PolicyFinding{Code: f.Code, Path: f.Path, Line: f.Line,
			Blocking: f.Blocking, Message: f.Message})
	}
	// Review history and findings.
	r.Reviews = p.reviews
	for _, e := range p.ledger.Entries {
		f := reports.Finding{ID: e.ID, Attempt: e.Attempt, Severity: e.Finding.Severity, Category: e.Finding.Category,
			ResolutionClass: firstNonEmpty(e.Class, ClassFixable), Status: e.Status, Title: e.Finding.Title, File: e.Finding.File,
			LineStart: e.Finding.LineStart, LineEnd: e.Finding.LineEnd, OpenedRound: e.OpenedRound, ClosedRound: e.ClosedRound,
			ReopenCount: e.ReopenCount, DisputeCount: e.DisputeCount, Ruling: e.Note}
		if e.Response != nil {
			f.Response, f.ResponseDetail = e.Response.Response, e.Response.Detail
		}
		r.Findings = append(r.Findings, f)
		if e.Attempt == p.attempt && e.Status == CodeDeferred {
			r.Deferred = append(r.Deferred, reports.Deferred{ID: e.ID, Severity: e.Finding.Severity, Category: e.Finding.Category,
				Title: e.Finding.Title, File: e.Finding.File, Reason: e.DeferReason})
		}
	}
	// Implementation, verified against the diff.
	if rep := p.att.lastReport; rep != nil {
		r.Implementation.Summary = rep.Summary
		r.Implementation.Decisions = rep.DesignDecisions
		r.Implementation.KnownGaps = rep.KnownGaps
		reported := map[string]bool{}
		for _, f := range rep.FilesChanged {
			reported[f.Path] = true
			if !slices.Contains(diffPaths, f.Path) {
				r.Implementation.Unverified = append(r.Implementation.Unverified, "reported change to "+f.Path+" is not in the diff")
			}
		}
		for _, t := range rep.Tests {
			r.Implementation.Tests = append(r.Implementation.Tests, reports.TestChange{Path: t.Path, Change: t.Change, Covers: t.Covers})
			if !slices.Contains(diffPaths, t.Path) {
				r.Implementation.Unverified = append(r.Implementation.Unverified, "reported test "+t.Path+" is not in the diff")
			}
		}
		if len(rep.FilesChanged) > 0 {
			for _, c := range changes {
				if !reported[c.Path] {
					r.Implementation.Unverified = append(r.Implementation.Unverified, c.Path+" changed but is not in the implementation report")
				}
			}
		}
	}
	if r.Implementation.Summary == "" {
		r.Implementation.Summary = rev.Summary
	}
	// Usage.
	keys := make([]string, 0, len(p.usage))
	for k := range p.usage {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.Usage.Agents = append(r.Usage.Agents, *p.usage[k])
	}
	r.Usage.DurationMS = p.d.Now().Sub(p.started).Milliseconds()
	r.Digest = reports.BuildDigest(r)
	return r, nil
}
