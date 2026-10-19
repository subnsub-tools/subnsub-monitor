//go:build !windows

package main

import (
	"os"
	"strings"
	"syscall"
)

// Become the new binary in place, without asking anyone's permission or help.
//
// execve replaces this process's image and keeps everything a supervisor uses
// to identify it: the PID, the parent, the process group and session, the
// controlling terminal, the cgroup, the open file descriptors that are not
// close-on-exec. Nothing observes an exit, so nothing has to be watching in
// order for the update to land. That is the property worth having here — it is
// true on a systemd host, true under launchd, true under a shell loop, and
// true under a container init that supervises nothing at all, which is the case
// this exists for.
//
// It is also strictly better on the hosts that DID work before. systemd never
// sees the service stop, so there is no RestartSec=30 gap where the machine is
// missing from the dashboard, and no window in which a crash-looping successor
// could be mistaken for the update failing.
//
// What it gives up is small and worth naming: the environment is inherited
// from this process rather than re-read from the unit's EnvironmentFile, so a
// token changed on disk since this process started is not picked up by an
// update. It never was — a token change means running install.sh again, and
// that restarts the service outright.
//
// Go's syscall.Exec handles the multithreaded runtime for us: execve tears
// down every other thread as part of replacing the image. Returns only on
// failure; on success there is no "after".
func restartInto(self string) error {
	// argv[0] is the resolved path, not os.Args[0], which may be a bare name
	// found on PATH or a relative path from a working directory this process
	// no longer has any claim to.
	argv := append([]string{self}, os.Args[1:]...)
	// One line added to the inherited environment: which version is handing
	// over. The successor reads it once (noteInstalledFrom) and its first
	// reading says "installed, from <this>" — the outcome this process cannot
	// report, because by the time it is true this process is gone. Any stale
	// copy from an earlier handover is dropped first; the successor unsets it
	// on reading, but a handover that failed to exec would leave it set.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, updatedFromEnv+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, updatedFromEnv+"="+helperVersion)
	return syscall.Exec(self, argv, env)
}
