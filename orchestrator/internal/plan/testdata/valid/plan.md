---
implementer:
  effort: high
---
# Todo Service — Implementation Plan

A small HTTP service that stores todo items.

## Requirements

### Functional

- REQ-001: Users can create a todo item with a title.
- REQ-002: Users can list todo items.

### Non-functional

- REQ-003: Every endpoint answers within 200 ms at the 95th percentile.

### Constraints

| ID | Kind | Text |
|---|---|---|
| REQ-004 | constraint | Pure Go, no CGO. |

## Phase 1: Storage Layer

### Goal
Persist todo items in memory behind an interface.

### Scope
- `store` package with a `Store` interface and an in-memory implementation.
- Out of scope: persistence to disk.
- Paths: `internal/store/**`, `go.mod`

### Technical Requirements
- Safe for concurrent use.

### Acceptance Criteria
- AC-01.1: `Create` returns the new item with an ID.
- AC-01.2: `List` returns items in creation order.
- Concurrent `Create` calls from 100 goroutines all succeed:
  1. no item is lost;
  2. IDs are unique.

### Deliverables
- `internal/store`

### Requirements
- REQ-001: AC-01.1
- REQ-002: AC-01.2
- REQ-004

### Quality Gates
- typecheck: `go vet ./...`
- lint: `staticcheck ./...`
- unit: `go test ./... -short -json`
- integration: not_applicable — the store has no external dependencies
- build: required
- security: scanners: secrets, govulncheck

## Phase 2: HTTP API

### Goal
Expose the store over HTTP.

### Scope
- `POST /todos` and `GET /todos` handlers.
- Paths: `internal/api/**`, `cmd/todo/**`

### Acceptance Criteria
- `POST /todos` with a title returns 201 and the item.
- `GET /todos` returns all items as JSON.
- A load test shows p95 latency under 200 ms.

### Deliverables
- `internal/api`
- `cmd/todo`

### Depends On
Phase 1

### Requirements
- REQ-001
- REQ-002
- REQ-003: AC-02.3

### Quality Gates
- typecheck: `go vet ./...`
- lint: `staticcheck ./...`
- unit: `go test ./... -short -json`
- integration: `go test -tags=integration ./... -json`
- build: `go build ./...`
- security: `gosec ./...`; scanners: secrets, gosec

## Phase 3: Documentation

### Goal
Document the API.

### Scope
- README with request examples.

### Acceptance Criteria
- AC-03.1: README lists both endpoints with curl examples.
- AC-03.2: README states the latency target.
- AC-03.3: Examples in the README are checked by a test.

### Deliverables
- `README.md`

### Depends On
- Phase 1
- Phase 2

### Requirements
- REQ-003: AC-03.2

### Quality Gates
- typecheck: required
- lint: required
- unit: `go test ./... -run Example`
- integration: not applicable (documentation only)
- build: required
- security: scanners: secrets
