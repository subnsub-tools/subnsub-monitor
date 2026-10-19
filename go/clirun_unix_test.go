//go:build unix

package main

// The tree variant of the vendor run: what the CLI started dies with it.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// `sh -c 'sleep & wait'` is agy's shape — a leader with a child of its own.
// Past the deadline the leader is killed either way; the child is what the
// process group is for, and this checks it is gone rather than reparented
// and still sleeping.
func TestRunVendorCLITreeKillsTheGrandchild(t *testing.T) {
	sh := "/bin/sh"
	if _, err := os.Stat(sh); err != nil {
		t.Skip("no /bin/sh")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	_, _, err := runVendorCLITree(sh, []string{"-c", "sleep 60 & echo $! > " + pidFile + "; wait"},
		[]string{"PATH=/usr/bin:/bin"}, 500*time.Millisecond)
	if err != errAmpTimeout {
		t.Fatalf("expected the deadline, got %v after %s", err, time.Since(start))
	}
	raw, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatalf("the shell never wrote its child's pid: %v", rerr)
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil || pid <= 0 {
		t.Fatalf("pid file: %q", raw)
	}
	// The group is signalled before Wait returns, but the child's exit is
	// collected by init in its own time; a moment's grace, not a race.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL) // tidy up before failing
	t.Fatalf("the grandchild (pid %d) outlived the run", pid)
}
