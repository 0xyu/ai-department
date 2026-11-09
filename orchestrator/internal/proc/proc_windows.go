//go:build windows

package proc

import (
	"errors"
	"fmt"
	"math"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The child is created suspended, put into a job object that kills every
// member when it is terminated or its last handle closes, and only then
// resumed. So every descendant is in the job, even one started by the
// child's first instruction.
type sysTree struct {
	mu  sync.Mutex
	job windows.Handle
}

var (
	ntdll           = windows.NewLazySystemDLL("ntdll.dll")
	ntResumeProcess = ntdll.NewProc("NtResumeProcess")
)

func (s *sysTree) start(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("proc: create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("proc: configure job object: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	fail := func(err error) error {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = windows.CloseHandle(job)
		return err
	}
	pid := cmd.Process.Pid
	if pid < 0 || pid > math.MaxUint32 {
		return fail(fmt.Errorf("proc: unexpected process ID %d", pid))
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME,
		false, uint32(pid))
	if err != nil {
		return fail(fmt.Errorf("proc: open process: %w", err))
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fail(fmt.Errorf("proc: assign job object: %w", err))
	}
	if r, _, _ := ntResumeProcess.Call(uintptr(h)); r != 0 {
		return fail(fmt.Errorf("proc: resume process: NTSTATUS 0x%x", r))
	}
	s.job = job
	return nil
}

func (s *sysTree) kill(_ *Tree, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == 0 {
		return ErrNotStarted
	}
	err := windows.TerminateJobObject(s.job, 1)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil // every member already exited
	}
	return err
}

func (s *sysTree) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == 0 {
		return nil
	}
	err := windows.CloseHandle(s.job) // KILL_ON_JOB_CLOSE ends any member left
	s.job = 0
	return err
}
