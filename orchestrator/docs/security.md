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
| `Secret.Reveal` | `internal/secure/` |
| `slog.New`, the slog handlers, `slog.Default`/`SetDefault`, top-level `slog.Info` etc. | `internal/logging/` |
| the stdlib `log` package | nowhere |
| `os.Getenv`, `os.LookupEnv`, `os.Environ`, `os.ExpandEnv`, `syscall.Getenv` | `internal/cli/cli.go`, `internal/doctor/doctor.go` |
| importing `securetest` | tests only |
| dot imports (`import . "os"`) of `os`, `syscall`, `log`, `log/slog` | nowhere (they would hide calls from these checks) |

The i18n guard test (`internal/i18n`) also forbids writing to stdout or stderr outside the printer.

## Adding a new output

A new output (event store, report writer, prompt builder, HTTP or MCP handler) must:

1. accept only typed data or text, and mask text with the run's `secure.Redactor`;
2. never take `any` and serialize it by reflection;
3. add a test that writes `securetest.All` into it and calls `AssertNoLeak` on what it produced.
