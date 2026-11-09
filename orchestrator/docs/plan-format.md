# Plan format (`internal/plan`)

A **plan package** is what Stage 1 produces and Stage 2 reviews: requirements, the Implementation Plan, one Phase
Specification per phase, and traceability from requirements to acceptance criteria. ACO reads it in three forms. All
three give the same model (`Package.Canonical()`), except raw text and source locations.

| Form | Input | Typical use |
|---|---|---|
| Package directory | `<repo>/.aco/plan/` | The frozen plan in the run branch |
| Single Markdown file | any `.md` | A plan written by hand and passed with `--plan` |
| Structured document | `.yaml`, `.yml`, `.json` ([schemas/plan.schema.json](../schemas/plan.schema.json)) | Stage 1 structured output |

`plan.Load(path, opts)` picks the form from the path. It returns an error only for I/O problems; every content problem
becomes a structured diagnostic (below), so it can be handed to the reviewer or back to the planner.

## Package directory

```
.aco/plan/
  IMPLEMENTATION_PLAN.md   optional YAML front matter, "# Title", overview (architecture, risks, phase list)
  requirements.md          requirement list (REQ-NNN)
  traceability.json        REQ -> phases -> AC
  phases/P01.md … PNN.md   one Phase Specification each
  plan-review.md           Stage 2 summary (hashed, not parsed)
  plan.lock.json           freeze manifest (not part of the package hash)
```

`IMPLEMENTATION_PLAN.md`, `requirements.md`, and `traceability.json` are required (`missing_file`). Phase file names
must be `PNN.md` and each must hold exactly one phase heading with the same number (`phase_file_invalid`). Phase
sections inside `IMPLEMENTATION_PLAN.md` are ignored in this form. When the directory is `<repo>/.aco/plan`, the
repository root is inferred and `AGENTS.md` is discovered there.

`traceability.json`:

```json
{
  "version": 1,
  "entries": [
    {"requirement": "REQ-001", "phases": ["P01", "P02"], "acceptance_criteria": ["AC-01.1", "AC-02.1"]}
  ]
}
```

Phase references may be `3`, `"3"`, `"P03"`, or `"Phase 3"` everywhere.

## Single Markdown file

An optional front matter block, a `# Title`, the overview, requirement definitions anywhere outside phase sections,
and phase sections. This document itself ([AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md](../AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md))
is a single-file plan: it parses into 14 phases.

```markdown
---
implementer:
  effort: high          # front matter is the "plan" config layer (same schema as .aco.yaml)
---
# Todo Service — Implementation Plan

## Requirements
### Functional
- REQ-001: Users can create a todo item.        # kind from the heading
- REQ-002 (non_functional): p95 under 200 ms.   # or an explicit (kind) / [kind]
| REQ-003 | constraint | Pure Go, no CGO. |      # table rows work too

## Phase 1: Storage Layer
### Goal
### Scope
### Technical Requirements
### Acceptance Criteria
### Deliverables
### Depends On
### Requirements
### Quality Gates
```

### Phase sections

A phase starts at a heading `Phase N: Title` (any level; `:`, `.`, `-`, `—` all work) and ends at the next heading of
the same or a higher level. Its sub-sections are the headings one level below (the first deeper heading sets that
level). Headings inside fenced code blocks are ignored. Unknown sub-sections are allowed and kept in the raw text.

| Section | Content | Model |
|---|---|---|
| Goal | paragraph(s) | `Goal` |
| Scope | list items; `Out of scope:` items (or an "Out of scope" sub-heading) are out of scope; `Paths:` items followed by globs in code spans (or a "Paths" sub-heading) are `scope.paths` | `Scope.In/Out/Paths` |
| Technical Requirements | list items | `TechnicalRequirements` |
| Acceptance Criteria | top-level list items; nested lists stay in their item | `AcceptanceCriteria` |
| Deliverables | list items | `Deliverables` |
| Depends On | `Phase 2, Phase 4`, `P02`, a list, or `None` | `DependsOn` |
| Requirements | `- REQ-001`, `- REQ-001: AC-01.1, AC-01.2`, or grouped `- REQ-001, REQ-004: AC-01.2` | `Requirements` (+ traceability) |
| Quality Gates | one item per gate type (below) | `Gates` |

A section with no list is read as paragraphs. Item text keeps nested lines, with the item's own indentation removed.

**Acceptance criteria IDs.** An item may start with an explicit ID (`AC-01.2: …`, `**AC-01.2** …`, `AC-01.2 — …`).
Without one, the item gets `AC-NN.<position>`. IDs are canonical (`AC-03.2`, `REQ-004`, `P03`) on every OS.

**Quality gates.** Every phase needs one entry for each of the six gate types (`typecheck`, `lint`, `unit`,
`integration`, `build`, `security`):

