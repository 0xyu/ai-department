# Claude Code adapter (`internal/agent/claudecode`)

ACO drives Claude Code headlessly (`claude -p --output-format stream-json`) in two profiles (§16.2):

| Profile | Used for | Model / effort (default) | Tools | Writes files? |
|---|---|---|---|---|
| planner | Stage 1: `Planner.Create`, `Planner.Revise` | `planner.model` / `planner.effort` (`claude-opus-5-5`, `high`) | `Read`, `Glob`, `Grep` | never: the plan package comes back as structured output and the orchestrator writes it |
| implementer | Stages 3–4: `Implementer.Start`, `Implementer.Continue` | `implementer.model` / `implementer.effort` (`claude-opus-5-5`, `medium`) | `Read`, `Edit`, `Write`, `Glob`, `Grep`, `Bash` | in the run worktree, checked by the guard hook |

The interfaces and shared types are in `internal/agent`; scripted fakes for offline engine tests are in
`internal/agent/fake`.

## One invocation

1. **Capabilities.** On first use the adapter runs `claude --version` and `claude --help` and records the flags it
   supports. A CLI without a [required flag](#required-flags) fails before anything starts. Effort uses `--effort`
   when present, else `effortLevel` in the generated settings; with `implementer.strict_effort: true` a CLI without
   `--effort` fails the implementer.
2. **Budget (api mode).** When the USD budget of any scope of the call (`run`, `planning`, `phase`, `final`) is used up,
   the call is refused (`budget_exceeded`). Otherwise the smallest remaining amount is passed as `--max-budget-usd`.
3. **Artifacts.** Into the invocation's directory in the run store (`agent.Output`): `prompt.md`, `system-prompt.md`,
   `guard-policy.json`, `settings.json`, later `transcript.jsonl`, `stderr.log`, and the guard's `guard-denials.jsonl`.
   Every file is masked with the run's `secure.Redactor`.
4. **Guard.** `guard-policy.json` holds the worktree root, protected paths, phase `scope.paths`, scope enforcement,
   and network mode. `settings.json` registers one `PreToolUse` hook for every tool (`matcher: "*"`):
   `"<aco>" hook pre-tool-use --run "<run>" --policy "<guard-policy.json>"`, plus the profile's deny rules. The adapter
   refuses to start when the `aco` binary for the hook does not exist, because a hook that cannot start would let
   the tool call through.
5. **Command.** Built by `BuildArgs` in a fixed order (golden files in `testdata/args-*.golden.json`). The prompt goes
   on **stdin**, never as an argument. `--setting-sources ""` and `--strict-mcp-config` make the generated settings
   the only settings: a repository's `.claude/settings.json` cannot add hooks or permissions, and no MCP server loads.
   Schemas are passed without their root `$schema`/`$id` (the CLI's validator rejects the draft 2020-12 URI).
6. **Environment.** `auth.ChildEnv`: an allowlist of the parent's variables (system, locale, proxy, certificates,
   toolchains), never a secret or `ACO_*` variable, never the §16.5 removal list. Subscription mode passes
   `CLAUDE_CODE_OAUTH_TOKEN` if set; api mode sets `ANTHROPIC_API_KEY` from the resolved key (process env, env file,
   keychain). `DISABLE_AUTOUPDATER=1` is added.
7. **Process.** Started as a process tree (`internal/proc`: Unix process group; Windows job object assigned while the
   process is still suspended). Cancellation, the invocation timeout (`limits.invocation_timeout`), the idle timeout
   (`limits.idle_timeout`, no stdout line), and every early stop kill the whole tree; after a normal exit the tree is
   killed too, so background shells cannot outlive the call.
8. **Stream.** Each stdout line is appended to the transcript and parsed. Unknown message types and fields are ignored;
   lines that are not JSON are counted and skipped. The adapter stops the call at once when:
   - the init message's `apiKeySource` does not match the resolved mode (`none` = subscription,
     `ANTHROPIC_API_KEY` = api) → `auth`;
   - a planner session lists a write tool → `crash`;
   - a `PreToolUse` hook response exits with neither 0 (allow) nor 2 (block), or a tool result arrives while no guard
     hook ever answered (the settings were not applied — `-p` ignores invalid settings silently) → `crash`;
   - more assistant turns than allowed (only as a backstop when the CLI enforces `--max-turns` itself) → `max_turns`.
