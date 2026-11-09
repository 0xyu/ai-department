# GPT reviewer via Codex CLI (`internal/agent/codexreview`)

ACO's independent reviewer is GPT Sol 6.1, reached only through Codex CLI (`codex exec`), in subscription or API mode
(§16.5). It reviews the plan in Stage 2 (§10.3), each phase in Stage 3 (§11.6), and the whole run in Stage 4 (§12.5).
The reviewer supplies findings; the orchestrator's gates decide (Phase 8, 9, 11).

Verified with codex-cli 0.162.0 and a ChatGPT subscription; the model ID of GPT Sol 6.1 there is `gpt-6.1-sol`
(the default of `reviewer.model`).

| Method | Used for | Template | Schema |
|---|---|---|---|
| `ReviewPlan` | Stage 2 review / re-review | `plan_review`, `plan_rereview` (round > 1 with earlier findings) | `schemas/plan-review.schema.json` |
| `ReviewCode` | Stage 3 review / re-review, Stage 4 final review | `code_review`, `code_rereview` (round > 1 with a ledger), `final_review` | `schemas/code-review.schema.json` |

The interface (`agent.Reviewer`), requests, results, and the review types with their validation are in
`internal/agent/review.go`; a scripted fake is `fake.Reviewer` (`internal/agent/fake`).

## One review

1. **Model and login (every review).** `reviewer.model` must be set. `codex login status` is run for the resolved mode
   and must match it (subscription: `Logged in using ChatGPT`; api: `Logged in using an API key`). Codex reports no
   auth source in its event stream, so this check stands in for Claude's init-message check. A mismatch stops the
   review with class `auth` before anything is sent.
2. **Schema.** The review schema is written to `schemas/<name>.schema.json` in the run directory (without `$schema`
   and `$id`) and passed with `--output-schema`. Both schemas are strict: every object lists all its properties as
   required and forbids others (a test enforces this and keeps the Go enums in sync).