```markdown
- typecheck: `go vet ./...`                      # one or more commands in code spans
- unit: go test ./...                            # or one plain command
- build: required                                # required; command from config or auto-detection
- integration: not_applicable — no external services   # needs a justification
- security: `gosec ./...`; scanners: secrets, gosec    # scanners: secrets gosec govulncheck semgrep npm-audit pip-audit trivy
```

Aliases such as `Type Check`, `Unit Tests`, `Security Scanning` are accepted in Markdown; the structured form uses the
exact names. Only the gate label and the control words (`required`, `not_applicable`, `scanners:`) are normalized;
commands keep their spacing exactly (`` `sh -c 'echo "a   b"'` `` reaches the gate runner unchanged).

### Traceability in a single file

It is derived from each phase's Requirements section: a requirement maps to the AC listed next to it, or to all of the
phase's AC when none are listed (`Package.TraceDerived`). A phase may only list its own AC; listing another phase's AC
is an error (`trace_ac_outside_phases`).

An entry may name several requirements. The AC listed in it apply to **each** requirement in the entry, and each
pairing is checked against the declaring phase: `- REQ-004, REQ-001: AC-02.1` in Phase 1 gives two
`trace_ac_outside_phases` errors. An entry that lists no AC (`- REQ-004, REQ-003`) maps every listed requirement to
all of the phase's AC, as for a single requirement.

The implementation plan itself follows this format: §4.1 lists `REQ-001`–`REQ-030`, and every phase maps its
requirements to specific AC and declares all six gates, so `aco validate-plan` (also with `--strict`) accepts it.

## Structured form

See [schemas/plan.schema.json](../schemas/plan.schema.json) (embedded as `schemas.Plan`). `version: 1` is required,
unknown fields are errors, `config` holds front-matter config, `agents_md` holds a proposed AGENTS.md (Stage 1 only),
and each phase has `gates: {typecheck: {...}, …}` where an empty entry means "required". `Phase.Raw` for this form is
the phase rendered as Markdown (`RenderPhase`), so prompts always get Markdown.

`plan.WriteDir(dir, pkg)` writes any package as a package directory; loading it back gives the same model (tested
for all three forms). Phase 8 uses this to write Claude's structured output.

## Raw text and hashing

- `Phase.Raw` is the phase's Markdown exactly as written (byte-for-byte, original line endings), from its heading to
  its last non-blank line, without a trailing `---`. It is what prompts embed.
- File hashes (`HashText`) are SHA-256 of the text with a leading BOM removed and CRLF / CR turned into LF, written
  `sha256:<hex>`. The same plan hashes the same on Windows, macOS, and Linux.
- `Package.Hash()` combines the file hashes in `sha256sum` layout (`<hash>  <path>\n`, sorted by path).

## Deterministic checks (§10.4)

`plan.Validate` returns the parse diagnostics plus `plan.Check`; `plan.ValidateFull` adds the front matter check and
the freeze manifest check. Each diagnostic is structured:

```json
{"code": "missing_gate_entry", "severity": "error",
 "location": {"file": "phases/P01.md", "line": 30, "phase": "P01", "section": "quality_gates", "ref": "lint"},
 "message": "P01 has no entry for quality gate \"lint\""}
```

`message` is English (it goes to the agents); the CLI shows the same diagnostic in the operator language from the
i18n catalog (`plan.check.<code>`). Errors fail validation; warnings fail it only with `--strict`.

| Code | Severity | Meaning |
|---|---|---|
| `parse_error` | error | YAML/JSON syntax, unknown field, bad phase reference, non-regular file |
| `unsupported_version` | error | structured form or `traceability.json` not `version: 1` |
| `missing_file` | error | required package file missing |
| `phase_file_invalid` | error | phase file name/heading mismatch |
| `no_phases` | error | no phase sections |
| `phase_numbering` | error | phases not numbered 1..N in order |
| `duplicate_id` | error | duplicate phase number, REQ, AC, or traceability entry |
| `invalid_id` | error | malformed ID, AC ID of another phase, unknown requirement kind |
| `duplicate_section` | error | a phase sub-section appears twice |
| `missing_title` / `missing_scope` / `missing_acceptance_criteria` / `missing_deliverables` | error | required part missing |
| `missing_goal` | warning | no Goal |
| `dependency_invalid` | error | Depends On text not readable |
| `unknown_dependency` | error | depends on a phase that does not exist |
| `forward_dependency` | error | depends on the same or a later phase |
| `dependency_cycle` | error | cycle, e.g. `P01 → P02 → P01` |
| `missing_gate_entry` | error | one of the six gate types not declared |
| `unknown_gate_type` / `duplicate_gate_entry` / `gate_entry_invalid` | error | bad gate entry (incl. unknown scanner) |
| `gate_not_applicable_unjustified` | error | `not_applicable` without justification |
| `gate_entry_conflict` | error | `not_applicable` with commands or scanners |
| `no_requirements` | error | no REQ-NNN defined |
| `requirement_kind_missing` | warning | requirement without functional / non_functional / constraint |
| `requirement_uncovered` | error | REQ has no valid pair: an existing, non-empty AC whose own phase is in the trace's phases |
| `trace_ac_outside_phases` | error | a trace (explicit or derived) lists an AC whose phase is not in its phase list, or a phase lists another phase's AC |
| `empty_acceptance_criterion` | error | an AC with empty or whitespace-only text (e.g. only `AC-01.2:`); it never counts for coverage |
| `unknown_reference` | error | phase or traceability refers to an unknown REQ, phase, or AC |
| `trace_mismatch` | warning | explicit traceability and a phase's Requirements list disagree about which phases cover a REQ |
| `phase_too_many_ac` | warning | more than 15 AC (`CheckOptions.MaxAC`) |
| `phase_spec_too_long` | warning | phase Markdown over 24 KiB (`CheckOptions.MaxSpecBytes`) |
| `front_matter_invalid` | error | front matter is not a valid config layer |
| `lock_mismatch` | error | a frozen file differs from `plan.lock.json`, or the lock is malformed |
| `lock_unverified` | warning/error | lock present but cannot be checked (package not at `<repo>/.aco/plan`, I/O) |
| `agents_md_not_regular` | error | `AGENTS.md` is a symlink or other non-regular file |
| `path_not_contained` | error | `.aco`, `.aco/plan`, a directory or file inside it, or `plan.lock.json` is a symlink, junction, or other reparse point |

