# ACO configuration

ACO reads configuration in layers. Each layer overrides the ones before it:

| # | Layer | Source | Missing file |
|---|---|---|---|
| 1 | defaults | built in (`internal/config/defaults.go`) | — |
| 2 | user | `~/.aco/config.yaml` | skipped |
| 3 | repo | `<repo>/.aco.yaml` (`--repo`, default: current directory) | skipped |
| 4 | file | each `--config <file>`, in order | **error** |
| 5 | plan | YAML front matter of the plan (Phase 2 wires this up) | — |
| 6 | flags | each `--set key.path=value`, in order | — |

`--set` values are parsed as YAML, so `--set git.push=true`, `--set limits.review_rounds=7`, and
`--set gates.lint.commands="[golangci-lint run]"` keep their types.

Lists are replaced, not merged. Maps (`pricing.codex`) are merged by key. A `null` value keeps the lower layer's value.

Each config file (and the plan front matter, and each `--set` value) must hold exactly one YAML document. A second
document (`---` followed by more YAML) is a `parse` error with its line.

Errors from `--set` name the option and key path (`--set auth.mode`), never the value. A malformed `--set`, or one
whose key is not a known schema path (an unknown field or a map key such as a `pricing.codex` model), is named by its
position (`--set #2`), since the key itself might be a credential. Unknown keys in field paths show as `[hidden]`
(`planner.[hidden]`), with the file and line. Rejected values in other errors are
repeated only when short and plain (`subscriptoin`, `extreme`); anything else shows as `[hidden]`. See
[security.md](security.md).

## Validation

All config errors are reported at once, each with the layer, file, line, and dotted field path.
`aco` exits with code **2** on any config error.

| Code | Meaning |
|---|---|
| `parse` | YAML syntax error |
| `unknown_field` | key not in the schema (unknown keys are always errors) |
| `unknown_gate_type` | `gates.<x>` where `x` is not one of the six gate types |
| `secret_field` | credential-like key (`api_key`, `token`, `secret`, `password`, …). Secrets never go in config files |
| `invalid_type` | wrong YAML type (e.g. a string where an integer is expected) |
| `invalid_value` | value not in the allowed set |
| `out_of_range` | number outside its range; `.nan`, `.inf`, and `-.inf` are always out of range |
| `invalid_duration` | not a Go duration (`60m`, `4h`, `72h`) |
| `unsupported_version` | `version` is not `1` |
| `required` | empty value where one is required |
| `file_not_found` | a `--config` file does not exist |
| `invalid_override` | `--set` not in `key.path=value` form |

## Schema (version 1)

```yaml
version: 1                          # must be 1 if present

planner:                            # Stage 1 (Claude Code)
  kind: claude-code                 # claude-code | fake
  model: claude-opus-5-5
  effort: high                      # low | medium | high | xhigh | max
implementer:                        # Stages 3–4 (Claude Code)
  kind: claude-code                 # claude-code | fake
  model: claude-opus-5-5
  effort: medium                    # low | medium | high | xhigh | max
  strict_effort: true               # fail preflight if the CLI cannot set the effort
  extra_allowed_bash: []            # extra Bash command prefixes allowed to the implementer
reviewer:                           # GPT Sol 6.1 via Codex CLI
  kind: codex-cli                   # codex-cli | fake
  model: ""                         # Codex model ID for GPT Sol 6.1 — required before any review; `aco doctor` warns while empty
  fallback_model: ""
  reasoning_effort: high            # minimal | low | medium | high | xhigh
  final_review: true

auth:                               # defaults only; flags and env override (see below). No key fields exist.
  mode: subscription                # subscription | api
  claude: ""                        # "" | subscription | api   (per-agent override)
  codex: ""                         # "" | subscription | api
  fallback: none                    # none | api
  env_file: ~/.aco/.env

pricing:                            # API mode only: USD per million tokens, by Codex model ID
  codex: {}                         # e.g. {"<model-id>": {input_per_mtok: 0, output_per_mtok: 0}}

language:
  operator: zh-CN                   # zh-CN | en — CLI output, doctor, error messages
  documents: en                     # en | zh-CN — plans, reports, prompts, commit messages

gates:                              # exactly these six gate types (plus auto_detect); see quality-gates.md
  typecheck:   {commands: [], timeout: 0s}   # timeout per command; 0s = 30m
  lint:        {commands: [], timeout: 0s}
  unit:        {commands: [], timeout: 0s}
  integration: {commands: [], timeout: 0s}
  build:       {commands: [], timeout: 0s}
  security:
    commands: []
    scanners: [secrets]             # secrets | gosec | govulncheck | semgrep | npm-audit | pip-audit | trivy
    fail_severity: high             # low | medium | high | critical
    timeout: 0s
  auto_detect: true

policy:
  protected_paths: [".aco/**", "AGENTS.md", ".github/**", ".aco.yaml", "**/*.pem", ".env*"]
  scope_enforcement: flag           # flag | block
  tests_missing: fail               # warn | fail
  anti_gaming: fail                 # warn | fail
  allow_unverifiable_ac: false
  network: allowlist                # deny | allowlist | allow
  max_file_bytes: 5242880           # > 0; a phase diff that adds or changes a larger file is rejected
  binary_files: warn                # warn | fail — binary files in a phase diff

git:
  tag: true
  notes: true
  push: false
  open_pr: false
  merge_to_base: none               # none | ff-only

sandbox:
  mode: host                        # host | docker

limits:                             # §14.3 of the implementation plan
  plan_review_rounds: 4             # all counts >= 1 except phase_restarts / final_fix_rounds (>= 0)
  gate_fix_attempts: 4
  review_rounds: 5
  phase_restarts: 1
  final_fix_rounds: 2
  max_reopen: 2
  new_blocker_cutoff_round: 3
  policy_violations: 5
  claude_max_turns: 200
  invocation_timeout: 60m           # durations > 0
  idle_timeout: 10m
  phase_timeout: 4h
  run_timeout: 72h
  agent_calls: {run: 600, planning: 20, phase: 40, final: 20, soft_ratio: 0.8}   # soft_ratio in (0, 1]
  claude_turns: {run: 20000, phase: 1500}
  budget_usd: {run: 250, planning: 20, phase: 40, final: 30, soft_ratio: 0.8}   # api-mode agents only; >= 0
  max_concurrent_runs: 1
  max_pause: 12h
```

