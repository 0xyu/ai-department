package gitops

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// NotesRef is the notes namespace ACO writes (git notes --ref=aco).
const NotesRef = "aco"

// TagName returns aco/<run-id>/<name> (name: plan-v1, phase-NN, final).
func TagName(runID, name string) string { return "aco/" + runID + "/" + name }

// PhaseTag returns the tag name of a committed phase.
func PhaseTag(runID string, phase int) string {
	return TagName(runID, fmt.Sprintf("phase-%02d", phase))
}

// ErrTagExists reports a tag that already points at a different commit.
var ErrTagExists = errors.New("gitops: tag exists and points elsewhere")

// Tag creates the annotated tag name at commit. Creating the same tag at the
// same commit again is a no-op; a tag at another commit is ErrTagExists.
func (r *Repo) Tag(ctx context.Context, name, commit, message string) error {
	ctx, err := r.scope(ctx)
	if err != nil {
		return err
	}
	ref := "refs/tags/" + name
	if err := r.checkRefFormat(ctx, ref); err != nil {
		return err
	}
	if ok, err := r.RefExists(ctx, ref); err != nil {
		return err
	} else if ok {
		at, err := r.ResolveCommit(ctx, ref)
		if err != nil {
			return err
		}
		if at == commit {
			return nil
		}
		return ErrTagExists
	}
	if message == "" {
		message = name
	}
	_, err = r.run(ctx, call{
		args:  []string{"tag", "-a", "-F", "-", "--end-of-options", name, commit},
		stdin: strings.NewReader(r.opts.Redactor.String(message)),
		env:   identityEnv(r.opts.Identity),
	})
	return err
}

// AddNote attaches (or replaces) the aco note of commit with data, usually
// the JSON record of that commit. Data is masked before it is written.
func (r *Repo) AddNote(ctx context.Context, commit string, data []byte) error {
	_, err := r.run(ctx, call{
		args:  []string{"notes", "--ref=" + NotesRef, "add", "-f", "-F", "-", commit},
		stdin: strings.NewReader(r.opts.Redactor.String(string(data))),
		env:   identityEnv(noteIdentity(r.opts.Identity)),
	})
	return err
}

func noteIdentity(id Identity) Identity {
	if id.Name == "" && id.Email == "" {
		return checkpointIdentity
	}
	return id
}

// Note returns the aco note of commit, or nil if there is none.
func (r *Repo) Note(ctx context.Context, commit string) ([]byte, error) {
	b, err := r.run(ctx, call{args: []string{"notes", "--ref=" + NotesRef, "show", commit}})
	if err != nil {
		if exitCode(err) == 1 {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

// SaveBlocked stores the working state of a blocked phase in
// refs/aco/<run>/blocked/PNN (§11.7). The run branch is not changed.
func (r *Repo) SaveBlocked(ctx context.Context, runID string, phase int, reason string) (*Checkpoint, error) {
	if err := validRunID(runID); err != nil {
		return nil, err
	}
	return r.Checkpoint(ctx, BlockedRef(runID, phase), fmt.Sprintf("aco: blocked phase %02d: %s", phase, oneLine(reason)))
}

// ShadowRefs lists the shadow refs of a run (refs/aco/<run>/…).
func (r *Repo) ShadowRefs(ctx context.Context, runID string) ([]string, error) {
	if err := validRunID(runID); err != nil {
		return nil, err
	}
	b, err := r.run(ctx, call{args: []string{"for-each-ref", "--format=%(refname)", runRefs(runID)}})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// PruneRefs deletes a run's checkpoint refs in one transaction. Blocked WIP
// refs are kept when keepBlocked is set. Branches, tags, and notes stay.
func (r *Repo) PruneRefs(ctx context.Context, runID string, keepBlocked bool) (int, error) {
	ctx, err := r.scope(ctx)
	if err != nil {
		return 0, err
	}
	refs, err := r.ShadowRefs(ctx, runID)
	if err != nil {
		return 0, err
	}
	var in strings.Builder
	in.WriteString("start\n")
	n := 0
	for _, ref := range refs {
		if keepBlocked && strings.HasPrefix(ref, runRefs(runID)+"blocked/") {
			continue
		}
		in.WriteString("delete " + ref + "\n")
		n++
	}
	in.WriteString("commit\n")
	if n == 0 {
		return 0, nil
	}
	if _, err := r.run(ctx, call{args: []string{"update-ref", "--stdin"}, stdin: strings.NewReader(in.String())}); err != nil {
		return 0, err
	}
	return n, nil
}

// Push pushes branch (and, if withTags, the run's tags) to remote. It never
// forces; a rejected push is an error. GitHub tokens stay in the environment
// so that a credential helper can use them.
func (r *Repo) Push(ctx context.Context, remote, branch string, tags []string) error {
	if strings.HasPrefix(remote, "-") || remote == "" {
		return fmt.Errorf("gitops: invalid remote")
	}
	if err := r.CheckBranchName(ctx, branch); err != nil {
		return err
	}
	refspecs := []string{"refs/heads/" + branch + ":refs/heads/" + branch}
	for _, t := range tags {
		if err := r.checkRefFormat(ctx, "refs/tags/"+t); err != nil {
			return err
		}
		refspecs = append(refspecs, "refs/tags/"+t+":refs/tags/"+t)
	}
	args := append([]string{"push", "--porcelain", "--no-verify", "--end-of-options", remote}, refspecs...)
	_, err := r.run(ctx, call{args: args, tokens: true})
	return err
}

// PRRequest describes a pull request opened with the GitHub CLI.
type PRRequest struct {
	Base  string
	Head  string
	Title string
	Body  string
}

// PRArgs returns the `gh pr create` arguments for req; the body is passed on
// stdin (--body-file -).
func PRArgs(req PRRequest) []string {
	return []string{"pr", "create", "--base", req.Base, "--head", req.Head, "--title", oneLine(req.Title), "--body-file", "-"}
}

// OpenPR opens a pull request with the GitHub CLI (gh) in the worktree. It is
// used only when git.open_pr is enabled. ghBin defaults to "gh".
func (r *Repo) OpenPR(ctx context.Context, ghBin string, req PRRequest) (string, error) {
	if ghBin == "" {
		ghBin = "gh"
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghBin, PRArgs(req)...) // #nosec G204 -- fixed binary; arguments built here
	cmd.Dir = r.dir
	cmd.Env = r.gitEnv(call{tokens: true})
	cmd.Stdin = strings.NewReader(r.opts.Redactor.String(req.Body))
	var out, errb limitedBuffer
	out.max, errb.max = 1<<20, 64<<10
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", &Error{Op: "gh pr create", ExitCode: -1, Stderr: r.cleanStderr(errb.Bytes()), Err: err}
	}
	return strings.TrimSpace(out.String()), nil
}
