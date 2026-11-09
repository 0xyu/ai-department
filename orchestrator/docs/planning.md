# Stages 1 and 2: plan creation, review, gate, and freeze

Phase 8 implements the planning stages of §10 in `internal/engine` (`RunPlanning`) and the `aco plan` command.

```
aco plan --repo <path> --requirements <file> [--base <branch>]     # Stage 1 → Stage 2 → freeze
aco plan --repo <path> --plan <file|dir> [--requirements <file>]   # Stage 2 → freeze (Stage 1 skipped)
```

`aco plan` creates a run in mode `plan_only`; it ends `completed` after the freeze. `aco run` (Phase 10) uses the
same pipeline in mode `full`, where the run moves on to `executing` and the phases are already registered.

## Flow

```
preflight ──requirements──▶ planning ─▶ plan_review ─▶ (gate PASS) ─▶ freeze ─▶ completed | executing
          └──── --plan ───────────────▶ plan_review
                                        │ gate FAIL, rounds left
                                        ▼
                                     planning (revise) ─▶ plan_review …
                                        │ rounds used up
                                        ▼
                                     blocked (plan_review_rounds | plan_invalid)
```

1. **Preflight** (`aco plan`): run lock for the repository, `git` preflight (version, base branch, no pending
   merge/rebase, free worktree path), run worktree `~/.aco/worktrees/<run-id>` on branch
   `aco/<yyyymmdd>-<plan-slug>-<short-id>` at the base commit. A preflight problem fails the run
   (`preflight_failed`, exit code 3). The user's checkout is never written.
2. **Stage 1** (requirements given): one Claude Code planner call (read-only profile, §16.2) with the requirement
   document, a repository overview (languages, a bounded file tree, existing tests), the existing `AGENTS.md`, the plan
   format guide (`internal/engine/planformat.md`), and the gate catalogue (configured or auto-detected commands per gate
   type and the available scanners). The planner returns the package as structured output; the **engine** writes it.
3. **Materialize**: the package is written to the worktree's `.aco/plan/` (replacing any earlier version), plus
   `AGENTS.md` when the base commit has none and the planner proposed one. It is then read back from disk exactly as it
   will be frozen, and checked with `plan.ValidateFull` — the same checks as `aco validate-plan` (parse, deterministic
   plan checks, front matter, lock). Problems that only show in the planner's structured form are added. A copy of each
   package version and its check report is kept in the run artifacts (`planning/package-NN/`).
4. **Deterministic checks first**: a package that fails them goes back to Claude with the rendered check results
   and **no GPT call is made**.
5. **Stage 2**: one Codex review (`agent.Reviewer.ReviewPlan`) with the requirements, every package file verbatim,
   the requirement IDs, the repository overview, `AGENTS.md` (existing or proposed), the check results, and — on a
   re-review — every open finding with the planner's answer.
6. **Plan Decision Gate** (`DecidePlanGate`, a pure function with table tests). The reviewer's verdict is recorded but
   never used:

   ```
   PASS ⇔ deterministic plan checks pass
        ∧ every REQ has coverage status "covered"
        ∧ no dimension rated "unsound"
        ∧ no open blocker/major plan finding
        ∧ every previous plan finding has a status
   ```

   Reason codes: `checks_failed`, `not_reviewed`, `requirement_not_in_review`, `requirement_not_covered`,
   `dimension_unsound`, `open_finding`, `previous_finding_without_status`.
7. **Revision**: on FAIL the open findings and the check results go to Claude — `Revise` in the Stage 1 session, or,
   for a supplied plan that has no session yet, one `Create` call that converts and fixes the plan
   (`PlanRequest.Findings`/`CheckResults`, answered with the revision schema). Claude answers each finding `fixed` or
   `disputed`; the answers go back to the reviewer.
8. **Round limit**: every pass through the loop (review or check failure) counts against
   `limits.plan_review_rounds` (default 4). When it is used up the run is **blocked**: `plan_review_rounds` if the last
   package passed the checks, else `plan_invalid`. Nothing is committed; the latest package stays in the worktree, all
   versions and reviews are in the artifacts, and `reports/plan-blocked.md` lists the last gate reasons and the open
   findings.

## Plan finding ledger

Findings get engine-assigned IDs `PF-001…` and are stored in the `findings` table (`scope = plan`) after every change.

| Reviewer status of an earlier finding | Ledger status | Open? | Event |
|---|---|---|---|
| (new finding) | `open` | yes | `finding.opened` |
| `resolved` | `resolved` | no | `finding.resolved` |
| `dispute_accepted` | `dispute_accepted` | no | `finding.resolved` (status `dispute_accepted`) |
| `dispute_rejected` | `dispute_rejected` | yes | `finding.disputed` (status `dispute_rejected`) |
| `unresolved` after the planner said `fixed` | `open`, `reopen_count` + 1 | yes | `finding.reopened` |

