package reports

import (
	"fmt"
	"strings"
)

// cell makes s safe for one Markdown table cell.
func cell(s string) string {
	s = oneLine(s)
	if s == "" {
		return "-"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

func list(s []string) string {
	if len(s) == 0 {
		return "-"
	}
	return cell(strings.Join(s, ", "))
}

// RenderPhaseMarkdown is phase-NN.md: every item of §11.9 in readable form.
// The JSON report is the authoritative record.
func RenderPhaseMarkdown(r *PhaseReport) string {
	r.normalize()
	var b strings.Builder
	w := func(format string, a ...any) { b.WriteString(fmt.Sprintf(format, a...)) }

	w("# Phase %02d: %s\n\n", r.Phase, oneLine(r.Title))
	w("%s\n\n", strings.TrimSpace(r.Goal))
	w("| Item | Value |\n|---|---|\n")
	w("| Run | %s |\n", cell(r.RunID))
	w("| Plan | %s (%s) |\n", cell(r.PlanVersion), cell(r.PlanHash))
	w("| Base commit | %s |\n", cell(r.BaseCommit))
	w("| Reviewed tree | %s |\n", cell(r.ReviewedTree))
	w("| Attempt | %d of %d |\n", r.Attempt, r.Attempts)
	var sessions []string
	for _, s := range r.Sessions {
		sessions = append(sessions, fmt.Sprintf("a%d: %s", s.Attempt, s.SessionID))
	}
	w("| Claude sessions | %s |\n", list(sessions))
	w("| Implementer | %s |\n", cell(r.ImplementerModel))
	w("| Reviewer | %s |\n", cell(r.ReviewerModel))
	w("| Generated | %s |\n\n", cell(r.GeneratedAt))

	w("## Acceptance criteria\n\n| ID | Criterion | Status | Evidence | Tests |\n|---|---|---|---|---|\n")
	for _, a := range r.AcceptanceCriteria {
		w("| %s | %s | %s | %s | %s |\n", cell(a.ID), cell(a.Text), cell(a.Status), cell(a.Evidence), list(a.Tests))
	}
	w("\n## Requirements covered\n\n%s\n", list(r.Requirements))

	w("\n## Files changed\n\n")
	if len(r.Files) == 0 {
		w("None.\n")
	} else {
		w("| Status | Path | + | - |\n|---|---|---|---|\n")
		for _, f := range r.Files {
			p := f.Path
			if f.OldPath != "" && f.OldPath != f.Path {
				p = f.OldPath + " → " + f.Path
			}
			add, del := fmt.Sprint(f.Added), fmt.Sprint(f.Deleted)
			if f.Binary {
				add, del = "bin", "bin"
			}
			w("| %s | %s | %s | %s |\n", f.Status, cell(p), add, del)
		}
	}
	if s := strings.TrimSpace(r.DiffStat); s != "" {
		w("\n```\n%s\n```\n", strings.ReplaceAll(s, "```", "'''"))
	}

	w("\n## Quality gates\n\n| Gate | Status | Source | Commands | Duration | Baseline delta | Summary |\n|---|---|---|---|---|---|---|\n")
	for _, g := range r.Gates {
		d := g.Baseline
		delta := "no baseline"
		if d.HasBaseline {
			delta = fmt.Sprintf("new failing %d, regressions %d, fixed %d, pre-existing %d; new issues %d (pre-existing %d); new vulns %d (pre-existing %d)",
				d.NewFailingTests, d.Regressions, d.FixedTests, d.PreexistingFailures, d.NewIssues, d.PreexistingIssues, d.NewVulns, d.PreexistingVulns)
		}
		w("| %s | %s | %s | %s | %.1fs | %s | %s |\n", g.Gate, g.Status, cell(g.Source), cell(strings.Join(g.Commands, "; ")),
			float64(g.DurationMS)/1000, cell(delta), cell(g.Summary))
	}
	w("\nRegressions on the committed tree: %s\n\n", list(r.Regressions))
	w("Regressions detected during the phase (fixed before review): %s\n", list(r.RegressionsSeen))

	w("\n## Security\n\n- Scanners: %s\n- New findings: %s\n- Pre-existing findings: %s\n", list(r.Security.Scanners),
		list(r.Security.NewFindings), list(r.Security.Preexisting))
	if len(r.Security.Policy) > 0 {
		w("\n| Policy | Path | Line | Blocking | Message |\n|---|---|---|---|---|\n")
		for _, p := range r.Security.Policy {
			w("| %s | %s | %d | %v | %s |\n", cell(p.Code), cell(p.Path), p.Line, p.Blocking, cell(p.Message))
		}
	}

	w("\n## Review history\n\n")
	if len(r.Reviews) == 0 {
		w("No reviews.\n")
	} else {
		w("| Attempt | Round | Verdict | Gate | New findings | Gate reasons |\n|---|---|---|---|---|---|\n")
		for _, v := range r.Reviews {
			gate := "FAIL"
			if v.GatePassed {
				gate = "PASS"
			}
			w("| %d | %d | %s | %s | %s | %s |\n", v.Attempt, v.Round, v.Verdict, gate, list(v.NewFindings), list(v.Reasons))
		}
	}
	w("\n### Findings\n\n")
	if len(r.Findings) == 0 {
		w("None.\n")
	} else {
		w("| ID | Attempt | Severity | Category | Class | Status | Title | Response | Ruling | Reopened | Disputes |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, f := range r.Findings {
			resp := f.Response
			if f.ResponseDetail != "" {
				resp += ": " + f.ResponseDetail
			}
			w("| %s | %d | %s | %s | %s | %s | %s | %s | %s | %d | %d |\n", cell(f.ID), f.Attempt, f.Severity, cell(f.Category),
				f.ResolutionClass, cell(f.Status), cell(f.Title), cell(resp), cell(f.Ruling), f.ReopenCount, f.DisputeCount)
		}
	}

	w("\n## Deferred items\n\n")
	if len(r.Deferred) == 0 {
		w("None.\n")
	}
	for _, d := range r.Deferred {
		w("- %s [%s/%s] %s (%s)%s\n", d.ID, d.Severity, cell(d.Category), cell(d.Title), cell(d.Reason), fileSuffix(d.File))
	}

	w("\n## Implementation summary\n\n%s\n", strings.TrimSpace(r.Implementation.Summary))
	w("\nKey decisions:\n\n")
	bullets(&b, r.Implementation.Decisions)
	w("\nKnown gaps:\n\n")
	bullets(&b, r.Implementation.KnownGaps)
	if len(r.Implementation.Tests) > 0 {
		w("\nTests reported:\n\n")
		for _, t := range r.Implementation.Tests {
			w("- %s %s (covers %s)\n", t.Change, cell(t.Path), list(t.Covers))
		}
	}
	w("\nClaims not confirmed by the diff:\n\n")
	bullets(&b, r.Implementation.Unverified)

	w("\n## Usage\n\n| Agent | Role | Model | Auth | Calls | Turns | Input tokens | Output tokens | USD |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, a := range r.Usage.Agents {
		usd := fmt.Sprintf("%.4f", a.CostUSD)
		if !a.Charged {
			usd += " (estimate, not charged)"
		}
		w("| %s | %s | %s | %s | %d | %d | %d | %d | %s |\n", a.Agent, a.Role, cell(a.Model), a.AuthMode, a.Calls, a.Turns,
			a.InputTokens, a.OutputTokens, usd)
	}
	w("\nDuration: %.1fs. Subscription pauses: %d. API fallbacks: %d.\n", float64(r.Usage.DurationMS)/1000,
		r.Usage.SubscriptionPauses, r.Usage.APIFallbacks)

	w("\n## Digest\n\n```text\n%s\n```\n", strings.ReplaceAll(strings.TrimRight(r.Digest, "\n"), "```", "'''"))
	return b.String()
}

func fileSuffix(f string) string {
	if f == "" {
		return ""
	}
	return " — " + cell(f)
}

func bullets(b *strings.Builder, items []string) {
	if len(items) == 0 {
		b.WriteString("- none\n")
		return
	}
	for _, s := range items {
		b.WriteString(fmt.Sprintf("- %s\n", oneLine(s)))
	}
}
