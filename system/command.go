package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/goss-org/goss/util"
)

// ContextKey is  for minting unique keys in a context.
type ContextKey struct{}

// CommandIDKey is the only instance that must be used everywhere.
var CommandIDKey = ContextKey{}

// errEmptyCommand is returned when a command has neither a shell string nor an
// exec-style argument list.
var errEmptyCommand = errors.New("empty command")

type Command interface {
	Command() util.ExecCommand
	Exists() (bool, error)
	ExitStatus() (int, error)
	Stdout() (io.Reader, error)
	Stderr() (io.Reader, error)
}

type DefCommand struct {
	Ctx        context.Context
	command    util.ExecCommand
	exitStatus int
	stdout     io.Reader
	stderr     io.Reader
	loaded     bool
	Timeout    int
	err        error
}

// NewDefCommand accepts a command specified either shell style (string) or exec
// style ([]string). The concrete type is validated here; an unexpected type is
// recorded as an error on the returned Command so it surfaces as a failing test
// result rather than a panic. Values loaded from a gossfile are already
// validated by util.ExecCommand's unmarshalers, so this only guards against
// programming errors.
func NewDefCommand(ctx context.Context, command any, system *System, config util.Config) Command {
	c := &DefCommand{
		Ctx:     ctx,
		Timeout: config.TimeOutMilliSeconds(),
	}
	switch cmd := command.(type) {
	case string:
		c.command = util.ExecCommand{CmdStr: cmd}
	case []string:
		c.command = util.ExecCommand{CmdSlice: cmd}
	default:
		c.err = fmt.Errorf("command type must be either a string or a list of strings, got %T", command)
		c.loaded = true
	}
	return c
}

func (c *DefCommand) setup() error {
	if c.loaded {
		return c.err
	}
	c.loaded = true

	var cmd *util.Command
	switch {
	case c.command.CmdStr != "":
		cmd = commandWrapper(c.command.CmdStr)
	case len(c.command.CmdSlice) > 0:
		cmd = util.NewCommand(c.command.CmdSlice[0], c.command.CmdSlice[1:]...)
	default:
		c.err = errEmptyCommand
		return c.err
	}
	err := runCommand(cmd, c.Timeout)

	// We don't care about ExitError since it's covered by status
	if _, ok := err.(*exec.ExitError); !ok {
		c.err = err
	}
	c.exitStatus = cmd.Status
	stdoutB := cmd.Stdout.Bytes()
	stderrB := cmd.Stderr.Bytes()

	id := c.Ctx.Value(CommandIDKey)
	logBytes(stdoutB, fmt.Sprintf("[Command][%s][stdout] ", id))
	logBytes(stderrB, fmt.Sprintf("[Command][%s][stderr] ", id))
	c.stdout = bytes.NewReader(stdoutB)
	c.stderr = bytes.NewReader(stderrB)

	return c.err
}

func (c *DefCommand) Command() util.ExecCommand {
	return c.command
}

func (c *DefCommand) ExitStatus() (int, error) {
	err := c.setup()

	return c.exitStatus, err
}

func (c *DefCommand) Stdout() (io.Reader, error) {
	err := c.setup()

	return c.stdout, err
}

func (c *DefCommand) Stderr() (io.Reader, error) {
	err := c.setup()

	return c.stderr, err
}

// Stub out
func (c *DefCommand) Exists() (bool, error) {
	return false, nil
}

// runCommand runs cmd and enforces timeout. On timeout it kills the whole
// process tree started for the check (the shell and everything it spawned)
// and waits for the runner goroutine to finish reaping the process and
// draining its output before returning. That guarantees the timed-out command
// keeps no residual or zombie processes, cannot write into the results after
// the fact, and cannot race the readers of its output buffers. A non-positive
// timeout means no time limit: the command runs to completion.
func runCommand(cmd *util.Command, timeout int) error {
	prepareCommand(cmd)

	// ExitErrors carry the command's exit status and are not setup errors, so
	// they are reported through cmd.Status exactly as before.
	normalize := func(err error) error {
		if _, ok := err.(*exec.ExitError); ok {
			return nil
		}
		return err
	}

	// Start in this goroutine so the process (and its PID) exists before the
	// timeout timer is armed and before any other goroutine can touch it.
	if err := cmd.Start(); err != nil {
		return normalize(err)
	}

	// Wait runs in its own goroutine; it reaps the process and joins the
	// output-copy goroutines, so receiving from it establishes that no output
	// buffer can still be written to.
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	if timeout <= 0 {
		return normalize(<-done)
	}

	timeoutD := time.Duration(timeout) * time.Millisecond
	timer := time.NewTimer(timeoutD)
	defer timer.Stop()
	select {
	case err := <-done:
		return normalize(err)
	case <-timer.C:
		// Best effort: SIGKILL the process group / kill the Windows tree.
		// ESRCH-style "already gone" answers are harmless; waiting on the
		// runner below is what guarantees the process has been reaped.
		_ = cmd.KillProcessTree()
		<-done
		return fmt.Errorf("Command execution timed out (%s)", timeoutD)
	}
}
