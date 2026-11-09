# Quality gates

`internal/gates` runs the six quality gates (§11.5, §12 of the implementation plan) without any agent: it resolves
each gate's commands, runs them in the worktree, parses machine-readable output, and compares the result with a
baseline. The phase engine (Phase 9) and Stage 4 (Phase 11) call it; this phase provides the runner only.

## Gates and order

| Gate | Typical commands |
|---|---|
| `typecheck` | `go vet ./...`, `tsc --noEmit`, `mypy`, `pyright`, `cargo check` |
| `lint` | gofmt check, `golangci-lint run`, `staticcheck`, `eslint`, `ruff check`, `cargo clippy` |
| `unit` | `go test ./... -short -json`, `npm test`, `pytest -m "not integration"`, `cargo test --lib` |
| `integration` | `go test -tags=integration ./... -json`, `npm run test:integration`, `pytest -m integration` |
| `build` | `go build ./...`, `npm run build`, `python -m compileall`, `cargo build --release` |
| `security` | built-in diff policy and secret scanner, plus `gosec`, `govulncheck`, `semgrep`, `npm audit`, `pip-audit`, `trivy` |

Gates run in this order. A normal round is fail-fast (`RunOptions.FailFast`): after the first gate that does not pass,
the remaining required gates are `skipped` (no review is spent on a red build). A baseline round always runs every gate.

## Resolution and preflight

`gates.Resolve` picks each gate's commands in this order:

1. the frozen Phase Specification (`- unit: \`go test ./...\``);
2. `gates.<type>.commands` in the config;
3. auto-detection, when `gates.auto_detect` is true.

A gate the Phase Specification marks `not_applicable` needs its justification (approved in Stage 2); it is then not
run and its result is `not_applicable`. Everything else is required. Resolve returns preflight problems; any problem
fails the preflight:

| Code | Class | Meaning |
|---|---|---|
| `gate_unresolved` | environment | a required gate has no command |
| `not_applicable_unjustified` | internal | `not_applicable` without a justification |
| `command_not_found` | environment | a command's program is not on `PATH` |
| `unknown_scanner` | internal | a security scanner name ACO does not know |

The program check reads the first word of each simple command (`a && b | c`, `$(…)`), skipping shell builtins and
`VAR=value` prefixes. Words it cannot judge (`$TOOL`) are skipped; a missing program then fails at run time with
exit 127, which is also classified `environment`.

### Auto-detection

Detection reads manifests at the worktree root. Several languages contribute side by side.

| Project | Recognized by | Gates |
|---|---|---|
| Go | `go.mod` | `go vet`; gofmt check (+ `golangci-lint` or `staticcheck` if installed); `go test -short -json`; `go test -tags=integration -json` if a `_test.go` file has an `integration` build tag; `go build`; scanners `govulncheck`, `gosec` if installed |
| Node | `package.json` | scripts `typecheck`/`type-check`/`tsc`, else `tsc --noEmit` with a `tsconfig.json`; `lint`, else `eslint . -f json` with an ESLint config; `test:unit`/`unit`/`test`; `test:integration`/`integration`/`test:e2e`/`e2e`; `build`; `npm-audit` with a lockfile |
| Python | `pyproject.toml`, `setup.py`, `setup.cfg`, `requirements.txt` | `mypy` or `pyright` (configured or installed); `ruff` or `flake8`; pytest with JUnit XML, split by an `integration` marker (or an `integration` test directory); `python -m compileall`; `pip-audit` |
| Rust | `Cargo.toml` | `cargo check`, `cargo clippy -D warnings`, `cargo test --lib --bins`, `cargo test --tests` if `tests/` exists, `cargo build --release` |
| Make | `Makefile` | fills gates no language resolved, from targets `typecheck`/`vet`/`check`, `lint`, `test-unit`/`unit`/`test`, `test-integration`/`integration`/`e2e`, `build` |

Core toolchains (`go`, `npm`, `python`, `cargo`) are always proposed, so a missing toolchain fails the preflight.
Optional tools are proposed only when found, so a missing optional linter never blocks a run. `secrets` is always a
security scanner.

### Stage 4 (full-project mode)

`ModeFull` resolves the config or auto-detected (whole-project) commands plus every command any phase declared. A
gate is `not_applicable` only if every phase said so and nothing else resolves. The security gate scans every file of
the working state and every commit in `Base..HEAD` for secrets (see below).

## Execution

- Commands are POSIX shell command lines run with `sh -c` in the worktree (on Windows, the `sh` of Git for Windows,
  found on `PATH` or next to `git.exe`; there is no `cmd.exe` fallback).
- The environment is `auth.GateEnv`: the same allowlist as the agent CLIs (system, locale, proxy, toolchain
  variables), never an agent credential, a secret-looking variable, or an `ACO_*` variable, plus `CI=true`,
  `GIT_TERMINAL_PROMPT=0`, `NO_COLOR=1`. Services an integration suite needs must be configured outside the
  environment or passed as fixed non-secret `ExtraEnv`.