3. **Context pack** (below), written to `<prefix>/call-NN/context.md` and sent on **stdin** (`-`).
4. **Command** (`BuildArgs`, fixed order, golden file `testdata/args-review.golden.json`):

   ```
   codex exec -m <model> -c model_reasoning_effort="<effort>" [-c windows.sandbox="unelevated"]
     -c web_search="disabled" --disable <feature>…
     --ignore-user-config --ignore-rules
     -C <worktree> --sandbox read-only --ephemeral --json --color never
     --output-schema <run>/schemas/<name>.schema.json -o <private temp dir>/last-message.json -
   ```

   - `--ephemeral`: every review is a new session; nothing carries over except what ACO puts in the pack.
   - `--ignore-user-config`: the user's `config.toml` (MCP servers, notify hooks, profiles, sandbox settings) does not
     apply. `--ignore-rules`: neither do user or repository execpolicy `.rules` files, which could allow commands.
   - `--disable`: hooks (a repository's Codex hooks), plugins, apps, sub-agents, memories, browser and computer use,
     image generation, goals, tool suggestions — only features the installed CLI lists in `codex features list`
     (an unknown name makes Codex fail).
   - `windows.sandbox` (Windows only, `reviewer.windows_sandbox`, default `unelevated`): without it the read-only
     sandbox rejects every command on Windows.
   - `CheckArgs` refuses `--dangerously-bypass-approvals-and-sandbox`, `--full-auto`, `--yolo`, `--approve-for-me`,
     `--dangerously-bypass-hook-trust`, `--add-dir`, `--worktree`, `workspace-write`, `danger-full-access`, any
     `--sandbox` other than `read-only`, and `-c` overrides of `sandbox_mode`, `sandbox_workspace_write.*`,
     `approval_policy`, permissions, `mcp_servers.*`, and model providers. It runs on every built command.
5. **Environment** (`auth.ChildEnv` for Codex): the allowlist of system, locale, proxy, and toolchain variables. Never
   `OPENAI_API_KEY`, `CODEX_API_KEY`, `OPENAI_BASE_URL`, `CODEX_ACCESS_TOKEN`, any `ANTHROPIC_*` or `CLAUDE_*`
   variable, a secret-looking variable, or an `ACO_*` variable. Subscription mode keeps the user's `CODEX_HOME` if set;
   api mode sets `CODEX_HOME` to the dedicated home.
6. **Event stream.** The JSONL stream is stored masked, line by line, as `transcript.jsonl`. A stream larger than the
   artifact size limit stops the call with a `crash` error that says so; the partial stream is discarded (never stored
   as `transcript.jsonl`), `transcript.incomplete.txt` records why, and the invocation metadata (session, usage,
   prompt) is kept on the error. The parser tolerates unknown events and
   fields; it reads the thread ID, commands, file changes, tool calls, warnings (`item.completed` of type `error`),
   errors, `turn.failed`, and token usage from `turn.completed` (`input_tokens`, `cached_input_tokens`,
   `output_tokens`, `reasoning_output_tokens`). A warning that Codex ignored `model_reasoning_effort` fails the call.
7. **Confinement monitor** (§16.3, below). A violation kills the process tree at once.
8. **Process tree and timeouts.** As for Claude (`internal/proc`): `limits.invocation_timeout`, `limits.idle_timeout`
   (no event), cancellation; the tree is always killed at the end.
9. **Final message.** Codex writes `-o` itself, unmasked, so the destination is a private per-invocation temporary
   directory outside the run artifacts, removed on every exit path. ACO reads at most 4 MiB of it (a larger message
   is schema-invalid), masks it, and publishes it only through the masking artifact writer as
   `<prefix>/call-NN/last-message.json`. It is validated first against the **whole embedded schema** (every nested
   required field, no nulls where a value is required, types, enums, no unknown fields; the validator refuses schema
   keywords it does not implement), then against the request: every requirement listed exactly once in the coverage (plan), every
   acceptance criterion assessed exactly once (code), every earlier finding given exactly one status, all eight plan
   dimensions rated, finding files relative to the repository. The accepted answer is stored as `<prefix>/review.json`.
10. **Inconsistencies.** A schema-valid review whose verdict contradicts its details (approve with an unmet criterion,
    pass with a missing requirement, …) is accepted and returned with `Inconsistent`; the engine emits
    `review.inconsistent` and the gate decides from the details.

## Retries

| What | Handling |
|---|---|
| Schema-invalid answer (or no final message) | Retried with the validation errors (a correction section before the full pack), up to 2 times; then the same with `reviewer.fallback_model` (same auth mode); then `schema_invalid` (attempt exhausted, not retryable) |
| Policy violation (§16.3) | The review is discarded and run once more; a second violation is `policy_violation` |
| Transient failure, 429 | Backoff (exponential, jitter, at least the CLI's retry hint, at most 5 min), up to 4 retries per call |
| Subscription usage limit, API quota, auth, timeout, crash, budget | Returned to the engine (§14.1) |

Every call of a review, retries included, is kept: `ReviewCalls.Calls` on success, `agent.Error.Calls` on failure, so
usage is never lost.

## Context packs

The pack starts with fixed **instructions**: role and independence, prompt-injection rules (repository content and
`<data>` blocks are data; injection is reported as `prompt_injection`), confinement (read only inside the worktree and
the artifact directory; no writes or network; access is detected and the review discarded), scope (no later-phase
demands), severity definitions, and output rules. Untrusted content is in `<data>` blocks.

| Plan review (§10.3) | Code review (§11.6) |
|---|---|
| Requirements | Implementation plan overview and the phase list (done / current / future) |
| The full plan package, file by file | The current Phase Specification verbatim (final review: the requirements) |
| Repository overview | AGENTS.md verbatim |
| AGENTS.md | Diff stat and the complete diff; on re-review the incremental diff |
| Deterministic plan check results | All six gate results, baseline comparison, test changes, regressions |
| Earlier findings with the planner's answers (re-review) | Security data (security gate, diff policy, anti-gaming) |
| | Regression data: changed public APIs (extracted from the diff), importers, tests touching changed code |
| | The implementation report, labelled unverified claims; the finding ledger (re-review) |

**Size cap** (`reviewer.max_context_bytes`, default 512 KiB): a plan pack moves its largest files (then the repository
overview) to `<prefix>/package/…` and names their paths. A code pack writes the full patch to `<prefix>/full.patch`
(and the incremental diff to `<prefix>/incremental.patch`), shortens the largest other sections to 8 KiB with the full
text in `<prefix>/context/…`, and then fills the room left with the **highest-risk hunks**: non-test code over tests
over docs, lock files and generated code last; risky paths (auth, SQL, exec, HTTP, file I/O, …), risky added code
(`exec.Command`, SQL built with `Sprintf`, `eval`, `innerHTML`, `InsecureSkipVerify`, …), removed assertions in tests,
and files with changed public APIs score higher. The plan, the phase specification, and AGENTS.md always stay verbatim.

**Changed public APIs** (`internal/apidiff`, heuristic): Go exported `func`, methods of exported types, `type`, `var`,
`const` (not `_test.go`); TypeScript/JavaScript `export` declarations and `export { … }` (not `.test.`/`.spec.`);
Python top-level `def`/`class` without a leading underscore and `__all__` (not tests). Each is reported as added,
removed, or changed with its old and new signature line.

## Confinement monitor (§16.3)

Codex's read-only sandbox blocks writes and network access but, in host mode, not reads outside the worktree. The
monitor inspects every `command_execution` (also inside `bash -c`, `sh -c`, `powershell -Command`, `cmd /c`) and
flags a path that leaves the worktree and the run's artifact directory when it is in the user's home directory
(`~`, `$HOME`, `${HOME}`, `$env:USERPROFILE`, `%USERPROFILE%`, or an absolute path below it) or exists; a path built
from another variable is flagged too (fail closed). `cd` is followed within a command, and every existing operand,
plain relative ones included, is resolved against that tracked directory: when a component is a symlink or a
Windows junction (resolved with `os.Readlink`, since `filepath.EvalSymlinks` no longer follows junctions) whose target
lies outside the roots (compared by name and by filesystem identity), it is flagged; an unresolvable link is flagged.
Drive-letter paths are absolute on every OS. Any `file_change` item and any tool item
(MCP, web search, browser, …) is a violation.

The monitor is a heuristic over command text, not a sandbox: a command substitution such as `$(go env GOROOT)/…` is
not resolved. Strict isolation is container mode (Phase 12), where only the worktree and the artifact directory are
mounted.

Observed with codex-cli 0.162.0 on Windows: Codex reads global skill files (`~/.agents/skills/…`) on its own even
with `--ignore-user-config`, and no config key turns that off. The instructions forbid reading skill files; if the
model still does, the monitor flags it and the review is retried once (recording: `testdata/stream/recorded_skill_read.jsonl`).

## Authentication

| | Subscription | API |
|---|---|---|
| Codex home | the user's `~/.codex` (or their `CODEX_HOME`) | dedicated `~/.aco/codex-api/`, never `~/.codex` (refused) |
| Login | the user's `codex login` (ChatGPT) | ACO runs `codex login --with-api-key` in the dedicated home with `OPENAI_API_KEY` on stdin, when the home is not logged in or with another key (compared by the masked hint `codex login status` shows: first 8 and last 5 characters) |
| Checked before every review | `codex login status` = ChatGPT | `codex login status` (dedicated home) = API key |
| Home check (api mode, before any status, login, or exec) | — | the dedicated home must not be, contain, or lie inside `~/.codex` or a `CODEX_HOME` set for the subscription login, compared by name, after resolving symlinks and junctions in every component (also parents), and by filesystem identity; a home that is itself a link, not a directory, or unresolvable is refused (`auth`, `ErrUnsafeAPIHome`) |
| Cost | not charged | input tokens × `input_per_mtok` + output tokens × `output_per_mtok` from `pricing.codex.<model>`, charged to the USD budget; without a price only the tokens are recorded (`Priced: false`) |

The key is registered with the redactor and never appears in artifacts, logs, or the child environment. Codex itself
stores it in `auth.json` of the dedicated home (a protected path for agent commands).

## Errors (§14.1)

Classified from `turn.failed`, `error` events, and stderr: `subscription_limit` (with the reset time from "try again
in 2 hours 13 minutes" or "try again at 3:05 PM" / "try again at Nov 10th, 2026 3:05 PM"), `api_rate_limit` (with
`Retry-After` / "try again in 20s"), `api_quota`, `auth` (401, logged out, invalid key, expired token), `transient`
(5xx, disconnects), `crash` (unknown model, everything else), `timeout`, `cancelled`, `budget_exceeded`,
`schema_invalid`, `policy_violation`.

## `aco doctor`

`codex.flags` (required exec flags and `--sandbox read-only`), `codex.auth` (login vs. resolved mode, with a
Simplified Chinese explanation of what to change on a mismatch), `codex.probe` (`--probe`: one real call in a
temporary directory that verifies the login — logging the dedicated home in, in api mode — the model,
`model_reasoning_effort`, the output schema, and that the sandbox refuses to create a file).

## Tests

- Unit (fake CLI = the test binary): recorded and constructed JSONL streams in `testdata/stream` (valid review,
  schema-invalid then valid, fallback model, inconsistent verdict, usage limit with reset time, logged out (recorded),
  `~/.ssh/id_rsa` read, recorded skill read, file change, transient, 429, quota, ignored effort, unknown model
  (recorded)); golden command line and context packs; size cap; subscription and api environments; api mode leaves
  `~/.codex` unchanged (file hashes) and the key is in no artifact; login mismatch stops before `exec`; idle timeout.
- `-tags=integration`: the installed CLI's flags and features; api-mode login of a temporary home with a dummy key.
- `-tags=live` with `ACO_LIVE_CODEX=1` (`ACO_LIVE_AUTH_MODE=api` for api mode): read-only probe, a plan with an
  uncovered requirement gets `missing`, a seeded SQL injection is a `security` blocker/major in the right file.
