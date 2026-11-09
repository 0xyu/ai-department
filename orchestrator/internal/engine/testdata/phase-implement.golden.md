# Task: implement phase 1 — Operation 1 (attempt 1)

Implement this phase in the current worktree, following the frozen plan. This is a fresh session; nothing from earlier phases is carried over except what is in the repository and the digests below.

## Plan overview (frozen)
<data name="plan_overview">
# Toy calculator

## Goals

A tiny calculator.

## Architecture

One package.
</data>

## Phase Specification (frozen, verbatim)
<data name="phase_spec">
## Phase 1: Operation 1

### Goal
Implement operation 1.

### Scope
- operation 1
- Out of scope: UI
- Paths: `calc/**`

### Technical Requirements
- standard library only

### Acceptance Criteria
- AC-01.1: Operation 1 returns the right result for positive inputs.
- AC-01.2: Operation 1 is covered by a table test.

### Deliverables
- calc/calc.go
- calc/calc_test.go

### Depends On
None

### Requirements
- REQ-001: AC-01.1

### Quality Gates
- typecheck: `go vet ./...`
- lint: `gofmt -l .`
- unit: `go test ./...`
- integration: not_applicable — no external services
- build: `go build ./...`
- security: scanners: secrets
</data>

## AGENTS.md
<data name="agents_md">
# Existing conventions
</data>

## Completed phases
None: this is the first phase.

## Scope
Change only files matching: calc/** (and tests for them). Changes elsewhere are flagged or blocked.
Protected (read-only): .aco/**, AGENTS.md, .github/**, .aco.yaml, **/*.pem, .env*

## Quality gates you will be judged by (run by the orchestrator)
- typecheck: go vet ./...
- lint: gofmt -l .
- unit: go test ./...
- integration: not applicable
- build: go build ./...
- security: diff policy, anti-gaming checks, and secret scan of the phase diff (built in)

You may run these commands yourself while working.

## Report
End with the implementation report as the structured output. Assess every acceptance criterion: AC-01.1, AC-01.2. finding_responses is empty for this first implementation.
