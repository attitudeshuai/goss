//go:build windows
// +build windows

package util

import (
	"strconv"
	"strings"

	"os/exec"
	"syscall"
)

// KillProcessTree forcibly terminates the command and every process it
// spawned. taskkill's /T flag walks the process tree parented to the command
// (cmd.exe and everything it launched) and /F forces termination, the Windows
// counterpart of SIGKILL on POSIX.
func (c *Command) KillProcessTree() error {
	if c.Cmd == nil || c.Cmd.Process == nil {
		return nil
	}
	kill := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(c.Cmd.Process.Pid))
	return kill.Run()
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
