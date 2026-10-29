//go:build windows
// +build windows

package util

import (
	"context"
	"strings"

	"os/exec"
	"syscall"
)

// AttachContext rebuilds the underlying exec.Cmd so that cancelling ctx kills
// the running process, preserving the Windows-specific SysProcAttr (the
// command line is carried there rather than in the argument vector).
// It must be called before Run.
func (c *Command) AttachContext(ctx context.Context) {
	sysProcAttr := c.Cmd.SysProcAttr
	c.Cmd = exec.CommandContext(ctx, c.name)
	c.Cmd.SysProcAttr = sysProcAttr
}

func NewCommandForWindowsCmd(name string, arg ...string) *Command {
	//fmt.Println(arg)
	command := new(Command)
	command.name = name

	// cmd.exe has a unique unquoting algorithm
	// provide the full command line in SysProcAttr.CmdLine, leaving Args empty.
	// more information: https://golang.org/pkg/os/exec/#Command
	command.Cmd = exec.Command(name)
	command.Cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    false,
		CmdLine:       strings.Join(arg, " "),
		CreationFlags: 0,
	}

	return command
}

func NewCommandForWindowsPowershell(name string, arg ...string) *Command {
	command := new(Command)
	command.name = "powershell"

	// Build the powershell command line with -NoProfile -Command
	// The name and args are the PowerShell commands to execute
	cmdLine := "-NoProfile -Command " + name
	if len(arg) > 0 {
		cmdLine += " " + strings.Join(arg, " ")
	}

	command.Cmd = exec.Command("powershell")
	command.Cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    false,
		CmdLine:       cmdLine,
		CreationFlags: 0,
	}

	return command
}
