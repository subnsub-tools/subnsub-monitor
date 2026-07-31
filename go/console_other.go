//go:build !unix

package main

// Platforms this helper is not built for, kept compiling anyway — same reason
// system_other.go exists. No process groups here: the deadline kills the
// command itself and cmd.WaitDelay bounds the wait for whatever it left behind.

import (
	"os/exec"
	"syscall"
)

func consoleProcAttr() *syscall.SysProcAttr { return nil }

func consoleKill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
