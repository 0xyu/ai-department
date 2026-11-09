# Secret handling (`internal/secure`)

ACO handles API keys for Claude Code and Codex CLI. This layer makes sure they never reach an output: the terminal,
logs, and (in later phases) events, reports, agent prompts, commit messages, and API responses.

Earlier versions tried to find secrets inside arbitrary values just before printing them. That cannot be finished:
any Go value can carry its own `String`, `Format`, `MarshalJSON`, or `MarshalText`, and one credential can be printed
raw, quoted, JSON-escaped, Base64, hex, or as a list of byte values. This layer works the other way round: secrets
are typed when they are read, and every output accepts only what it knows is safe.

## Rules

1. **Secrets are typed at the source.** API keys and other secret variables (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`,
   any name ending in `_KEY`, `_TOKEN`, `_SECRET`, `PASSWORD`, `_PASS`) are read from the process environment or the
   env file as `secure.Secret`. A `Secret` holds only a handle, so every way of printing or encoding it gives
   `[REDACTED]`, including `%#v`, `%x`, JSON, and fields fmt cannot reach. The value is read with `Reveal()`, which
   only allowlisted packages may call. Today that is `internal/secure` alone; the Claude and Codex adapters will be
   added when they need to pass keys to the child process.
2. **Every output masks.**
   - Operator text goes through `i18n.Printer`, filtered by a `secure.Redactor`. The redactor masks every registered
     secret, plus its quoted, JSON-escaped, and URL-escaped forms, and the credential patterns
     (`sk-ant-…`, `sk-…`, `ghp_…` and the other GitHub token prefixes, `github_pat_…`, `AKIA…`).
   - Logs go through `internal/logging`, which masks the message, keys, group names, and strings. It writes only these
     value types:

     | Logged as | Types |
     |---|---|
     | the value | strings, numbers, booleans, durations, times, named types with those underlying kinds, `[]string` |
     | `Error()`, called once (a panic becomes `!PANIC: log value conversion failed`) | errors |
     | checked against the same rules | groups and `slog.LogValuer` results |
     | `[REDACTED]` | `secure.Secret` |
     | `[UNLOGGABLE <type>]`, with no method called on the value | everything else |

     To log a structured value, give its type a `LogValue` method that returns a group of allowed values.
     Expansion is bounded per record: at most 16 nested groups and 256 `LogValue` calls across all attributes. Beyond
     that a value is written as `[TRUNCATED: log value expansion limit]`, so a `LogValuer` that contains itself cannot
     overflow the stack. Plain attributes cost nothing and are always written.
3. **Diagnostics name things; they don't repeat input.** Errors name the field, flag, or variable. Field paths are
   rendered by `config.DisplayPath` from trusted parts only: schema field names, list indexes, and a fixed list of
   secret key names (`api_key`, `token`, ...). An unknown key or a map key becomes `[hidden]`
   (`planner.[hidden]`); the file and line still point at it. `--set` errors name the key path (`--set auth.mode`)
   only when it is fully trusted, else the position (`--set #2`). Bad integer or boolean flag values are never
   repeated. Any other rejected value is repeated only if `Redactor.Echo` allows it: at most 20 characters from
   `A-Z a-z 0-9 . _ : / + -`, no credential pattern, and no registered secret inside it, including ones shorter than
   the 8-character masking threshold. Otherwise it shows as `[hidden]`. Typos such as `subscriptoin` are still shown.
4. **One leak corpus tests every output.** `internal/secure/securetest` has the fake credentials:
   - a pattern-matching one;
   - an opaque one with a quote and a backslash;
   - an unregistered one that no rule recognizes.

   For each fake it lists every encoding: raw, Go-quoted, JSON-escaped, URL-escaped, hex, decimal lists, and Base64 at
   all three alignments. An output's tests feed the fakes in and call `AssertNoLeak`. The CLI corpus test
   (`internal/cli/secure_corpus_test.go`) covers flags, `--set`, positional arguments, env vars, config files, and the
   env file, in text and JSON logs.

## Guard tests

`internal/secure/guard_test.go` checks all non-test code. Each allowlist is in that file and should change only with
a reason.

| Rule | Allowed in |
|---|---|
| `Secret.Reveal` | `internal/secure/`, `internal/auth/childenv.go` (hands an API key to the matching agent CLI only) |
| `slog.New`, the slog handlers, `slog.Default`/`SetDefault`, top-level `slog.Info` etc. | `internal/logging/` |
| the stdlib `log` package | nowhere |
| `os.Getenv`, `os.LookupEnv`, `os.Environ`, `os.ExpandEnv`, `syscall.Getenv` | `internal/cli/cli.go`, `internal/doctor/doctor.go`, `internal/gitops/exec.go` (builds the git child environment with secret variables removed), `internal/auth/childenv.go` (builds agent CLI environments from an allowlist) |
| importing `securetest` | tests only |
| dot imports (`import . "os"`) of `os`, `syscall`, `log`, `log/slog` | nowhere (they would hide calls from these checks) |

The i18n guard test (`internal/i18n`) also forbids writing to stdout or stderr outside the printer.

## Adding a new output

A new output (event store, report writer, prompt builder, HTTP or MCP handler) must:

1. accept only typed data or text, and mask text with the run's `secure.Redactor`;
2. never take `any` and serialize it by reflection;
3. add a test that writes `securetest.All` into it and calls `AssertNoLeak` on what it produced.

`aco validate-plan` (Phase 2) repeats plan text (titles, dependency text, gate labels) in its diagnostics. Its output
goes through the same filtered printer, so registered secrets and credential patterns are masked in text and JSON
output (`TestValidatePlanOutputMasksSecrets`). Unregistered opaque text inside a plan is plan content and is shown.

Phase 3 outputs follow the same rules (`docs/lifecycle.md`):

- **State database** (`internal/state`): every free-text column (reason details, titles, errors, model names, paths,
  session IDs) and every JSON document (step intents and outcomes, reviews, gate summaries, finding data) is masked
  before it is written; JSON is accepted only as bytes the caller marshalled from a typed value, and must stay valid
  after masking. `TestStoreNeverStoresSecrets` writes the corpus through every write API and scans every column of
  every table.
- **Events** (`internal/events`): payloads hold only typed fields (`Str`, `Int`, `Bool`, `Strs`); strings and the
  message are masked when the event is appended. `TestEventsNeverCarrySecrets` checks replayed and JSON-encoded events.
- **Artifacts** (`internal/artifacts`): content is masked before the atomic write; streamed artifacts are masked as a
  whole on `Commit`, so a secret split across writes is still caught. `TestArtifactsNeverStoreSecrets` covers the
  encodings the redactor handles; hex, decimal, and Base64 forms are not masked in free text, so producers must not
  write encoded secrets in the first place.

Phase 4 outputs (`docs/git.md`):

- **Git** (`internal/gitops`): secret variables are removed from the environment of every git call (GitHub tokens are
  kept only for push and PR); commit messages, tag messages, and notes are masked before Git sees them; the branch
  reflog gets a fixed message, never the commit subject; error messages name the git subcommand only and carry
  masked stderr. No repository-selected program runs: hooks, fsmonitor, and every configured clean/smudge/process
  filter are disabled on every call. `TestCommitMessagesAreMasked` (commit message and reflog),
  `TestTimeoutAndErrorsAreMasked`, `TestGitEnvScrubsSecrets`, `TestHooksDoNotRun`, and
  `TestExternalFiltersNeverRun` cover this.
- **Diff policy** (`internal/policy`): a finding records the path, line, and rule of a detected secret, never the
  text. `TestSecretFindingsNeverContainTheSecret` checks findings as JSON and text.

## Agent processes (Phase 5)

- **Environment.** Agent CLIs get an allowlist of the parent's variables (`auth.ChildEnv`), never a secret or `ACO_*`
  variable and never the §16.5 removal list (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`,
  `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, `CLAUDE_CODE_USE_FOUNDRY`, `OPENAI_API_KEY`, `CODEX_API_KEY`).
  Only api mode adds `ANTHROPIC_API_KEY`, from the resolved key. Tests dump the child environment of a fake CLI.
- **Auth source.** Every Claude invocation checks `apiKeySource` in the init message against the resolved mode and
  stops the process tree on a mismatch (class `auth`).
- **Prompts and transcripts.** Prompts go to the CLI on stdin after masking; prompts, transcripts, stderr, settings,
  and guard files are written through the artifact store, which masks them again.
- **Guard hook.** `aco hook pre-tool-use` decides every tool call from a policy file the orchestrator wrote outside the
  worktree; anything it cannot decide is blocked. Its deny log records tool, rule, and path, never command text. The
  adapter verifies from the stream that the hook ran (see docs/claude-adapter.md).
- **Settings isolation.** `--setting-sources ""` and `--strict-mcp-config`: user, project, and local Claude settings
  and MCP servers do not apply to ACO's invocations.

### Reviewed gosec findings (Phase 5)

`gosec` on the Phase 5 packages reports only these, all accepted as designed: G204 (the `claude` binary is started
with ACO-built arguments and no shell), G304 (reading the ACO-written guard policy and the operator's env file),
G101 (environment variable *names* such as `ANTHROPIC_API_KEY`, not values), G103 (the `unsafe.Pointer` the Win32
job-object API requires).

### Quality gates (Phase 6)

- Gate commands run with `auth.GateEnv`: the agent allowlist without any credential, secret-looking variable, or
  `ACO_*` variable (tested by dumping a gate command's environment).
- Gate logs, parsed messages, and command lines in results are masked by the run's Redactor before they are stored
  or trimmed; the secret scanner reports rule, path, and line, never the value.
- Reviewed gosec items in `internal/gates`: G204 (gate commands come from the frozen plan, the config, or
  auto-detection and run through `sh -c` by design) and G304 (manifests in the worktree and files in ACO's private
  temporary directories).
