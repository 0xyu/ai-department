// Package proc starts child processes as a killable tree: on Unix the child
// leads a new process group, on Windows it runs in a job object that is
// assigned before the child's first instruction runs. Kill ends the child and
// every descendant it started, so a cancelled agent or gate cannot leave
// background processes behind (§14.1 "kill process tree").
package proc

import (
	"errors"
	"os/exec"
	"sync"
	"time"
)

// KillGrace is how long Kill waits after asking the tree to stop (SIGTERM on
// Unix) before it forces it (SIGKILL). Windows terminates the job at once.
const KillGrace = 2 * time.Second

// ErrNotStarted is returned by Kill and Wait on a Tree whose Start failed.
var ErrNotStarted = errors.New("proc: process not started")

// Tree is a started child process and its descendants.
type Tree struct {
	cmd *exec.Cmd
	sys sysTree

	waitOnce sync.Once
	waitErr  error
	done     chan struct{}
}

// Start starts cmd as a process tree. cmd must not be started yet, and its
// SysProcAttr must be nil (Start sets it).
func Start(cmd *exec.Cmd) (*Tree, error) {
	if cmd.SysProcAttr != nil {
		return nil, errors.New("proc: cmd.SysProcAttr must be nil")
	}
	t := &Tree{cmd: cmd, done: make(chan struct{})}
	if err := t.sys.start(cmd); err != nil {
		return nil, err
	}
	return t, nil
}

// Pid is the child's process ID.
func (t *Tree) Pid() int { return t.cmd.Process.Pid }

// Wait waits for the child to exit and releases its resources. It is safe to
// call more than once and from several goroutines; every call returns the
// same result. Descendants that outlive the child are not waited for; use
// Kill to end them.
func (t *Tree) Wait() error {
	t.waitOnce.Do(func() {
		t.waitErr = t.cmd.Wait()
		close(t.done)
	})
	return t.waitErr
}

// Done is closed after Wait has returned.
func (t *Tree) Done() <-chan struct{} { return t.done }

// Kill ends the whole tree: the child and all its descendants. It returns
// after the tree was told to stop; call Wait to reap the child. Kill after
// the child exited still ends descendants that are left.
func (t *Tree) Kill() error { return t.sys.kill(t, KillGrace) }

// Close releases OS resources held for the tree (the Windows job handle,
// which also ends any process still in it). Call it after Wait.
func (t *Tree) Close() error { return t.sys.close() }
