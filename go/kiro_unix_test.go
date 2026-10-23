//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Offline, kiro-cli opens sign-in pages rather than failing, so with no
// answer from Kiro's service it is never started; online it is asked as
// before (CodexBar KiroBackgroundConnectivityTests,
// steipete/CodexBar@49e7581587).
func TestKiroOfflineNeverStartsTheCLI(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	bin := filepath.Join(dir, "kiro-cli")
	script := "#!/bin/sh\n: > '" + ran + "'\n" +
		"echo 'Estimated Usage | resets on 2026-09-01 | KIRO FREE'\n" +
		"echo 'Credits (1.00 of 50 covered in plan)'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(kiroBinEnv, bin)
	saved := kiroReachable
	t.Cleanup(func() { kiroReachable = saved })

	kiroReachable = func() bool { return false }
	p := fetchKiro()
	if p.OK || p.Error != "unreachable" || p.DetailCode != "req-failed" {
		t.Fatalf("offline: %+v", p)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("kiro-cli was started with no network")
	}

	kiroReachable = func() bool { return true }
	p = fetchKiro()
	if !p.OK || len(p.Limits) != 1 || p.Limits[0].UsedPercent != 2 {
		t.Fatalf("online: %+v", p)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatal("online, kiro-cli is asked")
	}
}