9. **Result.** Usage (`num_turns`, tokens, `total_cost_usd`, duration) is recorded. In api mode the cost is charged to
   the budget even when the call failed. The structured output is `structured_output`, or a JSON object in the result
   text as a fallback.

## Results and validation

- **Planner.** The structured output must parse as a plan package (`schemas/plan.schema.json`); parse errors →
  `incomplete`. Deterministic plan checks are the engine's job (Phase 8). `Revise` resumes the `Create` session
  (`--resume`) with a revision schema: the plan schema plus a required root `finding_responses`
  (`fixed` | `disputed`); a finding without an answer → `incomplete` (the result is still returned).
- **Implementer.** The report must match `schemas/impl-report.schema.json` (unknown fields rejected), assess every
  acceptance criterion of the request, and answer every finding or gate failure ID (`fixed`, `disputed`,
  `cannot_fix_within_spec`, `cannot_fix_safely`). Otherwise → `incomplete`, with the result returned so the engine can
  `Continue` with the problems. `Start` always uses a new session ID; `Continue` resumes the given one.

## Error classes (`agent.ErrorClass`, §14.1)

| Class | Detected from | Handling (engine, later phases) |
|---|---|---|
| `transient` | overloaded / 5xx / network texts, `api_error_status` ≥ 500; 429 in subscription mode | back off, retry |
| `subscription_limit` | a `rate_limit_event` with status `rejected`, or texts like "You've hit your limit · resets 3pm (Europe/Berlin)", "usage limit reached\|<unix>" | pause until `ResetAt` (or fallback to api mode) |
| `api_rate_limit` | 429 / `rate_limit_error` in api mode; `RetryAfter` from `retry-after` | back off |
| `api_quota` | "Credit balance is too low", quota / billing texts, 402 (api mode) | run fails (`api_quota`) |
| `max_turns` | result subtype `error_max_turns`, or ACO's backstop | continue the session ("produce report") |
| `incomplete` | no or invalid structured output; CLI subtype `error_max_structured_output_retries` | continue with the problems |
| `crash` | exit without a result, CLI usage errors, guard not running, planner with a write tool | retry once, then new session |
| `timeout` | invocation or idle timeout | kill tree, retry |
| `auth` | auth-source mismatch, 401/403, "Invalid API key", "Please run /login", missing api key | run fails (`auth`) |
| `budget_exceeded` | budget used up before the call, or CLI subtype `error_max_budget_usd` | run fails |
| `cancelled` | context cancelled | — |

Every `agent.Error` carries the `Invocation` (session ID for resume, usage, artifact refs).

## Guard hook (`aco hook pre-tool-use`)

Claude Code pipes each tool call as JSON on stdin. The hook loads the policy, evaluates the call
(`policy.GuardPolicy.Evaluate`), and either exits 0 with no output (no objection; Claude Code's own permission rules
still apply) or exits 2 with an `ACO guard: …` reason on stderr, which the model sees. Every denial is appended to the
deny log as `{time, tool, rule, path}` — never the command text. A missing or invalid policy, a policy of another run,
unreadable input, or an unexpected event all block.

What it blocks (rules are stable names, see `internal/policy/guard.go`):

