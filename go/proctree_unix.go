//go:build unix

package main

// Taking down what a child started, not only the child.
//
// A CLI installed from npm is typically a script that spawns the real binary
// with inherited stdio. Killing the script leaves that binary running — still
// holding the write end of every pipe it was handed. Closing our end of its
// stdin is the polite way out and collects a healthy server; a wedged one needs
// this, and without it a failing collector leaves one live process behind per
// attempt, forever.

import (
	"os/exec"
	"syscall"
)

// Give the child a process group of its own, so killProcessTree has something
// to aim at. Must be called before Start; after it, the group is already set.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Kill the child and everything it started.
//
// The group is checked against the child's own pid before it is signalled, and
// that check is load-bearing rather than defensive habit. A negative pid means
// "the whole group", and if Setpgid did not take — an old kernel, a sandbox
// that refuses it — the group the child sits in is OURS. The same call would
// then kill the helper. Falling back to killing the one process is the correct
// failure: one stray grandchild costs memory, killing ourselves costs the
// machine its monitoring.
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil && pgid == cmd.Process.Pid {
		if syscall.Kill(-pgid, syscall.SIGKILL) == nil {
			return
		}
	}
	cmd.Process.Kill()
}
