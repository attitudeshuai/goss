//go:build windows
// +build windows

package system

import (
	"syscall"

	"github.com/goss-org/goss/util"
)

const windowsShell string = "cmd"

func commandWrapper(cmd string) *util.Command {
	return util.NewCommandForWindowsCmd(windowsShell, "/c", cmd)
}

// prepareCommand places the command in a new process group, independent of
// goss's own console group. The timeout path kills the process tree rooted at
// the command with taskkill /T, which covers cmd.exe and everything it
// spawned.
func prepareCommand(cmd *util.Command) {
	attr := cmd.Cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
	cmd.Cmd.SysProcAttr = attr
}
