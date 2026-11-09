package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Trailer is one commit trailer ("Key: Value").
type Trailer struct {
	Key   string
	Value string
}

// Message is a commit message: subject, optional body, and trailers.
type Message struct {
	Subject  string
	Body     string
	Trailers []Trailer
}

// String renders the message. Newlines in the subject and in trailer values
// are flattened so a value cannot inject another trailer.
func (m Message) String() string {
	var b strings.Builder
	b.WriteString(oneLine(m.Subject))
	b.WriteString("\n")
	if body := strings.TrimSpace(strings.ReplaceAll(m.Body, "\r\n", "\n")); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	if len(m.Trailers) > 0 {
		b.WriteString("\n")
		for _, t := range m.Trailers {
			b.WriteString(oneLine(t.Key))
			b.WriteString(": ")
			b.WriteString(oneLine(t.Value))
			b.WriteString("\n")
		}
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// FreezeInfo fills the plan freeze commit message (§10.5).
type FreezeInfo struct {
	RunID         string
	PlanVersion   string // "v1"
	PlanHash      string
	ReviewRounds  int
	ReviewerModel string
}

// FreezeMessage is `docs(aco): freeze implementation plan vN` with trailers
// ACO-Run, ACO-Plan-Hash, ACO-Plan-Review-Rounds, Reviewed-by.
func FreezeMessage(f FreezeInfo) Message {
	v := f.PlanVersion
	if v == "" {
		v = "v1"
	}
	return Message{
		Subject: "docs(aco): freeze implementation plan " + v,
		Body:    "Frozen plan package and AGENTS.md after independent plan review.",
		Trailers: []Trailer{
			{"ACO-Run", f.RunID},
			{"ACO-Plan-Hash", f.PlanHash},
			{"ACO-Plan-Review-Rounds", fmt.Sprint(f.ReviewRounds)},
			{"Reviewed-by", f.ReviewerModel},
		},
	}
}

// ACRow is one acceptance-criterion row of a phase commit body.
type ACRow struct {
	ID     string
	Status string
	Text   string
}

// PhaseInfo fills a phase commit message (§11.8).
type PhaseInfo struct {
	RunID         string
	Phase         int
	Title         string
	Attempt       int
	PlanHash      string
	ReviewedTree  string
	ReviewRounds  int
	ReviewerModel string
	Summary       string
	AC            []ACRow
}

// PhaseAttemptKey is the ACO-Phase-Attempt trailer value, used to find an
// existing commit for the same attempt before committing again (§14.2).
func PhaseAttemptKey(runID string, phase, attempt int) string {
	return fmt.Sprintf("%s/P%02d/a%d", runID, phase, attempt)
}

// PhaseMessage is `feat(phase-NN): <title>` with a body (summary, AC table,
// review rounds) and the trailers of §11.8.
func PhaseMessage(p PhaseInfo) Message {
	var body strings.Builder
	if s := strings.TrimSpace(p.Summary); s != "" {
		body.WriteString(s)
		body.WriteString("\n\n")
	}
	if len(p.AC) > 0 {
		body.WriteString("| AC | Status | Criterion |\n|---|---|---|\n")
		for _, a := range p.AC {
			body.WriteString(fmt.Sprintf("| %s | %s | %s |\n", cell(a.ID), cell(a.Status), cell(a.Text)))
		}
		body.WriteString("\n")
	}
	body.WriteString(fmt.Sprintf("Review rounds: %d", p.ReviewRounds))
	return Message{
		Subject: fmt.Sprintf("feat(phase-%02d): %s", p.Phase, p.Title),
		Body:    body.String(),
		Trailers: []Trailer{
			{"ACO-Run", p.RunID},
			{"ACO-Phase", fmt.Sprintf("%02d", p.Phase)},
			{"ACO-Phase-Attempt", PhaseAttemptKey(p.RunID, p.Phase, p.Attempt)},
			{"ACO-Plan-Hash", p.PlanHash},
			{"ACO-Reviewed-Tree", p.ReviewedTree},
			{"ACO-Review-Rounds", fmt.Sprint(p.ReviewRounds)},
			{"Reviewed-by", p.ReviewerModel},
			{"Co-Authored-By", "Claude <noreply@anthropic.com>"},
		},
	}
}

func cell(s string) string { return strings.ReplaceAll(oneLine(s), "|", `\|`) }

// identityEnv returns author/committer variables for id (none when empty).
func identityEnv(id Identity) []string {
	var env []string
	if id.Name != "" {
		env = append(env, "GIT_AUTHOR_NAME="+id.Name, "GIT_COMMITTER_NAME="+id.Name)
	}
	if id.Email != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+id.Email, "GIT_COMMITTER_EMAIL="+id.Email)
	}
	return env
}