A planner answer `disputed` emits `finding.disputed` and increments `dispute_count`. Only open findings are sent for a
status on re-review. Minor findings may stay open: they do not block the gate.

## Freeze (§10.5)

On PASS:

1. `.aco/plan/plan-review.md` — final review summary (reviewer, rounds, dimension ratings, coverage, every finding with
   its final status).
2. `.aco/plan/plan.lock.json` — `aco.plan.lock/v1` with the SHA-256 of every plan file and `AGENTS.md`, plan version
   `v1`, run ID, reviewer model, review rounds, verdict `pass`, timestamp.
3. Freeze commit on the run branch: `docs(aco): freeze implementation plan v1` with trailers `ACO-Run`,
   `ACO-Plan-Hash`, `ACO-Plan-Review-Rounds`, `Reviewed-by`. The changed paths are checked first: anything outside
   `.aco/plan/**` and a generated `AGENTS.md` (an existing one is never changed) is an error. The commit uses
   `RequireExact` and `ExpectedHead` = base commit; the lock is verified again after the commit.
4. Tag `aco/<run-id>/plan-v1` (when `git.tag`), note `git notes --ref=aco` with a JSON record (when `git.notes`).
5. The run's plan hash, a `commits` row (`plan_freeze`), `git.commit` and `plan.frozen` events, and the phases
   (all `pending`) are recorded. The run moves to `completed` (plan-only) or `executing` (full).

From then on `.aco/plan/**` and `AGENTS.md` are protected paths (`policy.protected_paths`); Phase 9 re-verifies the
lock at every phase initialization.

## Agent errors and limits

| Problem | Run ends |
|---|---|
| transient, 429, crash, timeout, max turns, incomplete (planner) | retried up to 2 times, then: incomplete in planning → blocked `plan_invalid`; others → failed `internal` |
| incomplete revision that still carries a package (an unanswered finding) | accepted; the missing answer stays empty for the reviewer |
| subscription usage limit | **paused** with `resume_at` = reset time + 2 min (15 min when unknown); `subscription.limit_reached` |
| auth / API quota / USD budget | failed `auth` / `api_quota` / `budget_exceeded` |
| reviewer read outside its confinement (after the adapter's retry) | blocked `policy_violations`, `policy.violation` event |
| reviewer schema-invalid after retries and fallback | failed `internal` |
| `limits.agent_calls.planning` / `.run` reached | failed `usage_limit_exceeded` (`usage.warning` at `soft_ratio`) |
| context cancelled (Ctrl-C, lock lost) | cancelled |
| store, Git, or file-system error | failed `internal`; the error is returned to the caller |

Every agent call is an `invocations` row (`planner` / `plan_reviewer`) with tokens, cost, session, and transcript, plus
`agent.invocation.started|completed|failed` events. Automatic resume after a pause is Phase 10.

## Artifacts

```
~/.aco/state.db                                state store (all runs)
~/.aco/runs/<run-id>/planning/round-NN/       planner calls (prompt, transcript, settings) — one per call
~/.aco/runs/<run-id>/planning/package-NN/     every package version as written + checks.json
~/.aco/runs/<run-id>/plan-review/round-RR/    context pack, review.json, gate.json, transcripts
~/.aco/runs/<run-id>/reports/plan-blocked.md  only when blocked
~/.aco/worktrees/<run-id>/                    run worktree (frozen plan committed on the run branch)
```

## `aco plan` exit codes

| Code | Meaning |
|---|---|
| 0 | plan frozen (prints phases, rounds, revisions, freeze commit, tag, plan hash) |
| 1 | failed, paused (subscription limit), or the repository is locked by another run |
| 2 | usage, config, or unreadable input |
| 3 | preflight failed |
| 4 | cancelled |
| 5 | blocked (prints the gate reasons and open findings) |

## Tests

- `plangate_test.go`: gate table tests (every reason, sorting, determinism, verdict ignored) and the finding ledger.
- `planning_test.go` (fake planner/reviewer, real Git and state store): first-review pass (AC-08.1); uncovered REQ +
  forward dependency → revision → frozen at round 2 (AC-08.2); review keeps failing → blocked at `plan_review` with no
  freeze commit, latest package and review saved (AC-08.3); supplied Markdown plan skips Stage 1 (AC-08.4); supplied
  plan revised by a converting `Create`; check failure skips the GPT call; `plan_invalid`; agent error classes;
  planner retries; reviewer policy violation and schema failure; agent-call limit; cancellation; linked `.aco`
  refused (junction on Windows).
- `integration_test.go` (`-tags=integration`): full mode through a check failure, a failed review, a dispute, and a
  passing re-review; the frozen package passes `aco validate-plan` checks with a verified lock; the user's checkout is
  byte-identical.
- `live_test.go` (`-tags=live`, `ACO_LIVE_PLAN=1`): real Claude Code + Codex on a toy Go repository (AC-08.5).
- `internal/cli/plan_test.go`: `aco plan --plan` end to end with fakes, blocked exit code, usage and preflight errors.
