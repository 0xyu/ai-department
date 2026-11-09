package prompt

import "github.com/0xyu/aco/internal/agent"

// Reviewer templates (Codex CLI has no system prompt flag: the rules are
// the first section of the context pack on stdin).
const (
	ReviewerRules    = "reviewer_rules"
	PlanReview       = "plan_review"
	PlanReReview     = "plan_rereview"
	CodeReview       = "code_review"
	CodeReReview     = "code_rereview"
	FinalReview      = "final_review"
	ReviewCorrection = "review_correction"
)

// PlanReviewPack is the data of a plan review context pack (§10.3).
type PlanReviewPack struct {
	Round        int
	ArtifactDir  string // absolute; the reviewer may read below it
	Requirements string
	Files        []agent.PackFile
	ReqIDs       []string
	RepoOverview string
	AgentsMD     string
	CheckResults string
	Previous     []agent.PlanFindingRecord
	// Omitted lists files that did not fit the size cap, with the
	// artifact path where the reviewer can read them.
	Omitted []OmittedFile
}

// OmittedFile is content moved out of the pack into an artifact file.
type OmittedFile struct {
	What string
	Path string // absolute
}

// CodeReviewPack is the data of a code review context pack (§11.6, §12.5).
type CodeReviewPack struct {
	Phase        int
	Attempt      int
	Round        int
	ArtifactDir  string
	PlanOverview string
	Phases       []agent.PhaseStatus
	PhaseSpec    string
	Requirements string
	AgentsMD     string
	ACIDs        []string

	DiffStat string
	// Diff is the full diff, or only the highest-risk hunks when the full
	// diff does not fit (DiffPartial; the full patch is FullPatchPath).
	Diff            string
	DiffPartial     bool
	FullPatchPath   string
	IncrementalDiff string
	IncrementalPath string // set when the incremental diff did not fit

	GateResults string
	Baseline    string
	TestChanges string
	Regressions string
	Security    string
	ChangedAPIs string
	Importers   string
	ImplReport  string
	Ledger      []agent.LedgerEntry
	Omitted     []OmittedFile
}

// Correction asks the reviewer to fix a schema-invalid answer.
type Correction struct {
	Problems []string
	Answer   string // the rejected answer (masked, shortened)
}