- Each command gets a private reports directory outside the worktree. `{reports}` in a command is replaced by its
  quoted path (also in `ACO_GATE_REPORTS`); every file left there is parsed. Example:
  `python -m pytest --junitxml={reports}/unit.xml`.
- Each command has a timeout (`gates.<type>.timeout`, default 30 minutes). At the timeout the whole process tree is
  killed (`internal/proc`: process group on Unix, job object on Windows) and the result is `timeout`.
- Stdout and the combined log are captured to temporary files (at most 64 MiB each); the combined log is redacted and
  trimmed to 1 MiB (`Trim`): the beginning, a block around the first failure line, and the tail, with a marker for
  each cut. For `go test -json` the log is the test output reconstructed from the events. With `Options.Out` set, the
  full redacted log is also kept as a run artifact.
- Every result carries the tree hash of the working state it ran on (`gitops.WorkingTree`). If a gate changes the
  working state (generated files), `tree_after` records the new hash.

## Parsed output

| Format | Source | Yields |
|---|---|---|
| `go test -json` | stdout | per-test pass/fail/skip; a package that failed without a failing test (build error, panic, `TestMain`) is a test named by the package |
| JUnit XML | report files or stdout (`<testsuite>`/`<testsuites>`) | per-test results (JUnit, pytest `--junitxml`, jest-junit, surefire) |
| ESLint JSON (`-f json`) | stdout or report file | issues; errors high, warnings low |
| golangci-lint JSON | stdout or report file | issues |
| SARIF 2.1.0 | report file (gosec, semgrep, trivy) | issues; severity from `security-severity` (CVSS), a severity property, or the level |
| govulncheck JSON | stdout | vulnerabilities whose trace reaches a function (called code), severity high |
| `npm audit --json` | stdout | one vulnerability per advisory (report v2 and npm 6 format) |
| `pip-audit -f json` | report file or stdout | vulnerabilities, severity high |

govulncheck and pip-audit report no severity; ACO treats their findings as `high` (fail closed). Scanners exit
non-zero when they find something; a scanner command passes or fails by its parsed findings, and is an `error` when
its output cannot be read.

## Security gate

- **Diff policy** (phase mode, always): `policy.CheckDiff` from `Options.Base` (the previous phase's commit) to the
  working state — protected and scope paths, oversized and binary files, anti-gaming edits, missing tests (see
  [git.md](git.md)). Blocking findings fail the gate with class `policy`.
- **Secret scanner** (`secrets`): in phase mode, the secret rules of the same diff policy on the phase diff; findings
  never include the secret. In full mode: every file of the working state, and every commit in `Base..HEAD` (a secret
  added and removed again inside the run still blocks). Secrets in the current files are compared with the
  baseline (a secret that predates the run is reported, not blocking); secrets in the run's commits always block.
- **SAST and dependency scanners**: new findings at or above `security.fail_severity` (default `high`) fail the gate;
  lower and pre-existing ones are reported.

## Baseline comparison

`NewBaseline(round, commit)` records a baseline round; `BaselineStore` keeps it in the run artifacts
(`baselines/<sha>.json`). A gate compared with its baseline passes unless it has:

- a **new failing test**: failing now and not failing at baseline (tests that passed at baseline are also listed as
  **regressions**, by name);
- a **new issue** in `lint` or `typecheck` above `low`, or a new SAST finding at or above `fail_severity`;
- a **new vulnerability** at or above `fail_severity` (same advisory and package = same vulnerability, whatever the
  version);
- a **blocking policy finding** (judged on the diff; the baseline does not apply);
- a **new unstructured failure**: a command that fails without structured results whose normalized failure lines
  (line and column numbers, durations, and addresses removed) are not all in the baseline command's.

Pre-existing failing tests, issues, and vulnerabilities are listed in `comparison` and do not block. Issues are
matched without line numbers so edits above them do not make them new; secret findings keep their line.

Without a baseline every failure is new. A baseline round records results as they are.

## Results

| Status | Meaning |
|---|---|
| `passed` | nothing beyond the baseline |
| `failed` | class `quality` (fix the code), `policy` (diff policy or secret), or `environment` |
| `timeout` | a command was killed at its timeout (class `timeout`) |
| `error` | ACO could not read a scanner's output (class `internal`) |
| `not_applicable` | justified in the frozen Phase Specification |
| `skipped` | after an earlier gate failed (fail-fast) |

`environment` means a program was not found (exit 127, shell "not found" messages, Windows "is not recognized") or,
when no test failed, a required service was unreachable (connection refused, Docker daemon, DNS failure). Triage
(§11.7) blocks the phase on it instead of asking Claude to fix code.

`Result.Compact` bounds a result for the reviewer context pack and reports (failing tests only, at most 200 items
per list, logs cut with `Trim`). `StateRecords` maps a round onto `gate_results` rows (`timeout` is stored as
`error`; skipped gates are not stored).
