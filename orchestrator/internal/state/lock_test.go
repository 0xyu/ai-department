package state

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockInProcess(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	s, _ := openTest(t, Options{Now: clk.Now})
	repo := t.TempDir()
	a, prev, err := s.AcquireLock(ctx, repo, "run-a", 30*time.Second)
	if err != nil || prev != nil {
		t.Fatalf("acquire: %v %v", prev, err)
	}
	// Another spelling of the same repository is the same lock.
	_, _, err = s.AcquireLock(ctx, filepath.Join(repo, "x", ".."), "run-b", 30*time.Second)
	var le *LockedError
	if !errors.As(err, &le) || !errors.Is(err, ErrLocked) || le.Holder.RunID != "run-a" || le.Holder.PID != os.Getpid() {
		t.Fatalf("second acquire: %v", err)
	}
	// Heartbeats keep it alive past the TTL.
	clk.Add(20 * time.Second)
	if err := a.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Add(20 * time.Second)
	if _, _, err := s.AcquireLock(ctx, repo, "run-b", 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("lock taken over while heartbeating: %v", err)
	}
	// No heartbeat for the TTL: stale, taken over.
	clk.Add(31 * time.Second)
	b, prev, err := s.AcquireLock(ctx, repo, "run-b", 0)
	if err != nil || prev == nil || prev.RunID != "run-a" || prev.Owner != a.Owner() {
		t.Fatalf("takeover: %+v %v", prev, err)
	}
	if b.Info().TTL != DefaultLockTTL || b.Owner() == a.Owner() {
		t.Errorf("new lock %+v", b.Info())
	}
	if err := a.Heartbeat(ctx); !errors.Is(err, ErrLockLost) {
		t.Errorf("old holder heartbeat: %v", err)
	}
	if err := a.Release(ctx); !errors.Is(err, ErrLockLost) {
		t.Errorf("old holder release: %v", err)
	}
	if got, _ := s.GetLock(ctx, repo); got.RunID != "run-b" {
		t.Errorf("holder %+v", got)
	}
	if err := b.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLock(ctx, repo); !errors.Is(err, ErrNotFound) {
		t.Errorf("released lock still present: %v", err)
	}
	// A different repository is independent.
	if _, _, err := s.AcquireLock(ctx, repo, "run-c", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcquireLock(ctx, t.TempDir(), "run-d", 0); err != nil {
		t.Errorf("other repo: %v", err)
	}
}

func TestKeepAliveReportsLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, _ := openTest(t, Options{})
	repo := t.TempDir()
	l, _, err := s.AcquireLock(ctx, repo, "run-a", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	lost := l.KeepAlive(ctx, 20*time.Millisecond)
	time.Sleep(450 * time.Millisecond) // past the TTL: heartbeats keep it
	if _, _, err := s.AcquireLock(ctx, repo, "run-b", 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("keep-alive did not hold the lock: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM locks`); err != nil { // simulate a takeover
		t.Fatal(err)
	}
	select {
	case err := <-lost:
		if !errors.Is(err, ErrLockLost) {
			t.Errorf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loss not reported")
	}
}

// --- Cross-process tests -------------------------------------------------
//
// The test binary re-executes itself as a helper process (TestMain) so the
// lock and crash behaviour is checked between real OS processes.

const helperEnv = "ACO_STATE_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode, os.Args[len(os.Args)-3:]))
	}
	os.Exit(m.Run())
}

// runHelper modes (args: db path, repo path, run ID):
//
//	hold:  acquire the lock with a 1s TTL, keep it alive, print LOCKED, block.
//	try:   try to acquire once; exit 0 if acquired, 3 if locked.
//	step:  acquire the lock, begin a step, print INTENT, block (to be killed).
func runHelper(mode string, args []string) int {
	ctx := context.Background()
	s, err := Open(ctx, args[0], Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	l, _, err := s.AcquireLock(ctx, args[1], args[2], time.Second)
	switch {
	case errors.Is(err, ErrLocked):
		return 3
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch mode {
	case "try":
		return 0
	case "hold":
		l.KeepAlive(ctx, 100*time.Millisecond)
		fmt.Println("LOCKED")
	case "step":
		st, err := s.BeginStep(ctx, StepIntent{RunID: args[2], Kind: "implement", Key: "crash-step", Owner: l.Owner(),
			Intent: []byte(`{"prompt":"p"}`), CheckpointRef: "refs/aco/x/s1"})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("INTENT", st.ID)
	}
	select {} // until killed
}

type helper struct {
	cmd  *exec.Cmd
	line string
}

func startHelper(t *testing.T, mode, db, repo, runID, want string) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$", "--", db, repo, runID)
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &helper{cmd: cmd}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		if sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	select {
	case l, ok := <-lines:
		if !ok || !strings.HasPrefix(l, want) {
			cmd.Wait()
			t.Fatalf("helper %s: got %q, stderr %s", mode, l, stderr.String())
		}
		h.line = l
	case <-time.After(30 * time.Second):
		t.Fatalf("helper %s did not start; stderr %s", mode, stderr.String())
	}
	return h
}

func (h *helper) kill(t *testing.T) {
	t.Helper()
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	h.cmd.Wait()
}

func tryLock(t *testing.T, db, repo, runID string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$", "--", db, repo, runID)
	cmd.Env = append(os.Environ(), helperEnv+"=try")
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0
}

func skipShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("cross-process test; skipped with -short")
	}
}

