// Package schemas embeds ACO's versioned JSON Schemas (schemas/*.json), so
// the binary can hand them to the agent CLIs (--json-schema,
// --output-schema) and tests can check them against the Go types.
package schemas

import _ "embed"

// Plan is plan.schema.json: the structured plan package form.
//
//go:embed plan.schema.json
var Plan []byte

// Event is event.schema.json: one aco.event/v1 run event.
//
//go:embed event.schema.json
var Event []byte

// ImplReport is impl-report.schema.json: the implementer's structured
// report at the end of every Stage 3/4 invocation.
//
//go:embed impl-report.schema.json
var ImplReport []byte

// PlanReview is plan-review.schema.json: the Stage 2 reviewer's answer
// (Codex --output-schema).
//
//go:embed plan-review.schema.json
var PlanReview []byte

// CodeReview is code-review.schema.json: the Stage 3 and Stage 4
// reviewer's answer (Codex --output-schema).
//
//go:embed code-review.schema.json
var CodeReview []byte

// PhaseReport is phase-report.schema.json: the Phase Completion Report
// committed with each phase (.aco/reports/phase-NN.json).
//
//go:embed phase-report.schema.json
var PhaseReport []byte
