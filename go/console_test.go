package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The cap has to hold as the bytes ARRIVE, and it must not make the child's
// write fail — a short count from an io.Writer turns "we stopped listening"
// into "your command crashed".
func TestCapWriterStopsAtTheCapAndNeverShortWrites(t *testing.T) {
	w := &capWriter{max: 10}
	n, err := w.Write([]byte("0123456789abcdef"))
	if err != nil || n != 16 {
		t.Fatalf("want the whole write claimed, got n=%d err=%v", n, err)
	}
	if got := w.buf.String(); got != "0123456789" {
		t.Fatalf("kept %q", got)
	}
	if !w.over {
		t.Fatal("truncation not recorded")
	}

	// A second write past a full buffer still reports success and still keeps
	// nothing.
	if n, err := w.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("second write: n=%d err=%v", n, err)
	}
	if w.buf.Len() != 10 {
		t.Fatalf("buffer grew past the cap: %d", w.buf.Len())
	}
}

func TestCapWriterUnderTheCapIsUntouched(t *testing.T) {
	w := &capWriter{max: 64}
	w.Write([]byte("hello"))
	w.Write([]byte(" world"))
	if got := w.buf.String(); got != "hello world" {
		t.Fatalf("got %q", got)
	}
	if w.over {
		t.Fatal("claimed truncation it did not do")
	}
}

// The switch is a FILE, and everything that is not that file is off. This is
// the whole security posture of the feature, so it gets a test rather than a
// comment.
func TestConsoleIsOffWithoutTheFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(consoleEnvVar, "")
	if consoleEnabled() {
		t.Fatal("console on with no file and no env")
	}
	if err := setConsole(true); err != nil {
		t.Fatalf("setConsole(true): %v", err)
	}
	if !consoleEnabled() {
		t.Fatal("console still off after being switched on")
	}
	if err := setConsole(false); err != nil {
		t.Fatalf("setConsole(false): %v", err)
	}
	if consoleEnabled() {
		t.Fatal("console still on after being switched off")
	}
	// Off twice is not an error — `console off` on a machine that never had it
	// on is a thing a config-management run will do every time it runs.
	if err := setConsole(false); err != nil {
		t.Fatalf("second setConsole(false): %v", err)
	}
}

func TestConsoleEnvOverridesTheFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(consoleEnvVar, "1")
	if !consoleEnabled() {
		t.Fatal("MON_CONSOLE=1 did not enable it")
	}
	// And the off direction, which matters more: an operator who sets
	// MON_CONSOLE=0 in a unit file must not be overruled by a leftover file.
	t.Setenv(consoleEnvVar, "0")
	if err := setConsole(true); err != nil {
		t.Fatalf("setConsole: %v", err)
	}
	if consoleEnabled() {
		t.Fatal("MON_CONSOLE=0 was overruled by the file")
	}
}

func TestRunConsoleCommandReportsOutputAndExitCode(t *testing.T) {
	r := runConsoleCommand("c1", "printf 'a\\nb\\n'; printf 'e\\n' >&2; exit 3")
	if r.ID != "c1" {
		t.Fatalf("id %q", r.ID)
	}
	if r.Code != 3 {
		t.Fatalf("exit code %d, want 3", r.Code)
	}
	// stdout and stderr land in one stream, the way a terminal shows them.
	if !strings.Contains(r.Out, "a\nb\n") || !strings.Contains(r.Out, "e\n") {
		t.Fatalf("output %q", r.Out)
	}
	if r.Error != "" {
		t.Fatalf("unexpected error %q", r.Error)
	}
}

// The deadline has to end the whole tree, not just the shell. `sleep` inside a
// backgrounded subshell keeps the output pipe open, which is exactly the shape
// that hangs a naive implementation forever.
func TestRunConsoleCommandSurvivesABackgroundedChild(t *testing.T) {
	done := make(chan consoleResult, 1)
	go func() { done <- runConsoleCommand("c2", "(sleep 30 &); echo started") }()
	select {
	case r := <-done:
		if !strings.Contains(r.Out, "started") {
			t.Fatalf("output %q", r.Out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a backgrounded grandchild held the command open")
	}
}

// …and the process it left behind has to be GONE, which returning promptly
// does not prove on its own. This is the check the first version of these
// tests was missing: `sleep 300 >/dev/null 2>&1 &` lets the shell exit
// immediately and successfully, so the context deadline never fires and
// nothing kills the sleep unless the process group is taken down after Run.
func TestRunConsoleCommandLeavesNoStrayProcess(t *testing.T) {
	// A marker the pgrep below can find and no other test can collide with.
	marker := "subnsub-console-stray-probe"
	r := runConsoleCommand("c3", "sleep 300 "+marker+" >/dev/null 2>&1 & echo spawned")
	if !strings.Contains(r.Out, "spawned") {
		t.Fatalf("command did not run: %q %v", r.Out, r.Error)
	}
	// The kill is a signal, not a synchronous reap; give the scheduler a beat.
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", marker).Output()
		if len(strings.TrimSpace(string(out))) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a backgrounded process outlived its command: pids %s",
				strings.TrimSpace(string(out)))
		}
		time.Sleep(100 * time.Millisecond)
	}
}