// Two processes cannot lock the same repository; a killed holder's lock
// goes stale and is taken over.
func TestLockAcrossProcesses(t *testing.T) {
	skipShort(t)
	ctx := context.Background()
	dir := t.TempDir()
	db, repo := filepath.Join(dir, "state.db"), filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	holder := startHelper(t, "hold", db, repo, "run-a", "LOCKED")
	if code := tryLock(t, db, repo, "run-b"); code != 3 {
		t.Fatalf("second process: exit %d, want 3 (locked)", code)
	}
	if _, _, err := s.AcquireLock(ctx, repo, "run-c", 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("this process: %v", err)
	}
	time.Sleep(1500 * time.Millisecond) // > TTL: the live holder keeps heartbeating
	if code := tryLock(t, db, repo, "run-b"); code != 3 {
		t.Fatalf("heartbeating holder lost its lock (exit %d)", code)
	}

	holder.kill(t)
	if _, _, err := s.AcquireLock(ctx, repo, "run-c", 0); !errors.Is(err, ErrLocked) {
		t.Fatalf("lock free right after the kill (should wait for the TTL): %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, prev, err := s.AcquireLock(ctx, repo, "run-c", 0)
		if err == nil {
			if prev == nil || prev.RunID != "run-a" {
				t.Errorf("takeover did not report the stale holder: %+v", prev)
			}
			l.Release(ctx)
			break
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			t.Fatalf("stale lock not taken over: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A process killed between intent and outcome leaves a step that resume
// marks as needing reconciliation.
func TestKillBetweenIntentAndOutcome(t *testing.T) {
	skipShort(t)
	ctx := context.Background()
	dir := t.TempDir()
	db, repo := filepath.Join(dir, "state.db"), filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.CreateRun(ctx, NewRun{ID: "crash-run", RepoPath: repo, Mode: ModeFull, Input: InputPlan})
	if err != nil {
		t.Fatal(err)
	}

	h := startHelper(t, "step", db, repo, r.ID, "INTENT")
	h.kill(t)

	steps, err := s.ListSteps(ctx, r.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps after kill: %+v %v", steps, err)
	}
	if steps[0].Status != StepStarted || steps[0].EndedAt != nil || string(steps[0].Intent) != `{"prompt":"p"}` {
		t.Fatalf("intent not durable: %+v", steps[0])
	}

	// Resume: take over the stale lock, then mark interrupted steps.
	var lock *Lock
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, prev, err := s.AcquireLock(ctx, repo, r.ID, 0)
		if err == nil {
			if prev == nil {
				t.Error("takeover of the killed process's lock not reported")
			}
			lock = l
			break
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	marked, err := s.MarkInterrupted(ctx, r.ID, lock.Owner())
	if err != nil || len(marked) != 1 {
		t.Fatalf("mark: %+v %v", marked, err)
	}
	got, _ := s.GetStep(ctx, marked[0].ID)
	if got.Status != StepNeedsReconciliation || got.CheckpointRef != "refs/aco/x/s1" {
		t.Errorf("after resume: %+v", got)
	}
}
