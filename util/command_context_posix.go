//go:build !windows
// +build !windows

package util

import (
	"os/exec"
	"syscall"
)

// configureContextCancel makes cancelling a command kill its whole process
// group. Goss runs shell strings through `sh -c`, so killing only the shell
// would leave children of a compound command (e.g. `touch x && sleep 30`)
// running as orphans after the check was cancelled.
func configureContextCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative PID sends the signal to the process group led by the
		// spawned process.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