`Package.Graph()` exposes the dependency graph (`Dependencies`, `Ancestors`, `Cycles`) for the phase engine.

## Repository containment

A plan package at `<repo>/.aco/plan` must really live inside the repository. Every component is checked with `Lstat`,
never followed:

- `Load` refuses a package whose `.aco` or `.aco/plan` is a symlink or Windows junction **before reading anything**
  (one `path_not_contained` diagnostic, no files read). The root checked is the one the path itself names
  (`<root>/.aco/plan`), so an explicit `--repo` pointing elsewhere cannot skip the check. A plain plan directory that
  is not at `<root>/.aco/plan` has no such root and loads as usual. Inside the package, linked files and directories
  are reported and not read.
- For a rejected package, `CheckLock` does nothing: the manifest behind the link is never opened, so its content
  cannot produce diagnostics. For other packages, `CheckLock` checks `.aco`, `.aco/plan`, and `plan.lock.json` with
  `Lstat` before opening the manifest.
- `BuildLock`, `ReadLock`, `WriteLock`, and `VerifyLock` return a `*ContainmentError` (matching `ErrNotContained`)
  naming the offending path (`.aco`, `.aco/plan`, or `.aco/plan/plan.lock.json`) before reading or writing through it.
  `CheckLock` turns it into a `path_not_contained` diagnostic, also when `--repo` names a repository whose `.aco`
  links to the directory being validated.
- The repository root itself is trusted (it is what the caller named); the manifest-entry path rules below still apply.

## AGENTS.md and the freeze manifest (§10.5)

`DiscoverAgentsMD(repo)` reads `AGENTS.md` at the repository root (exact name first, then a case-insensitive match).
A symlink or other non-regular file is refused (`ErrNotRegular`), never followed.

`plan.lock.json`:

```json
{
  "format": "aco.plan.lock/v1",
  "plan_version": "v1",
  "plan_hash": "sha256:…",
  "run_id": "…",
  "reviewer_model": "…",
  "review_rounds": 2,
  "verdict": "pass",
  "frozen_at": "2026-11-08T09:00:00Z",
  "files": [{"path": ".aco/plan/IMPLEMENTATION_PLAN.md", "sha256": "sha256:…"}, {"path": "AGENTS.md", "sha256": "sha256:…"}]
}
```

- `BuildLock(repo, meta)` hashes every regular file under `.aco/plan/` (except the lock) and `AGENTS.md`;
  `WriteLock` writes it atomically.
- `ParseLock` rejects unknown fields and formats, malformed hashes, a `plan_hash` that does not match the file list,
  and any path that is absolute, contains `..`, `\`, or `:`, or lies outside `.aco/plan/` and `AGENTS.md`, so a
  tampered lock cannot make ACO read other files.
- `VerifyLock(repo, lock)` returns a `MismatchError` naming every file that is `modified`, `missing`, `unexpected`
  (a new file in the frozen area, including an `AGENTS.md` added after freeze), or `not_regular`. Line-ending
  changes alone do not count as modifications.

## `aco validate-plan`

```
aco validate-plan <file|dir> [--strict] [--json] [--repo <path>]
```

Runs `ValidateFull` and prints a summary, the phase list, and the problems in the operator language, or the report
as JSON (`aco.validate-plan/v1`, golden-tested in `internal/cli/testdata/validate-plan/`). `--repo` sets the
repository used for `AGENTS.md` and lock verification when it cannot be inferred. Exit codes: 0 valid, 1 invalid,
2 usage error or unreadable path.
