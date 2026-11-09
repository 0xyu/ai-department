package codexreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
	"github.com/0xyu/aco/internal/prompt"
	"github.com/0xyu/aco/schemas"
)

// Schema file names in the run's schemas/ directory.
const (
	SchemaPlanReview = "plan-review.schema.json"
	SchemaCodeReview = "code-review.schema.json"
)

// ErrNoModel reports an unset reviewer.model.
var ErrNoModel = errors.New("codexreview: reviewer.model is not set")

// parseFunc validates a final message: the parsed value, the
// inconsistencies, and the problems (nil = accepted).
type parseFunc func(msg []byte) (value any, inconsistent []string, problems []string)

type reviewSpec struct {
	pack      string
	schemaRel string
	ws        agent.Workspace
	out       agent.Output
	scopes    []agent.Scope
	parse     parseFunc
}

type reviewOutcome struct {
	inv          agent.Invocation
	calls        agent.ReviewCalls
	raw          json.RawMessage
	value        any
	inconsistent []string
}

func (a *Adapter) store(run *artifacts.RunDir) Store {
	return func(rel string, data []byte) (string, error) {
		if _, err := run.Write(rel, data); err != nil {
			return "", err
		}
		return run.Abs(rel)
	}
}

func (a *Adapter) writeSchema(run *artifacts.RunDir, name string, schema []byte) (string, error) {
	b, err := cliSchema(schema)
	if err != nil {
		return "", err
	}
	rel := artifacts.SchemaFile(name)
	if _, err := run.Write(rel, b); err != nil {
		return "", err
	}
	return rel, nil
}

// prepare checks the model and the login (§16.5: before every review).
func (a *Adapter) prepare(ctx context.Context, out agent.Output) error {
	if a.opts.Model == "" {
		return a.fail(agent.Invocation{AuthMode: string(a.opts.Mode)}, agent.ClassCrash, ErrNoModel.Error(), ErrNoModel)
	}
	if out.Run == nil || out.Prefix == "" {
		return errors.New("codexreview: an artifact output is required")
	}
	if err := a.EnsureLogin(ctx); err != nil {
		return a.fail(agent.Invocation{AuthMode: string(a.opts.Mode)}, agent.ClassAuth, err.Error(), err)
	}
	return nil
}

// ReviewPlan implements agent.Reviewer (Stage 2, §10.3).
func (a *Adapter) ReviewPlan(ctx context.Context, req agent.PlanReviewRequest) (*agent.PlanReviewResult, error) {
	if err := a.prepare(ctx, req.Output); err != nil {
		return nil, err
	}
	schemaRel, err := a.writeSchema(req.Output.Run, SchemaPlanReview, schemas.PlanReview)
	if err != nil {
		return nil, err
	}
	pack, err := BuildPlanPack(req, req.Output.Run.Path(), req.Output.Prefix, a.opts.MaxContextBytes, a.store(req.Output.Run))
	if err != nil {
		return nil, err
	}
	var prev []string
	for _, p := range req.Previous {
		prev = append(prev, p.Finding.ID)
	}
	o, err := a.review(ctx, reviewSpec{
		pack: pack.Text, schemaRel: schemaRel, ws: req.Workspace, out: req.Output, scopes: req.BudgetScopes,
		parse: func(msg []byte) (any, []string, []string) {
			r, probs := agent.ParsePlanReview(msg, req.ReqIDs, prev)
			if r == nil {
				return nil, nil, probs
			}
			return r, r.Inconsistencies(), nil
		},
	})
	if err != nil {
		return nil, err
	}
	return &agent.PlanReviewResult{Invocation: o.inv, ReviewCalls: o.calls, Raw: o.raw, Review: o.value.(*agent.PlanReview), Inconsistent: o.inconsistent}, nil
}

// ReviewCode implements agent.Reviewer (Stage 3 §11.6, Stage 4 §12.5).
func (a *Adapter) ReviewCode(ctx context.Context, req agent.CodeReviewRequest) (*agent.CodeReviewResult, error) {
	if err := a.prepare(ctx, req.Output); err != nil {
		return nil, err
	}
	schemaRel, err := a.writeSchema(req.Output.Run, SchemaCodeReview, schemas.CodeReview)
	if err != nil {
		return nil, err
	}
	pack, err := BuildCodePack(req, req.Output.Run.Path(), req.Output.Prefix, a.opts.MaxContextBytes, a.store(req.Output.Run))
	if err != nil {
		return nil, err
	}
	var prev []string
	for _, l := range req.Ledger {
		prev = append(prev, l.ID)
	}
	o, err := a.review(ctx, reviewSpec{
		pack: pack.Text, schemaRel: schemaRel, ws: req.Workspace, out: req.Output, scopes: req.BudgetScopes,
		parse: func(msg []byte) (any, []string, []string) {
			r, probs := agent.ParseCodeReview(msg, req.ACIDs, prev)
			if r == nil {
				return nil, nil, probs
			}
			return r, r.Inconsistencies(), nil
		},
	})
	if err != nil {
		return nil, err
	}
	return &agent.CodeReviewResult{Invocation: o.inv, ReviewCalls: o.calls, Raw: o.raw, Review: o.value.(*agent.CodeReview), Inconsistent: o.inconsistent}, nil
}

