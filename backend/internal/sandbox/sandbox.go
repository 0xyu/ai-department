// Package sandbox runs shell commands inside Docker containers that mount ./workspace.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	Image       = "aidept-sandbox:latest"
	RWContainer = "aidept-sandbox"    // workspace mounted read-write
	ROContainer = "aidept-sandbox-ro" // workspace mounted read-only (Sec)
)

type Sandbox struct {
	WorkspaceDir string // absolute host path
	DockerfileDir string
}

type Result struct {
	ExitCode int
	Output   string
	TimedOut bool
}

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// Ensure builds the image if missing and starts both containers.
func (s *Sandbox) Ensure(ctx context.Context) error {
	if out, err := docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("docker is not running: %s", strings.TrimSpace(out))
	}
	if _, err := docker(ctx, "image", "inspect", Image); err != nil {
		fmt.Println("[sandbox] building image", Image, "(first run only)...")
		bctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		if out, err := docker(bctx, "build", "-t", Image, s.DockerfileDir); err != nil {
			return fmt.Errorf("build sandbox image: %v\n%s", err, out)
		}
	}
	for _, c := range []struct {
		name, mount string
	}{
		{RWContainer, s.WorkspaceDir + ":/workspace"},
		{ROContainer, s.WorkspaceDir + ":/workspace:ro"},
	} {
		state, err := docker(ctx, "inspect", "-f", "{{.State.Running}}", c.name)
		if err == nil && strings.TrimSpace(state) == "true" {
			continue
		}
		if err == nil { // exists but stopped
			if out, err := docker(ctx, "start", c.name); err != nil {
				return fmt.Errorf("start %s: %v %s", c.name, err, out)
			}
			continue
		}
		if out, err := docker(ctx, "run", "-d", "--name", c.name, "-v", c.mount, "-w", "/workspace",
			"--memory", "2g", "--cpus", "2", Image, "sleep", "infinity"); err != nil {
			return fmt.Errorf("run %s: %v %s", c.name, err, out)
		}
	}
	return nil
}

// Exec runs a bash command in the sandbox. workdir is relative to /workspace.
func (s *Sandbox) Exec(ctx context.Context, readOnly bool, workdir, command string, timeout time.Duration) Result {
	container := RWContainer
	if readOnly {
		container = ROContainer
	}
	wd := "/workspace"
	if w := strings.Trim(strings.ReplaceAll(workdir, "\\", "/"), "/"); w != "" && w != "." {
		if strings.Contains(w, "..") {
			return Result{ExitCode: -1, Output: "workdir must stay inside /workspace"}
		}
		wd = "/workspace/" + strings.TrimPrefix(w, "workspace/")
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "docker", "exec", "-w", wd, container, "bash", "-lc", command)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	r := Result{Output: out.String()}
	if cctx.Err() == context.DeadlineExceeded {
		r.TimedOut = true
		r.ExitCode = 124
		// make sure nothing keeps running inside the container
		_, _ = docker(context.Background(), "exec", container, "bash", "-c", "pkill -f -- "+shellQuote(firstWord(command))+" || true")
		return r
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			r.ExitCode = ee.ExitCode()
		} else {
			r.ExitCode = -1
			r.Output += "\n" + err.Error()
		}
	}
	return r
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return "__none__"
	}
	return f[0]
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
