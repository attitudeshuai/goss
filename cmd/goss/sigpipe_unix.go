//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// On Unix the Go runtime kills the process with SIGPIPE (shell status 141)
// when stdout is a pipe whose reader has closed, before any write error can
// be observed. Asking the runtime to deliver SIGPIPE instead makes writes to
// the closed pipe fail with EPIPE, so every outputer goes through the shared
// write-error path and goss can exit with its own output-error status.
func init() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}
