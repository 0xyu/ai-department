# aco CLI

```
aco <command> [flags]
```

Operator-facing output is in Simplified Chinese by default; set `language.operator: en` (in `~/.aco/config.yaml`,
`<repo>/.aco.yaml`, or `--set language.operator=en`) for English. Flags may appear before or after positional
arguments. `aco help <command>` and `aco <command> --help` show command help.

`--` ends flag parsing: every token after it is a positional argument, even `--help` or `-x`. A flag that takes a
value and is written without `=` takes the next token as its value, even if it looks like a flag
(`--repo --help` sets the repo to `--help`).

A bad value for an integer or boolean flag is reported by flag name and expected type only
(`Invalid value for --from-phase (expected an integer)`); the rejected value is never printed.

## Commands

| Command | Status (Phase 1) | Flags |
|---|---|---|
| `run` | stub | `--repo --requirements --plan --base --detach --config --set --env-file --auth-mode --claude-auth-mode --codex-auth-mode` |
| `plan` | stub | `--repo --requirements --base --config --set --env-file` + auth flags |
| `resume <run-id>` | stub | `--retry --from-phase N --revise-plan` + config and auth flags |
| `status [<run-id>]` | stub | `--follow --json` |
| `pause <run-id>` | stub | |
| `cancel <run-id>` | stub | |
| `report <run-id>` | stub | `--phase N --final --format md\|json` |
| `validate-plan <file\|dir>` | stub | `--strict --json` |
| `doctor` | **implemented (v1)** | `--repo --config --set --env-file --auth-mode --claude-auth-mode --codex-auth-mode --security` |
| `serve` | stub | `--addr` (default `127.0.0.1:7420`) + config flags |
| `mcp` | stub | `--transport stdio\|http` |
| `hook <event>` | stub (internal) | `--run <run-id>` |
| `version` | **implemented** | |

Stubs print "not implemented" (in the operator language) and exit with code 2.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | completed |
| 1 | failed (system problem) |
| 2 | usage or config error, or command not implemented |
| 3 | preflight / doctor checks failed |
| 4 | cancelled |
| 5 | blocked (needs a human decision) |

## `aco doctor` (v1)

Loads config, the env file, and resolves auth, then checks in this order:

| Check ID | Result |
|---|---|
| `config.layers` | OK — lists applied config layers |
| `config.reviewer_model` | WARN when `reviewer.model` is empty |
| `auth.env_file` | INFO if not found, WARN if group/others can read it (Unix), OK if loaded |
| `auth.mode.claude`, `auth.mode.codex` | INFO — resolved mode and its source |
| `auth.fallback` | INFO — fallback and its source |
| `auth.key.claude`, `auth.key.codex` | only for agents that need a key (API mode, or fallback `api`): OK with source, or FAIL naming the missing variable |
| `tool.git` | FAIL if missing or older than 2.30 |
| `tool.claude`, `tool.codex` | FAIL if missing or `--version` fails; version shown |
| `aco_home` | FAIL if `~/.aco` is not writable |
| `repo` | only with `--repo`: FAIL if not a Git repository |
| `security` | only with `--security`: placeholder until the security phase |

Exit code: 0 when nothing failed (warnings allowed), 3 when any check failed, 2 on config/env/auth errors.

Version probes run with secret environment variables removed and `GIT_TERMINAL_PROMPT=0`. Later phases extend
`doctor` with CLI capability checks (flags, effort, model availability, auth-source verification).

## Build info

`aco version` prints version, commit, and build date. Release builds inject them with:

```
go build -ldflags "-X github.com/0xyu/aco/internal/buildinfo.Version=v1.0.0 \
  -X github.com/0xyu/aco/internal/buildinfo.Commit=$(git rev-parse --short HEAD) \
  -X github.com/0xyu/aco/internal/buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/aco
```

Without them, commit and date come from the Go VCS stamp when available.

## i18n rule

Every operator-facing string is a catalog key in `internal/i18n/keys.go` with zh-CN and en text in
`internal/i18n/catalog.go`. Tests fail when a key lacks a translation, format verbs differ between languages, or any
non-test code writes to stdout/stderr directly (`fmt.Print*`, `fmt.Fprint*`, `io.WriteString`, `os.Stdout.*`,
`print`/`println`) or passes a string literal to the printer.
