# ACO plan package format (v1)

The plan package is returned as structured output (plan.schema.json). ACO writes it to `.aco/plan/` as
`IMPLEMENTATION_PLAN.md`, `requirements.md`, `traceability.json`, and `phases/PNN.md`.

## Requirements

- Number every requirement `REQ-001`, `REQ-002`, … and classify it: `functional`, `non_functional`, or `constraint`.
- Take requirements from the requirement document only. Do not invent requirements that were not asked for.

## Phases

- Phases are numbered 1..N without gaps. Each phase must be small enough for one implementation session and
  independently verifiable.
- Each phase has: Title, Goal, Scope (in scope, out of scope, optional `scope.paths` globs), Technical Requirements,
  Acceptance Criteria, Deliverables, Depends On, Requirements covered, and Quality Gates.
- Acceptance criteria get IDs `AC-NN.M` (NN = phase number, M = 1..). Each one must be specific and testable by the
  phase's declared gates. Every phase has at least one.
- `Depends On` may only name earlier phases. No cycles.

## Quality gates

Every phase declares all six gate types, in this order: `typecheck`, `lint`, `unit`, `integration`, `build`,
`security`. For each gate give the commands to run, or mark it `not_applicable` with a justification. Leaving the
commands empty lets ACO use the configured or auto-detected commands for the repository. The security gate lists its
scanners (for example `secrets`, `gosec`, `govulncheck`, `npm-audit`, `pip-audit`, `semgrep`).

## Traceability

`traceability` maps every requirement to at least one phase and at least one acceptance criterion of those phases.
A requirement may only list acceptance criteria of the phases it lists.

## AGENTS.md

When the repository has no AGENTS.md, propose one in `agents_md`: project conventions, build and test commands,
coding rules, test rules, and forbidden actions. When the repository has one, leave `agents_md` empty.

## Checks ACO runs (deterministic, not negotiable)

Package parses; phase numbers contiguous; IDs unique; dependencies acyclic and pointing to earlier phases; every
phase has a Scope, Deliverables, at least one AC, and a gate entry for each of the six gate types (a
`not_applicable` entry needs a justification); every REQ appears in traceability with at least one valid phase/AC
pair; every AC referenced exists. Phases with many acceptance criteria or very long specifications get warnings.