| Rule | Examples |
|---|---|
| `outside_worktree`, `home_reference` | `Read ../../etc/passwd`, `cat ~/.claude/.credentials.json`, `rm -rf /`, `cd /etc`, `X=/etc; cat $X/passwd`, symlinks leaving the worktree |
| `git_dir` | reading or writing `.git/**`, `GIT_DIR=…`, `git --git-dir` |
| `worktree_root` | `rm -rf .` |
| `protected_path` | writes to `.aco/**`, `AGENTS.md`, `.github/**`, `.env*`, … by tool, redirection, `mv`, `cp` target, `sed -i`, `tee`, `dd of=`, `find -delete`, `find -exec rm {}` |
| `scope_violation` | writes outside `scope.paths` when `policy.scope_enforcement: block` (with `flag`, the diff policy reports them instead) |
| `forbidden_git` | every git subcommand except read-only ones (`status`, `diff`, `log`, `show`, `blame`, `grep`, `ls-files`, …); `git -c`; `--output`, `--ext-diff`, `--textconv` |
| `privilege` | `sudo`, `su`, `doas`, … anywhere in a command line |
| `network`, `download_exec` | `curl`, `wget`, `ssh`, … unless `policy.network: allow`; a download piped into an interpreter always; `WebFetch` only to allowlisted hosts; `WebSearch` only with `allow` |
| `eval`, `unparsable_command` | `eval`; `cmd /c`, `powershell -Command`; unbalanced quotes; unterminated heredocs |
| `read_only` | any write or command by the planner |
| `unknown_tool` | MCP tools, sub-agents (`Task`), `PowerShell`, anything not listed |

Commands are read by a small shell parser (`internal/policy/shell.go`): pipelines, `;`/`&&`/`||`, subshells,
quotes, `$(…)` and backticks (checked recursively), `bash -c` scripts, redirections, heredocs (bodies are data), and
wrappers such as `env`, `timeout`, `xargs`, `command`.

The guard is a policy check, not a sandbox: a program the agent may run (a test, a build script) can do anything its
OS user can. Container mode (Phase 12) is the isolation boundary.

## Required flags

`--print`, `--output-format`, `--verbose`, `--session-id`, `--resume`, `--model`, `--permission-mode`, `--tools`,
`--allowedTools`, `--disallowedTools`, `--settings`, `--setting-sources`, `--strict-mcp-config`, `--json-schema`,
`--append-system-prompt`. Used when present: `--effort`, `--max-turns` (hidden in 2.1.311's help, assumed from 2.x),
`--max-budget-usd`, `--permission-prompts none`, `--include-hook-events`. Verified with Claude Code 2.1.311.

## `aco doctor` checks

| Check ID | Result |
|---|---|
| `claude.flags` | FAIL when a required flag is missing |
| `claude.effort` | OK with `--effort`; otherwise WARN (settings key), or FAIL when `implementer.strict_effort` |
| `claude.auth` | subscription mode: `claude auth status --json` must show a subscription login; FAIL when logged out or logged in with an API key / console. api mode: INFO (the key is checked by `auth.key.claude`, the source on every call) |
| `claude.probe` | only with `--probe`: one small real call with the implementer's model and effort in the resolved mode; FAIL with the error class. Without `--probe`: INFO |

## Tests

| Test | What it shows |
|---|---|
| `TestStreamFixtures` | recorded stream-json fixtures (`testdata/stream/`): success, max turns, overloaded, usage limit (event and legacy text), invalid key, quota, 429, no result — each to the right result or class (AC-05.1) |
| `TestBuildArgsGolden` | golden command lines; implementer has `--model claude-opus-5-5 --effort medium`, planner `--effort high` and no write tools (AC-05.2) |
| `TestSubscriptionMode`, `TestAPIMode` | the child environment dumped by a fake CLI; auth-source mismatch stops with `auth`; `total_cost_usd` charged; budget refusal (AC-05.3, AC-05.4) |
| `internal/policy` `TestGuard*`, `internal/cli` `TestHook*` | guard rules and the hook command (AC-05.5) |
| `TestCancelKillsProcessTree`, `internal/proc` | Claude and its child end within 5 s of cancellation (AC-05.6) |
| `TestGuardHookEndToEnd` (`-tags=integration`) | the real `aco` binary run from the generated settings blocks and allows as specified |
| `TestLive*` (`-tags=live`, `ACO_LIVE_CLAUDE=1`) | real CLI: schema-valid plan package; implementer edits a temp repo; `Continue` resumes the session (AC-05.7). `ACO_LIVE_AUTH_MODE=api` for api mode |

The fake CLI is the test binary itself (`TestFakeClaude`): it records its arguments, environment, and stdin, plays a
fixture, and can hang, spawn a grandchild, or run the hook command from the settings file.
