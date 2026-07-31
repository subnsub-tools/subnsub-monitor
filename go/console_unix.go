//go:build unix

package main

// Running a console command, and killing what it started.
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
	"context"
	"os/exec"
	"syscall"
)

// One command, ready to start. `/bin/sh -c LINE` — an absolute path rather
// than a PATH lookup, because what runs here is decided by this file and not by
// the environment the service manager happened to hand us.
func consoleCommand(ctx context.Context, line string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", line)
	// Own process group, so the deadline can take the whole tree down rather
	// than the shell only.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// Nothing to do once it is running: Setpgid took effect at fork, and a process
// group outlives the process that led it — which is what lets consoleKill work
// even after the shell itself has been reaped. Windows has no such thing and
// has to do its arranging here instead.
func consoleAdopt(_ *exec.Cmd) {}

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
