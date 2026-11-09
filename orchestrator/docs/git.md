# Git manager and diff policy

Phase 4 adds two packages:

- `internal/gitops`: everything ACO does with Git (§15 of the implementation plan). Only the orchestrator runs Git;
  agents never do.
- `internal/policy`: the diff policy, part 1. It checks a phase diff for protected paths, paths outside the phase
  scope, large and binary files, secrets, anti-gaming edits, and missing tests.

## Safety rules

| Rule | How it is enforced |
|---|---|
| The user's checkout, index, and base branch are never written | Work happens in a separate ACO-owned worktree (see [Worktree ownership](#worktree-ownership)); commits, resets, and worktree removal refuse every other target before writing anything. Tree hashing and staging use a temporary index file (`GIT_INDEX_FILE`) in the worktree's git directory. `TestSimulatedRun` (integration) checks that the user's files, `.git/index`, `.git/HEAD`, `refs/heads/<base>`, and `git status` are byte-identical before and after a full run, including uncommitted user changes. |
| No prompts, no hangs | Every call has a timeout (default 2 min) and runs with `GIT_TERMINAL_PROMPT=0`, empty `GIT_ASKPASS`/`SSH_ASKPASS`, `GCM_INTERACTIVE=never`, `GIT_EDITOR=:`, and `LC_ALL=C`. |
| No repository-controlled programs run | Each call sets `core.hooksPath` to a directory that does not exist, `core.fsmonitor=false`, and passes `--no-pager`. Diffs use `--no-ext-diff --no-textconv`. Commits are created with `commit-tree` + `update-ref`, so commit hooks never apply. `TestHooksDoNotRun` installs hooks and checks that none ran. External filters: see [Git filters](#git-filters). |
| No secret in the reflog | Run-branch commits write the fixed reflog message `aco: commit` (checkpoints: `aco checkpoint`); the commit subject never reaches the reflog, which is not masked. `TestCommitMessagesAreMasked` puts a registered fake secret in the subject and checks the commit message and the branch reflog. |
| Secrets do not reach Git | Every variable that `secure.IsSecretVar` flags is removed from the child environment; `GITHUB_TOKEN`/`GH_TOKEN` are kept only for `Push` and `OpenPR`. Variables that redirect Git (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, …) are removed. Commit messages, tag messages, notes, and stderr in errors are masked with the run's `secure.Redactor`. Errors name the git subcommand only, never its arguments. |
| Commits contain exactly the reviewed tree | See [Reviewed-tree commits](#reviewed-tree-commits). |
| No force | `Push` never forces and uses `--no-verify`; tags are never moved (`ErrTagExists`). |

### Git filters

`.gitattributes` can select a `filter=<driver>` whose `clean`, `smudge`, or `process` command Git would run while
staging, checking out, refreshing the index, computing status, and even writing trees. ACO never runs them:

- At the start of every operation, ACO reads the configured drivers (`git config --get-regexp '^filter\.'`, all
  scopes, includes honoured) and passes `-c filter.<driver>.clean=`, `.smudge=`, and `.process=` on **every** git
  call of that operation. The overrides also reach Git's child processes, such as the checkout inside
  `worktree add`. With an empty command Git stores and checks out the raw bytes, as if the filter did not exist;
  the other attribute handling (`text`, `eol`, `ident`, `working-tree-encoding`) stays Git's normal built-in
  behaviour.
- A driver marked `filter.<driver>.required=true` (for example Git LFS) cannot be skipped safely, because the raw
  file would be committed instead of what the filter produces. Git then fails, and ACO returns
  `ErrFilterUnavailable`. A failed `AddWorktree` removes its partial worktree and branch.
- A driver name that cannot be written as a `-c` key (it contains `=` or a line break) gives `ErrUnsafeFilterName`.

`TestExternalFiltersNeverRun` configures clean, smudge, and process filters that write a marker file, then drives
worktree creation, working-tree hashing, checkpoints, status, reset in both directions, a reviewed commit, and the
Stage 4 worktree. The marker must not appear, and a control step proves plain Git runs the same filter.
`TestRequiredFilterFailsClearly` covers the required case.

Limitations: a repository that needs a required filter (Git LFS) cannot be used by ACO in v1. The filter
configuration is read once per operation, not before each git call.

### Worktree ownership

A `Repo` from `gitops.Open` is the user's side. It can read, create and remove ACO worktrees, and write shadow refs,
tags, and notes, but `Commit`, `CommitReviewed`, and `ResetTo` refuse it with `ErrNotOwnedWorktree`.

`AddWorktree` (run worktree on the run branch) and `AddDetachedWorktree` (Stage 4 verification worktree) write a
marker file `aco-worktree` into the new worktree's private git directory (`<common>/worktrees/<name>/`) and return an
owned `Repo`. After a restart, `OpenWorktree(ctx, path, branch)` (`branch` empty for a verification worktree) returns
an owned `Repo` only if the checks below pass.

Before writing anything, every commit, reset, and removal re-checks, against Git and the file system:

1. the `Repo` has an allowed role: commits need a run worktree; reset accepts run and verification worktrees;
2. Git reports the directory as the top of a **linked** worktree, not the main checkout;
3. its common git directory is the repository it was created or opened from (compared by file identity, so case,
   symlinks, and short names do not matter);
4. the worktree's private git directory holds ACO's marker for exactly that role and branch;
5. for commit and reset: HEAD is on the run branch (run worktree) or detached (verification worktree).

A commit then advances only the recorded run branch, with a compare-and-swap. `RemoveWorktree(ctx, wt)` takes the
owned `Repo`, checks 1–4 (HEAD may have moved), refuses the caller's own directory and the main checkout, and only
prunes metadata if the directory is already gone. The user's checkout, a worktree the user created, a worktree of
another repository, and a run worktree whose HEAD left the run branch are all refused.
`TestMutationsRefuseUnownedTargets` tries each of them, including hand-forged `Repo` values, and checks that the
user's files, index, HEAD, base branch, the user's own worktree, and its branch stay byte-identical.

Limitation: ownership relies on agents not writing inside `.git` (forbidden, and enforced by the guard hook in
Phase 12); a process that can write `.git` could forge the marker.

The file `internal/gitops/exec.go` reads the process environment to build the scrubbed child environment, so it is on
the env allowlist of the security guard test.

## API (`internal/gitops`)

`gitops.Open(dir, Options)` returns a `*Repo` for a repository or worktree. `Options` holds the git binary, the
per-call timeout, the commit `Identity` (empty fields use the repository's own config), the `Redactor`, and extra
environment variables (tests use them to isolate Git config).

| Area | Functions |
|---|---|
| Preflight | `Preflight(ctx, base, worktreePath)`: git ≥ 2.30, not bare, base branch resolves (empty = current branch), no merge/rebase/cherry-pick/bisect in progress, worktree path free. Returns version, root, base branch, base commit. Writes nothing. |
| Run branch | `BranchName(date, planTitle, shortID)` → `aco/<yyyymmdd>-<plan-slug>-<short-id>`; `CheckBranchName`. |
| Worktrees | `AddWorktree(ctx, path, branch, baseCommit)` creates the owned run worktree on a new branch (fails if the branch exists). `AddDetachedWorktree(ctx, path, commit)` creates the owned clean Stage 4 checkout. `OpenWorktree(ctx, path, branch)` reopens one after a restart. `RemoveWorktree(ctx, wt)` removes an owned worktree and prunes metadata; the branch stays. `Repo.Branch()` is the run branch. |
| Working state | `WorkingTree(ctx)`: tree hash of HEAD plus all tracked changes and all untracked, non-ignored files. `IsClean`, `Head`, `CurrentBranch`, `ResolveCommit`, `ResolveTree`, `IsAncestor`, `RefExists`. |
| Checkpoints | `Checkpoint(ctx, ref, msg)` stores the working state as a commit (parent HEAD, not signed, author `ACO Checkpoint`) under `refs/aco/…`. `CheckpointRef(run, phase, attempt, step)` → `refs/aco/<run>/PNN/aK/sM`. |
| Reset | `ResetTo(ctx, rev)` restores the working state to exactly `rev`'s tree: tracked files are overwritten or deleted, untracked non-ignored files are removed, ignored files stay, HEAD does not move, and the index is set back to HEAD. It verifies the result and returns `ErrResetMismatch` otherwise. `ResetTo(ctx, "HEAD")` is the phase restart. |
| Diffs | `Changes(ctx, from, to)`: status (A/M/D/R/C/T), old path, modes, blobs, old and new size, and added/deleted line counts and binary flag from git numstat (these follow `.gitattributes` and are for reports only), with rename detection. `ReadBlob(ctx, blob)` returns a blob's raw bytes (no conversion, filter, or textconv). `Patch(ctx, from, to, context)`, `DiffStat`, `ChangedPaths`. A phase diff is `phase base → WorkingTree()`, an incremental diff is `last reviewed tree → WorkingTree()`, the full run diff is `base commit → HEAD`. |
| Commits | `Commit(ctx, CommitSpec)`, `CommitReviewed(ctx, paths, expectedTree, extraFiles, msg)`, `TrailersOf`, `FindCommitByTrailer`. |
| Messages | `FreezeMessage(FreezeInfo)` (§10.5), `PhaseMessage(PhaseInfo)` (§11.8), `PhaseAttemptKey`. |
| Tags | `Tag(ctx, name, commit, msg)` (annotated; same commit again is a no-op), `TagName(run, "plan-v1")`, `PhaseTag(run, n)` → `aco/<run>/phase-NN`. |
| Notes | `AddNote(ctx, commit, data)` / `Note(ctx, commit)` on `refs/notes/aco`. |
| Blocked phases | `SaveBlocked(ctx, run, phase, reason)` stores the work in progress in `refs/aco/<run>/blocked/PNN`; the run branch does not change. |
| Pruning | `ShadowRefs(ctx, run)`, `PruneRefs(ctx, run, keepBlocked)` deletes a run's shadow refs in one `update-ref --stdin` transaction. Branches, tags, and notes stay. |
| Push / PR | `Push(ctx, remote, branch, tags)`; `OpenPR(ctx, ghBin, PRRequest)` runs `gh pr create … --body-file -`. Both are used only when `git.push` / `git.open_pr` are enabled (off by default). |

Paths passed to commits are repository-relative and slash-separated. Absolute paths, `..`, backslashes, drive
letters, unclean paths, and any `.git` segment are rejected with `ErrBadPath`.

## Reviewed-tree commits

At review time the engine records `reviewed := WorkingTree()` and the changed paths
(`ChangedPaths(phaseBase, reviewed)`). After the review passes, it writes the Phase Completion Report and calls
`CommitReviewed(paths, reviewed, reportFiles, msg)`:

1. The paths are staged on top of HEAD in a temporary index. The result must equal `reviewed`, else `ErrTreeMismatch`
   (a reviewed file was edited after review, or a reviewed path is missing from the list).
2. The paths plus the report files are staged. The result must equal `reviewed` + the report files, else
   `ErrTreeMismatch`.
3. The whole working state (plus the report files, even if ignored) must equal that same tree. Any other change,
   including a new untracked file, gives `ErrUnreviewedChange`.
4. The commit is created with `commit-tree` (honouring `commit.gpgSign`) and the branch is advanced with a
   compare-and-swap `update-ref`; if the branch moved (or `ExpectedHead` does not match), `ErrBranchMoved`.
5. The worktree's index is reset to the new HEAD; files are not touched.

The freeze commit uses `Commit` with `RequireExact: true`: only the plan files, the lock, and AGENTS.md may differ
from HEAD.

Before committing, the engine calls `FindCommitByTrailer(ctx, "HEAD", "ACO-Phase-Attempt", PhaseAttemptKey(…))` so
that a resumed run never commits the same attempt twice (§14.2).

### Commit messages

Freeze commit:

```
docs(aco): freeze implementation plan v1

Frozen plan package and AGENTS.md after independent plan review.

ACO-Run: <run-id>
ACO-Plan-Hash: <hash>
ACO-Plan-Review-Rounds: <n>
Reviewed-by: <model>
```

Phase commit:

```
feat(phase-NN): <title>

<summary>

| AC | Status | Criterion |
|---|---|---|
| AC-NN.1 | met | … |

Review rounds: <n>

ACO-Run: <run-id>
ACO-Phase: NN
ACO-Phase-Attempt: <run-id>/PNN/aK
ACO-Plan-Hash: <hash>
ACO-Reviewed-Tree: <tree>
ACO-Review-Rounds: <n>
Reviewed-by: <model>
Co-Authored-By: Claude <noreply@anthropic.com>
```

Newlines in the subject and in trailer values are folded into spaces, so a value cannot add a trailer. `|` in AC
cells is escaped. Tests parse the trailers with `git interpret-trailers --parse`.

## Diff policy (`internal/policy`)

`policy.FromConfig(cfg.Policy, phase.Scope.Paths, redactor)` builds a `policy.Config`. `policy.CheckDiff(ctx, repo,
from, to, cfg)` reads the diff from Git (`Collect`, below) and returns a `Result` with sorted `Finding`s
(`code`, `path`, `line`, `rule`, `blocking`, `message`). `Result.Rejected()` is true when any finding blocks.

Findings never contain the offending text, only its location and rule, so a detected secret is not copied into
reports, prompts, or events.

### How the diff is read

`Collect` takes the changed paths from `git diff-tree` (rename detection on). It then reads the raw old and new blob
of each changed file with `ReadBlob` and compares them with its own line diff (`DiffLines`, Myers). Git's patch
output is not used, so:

- `.gitattributes` cannot hide lines: a text file marked `-diff`, or given a diff driver that is marked binary, is
  still scanned line by line. `TestGitattributesCannotHideAddedLines` changes `.gitattributes` to `*.go -diff`, adds a
  fake AWS key and a `t.Skip`, and keeps `binary_files: warn`. Git's own patch shows "Binary files differ", but both
  findings are reported as blocking, and the `binary_file` finding stays a separate warning.
- Nothing is dropped: lines have no length limit (`TestLongLineBeforeLaterChange` puts a 3 MiB line before a later
  fake key), any read error fails `Collect` and `CheckDiff`, and a file whose old or new blob is larger than
  `policy.MaxScanBytes` (64 MiB) is not scanned and gets a blocking `scan_incomplete` finding.
- Added lines keep diff semantics: a line counts as added when it is in the new version but not matched by a
  shortest edit script, numbered by its line in the new file. A line that only moved is reported as removed and
  added, as in `git diff`. Beyond 1000 edits between the common prefix and suffix, every line in between counts as
  added, which can only add findings.
- `binary` is true when git numstat reports the file binary (this follows `.gitattributes`) or the new content has a
  NUL byte. It only drives the `binary_files` check; binary content is still scanned for secrets.

| Code | When | Blocks |
|---|---|---|
| `protected_path` | a changed path (old or new) matches `policy.protected_paths`, case-insensitively | always |
| `scope_violation` | `scope.paths` is set and a changed path matches none of them | `policy.scope_enforcement: block` |
| `file_too_large` | an added or changed file is larger than `policy.max_file_bytes` (default 5 MiB) | always |
| `binary_file` | an added or changed file is binary | `policy.binary_files: fail` |
| `secret_file` | file name such as `id_rsa`, `*.pem`, `*.key`, `*.p12`, `.env`, `.env.*` (not `.example`/`.sample`/`.template`/`.dist`), `.netrc`, `.npmrc`, `credentials.json` | always |
| `secret_detected` | an added line matches a credential rule (below) | yes, except `generic_assignment` |
| `test_deleted` | a test file is deleted, or renamed to a non-test name | `policy.anti_gaming: fail` |
| `test_skipped` | an added line skips or focuses tests | `policy.anti_gaming: fail` |
| `lint_suppressed` | an added line suppresses a linter or security finding | `policy.anti_gaming: fail` |
| `tests_missing` | source files changed and no test file or test data was added or changed | `policy.tests_missing: fail` |
| `scan_incomplete` | a changed file is larger than `policy.MaxScanBytes` (64 MiB), so its content was not scanned | always |

Secret rules: `aws_access_key` (`AKIA…`, `ASIA…`), `aws_secret_key`, `private_key` (`-----BEGIN … PRIVATE KEY-----`),
`anthropic_key`, `openai_key`, `github_token`, `slack_token`, `google_api_key`, `stripe_key`, `registered_secret` (a
value registered with the run's redactor that matches no pattern), and `generic_assignment` (a long literal assigned
to `password`, `token`, `api_key`, …; obvious placeholders are skipped; reported for review, not blocking).

Anti-gaming rules: `go_skip` (`t.Skip`, `t.SkipNow`, `t.Skipf`), `pytest_skip` (`@pytest.mark.skip`, `skipif`,
`xfail`, `pytest.skip(`), `unittest_skip`, `js_skip` (`it.skip`, `describe.skip`, `test.todo`, `xit`, …), `js_only`
(`.only`, `fit`, `fdescribe`), `junit_disabled` (`@Disabled`, `@Ignore`), `rust_ignore` (`#[ignore]`),
`eslint_disable`, `nosec` (`#nosec`), `nolint`, `noqa`, `type_ignore`, `ts_ignore` (`@ts-ignore`, `@ts-nocheck`,
`@ts-expect-error`), `nosemgrep`, `lint_ignore_staticcheck`.

Test files are recognised by name (`*_test.go`, `test_*.py`, `*_test.py`, `*.test.ts`, `*.spec.js`, `*Test.java`,
`*_spec.rb`, …) or as source files inside `test/`, `tests/`, `__tests__/`, `spec/`. Files under `testdata/`,
`fixtures/`, `__snapshots__/`, and `*.snap`/`*.golden` count as test changes for `tests_missing`.

### Path patterns

`policy.Match(pattern, path, fold)` is used for protected paths and `scope.paths`. It follows `.gitignore`:

- A pattern without `/` (`AGENTS.md`, `*.pem`, `.env*`) matches a path segment at any depth: a file of that name, or
  everything inside a directory of that name.
- A pattern with `/` is anchored at the repository root (a leading `/` is ignored). `**` matches zero or more
  segments; `*`, `?`, and `[…]` match within one segment.
- A pattern that matches a directory also covers everything inside it (`internal/gitops` covers
  `internal/gitops/x.go`).

Protected paths match case-insensitively, so `.ACO/plan/x.md` is still protected on a case-insensitive file system.
`scope.paths` match case-sensitively.

## Configuration

New fields in Phase 4 (see [config.md](config.md)):

```yaml
policy:
  max_file_bytes: 5242880   # > 0
  binary_files: warn        # warn | fail
```

## Tests

- Unit tests (`go test ./internal/gitops/ ./internal/policy/`) use real Git in temporary repositories with isolated
  config (`GIT_CONFIG_GLOBAL`, `GIT_CONFIG_NOSYSTEM=1`). They cover preflight, branch names, the temporary index,
  checkpoint and reset, reviewed-tree commits and every refusal, trailers, tags, notes, duplicate detection, blocked
  refs and pruning, the Stage 4 worktree, environment scrubbing, hooks, external filters (including required
  ones), worktree ownership refusals, timeouts, masking (commit message and branch reflog), and every policy rule.
  The policy tests also cover `.gitattributes` hiding lines, long lines, the line diff (table cases, 300
  randomized edit scripts checked for completeness, the edit limit), and unscanned files.
- `go test -tags=integration ./internal/gitops/...` runs `TestSimulatedRun`, the end-to-end run described above.
