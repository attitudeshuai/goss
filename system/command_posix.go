//go:build linux || darwin || !windows
// +build linux darwin !windows

package system

import (
	"syscall"

	"github.com/goss-org/goss/util"
)

const linuxShell string = "sh"

func commandWrapper(cmd string) *util.Command {
	return util.NewCommand(linuxShell, "-c", cmd)
}

// prepareCommand places the command in a brand new process group whose ID is
// the command's own PID. That lets the timeout path SIGKILL the shell together
// with every process it spawned by signaling the negative PID, without
// touching goss's own process group or any other running check.
func prepareCommand(cmd *util.Command) {
	attr := cmd.Cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.Setpgid = true
	cmd.Cmd.SysProcAttr = attr
}
