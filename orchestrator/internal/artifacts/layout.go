package artifacts

import "fmt"

// Run directory layout (all paths relative to <root>/runs/<run-id>/):
//
//	schemas/<name>.schema.json                 schemas handed to the agent CLIs
//	planning/round-RR/<file>                   Stage 1 prompts, transcripts, plan drafts
//	plan-review/round-RR/<file>                Stage 2 context packs, reviews, transcripts
//	phases/PNN/aK/baseline/<file>              baseline gate logs of attempt K
//	phases/PNN/aK/steps/sSSSS-<kind>/<file>    per-step prompts, transcripts, reports
//	phases/PNN/aK/gates/rRR/<gate>.log         gate logs of round RR
//	phases/PNN/aK/reviews/rRR/<file>           review context, result, transcript
//	phases/PNN/aK/diffs/rRR/<file>             full.patch, incremental.patch, stat.txt
//	final/<file>                               Stage 4 logs and review
//	reports/<file>                             Blocked / Failure Reports
//
// Two-digit counters keep listings sorted; they grow to more digits when a
// counter passes 99 (the paths stay valid).

// Planning returns planning/round-RR/<file>.
func Planning(round int, file string) string {
	return fmt.Sprintf("planning/round-%02d/%s", round, file)
}

// PlanReview returns plan-review/round-RR/<file>.
func PlanReview(round int, file string) string {
	return fmt.Sprintf("plan-review/round-%02d/%s", round, file)
}

// Attempt returns phases/PNN/aK.
func Attempt(phase, attempt int) string { return fmt.Sprintf("phases/P%02d/a%d", phase, attempt) }

// Baseline returns the baseline file of a phase attempt.
func Baseline(phase, attempt int, file string) string {
	return Attempt(phase, attempt) + "/baseline/" + file
}

// StepFile returns phases/PNN/aK/steps/sSSSS-<kind>/<file>.
func StepFile(phase, attempt int, stepID int64, kind, file string) string {
	return fmt.Sprintf("%s/steps/s%04d-%s/%s", Attempt(phase, attempt), stepID, kind, file)
}

// GateLog returns phases/PNN/aK/gates/rRR/<gate>.log.
func GateLog(phase, attempt, round int, gate string) string {
	return fmt.Sprintf("%s/gates/r%02d/%s.log", Attempt(phase, attempt), round, gate)
}

// ReviewFile returns phases/PNN/aK/reviews/rRR/<file>.
func ReviewFile(phase, attempt, round int, file string) string {
	return fmt.Sprintf("%s/reviews/r%02d/%s", Attempt(phase, attempt), round, file)
}

// DiffFile returns phases/PNN/aK/diffs/rRR/<file>.
func DiffFile(phase, attempt, round int, file string) string {
	return fmt.Sprintf("%s/diffs/r%02d/%s", Attempt(phase, attempt), round, file)
}

// Final returns final/<file>.
func Final(file string) string { return "final/" + file }

// ReportFile returns reports/<file>.
func ReportFile(file string) string { return "reports/" + file }

// SchemaFile returns schemas/<name>.
func SchemaFile(name string) string { return "schemas/" + name }
