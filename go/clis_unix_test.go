//go:build unix

package main

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO where a package.json should be is never opened: the open would
// wait for a writer for ever, and every reading after it with it.
func TestCLIsNeverOpenAFIFO(t *testing.T) {
	home, _ := cliMachine(t)
	pkg := filepath.Join(home, ".npm-global", "lib", "node_modules", "@openai", "codex")
	cliProgram(t, filepath.Join(pkg, "bin", "codex.js"), filepath.Join(t.TempDir(), "ran"))
	if err := syscall.Mkfifo(filepath.Join(pkg, "package.json"), 0o644); err != nil {
		t.Skip("no FIFO here:", err)
	}
	cliLink(t, filepath.Join(pkg, "bin", "codex.js"), filepath.Join(home, ".npm-global", "bin", "codex"))
	done := make(chan []CLI, 1)
	go func() { done <- scanCLIs(home) }()
	select {
	case got := <-done:
		if g := cliOf(got, "codex"); len(g) != 1 || g[0] != (CLI{ID: "codex", Source: "npm"}) {
			t.Fatalf("a FIFO is no package.json: %+v", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO held the scan")
	}
}
