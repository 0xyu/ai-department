package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/0xyu/aco/internal/exitcode"
	"github.com/0xyu/aco/internal/i18n"
	"github.com/0xyu/aco/internal/plan"
)

// planCheckKeys maps each plan diagnostic code to its catalog message.
var planCheckKeys = map[plan.Code]i18n.Key{
	plan.CodeParseError:            i18n.PlanCheckParseError,
	plan.CodeMissingFile:           i18n.PlanCheckMissingFile,
	plan.CodeNoPhases:              i18n.PlanCheckNoPhases,
	plan.CodePhaseNumbering:        i18n.PlanCheckPhaseNumbering,
	plan.CodePhaseFileInvalid:      i18n.PlanCheckPhaseFileInvalid,
	plan.CodeDuplicateID:           i18n.PlanCheckDuplicateID,
	plan.CodeInvalidID:             i18n.PlanCheckInvalidID,
	plan.CodeDuplicateSection:      i18n.PlanCheckDuplicateSection,
	plan.CodeMissingTitle:          i18n.PlanCheckMissingTitle,
	plan.CodeMissingGoal:           i18n.PlanCheckMissingGoal,
	plan.CodeMissingScope:          i18n.PlanCheckMissingScope,
	plan.CodeMissingAC:             i18n.PlanCheckMissingAC,
	plan.CodeMissingDeliverables:   i18n.PlanCheckMissingDeliverables,
	plan.CodeDependencyInvalid:     i18n.PlanCheckDependencyInvalid,
	plan.CodeUnknownDependency:     i18n.PlanCheckUnknownDependency,
	plan.CodeForwardDependency:     i18n.PlanCheckForwardDependency,
	plan.CodeDependencyCycle:       i18n.PlanCheckDependencyCycle,
	plan.CodeMissingGateEntry:      i18n.PlanCheckMissingGateEntry,
	plan.CodeUnknownGateType:       i18n.PlanCheckUnknownGateType,
	plan.CodeDuplicateGateEntry:    i18n.PlanCheckDuplicateGateEntry,
	plan.CodeGateEntryInvalid:      i18n.PlanCheckGateEntryInvalid,
	plan.CodeGateNAUnjustified:     i18n.PlanCheckGateNAUnjustified,
	plan.CodeGateEntryConflict:     i18n.PlanCheckGateEntryConflict,
	plan.CodeNoRequirements:        i18n.PlanCheckNoRequirements,
	plan.CodeRequirementKind:       i18n.PlanCheckRequirementKind,
	plan.CodeRequirementUncovered:  i18n.PlanCheckRequirementUncovered,
	plan.CodeUnknownReference:      i18n.PlanCheckUnknownReference,
	plan.CodeTraceMismatch:         i18n.PlanCheckTraceMismatch,
	plan.CodePhaseTooManyAC:        i18n.PlanCheckPhaseTooManyAC,
	plan.CodePhaseSpecTooLong:      i18n.PlanCheckPhaseSpecTooLong,
	plan.CodeFrontMatterInvalid:    i18n.PlanCheckFrontMatterInvalid,
	plan.CodeLockMismatch:          i18n.PlanCheckLockMismatch,
	plan.CodeLockUnverified:        i18n.PlanCheckLockUnverified,
	plan.CodeAgentsMDNotRegular:    i18n.PlanCheckAgentsMDNotRegular,
	plan.CodeUnsupportedPlanFormat: i18n.PlanCheckUnsupportedVersion,
	plan.CodePathNotContained:      i18n.PlanCheckPathNotContained,
	plan.CodeTraceACOutsidePhases:  i18n.PlanCheckTraceACOutside,
	plan.CodeEmptyAC:               i18n.PlanCheckEmptyAC,
}

var planKindKeys = map[plan.SourceKind]i18n.Key{
	plan.SourceDir:      i18n.PlanKindDir,
	plan.SourceMarkdown: i18n.PlanKindMarkdown,
	plan.SourceYAML:     i18n.PlanKindYAML,
	plan.SourceJSON:     i18n.PlanKindJSON,
}

// diagMsg renders a plan diagnostic in the operator language.
func diagMsg(d plan.Diagnostic) i18n.Msg {
	key, ok := planCheckKeys[d.Code]
	if !ok {
		return i18n.M(i18n.CLIRawLine, d.Message)
	}
	args := make([]any, len(d.Args))
	for i, a := range d.Args {
		args[i] = a
	}
	return i18n.M(key, args...)
}

