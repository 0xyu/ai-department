package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/0xyu/aco/internal/agent"
	"github.com/0xyu/aco/internal/auth"
	"github.com/0xyu/aco/internal/proc"
)

// ProbeResult is what one probe call showed.
type ProbeResult struct {
	Model      string
	AuthSource string
	Version    string
	CostUSD    float64
}

// probeSchema is the tiny structured output a probe asks for.
const probeSchema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`

// probeTimeout bounds a probe call.
const probeTimeout = 3 * time.Minute

// Probe makes one minimal real call with the implementer's model and effort
// in the resolved auth mode (§13 preflight: "model reachable, --effort
// available"): it checks that the model answers, that --effort and
// --json-schema are accepted, and that the init message reports the auth
// source of the mode. It uses a little of the subscription or API budget,
// so `aco doctor` runs it only with --probe.
func (a *Adapter) Probe(ctx context.Context) (*ProbeResult, error) {
	caps, err := a.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	if m := caps.Missing(); len(m) > 0 {
		return nil, &ErrMissingFlags{Flags: m}
	}
	env, err := a.childEnv(a.opts.Mode)
	if err != nil {
		return nil, &agent.Error{Agent: "claude", Class: agent.ClassAuth, Message: err.Error(), Err: err}
	}
	dir, err := os.MkdirTemp("", "aco-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--model", a.opts.ImplementerModel}
	if caps.EffortVia() == EffortFlag {
		args = append(args, "--effort", a.opts.ImplementerEffort)
	}
	args = append(args, "--permission-mode", "dontAsk", "--tools", "Read", "--setting-sources", "", "--strict-mcp-config",
		"--json-schema", probeSchema)
	if caps.Has("no-session-persistence") {
		args = append(args, "--no-session-persistence")
	}
	if caps.Has("max-turns") {
		args = append(args, "--max-turns", "3")
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.Command(a.opts.Bin, append(append([]string{}, a.opts.BinArgs...), args...)...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin = strings.NewReader("Return ok=true as the structured output. Do not use any tool other than the structured output.\n")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &limitedBuffer{max: maxStderr}
	cmd.Stderr = stderr
	tree, err := proc.Start(cmd)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	stop := context.AfterFunc(cctx, func() { _ = tree.Kill() })
	defer stop()

	st := newStream()
	br := bufio.NewReaderSize(out, 64<<10)
	for {
		line, rerr := readLine(br, maxLine)
		if len(line) > 0 {
			st.feed(line)
			if st.init != nil {
				if err := auth.CheckClaudeSource(a.opts.Mode, st.init.APIKeySource); err != nil {
					_ = tree.Kill()
					_ = tree.Wait()
					return nil, &agent.Error{Agent: "claude", Class: agent.ClassAuth, Message: err.Error(), Err: err}
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	exitErr := tree.Wait()
	_ = tree.Kill()
	if cctx.Err() != nil {
		return nil, &agent.Error{Agent: "claude", Class: agent.ClassTimeout, Message: "probe timed out"}
	}
	if st.init == nil {
		if e := outcome(a.opts.Mode, st, a.opts.Redactor.String(stderr.String()), exitErr, a.opts.Now()); e != nil {
			return nil, e
		}
		return nil, errors.New("probe: no init message")
	}
	if e := outcome(a.opts.Mode, st, a.opts.Redactor.String(stderr.String()), exitErr, a.opts.Now()); e != nil {
		e.Message = a.opts.Redactor.String(e.Message)
		return nil, e
	}
	var ok struct {
		OK bool `json:"ok"`
	}
	if so := extractJSON(st.result); so == nil || json.Unmarshal(so, &ok) != nil || !ok.OK {
		return nil, &agent.Error{Agent: "claude", Class: agent.ClassIncomplete, Message: "probe returned no valid structured output"}
	}
	return &ProbeResult{Model: st.init.Model, AuthSource: st.init.APIKeySource, Version: st.init.Version, CostUSD: st.result.TotalCostUSD}, nil
}
