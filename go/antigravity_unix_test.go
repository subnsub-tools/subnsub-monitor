//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every report run tells agy not to update itself, in the spelling agy
// reads ("true"; "1" is ignored), whatever the helper itself inherited
// (CodexBar AntigravityProviderDescriptor.childEnvironment,
// steipete/CodexBar@d6c651ca26).
func TestAgyRunDisablesTheAutoUpdater(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	script := "#!/bin/sh\n" +
		"echo \"update=$AGY_CLI_DISABLE_AUTO_UPDATE\"\n" +
		"env | grep -c '^AGY_CLI_DISABLE_AUTO_UPDATE=' | sed 's/^/count=/'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGY_CLI_DISABLE_AUTO_UPDATE", "false")
	out, exited, err := agyRun(bin)
	if err != nil || exited {
		t.Fatalf("run: %q exited=%v err=%v", out, exited, err)
	}
	if !strings.Contains(out, "update=true\n") || !strings.Contains(out, "count=1\n") {
		t.Fatalf("agy saw %q, want the updater off exactly once", out)
	}
}