func runValidatePlan(inv *invocation) int {
	switch {
	case len(inv.positional) == 0:
		inv.errOut.Say(i18n.ValidatePlanNeedPath)
		inv.errOut.Say(i18n.CLIErrHelpHint, inv.cmd.name)
		return exitcode.Usage
	case len(inv.positional) > 1:
		inv.errOut.Say(i18n.CLIErrUnexpectedArgs, echoArgs(inv.redactor, inv.positional[1:]))
		return exitcode.Usage
	}
	if inv.loadErr != nil {
		renderConfigError(inv.errOut, inv.redactor, inv.loadErr)
		return exitcode.Usage
	}
	target := inv.positional[0]
	if !filepath.IsAbs(target) {
		target = filepath.Join(inv.sys.cwd(), target)
	}
	repo := inv.str("repo")
	if repo != "" && !filepath.IsAbs(repo) {
		repo = filepath.Join(inv.sys.cwd(), repo)
	}
	pkg, err := plan.Load(target, plan.Options{RepoRoot: repo})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			inv.errOut.Say(i18n.ValidatePlanNotFound, inv.positional[0])
		} else {
			inv.errOut.Say(i18n.ValidatePlanReadErr, inv.positional[0], err.Error())
		}
		return exitcode.Usage
	}
	strict := inv.flagBool("strict")
	rep := plan.ValidateFull(pkg, plan.DefaultCheckOptions(), strict)
	inv.log.Debug("plan validated", "kind", string(pkg.Source.Kind), "phases", len(pkg.Phases), "valid", rep.Valid)

	if inv.flagBool("json") {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			inv.errOut.Say(i18n.CLIErrInternal, err.Error())
			return exitcode.Failed
		}
		inv.out.Say(i18n.CLIRawLine, string(b))
	} else {
		renderPlanReport(inv.out, rep)
	}
	if !rep.Valid {
		return exitcode.Failed
	}
	return exitcode.Completed
}

func renderPlanReport(p *i18n.Printer, r plan.Report) {
	p.Say(i18n.ValidatePlanHeader, r.Source.Name, i18n.M(planKindKeys[r.Source.Kind]))
	if r.Title != "" {
		p.Say(i18n.ValidatePlanTitle, r.Title)
	}
	p.Say(i18n.ValidatePlanStats, r.Summary.Phases, r.Summary.Requirements, r.Summary.AcceptanceCriteria)
	p.Say(i18n.ValidatePlanHash, r.PlanHash)
	if r.Source.Kind == plan.SourceDir {
		if r.AgentsMD != nil {
			p.Say(i18n.ValidatePlanAgents, r.AgentsMD.Path, r.AgentsMD.SHA256)
		} else {
			p.Say(i18n.ValidatePlanAgentsNone)
		}
	}
	if r.Lock != nil {
		if r.Lock.Verified {
			p.Say(i18n.ValidatePlanLockOK, r.Lock.PlanVersion)
		} else {
			p.Say(i18n.ValidatePlanLockBad)
		}
	}
	if len(r.Phases) > 0 {
		p.Say(i18n.ValidatePlanPhasesHeader)
		for _, ph := range r.Phases {
			var deps any = i18n.M(i18n.ValidatePlanDepsNone)
			if len(ph.DependsOn) > 0 {
				deps = strings.Join(ph.DependsOn, ", ")
			}
			p.Say(i18n.ValidatePlanPhaseRow, ph.ID, ph.Title, ph.AcceptanceCriteria, deps)
		}
	}
	if len(r.Diagnostics) > 0 {
		p.Say(i18n.ValidatePlanDiagHeader)
		for _, d := range r.Diagnostics {
			sev := i18n.PlanSeverityWarning
			if d.Severity == plan.SeverityError {
				sev = i18n.PlanSeverityError
			}
			p.Say(i18n.ValidatePlanDiagRow, i18n.M(sev), d.Location.String(), string(d.Code), diagMsg(d))
		}
	}
	if r.Strict {
		p.Say(i18n.ValidatePlanStrictNote)
	}
	if r.Valid {
		p.Say(i18n.ValidatePlanResultOK, r.Summary.Warnings)
	} else {
		p.Say(i18n.ValidatePlanResultFail, r.Summary.Errors, r.Summary.Warnings)
	}
}