## Authentication mode

Both models are always called through their CLIs. The auth mode decides how each CLI authenticates and is billed.

### Environment variables

| Variable | Values | Meaning |
|---|---|---|
| `ACO_AUTH_MODE` | `subscription` \| `api` | mode for both agents |
| `ACO_CLAUDE_AUTH_MODE` | `subscription` \| `api` | Claude Code override |
| `ACO_CODEX_AUTH_MODE` | `subscription` \| `api` | Codex CLI override |
| `ACO_AUTH_FALLBACK` | `none` \| `api` | on a subscription usage limit, continue in API mode |
| `ANTHROPIC_API_KEY` | key | required when Claude Code is in API mode (or fallback is `api`) |
| `OPENAI_API_KEY` | key | required when Codex CLI is in API mode (or fallback is `api`) |

Empty values count as unset. Any other value is an error that names the variable (exit code 2) — even when a
higher-precedence source would win, so typos are never silently ignored.

### Resolution order (per agent, highest first)

1. `--claude-auth-mode` / `--codex-auth-mode`
2. `--auth-mode`
3. `ACO_CLAUDE_AUTH_MODE` / `ACO_CODEX_AUTH_MODE`
4. `ACO_AUTH_MODE`
5. `auth.claude` / `auth.codex` (config, set by any non-default layer)
6. `auth.mode` (config, set by any non-default layer)
7. default: `subscription`

Fallback: `ACO_AUTH_FALLBACK` > `auth.fallback` > `none`.

`aco doctor` prints the resolved mode for each agent and where it came from.

### Env file

Variables are read from the process environment and from the env file. The process environment wins for the same
variable. The env file is `auth.env_file` (default `~/.aco/.env`); a missing default file is fine. `--env-file <file>`
selects another file, which must exist.

Syntax: `KEY=VALUE` per line, `#` comments, optional `export ` prefix, `'single'` (literal) and `"double"` (with
`\n \t \" \\` escapes) quotes, and ` # comment` after unquoted values. Syntax errors give the line number, never the
line content. On Unix, `aco doctor` warns when the file is readable by group or others.

### Where API keys may come from

Only the process environment, the env file, or the OS keychain (keychain lookup is an extension point;
no backend ships yet). Config files cannot hold keys: the schema has no key fields, and credential-like key names
are rejected with `secret_field`.

## Logging

Logs go to stderr in English. Messages, keys, group names, and string values are masked (secret environment
variables, including those from the env file, and credential patterns), and only allowlisted value types are written;
anything else appears as `[UNLOGGABLE <type>]`. See [security.md](security.md) for the full rules.

| Variable | Values | Default |
|---|---|---|
| `ACO_LOG_LEVEL` | `debug` \| `info` \| `warn` \| `error` | `warn` |
| `ACO_LOG_FORMAT` | `text` \| `json` | `text` |
