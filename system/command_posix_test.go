//go:build linux || darwin || !windows
// +build linux darwin !windows

package system

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goss-org/goss/util"
)

func TestCommandWrapper(t *testing.T) {
	t.Parallel()

	c := commandWrapper("echo hello world")
	cmdPath, _ := exec.LookPath(linuxShell)
	if c.Cmd.Path != cmdPath {
		t.Errorf("Command not wrapped properly for OS. got %s, want: %s", c.Cmd.Path, cmdPath)
	}
}

// treeCommand is a shell line that outlives any sane timeout and spawns a
// distinct child shell carrying sentinel in its argv. The root shell, the
// child shell and the sleeps they wait on all share the root's process group.
func treeCommand(sentinel string) string {
	return fmt.Sprintf(`sh -c "sleep 60 # %s" & sleep 60`, sentinel)
}

// assertProcessGone fails if a process whose argv contains sentinel is still
// visible to pgrep after wait, polling to absorb the short reaping delay after
// SIGKILL (zombies stay visible until their parent reaps them).
func assertProcessGone(t *testing.T, sentinel string, wait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		out, err := exec.Command("pgrep", "-f", sentinel).Output()
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process(es) for %q still in the process table after timeout: %s", sentinel, strings.TrimSpace(string(out)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunCommandTimeoutKillsProcessTree(t *testing.T) {
	t.Parallel()

	sentinel := "GOSS_CMD_TIMEOUT_TREE_TEST"
	cmd := commandWrapper(treeCommand(sentinel))

	start := time.Now()
	err := runCommand(cmd, 200)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error %q should say it timed out", err.Error())
	}
	// The configured duration must survive in the message (200ms -> "200ms").
	if !strings.Contains(err.Error(), "200ms") {
		t.Fatalf("timeout error %q should contain the configured duration 200ms", err.Error())
	}
	// The runner must not have waited for the command's natural (60s) exit.
	if elapsed > 10*time.Second {
		t.Fatalf("runCommand took %s, expected it to return shortly after the timeout", elapsed)
	}

	// Root shell, child shell and grandchildren must all be gone and reaped.
	assertProcessGone(t, sentinel, 5*time.Second)
}

func TestRunCommandTimeoutDrainsOutputBeforeReturning(t *testing.T) {
	t.Parallel()

	sentinel := "GOSS_CMD_TIMEOUT_CHATTY_TEST"
	// Floods stdout from the shell and a child, then idles long enough to time
	// out. After the timeout no writer may be alive and the buffers must be
	// safe to read under -race.
	line := fmt.Sprintf(`sh -c "i=0; while [ $i -lt 20000 ]; do echo %s $i; i=$((i+1)); done; sleep 60" & sleep 60 # %s`, sentinel, sentinel)
	cmd := commandWrapper(line)

	if err := runCommand(cmd, 200); err == nil {
		t.Fatal("expected a timeout error, got nil")
	}

	// Reading the captured output after runCommand returned must not race the
	// command's copy goroutines, and the snapshot must be frozen afterwards.
	snapshot := append([]byte(nil), cmd.Stdout.Bytes()...)
	time.Sleep(100 * time.Millisecond)
	if !bytes.Equal(cmd.Stdout.Bytes(), snapshot) {
		t.Fatal("output buffer kept changing after the timed-out command returned")
	}

	assertProcessGone(t, sentinel, 5*time.Second)
}

