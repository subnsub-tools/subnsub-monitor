//go:build !unix && !windows

package main

import "os/exec"

// Platforms build.sh does not target. Killing the one process is all the
// standard library offers portably, and a grandchild left behind on plan9 is a
// smaller problem than a package that will not compile there.
func setProcessGroup(_ *exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	cmd.Process.Kill()
}
