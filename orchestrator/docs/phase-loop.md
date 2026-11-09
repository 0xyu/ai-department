# Stage 3: the phase engine

Phase 9 implements one complete phase of §11 in `internal/engine` (`RunPhase`) and the Phase Completion Report in
`internal/reports`. `aco run` (Phase 10) calls `RunPhase` for each phase in the frozen order; Phase 9 adds no CLI
command.

```go
out, err := engine.RunPhase(ctx, engine.PhaseDeps{Store, Run, Implementer, Reviewer, Worktree, Config, Owner, …},
	engine.PhaseInput{RunID: id, Phase: n})
```

The run must be `executing` and the phase `pending` (or `initializing` after `aco resume --retry`). The outcome says
whether the phase was **committed** or **blocked**; when the run stopped first (subscription limit → `paused`; auth,
API quota, budget, run-level usage limit → `failed`; cancellation → `cancelled`), `Status` is empty and `RunStatus`
says what happened. Only system problems (state store, Git, file system) return an error; the run is then failed
with class `internal`.

## Flow

```
pending ─▶ initializing ─(checks fail)─────────────────────────────────────────▶ blocked (init_check_failed)
              │ checks pass, baseline recorded
              ▼
          implementing ─▶ quality_gates ─(red)─▶ fixing (gate fix) ─▶ quality_gates …
              (fresh session)   │ green              │ environment ─▶ blocked (environment)
                                ▼
                            reviewing ─(gate FAIL)─▶ triage ─▶ fixing (review fix) ─▶ quality_gates ─▶ reviewing …
                                │ PASS                 │ unsafe / spec_conflict / environment ─▶ blocked
                                ▼
                            commit_gate ─(tree changed)─▶ reviewing
                                │ pass                 │ policy / plan lock ─▶ blocked
                                ▼
                            committed

any guard trips ─▶ attempt_exhausted ─▶ initializing (restart, new session, lessons) ─▶ … ─▶ blocked after the restart limit
```

Every status change goes through the phase state machine of Phase 3 (`state.TransitionPhase`); where the machine has
no direct edge the engine takes the legal path (for example `quality_gates → fixing → attempt_exhausted`,
`reviewing → triage → blocked`).

## Initialization (§11.3)

1. **Plan integrity** — `plan.lock.json` is read and every frozen file (plan package and `AGENTS.md`) is verified;
   the lock's plan hash must equal the run's frozen plan hash.
2. **Git baseline** — the worktree is clean, `HEAD` is the previous phase's commit (the freeze commit for phase 1),
   `HEAD` is on the run branch, and the branch tip is `HEAD`.
3. **Dependencies** — every `Depends On` phase is `committed`, its commit is an ancestor of `HEAD`, and its Phase
   Completion Report exists and validates against the schema.

Any failure in 1–3 blocks the phase with `init_check_failed` (all problems in the detail) and **no agent is called**.
Then the gates are resolved (`gates.Resolve`, phase → config → auto-detect); a required gate without a command blocks
with `environment`.

