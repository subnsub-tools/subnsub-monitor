//go:build !unix && !windows

package main

// Platforms this helper is not built for, kept compiling anyway — same reason
// fs_other.go exists. No process groups and no job objects here: the deadline
// kills the command itself and cmd.WaitDelay bounds the wait for whatever it
// left behind.

import (
	"context"
	"os/exec"
)

func consoleCommand(ctx context.Context, line string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", line)
}

func consoleAdopt(_ *exec.Cmd) {}

func consoleKill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
