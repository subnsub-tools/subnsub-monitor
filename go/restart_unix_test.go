//go:build !windows

// The handover itself, proved rather than assumed.
//
// This is the file that would have caught the seven hours of silence: the old
// code exited and trusted a service manager to start the successor, and on a
// container whose PID 1 supervises nothing there was no successor. The
// property that makes the new code work under such an init is that NOTHING
// OBSERVES AN EXIT — execve replaces the image and keeps the PID — and that is
// exactly what is asserted below.

package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Re-executes THIS test binary, which is the only executable a test can be
// sure exists and is runnable on the machine running it.
//
// Stage 1 records its own PID, arms stage 2 in the environment, and hands over
// through the code under test. Stage 2 records its PID and stops. If the exec
// worked, stage 2 ran at all — and if it worked the way the update path needs,
// both stages printed the same PID, because there was only ever one process.
func TestRestartIntoBecomesTheNewBinaryWithoutAnExit(t *testing.T) {
	if os.Getenv("MON_RESTART_STAGE") != "" {
		return // running as the child; the real work is in TestRestartStages
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("no path to this test binary: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=TestRestartStages", "-test.v=false")
	cmd.Env = append(os.Environ(), "MON_RESTART_STAGE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	var pids []string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "stage-pid "); ok {
			pids = append(pids, rest)
		}
	}
	if len(pids) != 2 {
		t.Fatalf("want both stages to report, got %v\n%s", pids, out)
	}
	// The whole point. A different PID here would mean the successor was a
	// NEW process — which is what needs a supervisor, and what is not
	// available on the machines this exists for.
	if pids[0] != pids[1] {
		t.Fatalf("exec did not keep the process: stage 1 was pid %s, stage 2 pid %s", pids[0], pids[1])
	}
	if _, err := strconv.Atoi(pids[0]); err != nil {
		t.Fatalf("not a pid: %q", pids[0])
	}
}

// The child. Skipped entirely unless the parent above armed it, so an ordinary
// `go test ./...` neither runs it nor re-execs anything.
func TestRestartStages(t *testing.T) {
	stage := os.Getenv("MON_RESTART_STAGE")
	if stage == "" {
		t.Skip("only runs as the child of TestRestartIntoBecomesTheNewBinary…")
	}
	os.Stdout.WriteString("stage-pid " + strconv.Itoa(os.Getpid()) + "\n")
	if stage == "2" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("no path to this test binary: %v", err)
	}
	// restartInto passes os.Environ() through, so arming the next stage is a
	// Setenv here rather than anything the function needs to know about.
	os.Setenv("MON_RESTART_STAGE", "2")
	// Flushed before the handover: execve does not run deferred functions and
	// does not flush anything, so a buffered line would be lost with the image.
	os.Stdout.Sync()
	if err := restartInto(exe); err != nil {
		t.Fatalf("restartInto returned instead of handing over: %v", err)
	}
	t.Fatal("restartInto returned with no error, which cannot happen on unix")
}
