//go:build linux || darwin || !windows
// +build linux darwin !windows

package system

import (
	"context"

	"github.com/goss-org/goss/util"
)

const linuxShell string = "sh"

func commandWrapper(ctx context.Context, cmd string) *util.Command {
	return util.NewCommandContext(ctx, linuxShell, "-c", cmd)
}
