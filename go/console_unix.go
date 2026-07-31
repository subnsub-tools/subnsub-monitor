//go:build unix

package main

// Killing a console command means killing what it started, not just the shell.
//
// `sh -c 'sleep 999'` replaces nothing: the shell forks, and signalling only
// the shell leaves the child running with the output pipe still open. Putting
// the command in its own process group and signalling the GROUP is what makes
// a timeout mean the thing it says.
//
// Split into its own file for the build tag, not because the code wants to be
// separate: syscall.SysProcAttr has different fields on every platform, and
// the package still has to compile for the ones this helper is never built for.

import (
	"os/exec"
	"syscall"
)

func consoleProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// Negative pid = the process group. SIGKILL rather than SIGTERM: this fires
// only after the command already had its full timeout, so it is the deadline
// being enforced rather than a request to wind up.
func consoleKill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		// The group call fails if setpgid did not take (a platform that
		// ignored it, a process that already reaped). Falling back to the
		// process itself is strictly better than leaving it running.
		return cmd.Process.Kill()
	}
	return nil
}