// commitTree creates a commit object. sign=false disables signing (checkpoints);
// otherwise commit.gpgSign from the repository config is honoured.
func (r *Repo) commitTree(ctx context.Context, tree string, parents []string, message string, id Identity, sign bool) (string, error) {
	args := []string{"commit-tree"}
	if !sign {
		args = append(args, "--no-gpg-sign")
	}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-F", "-", "--end-of-options", tree)
	b, err := r.run(ctx, call{args: args, stdin: strings.NewReader(r.opts.Redactor.String(message)), env: identityEnv(id)})
	return strings.TrimSpace(string(b)), err
}

// Commit errors.
var (
	ErrTreeMismatch     = errors.New("gitops: staged tree differs from the expected tree")
	ErrUnreviewedChange = errors.New("gitops: working state has changes outside the reviewed tree")
	ErrBranchMoved      = errors.New("gitops: branch moved since the expected head")
	ErrBadPath          = errors.New("gitops: path is not a clean repository-relative path")
)

// reflogCommit is the fixed reflog message of run-branch commits. The commit
// subject is never copied into the reflog, which is not masked.
const reflogCommit = "aco: commit"

// CommitResult is a new commit on the worktree's branch.
type CommitResult struct {
	Commit string
	Tree   string
	Branch string // full ref
}

// cleanPaths validates repository-relative, slash-separated paths and returns
// them sorted and de-duplicated.
func cleanPaths(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" || strings.Contains(p, "\\") || strings.HasPrefix(p, "/") || path.Clean(p) != p ||
			p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "\x00") ||
			(len(p) >= 2 && p[1] == ':') {
			return nil, ErrBadPath
		}
		for _, seg := range strings.Split(p, "/") {
			if strings.EqualFold(seg, ".git") {
				return nil, ErrBadPath
			}
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return slices.Compact(out), nil
}

// stageTree builds a tree in a temporary index: start from base (a tree-ish;
// "" = HEAD), then take each path's current working-tree state (added,
// modified, or deleted). The worktree's own index is not used.
func (r *Repo) stageTree(ctx context.Context, base string, paths []string) (string, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return "", err
	}
	idx, done, err := r.tempIndex(ctx)
	if err != nil {
		return "", err
	}
	defer done()
	if err := r.loadIndex(ctx, idx, base); err != nil {
		return "", err
	}
	if len(paths) > 0 {
		in := strings.Join(paths, "\x00") + "\x00"
		if _, err := r.withIndex(ctx, idx, strings.NewReader(in),
			"--literal-pathspecs", "add", "--all", "--force", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return "", err
		}
	}
	b, err := r.withIndex(ctx, idx, nil, "write-tree")
	return strings.TrimSpace(string(b)), err
}

