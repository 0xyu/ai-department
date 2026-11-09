package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/plan"
	"github.com/0xyu/aco/internal/prompt"
	"github.com/0xyu/aco/schemas"
)

// Planner returns the adapter as an agent.Planner.
func (a *Adapter) Planner() agent.Planner { return plannerRole{a} }

// Implementer returns the adapter as an agent.Implementer.
func (a *Adapter) Implementer() agent.Implementer { return implementerRole{a} }

type plannerRole struct{ a *Adapter }
type implementerRole struct{ a *Adapter }

var (
	revisionOnce   sync.Once
	revisionSchema []byte
)

// RevisionSchema is plan.schema.json with a required root property
// finding_responses (answers to the review findings). It stays at the root
// so the schema's $defs references remain valid.
func RevisionSchema() []byte {
	revisionOnce.Do(func() {
		var s map[string]any
		if err := json.Unmarshal(schemas.Plan, &s); err != nil {
			panic("plan schema: " + err.Error())
		}
		props := s["properties"].(map[string]any)
		props["finding_responses"] = map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []any{"id", "response", "detail"},
				"properties": map[string]any{
					"id":       map[string]any{"type": "string", "minLength": 1},
					"response": map[string]any{"enum": []any{agent.ResponseFixed, agent.ResponseDisputed}},
					"detail":   map[string]any{"type": "string", "minLength": 1},
				},
			},
		}
		s["required"] = append(s["required"].([]any), "finding_responses")
		s["title"] = "ACO plan package revision (v1)"
		revisionSchema, _ = json.Marshal(s)
	})
	return revisionSchema
}

func (p plannerRole) Create(ctx context.Context, req agent.PlanRequest) (*agent.PlanResult, error) {
	a := p.a
	sys, err := prompt.Render(prompt.PlannerSystem, nil)
	if err != nil {
		return nil, err
	}
	text, err := prompt.Render(prompt.PlanCreate, req)
	if err != nil {
		return nil, err
	}
	// A supplied plan revised before any session existed answers its
	// findings in the same call (revision schema).
	revision := len(req.Findings) > 0
	schema := schemas.Plan
	if revision {
		schema = RevisionSchema()
	}
	r, err := a.run(ctx, call{
		profile: ProfilePlanner, sessionID: newUUID(), model: a.opts.PlannerModel, effort: a.opts.PlannerEffort,
		system: sys, prompt: text, schema: schema, ws: req.Workspace, out: req.Output, runID: req.RunID, scopes: req.BudgetScopes,
	})
	if err != nil {
		return nil, err
	}
	res, err := a.planResult(r, revision)
	if err != nil || !revision {
		return res, err
	}
	if missing := missingResponses(req.Findings, res.Responses); len(missing) > 0 {
		return res, a.fail(res.Invocation, agent.ClassIncomplete, "finding_responses has no answer for "+strings.Join(missing, ", "), nil)
	}
	return res, nil
}

// missingResponses lists the findings without an answer.
func missingResponses(fs []agent.PlanFinding, rs []agent.FindingResponse) []string {
	var missing []string
	for _, f := range fs {
		if !slices.ContainsFunc(rs, func(fr agent.FindingResponse) bool { return fr.ID == f.ID }) {
			missing = append(missing, f.ID)
		}
	}
	return missing
}

func (p plannerRole) Revise(ctx context.Context, req agent.PlanRevision) (*agent.PlanResult, error) {
	a := p.a
	if req.SessionID == "" {
		return nil, errors.New("claudecode: Revise needs the planning session ID")
	}
	sys, err := prompt.Render(prompt.PlannerSystem, nil)
	if err != nil {
		return nil, err
	}
	text, err := prompt.Render(prompt.PlanRevise, req)
	if err != nil {
		return nil, err
	}
	r, err := a.run(ctx, call{
		profile: ProfilePlanner, sessionID: req.SessionID, resume: true, model: a.opts.PlannerModel, effort: a.opts.PlannerEffort,
		system: sys, prompt: text, schema: RevisionSchema(), ws: req.Workspace, out: req.Output, runID: req.RunID, scopes: req.BudgetScopes,
	})
	if err != nil {
		return nil, err
	}
	res, err := a.planResult(r, true)
	if err != nil {
		return nil, err
	}
	if missing := missingResponses(req.Findings, res.Responses); len(missing) > 0 {
		return res, a.fail(res.Invocation, agent.ClassIncomplete, "finding_responses has no answer for "+strings.Join(missing, ", "), nil)
	}
	return res, nil
}