func TestRunCommandNormalCompletionUnchanged(t *testing.T) {
	t.Parallel()

	// Exit code 0 with stdout and stderr captured separately.
	cmd := commandWrapper("printf 'out-line\n'; printf 'err-line\n' 1>&2")
	if err := runCommand(cmd, 5000); err != nil {
		t.Fatalf("successful command returned error: %v", err)
	}
	if cmd.Status != 0 {
		t.Fatalf("status = %d, want 0", cmd.Status)
	}
	if got := cmd.Stdout.String(); got != "out-line\n" {
		t.Fatalf("stdout = %q, want %q", got, "out-line\n")
	}
	if got := cmd.Stderr.String(); got != "err-line\n" {
		t.Fatalf("stderr = %q, want %q", got, "err-line\n")
	}

	// Non-zero exit: ExitError is swallowed as before, the exit status is what
	// drives the check.
	cmd = commandWrapper("exit 42")
	if err := runCommand(cmd, 5000); err != nil {
		t.Fatalf("non-zero exit must not surface as an error, got: %v", err)
	}
	if cmd.Status != 42 {
		t.Fatalf("status = %d, want 42", cmd.Status)
	}

	// Exec-style (no shell) commands take the same path.
	cmd = util.NewCommand("sh", "-c", "echo slice")
	if err := runCommand(cmd, 5000); err != nil {
		t.Fatalf("exec-style command returned error: %v", err)
	}
	if got := cmd.Stdout.String(); got != "slice\n" {
		t.Fatalf("stdout = %q, want %q", got, "slice\n")
	}

	// Failures to start the process are real errors, not exit codes.
	cmd = util.NewCommand("/nonexistent-goss-command-xyz")
	if err := runCommand(cmd, 5000); err == nil {
		t.Fatal("expected an error for a command that cannot start, got nil")
	}

	// A zero timeout means "no time limit" (the goss add path relies on this):
	// the command runs to completion and its output is captured.
	cmd = commandWrapper("echo zerotimeout")
	if err := runCommand(cmd, 0); err != nil {
		t.Fatalf("zero-timeout command returned error: %v", err)
	}
	if got := cmd.Stdout.String(); got != "zerotimeout\n" {
		t.Fatalf("stdout = %q, want %q", got, "zerotimeout\n")
	}
}

func TestDefCommandTimeoutFailsWithDuration(t *testing.T) {
	t.Parallel()

	sentinel := "GOSS_DEFCOMMAND_TIMEOUT_TEST"
	c := NewDefCommand(
		context.Background(),
		treeCommand(sentinel),
		&System{},
		util.Config{Timeout: 200 * time.Millisecond},
	)

	status, err := c.ExitStatus()
	if err == nil {
		t.Fatal("expected the timed-out check to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "200ms") {
		t.Fatalf("error %q should contain the configured 200ms timeout", err.Error())
	}
	// SIGKILL shows up as the -1 signal-death status goss already reports for
	// signaled commands; it must never masquerade as a successful exit.
	if status != -1 {
		t.Fatalf("status = %d, want -1 for a command killed by signal", status)
	}
	for _, fn := range []func() (any, error){
		func() (any, error) { r, e := c.Stdout(); return r, e },
		func() (any, error) { r, e := c.Stderr(); return r, e },
	} {
		if _, err := fn(); err == nil {
			t.Fatal("expected stdout/stderr access to report the timeout error")
		}
	}

	assertProcessGone(t, sentinel, 5*time.Second)
}

func TestRunCommandConcurrentTimeoutsAreIsolated(t *testing.T) {
	t.Parallel()

	const n = 8
	errs := make([]error, n+1)
	var outs [n + 1]string
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sentinel := fmt.Sprintf("GOSS_CMD_TIMEOUT_CONC_%d", i)
			errs[i] = runCommand(commandWrapper(treeCommand(sentinel)), 100)
		}()
	}

	// A fast command scheduled alongside the timeouts must still succeed and
	// keep its own output: one timing out must not take others down with it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		cmd := commandWrapper("echo concurrent-ok")
		errs[n] = runCommand(cmd, 100)
		outs[n] = cmd.Stdout.String()
	}()

	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] == nil {
			t.Errorf("command %d: expected timeout error, got nil", i)
			continue
		}
		if !strings.Contains(errs[i].Error(), "100ms") {
			t.Errorf("command %d: error %q should contain 100ms", i, errs[i].Error())
		}
	}
	if errs[n] != nil {
		t.Errorf("fast command should succeed, got error: %v", errs[n])
	}
	if outs[n] != "concurrent-ok\n" {
		t.Errorf("fast command stdout = %q, want %q", outs[n], "concurrent-ok\n")
	}

	for i := 0; i < n; i++ {
		assertProcessGone(t, fmt.Sprintf("GOSS_CMD_TIMEOUT_CONC_%d", i), 5*time.Second)
	}
}
