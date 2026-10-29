//go:build !windows
// +build !windows

package util

import (
	"context"
	"os/exec"
)

// AttachContext rebuilds the underlying exec.Cmd so that cancelling ctx kills
// the running process. It must be called before Run.
func (c *Command) AttachContext(ctx context.Context) {
	c.Cmd = exec.CommandContext(ctx, c.name, c.args...)
}