// planResult parses the structured plan package. Revisions carry
// finding_responses at the root, which is split off first.
func (a *Adapter) planResult(r *raw, revision bool) (*agent.PlanResult, error) {
	res := &agent.PlanResult{Invocation: r.inv}
	if len(r.structured) == 0 {
		return nil, a.fail(r.inv, agent.ClassIncomplete, "no structured plan package in the result", nil)
	}
	body := r.structured
	if revision {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, a.fail(r.inv, agent.ClassIncomplete, "plan revision is not a JSON object", err)
		}
		if fr, ok := doc["finding_responses"]; ok {
			if err := json.Unmarshal(fr, &res.Responses); err != nil {
				return nil, a.fail(r.inv, agent.ClassIncomplete, "finding_responses is malformed", err)
			}
			delete(doc, "finding_responses")
		}
		body, _ = json.Marshal(doc)
	}
	res.Raw = body
	res.Package = plan.LoadBytes("plan.json", body)
	for _, d := range res.Package.Diagnostics {
		if d.Code == plan.CodeParseError || d.Code == plan.CodeUnsupportedPlanFormat {
			return res, a.fail(r.inv, agent.ClassIncomplete, "plan package does not match the schema: "+d.Message, nil)
		}
	}
	return res, nil
}

func (i implementerRole) Start(ctx context.Context, req agent.ImplementRequest) (*agent.ImplResult, error) {
	a := i.a
	name := prompt.PhaseImplement
	if req.Kind == agent.KindFinalFix {
		name = prompt.FinalFix
	}
	sys, err := prompt.Render(prompt.ImplementerSystem, nil)
	if err != nil {
		return nil, err
	}
	text, err := prompt.Render(name, req)
	if err != nil {
		return nil, err
	}
	r, err := a.run(ctx, call{
		profile: ProfileImplementer, sessionID: newUUID(), model: a.opts.ImplementerModel, effort: a.opts.ImplementerEffort,
		system: sys, prompt: text, schema: schemas.ImplReport, ws: req.Workspace, out: req.Output, runID: req.RunID, scopes: req.BudgetScopes,
	})
	if err != nil {
		return nil, err
	}
	return a.implResult(r, req.ACIDs, nil)
}

func (i implementerRole) Continue(ctx context.Context, req agent.ContinueRequest) (*agent.ImplResult, error) {
	a := i.a
	if req.SessionID == "" {
		return nil, errors.New("claudecode: Continue needs a session ID")
	}
	var name string
	var ids []string
	switch req.Kind {
	case agent.ContinueGateFix:
		name = prompt.GateFix
		for _, g := range req.GateFailures {
			ids = append(ids, g.ID)
		}
	case agent.ContinueReviewFix:
		name = prompt.ReviewFix
		for _, f := range req.Findings {
			ids = append(ids, f.ID)
		}
	case agent.ContinueFinish:
		name = prompt.Continue
	default:
		return nil, errors.New("claudecode: unknown continue kind " + string(req.Kind))
	}
	sys, err := prompt.Render(prompt.ImplementerSystem, nil)
	if err != nil {
		return nil, err
	}
	text, err := prompt.Render(name, req)
	if err != nil {
		return nil, err
	}
	r, err := a.run(ctx, call{
		profile: ProfileImplementer, sessionID: req.SessionID, resume: true, model: a.opts.ImplementerModel, effort: a.opts.ImplementerEffort,
		system: sys, prompt: text, schema: schemas.ImplReport, ws: req.Workspace, out: req.Output, runID: req.RunID, scopes: req.BudgetScopes,
	})
	if err != nil {
		return nil, err
	}
	return a.implResult(r, req.ACIDs, ids)
}

// implResult validates the implementation report. A missing or invalid
// report is ClassIncomplete; the result is still returned so the engine can
// continue the session with the problems (§14.1 "agent incomplete").
func (a *Adapter) implResult(r *raw, acIDs, findingIDs []string) (*agent.ImplResult, error) {
	res := &agent.ImplResult{Invocation: r.inv, Raw: r.structured}
	if len(r.structured) == 0 {
		return res, a.fail(r.inv, agent.ClassIncomplete, "no implementation report in the result", nil)
	}
	rep, probs := agent.ParseImplReport(r.structured)
	if rep != nil {
		probs = rep.CheckCoverage(acIDs, findingIDs)
	}
	res.Report = rep
	if len(probs) > 0 {
		return res, a.fail(r.inv, agent.ClassIncomplete, "implementation report: "+strings.Join(probs, "; "), nil)
	}
	return res, nil
}