// review runs the call / validate / retry loop (§14.1 malformed review):
// a schema-invalid answer is retried with the validation errors up to
// SchemaRetries times, then the fallback model gets the same chances; a
// policy violation discards the review and retries once (§16.3);
// transient failures and 429s back off and retry.
func (a *Adapter) review(ctx context.Context, s reviewSpec) (*reviewOutcome, error) {
	models := []string{a.opts.Model}
	if fb := a.opts.FallbackModel; fb != "" && fb != a.opts.Model {
		models = append(models, fb)
	}
	var calls agent.ReviewCalls
	n := 0
	policyRetried := false
	record := func(inv agent.Invocation) {
		calls.Calls = append(calls.Calls, inv)
		calls.Total.Add(inv.Usage)
		if calls.ContextRef == "" {
			calls.ContextRef = inv.PromptRef
		}
	}
	failWith := func(err error) error {
		var ae *agent.Error
		if errors.As(err, &ae) {
			ae.Calls = calls.Calls
		}
		return err
	}
	var lastProbs []string
	for mi, model := range models {
		var probs []string
		lastMsg := ""
		for try := 0; try <= a.opts.SchemaRetries; try++ {
			text := s.pack
			if probs != nil {
				corr, err := prompt.Render(prompt.ReviewCorrection, prompt.Correction{Problems: limit(probs, 40), Answer: shortText(lastMsg)})
				if err != nil {
					return nil, err
				}
				text = corr + "\n" + s.pack
			}
			var r *raw
			for retry := 0; ; retry++ {
				n++
				out := agent.Output{Run: s.out.Run, Prefix: fmt.Sprintf("%s/call-%02d", s.out.Prefix, n)}
				var err error
				r, err = a.invoke(ctx, call{model: model, prompt: text, schemaRel: s.schemaRel, ws: s.ws, out: out, scopes: s.scopes})
				if err == nil {
					break
				}
				var ae *agent.Error
				if !errors.As(err, &ae) {
					return nil, failWith(err)
				}
				record(ae.Invocation)
				switch {
				case (ae.Class == agent.ClassTransient || ae.Class == agent.ClassAPIRateLimit) && retry < a.opts.TransientRetries:
					if serr := a.opts.Sleep(ctx, a.opts.Backoff(retry, ae.RetryAfter)); serr != nil {
						return nil, failWith(a.fail(ae.Invocation, agent.ClassCancelled, "cancelled", serr))
					}
					continue
				case ae.Class == agent.ClassPolicy && !policyRetried:
					policyRetried = true // discard this review, run it once more
					continue
				case ae.Class == agent.ClassSchemaInvalid:
					// No final message: counts as a schema retry.
					probs, lastMsg = []string{ae.Message}, ""
				default:
					return nil, failWith(err)
				}
				break
			}
			if r == nil {
				lastProbs = probs
				continue
			}
			record(r.inv)
			value, inconsistent, p := s.parse([]byte(r.message))
			if p == nil {
				calls.FallbackUsed = mi > 0
				calls.ReviewRef = s.out.Prefix + "/review.json"
				if _, err := s.out.Run.Write(calls.ReviewRef, []byte(r.message)); err != nil {
					return nil, err
				}
				return &reviewOutcome{inv: r.inv, calls: calls, raw: json.RawMessage(r.message), value: value, inconsistent: inconsistent}, nil
			}
			probs, lastMsg, lastProbs = p, r.message, p
		}
	}
	last := agent.Invocation{AuthMode: string(a.opts.Mode)}
	if len(calls.Calls) > 0 {
		last = calls.Calls[len(calls.Calls)-1]
	}
	e := a.fail(last, agent.ClassSchemaInvalid,
		fmt.Sprintf("no schema-valid review after %d calls (%s): %s", len(calls.Calls), strings.Join(models, ", "), strings.Join(limit(lastProbs, 5), "; ")), nil)
	e.Calls = calls.Calls
	return nil, e
}

func limit(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return append(append([]string{}, s[:n]...), fmt.Sprintf("… and %d more", len(s)-n))
}
