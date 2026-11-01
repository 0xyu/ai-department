// Package app wires storage, events, sandbox, approvals, agents and the orchestrator together.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"aidept/internal/agents"
	"aidept/internal/approval"
	"aidept/internal/events"
	"aidept/internal/orchestrator"
	"aidept/internal/sandbox"
	"aidept/internal/store"
)

type App struct {
	Root      string
	Store     *store.Store
	Bus       *events.Bus
	Approvals *approval.Manager
	Defs      map[string]*agents.Def
	Orch      *orchestrator.Orchestrator
	Sandbox   *sandbox.Sandbox
}

// FindRoot walks up from the working directory to the folder that holds agents/.
func FindRoot() (string, error) {
	if r := os.Getenv("AIDEPT_ROOT"); r != "" {
		return filepath.Abs(r)
	}
	dir, _ := os.Getwd()
	for {
		if st, err := os.Stat(filepath.Join(dir, "agents")); err == nil && st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find the project root (a folder containing agents/); set AIDEPT_ROOT")
		}
		dir = parent
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// loadDotEnv sets variables from <root>/.env that are not already set.
func loadDotEnv(root string) {
	b, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func New(ctx context.Context) (*App, error) {
	root, err := FindRoot()
	if err != nil {
		return nil, err
	}
	loadDotEnv(root)
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY is not set (export it or put it in %s)", filepath.Join(root, ".env"))
	}
	for _, d := range []string{"workspace", "memory", "data"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	defs, err := agents.LoadDefs(filepath.Join(root, "agents"))
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(root, "data", "aidept.db"))
	if err != nil {
		return nil, err
	}
	bus := events.NewBus(st, agents.Order)
	sb := &sandbox.Sandbox{WorkspaceDir: filepath.Join(root, "workspace"), DockerfileDir: filepath.Join(root, "sandbox")}
	if err := sb.Ensure(ctx); err != nil {
		return nil, err
	}
	appr := approval.New(st, bus)
	env := &agents.Env{
		Client:       anthropic.NewClient(),
		Model:        envOr("AIDEPT_MODEL", "claude-sonnet-5-5"),
		Effort:       envOr("AIDEPT_EFFORT", "medium"),
		Store:        st,
		Bus:          bus,
		Sandbox:      sb,
		Approvals:    appr,
		WorkspaceDir: sb.WorkspaceDir,
		MemoryDir:    filepath.Join(root, "memory"),
		Defs:         defs,
	}
	return &App{Root: root, Store: st, Bus: bus, Approvals: appr, Defs: defs, Orch: orchestrator.New(env), Sandbox: sb}, nil
}
