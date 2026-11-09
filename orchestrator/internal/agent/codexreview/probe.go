package codexreview

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/artifacts"
)

// ProbeResult is what `aco doctor --probe` learned from one real call.
type ProbeResult struct {
	Model    string
	ReadOnly bool // the sandbox refused to create a file
	Usage    agent.Usage
}

var probeSchema = []byte(`{"type":"object","additionalProperties":false,"required":["ok","attempted_write"],` +
	`"properties":{"ok":{"type":"boolean"},"attempted_write":{"type":"boolean"}}}`)

const probePrompt = `This is a connectivity and sandbox probe by ACO.
Run exactly one shell command that tries to create an empty file named aco-probe.txt in the current working directory. Do not read or run anything else.
Then answer with the JSON object {"ok": true, "attempted_write": true}.
`

// Probe makes one real call with the configured model, reasoning effort,
// read-only sandbox, and output schema: it verifies the login of the
// resolved mode (logging the dedicated CODEX_HOME in, in api mode), that
// the model and model_reasoning_effort are accepted, and that the sandbox
// cannot create a file in the working directory.
func (a *Adapter) Probe(ctx context.Context) (*ProbeResult, error) {
	if err := a.prepare(ctx, agent.Output{Run: &artifacts.RunDir{}, Prefix: "probe"}); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "aco-codex-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	store := artifacts.New(filepath.Join(tmp, "artifacts"), a.opts.Redactor)
	run, err := store.Run("probe")
	if err != nil {
		return nil, err
	}
	schemaRel := artifacts.SchemaFile("probe.schema.json")
	if _, err := run.Write(schemaRel, probeSchema); err != nil {
		return nil, err
	}
	r, err := a.invoke(ctx, call{model: a.opts.Model, prompt: probePrompt, schemaRel: schemaRel,
		ws: agent.Workspace{Dir: work}, out: agent.Output{Run: run, Prefix: "probe"}, noGitRepo: true})
	if err != nil {
		return nil, err
	}
	var ans struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal([]byte(r.message), &ans) != nil || !ans.OK {
		return nil, a.fail(r.inv, agent.ClassSchemaInvalid, "the probe answer does not match its schema", nil)
	}
	res := &ProbeResult{Model: a.opts.Model, Usage: r.inv.Usage}
	_, statErr := os.Stat(filepath.Join(work, "aco-probe.txt"))
	// Read-only is proven only when the write was attempted.
	res.ReadOnly = errors.Is(statErr, os.ErrNotExist) && r.commands > 0
	return res, nil
}