4. **Baseline** — the gate results stored for `HEAD` (`baselines/<sha>.json`, written by the previous phase's commit)
   are the regression baseline. When there are none (phase 1) all gates run once on `HEAD` (`baseline` step) and the
   result is stored, so pre-existing failures are not blamed on the phase. That round is accepted only as evidence for
   the clean base tree (the tree of `HEAD`): a stale round (see gate evidence below) or one on another tree is not
   saved, since a failure it saw could later pass as pre-existing. The worktree is never reset; the round is retried
   up to 3 times, then the phase blocks with `init_check_failed` before the implementer is called.
5. **Fresh session** — the first implementer call of every attempt is `Implementer.Start`; nothing is resumed from
   an earlier phase or attempt. The session ID is recorded per attempt.
6. **Context** — the frozen `IMPLEMENTATION_PLAN.md`, `phases/PNN.md`, and `AGENTS.md` are read from the worktree
   **byte for byte** and passed verbatim (`ImplementRequest.PlanOverview`, `PhaseSpec`, `AgentsMD`), plus the digests of
   all committed earlier phases (from their reports), the resolved gate commands, the AC IDs, and the scope paths.

## Gates, review, triage, fix loop (§11.5–§11.7)

- **Gates** run fail-fast on the current tree and are compared with the baseline. Results are stored per round
  (`gate_results`), `gate.started`/`gate.completed` are emitted per gate, and every regression seen in any round is
  remembered for the review pack and the report.
- **Gate evidence is bound to one tree.** A round counts only when every result records the round's tree, no gate
  changed the tree while it ran (`TreeAfter`: the runner checks the tree after every command of a gate, so a command
  that changes it and a later one that restores it are still seen), and the working tree is still the round's tree
  when the round ends
  (`evidenceTree`). Otherwise the round is **stale** (another writer, or a gate that changes the tree, even one that a
  later gate restores): it is recorded (`stale: true` on the step and on `gate.completed`) but never authorizes review
  or commit, and the gates run again on the new candidate. A stale round counts as a gate-fix attempt, so a tree that
  keeps moving ends the attempt by `limits.gate_fix_attempts`. Before each review the engine checks again that the
  accepted round is for the tree it is about to review.
- A red round goes back to Claude (`Continue`, kind `gate_fix`) with each failing gate's summary, regressions, new
  issues, and trimmed log (IDs `G-<gate>-<round>`). A gate that fails for an **environment** reason (command not found,
  service unreachable, or a gate ACO could not run) blocks at once. **No review is spent on a red tree.**
- **Review** (green tree only): the context pack has the plan overview, phase list (done/current/future), the spec and
  `AGENTS.md` verbatim, the full diff from the phase base, the incremental diff since the last review, all gate
  results, the baseline comparison, test changes, regressions (current and earlier in the phase), security gate and
  diff-policy findings, files importing changed Go packages and tests next to changed code, the implementation report
  (unverified), and on re-review the ledger (open and closed findings with Claude's answers).
- **Finding ledger** — the engine assigns `F-<phase>-<seq>` (unique across attempts). Re-review statuses: `resolved`;
  `unresolved` (reopened when Claude said `fixed` or the finding was closed); `partially_resolved`; `dispute_rejected`;
  `dispute_accepted` closes a disputed finding — but when Claude answered `cannot_fix_within_spec` or
  `cannot_fix_safely`, an accepted dispute means the reviewer **confirms** it cannot be fixed, and triage blocks. A new
  finding with the fingerprint (category, file, title) of an existing one updates that one instead: it keeps its ID
  and history (opened round, reopen and dispute counts, Claude's answer) but takes the reviewer's current severity,
  proposed class, and details, and is reopened if it was closed or deferred. Triage then judges it as it is now, so a
  deferred minor that comes back as an in-scope major is fixable; the scope-creep downgrade is decided again for the
  current severity (it only applies to a finding first raised after the cutoff round).
- **Scope creep** — after round `limits.new_blocker_cutoff_round`, a new blocker/major that is not security or
  regression and does not touch a line changed since the last review is downgraded to minor and deferred.
- **Review Decision Gate** (`DecideReviewGate`, pure): quality gates green; `plan_conformance = conforms`; every AC `met`
  (`not_verifiable` only with `policy.allow_unverifiable_ac`); no open blocker/major (security ones reported as such);
  regression risk not `high` unless every high-risk item has passing test evidence (below); no open or deferred
  blocker/major except `out_of_scope`; no open finding of a blocking class (`unsafe`, `spec_conflict`, `environment`) at
  any severity; every previous finding has a status. The verdict is recorded, never used: `approve` with a failing gate
  emits `review.inconsistent`.
- **Regression evidence** — `regression_assessment.items` lists each risk with its own level and the tests that cover
  it. The risk is high when the assessment or any item says so; then there must be at least one high-risk item, and
  every high-risk item must cite at least one test. Each cited test must name exactly one test of the accepted round —
  by its full identity `package.TestName` (`gates.TestCase.ID`), or by a bare test name that identifies one test — and
  every result for it must be `pass`. A missing, ambiguous, skipped, or failing citation keeps the gate red
  (`regression_risk_high:item-N`) and becomes a work item. Free-text notes are never evidence, and a prefix or
  substring of a test name does not match. The review pack lists each gate's test identities so the reviewer can cite
  them. A review without `items` (written before the field existed) parses with no evidence.
- **Triage** (`ClassifyFinding`, pure) on every open finding, first match wins: confirmed `cannot_fix_safely` →
  `unsafe`; confirmed `cannot_fix_within_spec` → `spec_conflict`; downgraded, `out_of_scope`, minor, or nit →
  `deferred`; file or a path in `required_change` matching a protected path → `unsafe`; security finding still open
  after 2 re-reviews → `unsafe`; otherwise the reviewer's class (`spec_conflict`, `unsafe`, `environment`) or
  `fixable`. A reviewer-proposed `deferred` on an in-scope blocker or major (security included) is overridden to
  `fixable`: only out-of-scope, minor/nit, and downgraded findings are deferred, and the review gate re-checks that.
  Any `unsafe` / `spec_conflict` / `environment` blocks immediately (priority in that order).
- **Fix** — fixable findings go to Claude in the attempt's session (`review_fix`), blockers → majors, and within a
  severity security → regression → others. Gate reasons that are not findings (AC not met, plan deviation, high
  regression risk) are added as work items `R-<phase>-<round>-<n>`. Claude's answers are stored on the ledger.

### Convergence guards

| Guard | Trips when | Class |
|---|---|---|
| Policy | guard-hook denials in the attempt ≥ `limits.policy_violations`, or a reviewer confinement breach | `policy_violations` |
| Oscillation | a finding reopened `limits.max_reopen` times | `oscillation` |
| Dispute deadlock | the same finding disputed and rejected twice | `dispute_deadlock` |
| No progress | two fix rounds in a row without a tree change (disputes excepted; an accepted dispute resets) | `no_progress` |
| Review rounds | `limits.review_rounds` failed reviews | `iteration_limit` |
| Gate fixes | `limits.gate_fix_attempts` consecutive red rounds | `iteration_limit` |
| Agent incomplete | no usable report after the continue/retry limit, or a malformed review | `iteration_limit` |
| Usage | `limits.agent_calls.phase`, `limits.claude_turns.phase`, or `limits.phase_timeout` in the attempt (checked before each call) | `phase_usage_limit` |

A tripped guard ends the attempt (`guard.tripped`, `phase.attempt.exhausted`). Up to `limits.phase_restarts` times the
worktree is reset to the phase base and a new attempt starts with a fresh session and a **lessons brief** (why the
attempt ended, open findings, failing gates, known gaps). After that the phase is blocked with the guard's class.
Run-level `limits.agent_calls.run` and `limits.claude_turns.run` fail the run (`usage_limit_exceeded`); the run counts
include earlier stages (from the `invocations` table).

### Agent errors (§14.1)

| Error | Implementer | Reviewer |
|---|---|---|
| incomplete / max turns | `Continue` (kind `continue`) in the same session, max 2; a report that only misses answers is accepted | — |
| crash / timeout | reset to the step's checkpoint, retry, max 2 | retry, max 2 |
| transient / rate limit | retry, max 2 | retry, max 2 |
| still failing | attempt ends (`agent_incomplete`) | attempt ends |
| schema invalid | — | attempt ends |
| confinement breach | — | attempt ends (`policy_violations`), `policy.violation` |
| subscription limit | run paused, `resume_at` = reset + 2 min (15 min when unknown) | same |
| auth / API quota / budget | run failed (`auth`, `api_quota`, `budget_exceeded`) | same |

## Commit Gate (§11.8)

After a PASS no agent runs again. `DecideCommitGate` (pure) gets values the engine recomputes now:

- current tree and changed paths vs. the reviewed ones; the tree the gate evidence is bound to (`evidenceTree`, empty
  for a stale or mixed round) vs. the reviewed tree — any difference → **back to reviewing** (fresh gates first, then a
  new review round);
- `plan.lock.json` still verifies — otherwise blocked `plan_integrity`;
- diff policy on the final diff (protected paths, secrets, scope with `block`, tests missing with `fail`, anti-gaming,
  file size) — a blocking finding → blocked `commit_gate_blocked`;
- gates green, review passed for this tree, no open blocker/major, no blocking triage class — otherwise blocked
  `commit_gate_blocked`.

Then the engine writes `.aco/reports/phase-NN.{md,json}`, checks there is no commit with the same `ACO-Phase-Attempt`
trailer, and commits through `gitops.Commit` with the reviewed tree: exactly the reviewed paths plus the two reports, refusing
any other change or untracked file. The commit is bound to the exact report bytes (`ExtraData`): Git re-reads the
report files when it stages them, so before the branch moves each must be in the final tree as a regular,
non-executable file whose blob is exactly what the engine generated and validated, the same bytes as the note and the
`reports` digest (`ErrExtraMismatch` otherwise). Its parent is pinned to the phase's validated base (`ExpectedHead`),
in addition to the ref compare-and-swap, so a commit made on the run branch after the gates is never built on. The commit is `feat(phase-NN): <title>` with the summary, the AC table, the review rounds,
and the trailers `ACO-Run`, `ACO-Phase`, `ACO-Phase-Attempt`, `ACO-Plan-Hash`, `ACO-Reviewed-Tree`, `ACO-Review-Rounds`,
`Reviewed-by`, `Co-Authored-By`. Then the tag `aco/<run>/phase-NN`, a `git notes --ref=aco` note (report JSON + commit
SHA + trees), the `commits` and `reports` rows, the gate results stored as the next phase's baseline under the new
commit, and `phase → committed`.

**Worktree drift during staging.** If the worktree changes after the Commit Gate passed, the commit is refused
before the branch moves (`gitops.ErrTreeMismatch` for a reviewed path that changed, `gitops.ErrUnreviewedChange` for
any other change or untracked file, `gitops.ErrExtraMismatch` for a report file that no longer holds the generated
bytes). The engine then checks that `HEAD` has not moved, finishes the commit step as failed with outcome
`retry: worktree_drift`, and puts back the two report files as they were before this commit try — but only a file
that still holds exactly what this try wrote (bytes and mode); a report someone else replaced or removed is left as it
is, like every other worktree edit. It emits `commit_gate.failed` with reason `worktree_drift`,
and sends the phase back through fresh gates and an independent review of the new candidate; the Commit Gate and its
policy re-check then run again. Each drift counts as a failed review round (`limits.review_rounds`). Each commit try
has its own step key (`P01/a1/commit`, `P01/a1/commit-2`, …). A replaced report is a change to a protected path
(`.aco/**`), so after the re-review the Commit Gate's policy re-check blocks with `commit_gate_blocked`. A moved branch
(`ErrBranchMoved`, including a same-tree commit made between the gate and staging) or any failure after the branch
advanced (tag, note, store) is not drift: it fails the run with no phase commit.

## BLOCKED

Nothing is committed to the run branch. The working state is saved to `refs/aco/<run>/blocked/PNN`, the Blocked
Report `reports/phase-NN-blocked.md` (artifact; `reports` row of kind `blocked`) lists the class, detail, attempt, WIP
ref, what a human must decide, open findings with Claude's answers, the last gate round with log paths, the tripped
guard, and the resume options. The phase and the run become `blocked` with reason `{class, stage: execution, phase}`.

## Phase Completion Report (§11.9)

`.aco/reports/phase-NN.json` follows `schemas/phase-report.schema.json` (format `aco.phase-report/v1`; every object
closed and all fields required); `phase-NN.md` renders the same content. Contents: phase, title, goal, plan version and
hash, base commit, reviewed tree, attempt / attempts, Claude session IDs per attempt, models; AC table (text, final
status, evidence from the final review, covering tests from the implementation report); requirements; files with
status and line counts, diff stat; per-gate status, commands, duration, summary, and baseline delta; regressions on the
committed tree and regressions seen during the phase; security scanners, new and pre-existing findings, diff-policy
findings; review history (rounds, verdicts, gate result, new findings, gate reasons); every finding of every attempt
with class, status, Claude's answer, the reviewer's ruling, reopen and dispute counts; deferred items; implementation
summary, decisions, known gaps, reported tests, and the report's claims the diff does not confirm; usage per agent
role (auth mode, calls, turns, tokens, USD — marked as an estimate unless charged in api mode), duration; and the
**digest** (≤ 6000 characters) that later phases receive. The commit SHA is not in the report (it is known only after
the commit); it is in the note, the state store, and later reports.

Reports are masked with the run's redactor before they are written. `reports.ReadPhase` refuses links and non-regular
files and checks the phase number.

## Steps, checkpoints, artifacts (§14.2)

Every agent call is a step: checkpoint `refs/aco/<run>/PNN/aK/sM` before, intent (kind, model, auth mode, tree,
session; owner = run lock owner), invocation row (step ID), the call, checkpoint after, outcome (tree, checkpoint,
session, error class). Gate rounds (`gates`, `baseline`) and the commit are steps too (intent before acting).

| Path (run artifacts) | Content |
|---|---|
| `phases/PNN/aK/baseline/` | baseline gate logs |
| `phases/PNN/aK/steps/sSSSS-<kind>/` | prompts, transcripts, reports of each agent step |
| `phases/PNN/aK/gates/rRR/` | gate logs and `round.json` |
| `phases/PNN/aK/reviews/rRR/` | context pack, `review.json`, `gate.json` (decision + triage), `commit-gate.json` |
| `phases/PNN/aK/diffs/rRR/` | `full.patch`, `incremental.patch`, `stat.txt` |
| `baselines/<sha>.json` | gate results per commit (regression baseline) |
| `reports/phase-NN-blocked.md` | Blocked Report |

## Tests

- Pure functions with exhaustive tables: `DecideReviewGate`, `ClassifyFinding` / `Triage`, `ScopeCreep`,
  `DecideCommitGate`, `CheckGuards`, the ledger (`Apply`, reopen, disputes, confirmation, attempts), `FixOrder`.
- Offline scenarios (fake implementer, reviewer, and gate runner; real Git and state store), AC-09.1:
  1 happy path (commit content, trailers, tag, note, report, steps; phase 2 with a new session and the digest),
  2 dependency not committed, 3 `AGENTS.md` changed, 4 lint red twice, 5 integration regression, 6 fix and re-review,
  7 approve with a blocker, 8 unsafe, 9 confirmed `cannot_fix_within_spec`, 10 oscillation / no progress / round limit
  with a restart, 11 tree changed after review; plus environment gate, scope creep, subscription pause, and an
  incomplete report.
- `TestPhaseImplementPromptGolden` (AC-09.3): the implement prompt contains the phase spec, `AGENTS.md`, and the plan
  overview verbatim; golden file `internal/engine/testdata/phase-implement.golden.md` (`-update` to rewrite).
- `internal/reports`: schema validation and round trip, schema ↔ Go type sync, invalid reports, link refusal, digest
  bound, Markdown sections (AC-09.2).
- `-tags=integration`: two phases with the real gate runner and Go toolchain, a real `go vet` failure fixed in the
  same session, ordered commits, one baseline round for both phases.
