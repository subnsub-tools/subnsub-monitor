//go:build windows

package main

import "os/exec"

// Windows has no process group in the POSIX sense. The equivalent is a job
// object, which console_windows.go already builds for the console feature:
// create it kill-on-close, assign the process, and closing the handle takes the
// tree down with it.
//
// This is deliberately NOT a second copy of that machinery. The Windows target
// is switched off in build.sh — it only has to keep compiling and vetting — so
// a second, never-executed implementation of the trickiest thing in the package
// would be rot with a good name. What is here is the plain kill, which is what
// the code did on every platform until now, and consoleAdopt is the model to
// follow when the target comes back.
func setProcessGroup(_ *exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	cmd.Process.Kill()
}
