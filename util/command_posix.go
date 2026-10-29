//go:build !windows
// +build !windows

package util

import "syscall"

// KillProcessTree forcibly terminates the command and every process in its
// process group. Commands run through the timeout-aware runner are started as
// the leader of their own process group (see prepareCommand), so the group
// contains the shell and all of the processes it spawns; signaling the
// negative PID delivers SIGKILL to the whole tree in one call.
func (c *Command) KillProcessTree() error {
	if c.Cmd == nil || c.Cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-c.Cmd.Process.Pid, syscall.SIGKILL)
}
