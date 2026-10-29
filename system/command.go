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
	err := runCommand(c.Ctx, cmd, c.Timeout)

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

// runCommand runs cmd, bounding it by the per-check timeout and by ctx
// cancellation. When ctx is cancelled (for example because `goss serve`
// exhausted its shutdown grace period) the underlying process is killed and
// runCommand waits for the reaper goroutine to return, so no goroutine or
// child process is left behind.
func runCommand(ctx context.Context, cmd *util.Command, timeout int) error {
	timeoutD := time.Duration(timeout) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeoutD)
	defer cancel()
	cmd.AttachContext(ctx)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// CommandContext kills the process on cancellation, so the Run
		// goroutine returns promptly; wait for it instead of abandoning it.
		<-done
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("Command execution timed out (%s)", timeoutD)
		}
		return ctx.Err()
	}
}
