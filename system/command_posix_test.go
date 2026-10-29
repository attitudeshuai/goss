//go:build linux || darwin || !windows
// +build linux darwin !windows

package system

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestCommandWrapper(t *testing.T) {
	t.Parallel()

	c := commandWrapper(context.Background(), "echo hello world")
	cmdPath, _ := exec.LookPath(linuxShell)
	if c.Cmd.Path != cmdPath {
		t.Errorf("Command not wrapped properly for OS. got %s, want: %s", c.Cmd.Path, cmdPath)
	}
}

// TestRunCommandReturnsWhenContextCancelled pins that an in-flight check
// running a long-lived subprocess returns promptly when the caller cancels,
// instead of waiting out the command timeout.
func TestRunCommandReturnsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := commandWrapper(ctx, "sleep 30")

	done := make(chan error, 1)
	go func() {
		done <- runCommand(ctx, cmd, 60000)
	}()

	// Let the subprocess start before cancelling.
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want context.Canceled", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("runCommand took %s to return after cancellation", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runCommand did not return after the context was cancelled")
	}
}
