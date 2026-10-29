//go:build windows
// +build windows

package util

import "os/exec"

// configureContextCancel is a no-op on Windows: exec.CommandContext already
// kills the spawned process when the context is cancelled. Tearing down the
// whole process tree would require a Job Object and is intentionally left to
// the default behaviour.
func configureContextCancel(cmd *exec.Cmd) {}
