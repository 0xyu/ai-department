//go:build !windows

package proc

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

type sysTree struct{ pgid int }

func (s *sysTree) start(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.pgid = cmd.Process.Pid
	return nil
}

// kill sends SIGTERM to the process group, then SIGKILL after grace (or at
// once when the child already exited, since nothing then waits for grace).
func (s *sysTree) kill(t *Tree, grace time.Duration) error {
	if s.pgid == 0 {
		return ErrNotStarted
	}
	if err := syscall.Kill(-s.pgid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // group already gone
		}
		return err
	}
	select {
	case <-t.done:
	case <-time.After(grace):
	}
	err := syscall.Kill(-s.pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (s *sysTree) close() error { return nil }
