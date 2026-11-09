package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is not a real test: the tests below re-run the test
// binary in helper mode. "parent" starts a "child" (both sleep), writing the
// child's PID to a file first.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("ACO_PROC_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "parent":
		c := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		c.Env = append(os.Environ(), "ACO_PROC_HELPER=child")
		if err := c.Start(); err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(os.Getenv("ACO_PROC_PIDFILE"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
		time.Sleep(time.Minute)
	case "child":
		time.Sleep(time.Minute)
	case "exit":
		os.Exit(0)
	}
	os.Exit(0)
}

func helperCmd(t *testing.T, mode string) (*exec.Cmd, string) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	c := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	c.Env = append(os.Environ(), "ACO_PROC_HELPER="+mode, "ACO_PROC_PIDFILE="+pidFile)
	return c, pidFile
}

func readPID(t *testing.T, pidFile string) int {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
			n, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("helper did not write the child PID")
	return 0
}

// Killing the tree ends the child and its grandchild within 5 s (AC-05.6).
func TestKillEndsWholeTree(t *testing.T) {
	cmd, pidFile := helperCmd(t, "parent")
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	child := readPID(t, pidFile)
	if !alive(child) {
		t.Fatal("grandchild not running before kill")
	}

	start := time.Now()
	if err := tree.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = tree.Wait()
	for alive(child) || alive(tree.Pid()) {
		if time.Since(start) > 5*time.Second {
			t.Fatalf("process tree still running %v after Kill", time.Since(start))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("tree ended in %v", time.Since(start))
}

func TestWaitIsIdempotentAndKillAfterExit(t *testing.T) {
	cmd, _ := helperCmd(t, "exit")
	tree, err := Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err := tree.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := tree.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tree.Done():
	default:
		t.Fatal("Done not closed after Wait")
	}
	if err := tree.Kill(); err != nil {
		t.Errorf("Kill after exit: %v", err)
	}
}

func TestStartRejectsPresetSysProcAttr(t *testing.T) {
	cmd, _ := helperCmd(t, "exit")
	if _, err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	cmd2, _ := helperCmd(t, "exit")
	cmd2.SysProcAttr = cmd.SysProcAttr
	if _, err := Start(cmd2); err == nil {
		t.Fatal("want error for preset SysProcAttr")
	}
}

func TestStartMissingBinary(t *testing.T) {
	if _, err := Start(exec.Command(filepath.Join(t.TempDir(), "no-such-binary"))); err == nil {
		t.Fatal("want error")
	}
}