// existingPaths keeps the paths that exist in the working tree or in HEAD;
// git add rejects a pathspec that matches neither.
func (r *Repo) existingPaths(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	inHead := map[string]bool{}
	if ok, err := r.hasHead(ctx); err != nil {
		return nil, err
	} else if ok {
		b, err := r.run(ctx, call{args: []string{"ls-tree", "-r", "-z", "--name-only", "--full-tree", "HEAD"}})
		if err != nil {
			return nil, err
		}
		for _, p := range splitNUL(b) {
			inHead[p] = true
		}
	}
	var out []string
	for _, p := range paths {
		if inHead[p] || r.exists(p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// exists reports whether a repository-relative path exists in the worktree
// (without following a final symlink).
func (r *Repo) exists(p string) bool {
	_, err := os.Lstat(filepath.Join(r.dir, filepath.FromSlash(p)))
	return err == nil
}

// CommitSpec describes a commit of selected paths on the worktree's branch.
type CommitSpec struct {
	// Paths are the repository-relative paths to take from the working tree
	// (added, modified, or deleted). Nothing else is staged.
	Paths []string
	// ExpectedTree, when set, is the reviewed tree: the commit is refused
	// unless Paths staged on HEAD give exactly this tree, Paths plus Extra
	// give exactly ExpectedTree plus Extra, and the whole working state
	// equals that same tree (no unreviewed or untracked change outside it).
	ExpectedTree string
	// Extra are files added on top of the reviewed tree (the Phase Completion
	// Report). They must exist in the working tree.
	Extra []string
	// RequireExact refuses the commit when the working state has any change
	// outside Paths+Extra (used for the plan freeze commit).
	RequireExact bool
	// ExpectedHead, when set, must equal the branch head (protects against
	// the run branch being moved externally).
	ExpectedHead string
	Message      Message
}

// CommitReviewed commits exactly the reviewed tree plus extraFiles (§11.8).
// It fails with ErrTreeMismatch when the staged tree differs from expectedTree
// (+ extraFiles), and with ErrUnreviewedChange when the working state holds
// any change that is neither reviewed nor in extraFiles.
func (r *Repo) CommitReviewed(ctx context.Context, paths []string, expectedTree string, extraFiles []string, msg Message) (*CommitResult, error) {
	if expectedTree == "" {
		return nil, ErrTreeMismatch
	}
	return r.Commit(ctx, CommitSpec{Paths: paths, ExpectedTree: expectedTree, Extra: extraFiles, Message: msg})
}

// Commit stages spec.Paths (and spec.Extra) in a temporary index built from
// HEAD, verifies the result, creates the commit (honouring commit.gpgSign),
// and advances the run branch with a compare-and-swap. The worktree's index
// is then reset to the new HEAD; files are not touched. It fails with
// ErrNotOwnedWorktree, before writing anything, unless r is an ACO run
// worktree whose HEAD is on its run branch.
func (r *Repo) Commit(ctx context.Context, spec CommitSpec) (*CommitResult, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return nil, err
	}
	paths, err := cleanPaths(spec.Paths)
	if err != nil {
		return nil, err
	}
	extra, err := cleanPaths(spec.Extra)
	if err != nil {
		return nil, err
	}
	// Only an ACO run worktree whose HEAD is on its run branch may commit,
	// and only that branch is ever advanced.
	if err := r.assertOwned(ctx, true, roleRun); err != nil {
		return nil, err
	}
	branch := r.branch
	head := ""
	if ok, err := r.hasHead(ctx); err != nil {
		return nil, err
	} else if ok {
		if head, err = r.Head(ctx); err != nil {
			return nil, err
		}
	}
	if spec.ExpectedHead != "" && spec.ExpectedHead != head {
		return nil, ErrBranchMoved
	}
	for _, p := range extra {
		if !r.exists(p) {
			return nil, fmt.Errorf("gitops: extra file %s does not exist", p)
		}
	}
	stagePaths, err := r.existingPaths(ctx, paths)
	if err != nil {
		return nil, err
	}
	all, err := r.existingPaths(ctx, append(slices.Clone(paths), extra...))
	if err != nil {
		return nil, err
	}
	final, err := r.stageTree(ctx, "", all)
	if err != nil {
		return nil, err
	}

	if spec.ExpectedTree != "" {
		reviewed, err := r.stageTree(ctx, "", stagePaths)
		if err != nil {
			return nil, err
		}
		if reviewed != spec.ExpectedTree {
			return nil, ErrTreeMismatch
		}
		want, err := r.stageTree(ctx, spec.ExpectedTree, extra)
		if err != nil {
			return nil, err
		}
		if final != want {
			return nil, ErrTreeMismatch
		}
	}
	if spec.ExpectedTree != "" || spec.RequireExact {
		working, err := r.workingTree(ctx, extra)
		if err != nil {
			return nil, err
		}
		if working != final {
			return nil, ErrUnreviewedChange
		}
	}

	var parents []string
	if head != "" {
		parents = []string{head}
	}
	commit, err := r.commitTree(ctx, final, parents, spec.Message.String(), r.opts.Identity, true)
	if err != nil {
		return nil, err
	}
	old := head
	if old == "" {
		old = strings.Repeat("0", len(commit))
	}
	if _, err := r.run(ctx, call{args: []string{"update-ref", "-m", reflogCommit, branch, commit, old}}); err != nil {
		return nil, ErrBranchMoved
	}
	if err := r.syncIndexToHead(ctx); err != nil {
		return nil, err
	}
	return &CommitResult{Commit: commit, Tree: final, Branch: branch}, nil
}

// TrailersOf returns the trailers of a commit, parsed by
// `git interpret-trailers --parse`.
func (r *Repo) TrailersOf(ctx context.Context, commit string) ([]Trailer, error) {
	msg, err := r.run(ctx, call{args: []string{"cat-file", "commit", commit}})
	if err != nil {
		return nil, err
	}
	// Drop the commit header (everything up to the first blank line).
	if i := strings.Index(string(msg), "\n\n"); i >= 0 {
		msg = msg[i+2:]
	}
	b, err := r.run(ctx, call{args: []string{"interpret-trailers", "--parse"}, stdin: strings.NewReader(string(msg))})
	if err != nil {
		return nil, err
	}
	var out []Trailer
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			out = append(out, Trailer{Key: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
		}
	}
	return out, nil
}

// FindCommitByTrailer returns the newest commit reachable from rev whose
// trailer key has exactly value, or "" if none (§14.2 duplicate check).
func (r *Repo) FindCommitByTrailer(ctx context.Context, rev, key, value string) (string, error) {
	b, err := r.run(ctx, call{args: []string{"log", "-z", "--format=%H%x01%(trailers:key=" + key + ",valueonly,separator=%x02)", "--end-of-options", rev}})
	if err != nil {
		return "", err
	}
	for _, rec := range splitNUL(b) {
		sha, vals, _ := strings.Cut(strings.TrimLeft(rec, "\n"), "\x01")
		for _, v := range strings.Split(strings.TrimSpace(vals), "\x02") {
			if strings.TrimSpace(v) == value {
				return sha, nil
			}
		}
	}
	return "", nil
}
