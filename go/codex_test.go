package main

// What the Codex collector says when it is not allowed to read anything.
//
// The guard that refuses a multiply-named file is deliberate and stays. What
// was wrong was the sentence it produced: a whole session tree that something
// else had hard-linked reported "no quota recorded yet", which reads as "you
// have not used Codex" and sends whoever sees it looking at Codex. The cause
// was a leftover fixture in /tmp holding a second name for all 557 files, and
// finding that took stat(1) and a filesystem-wide search by inode.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const oneReading = `{"timestamp":"2026-07-21T21:57:41.161Z","type":"event_msg",` +
	`"payload":{"type":"token_count","rate_limits":{"primary":` +
	`{"used_percent":12.0,"window_minutes":10080,"resets_at":1785258178}}}}` + "\n"

// A sessions tree holding one rollout, at the path the collector looks in.
func writeRollout(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "07", "22")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "rollout-2026-07-22T05-09-06-abc.jsonl")
	if err := os.WriteFile(file, []byte(oneReading), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestCodexReadsARollout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRollout(t, home)

	p := collectCodex()
	if !p.OK || len(p.Limits) != 1 || p.Limits[0].UsedPercent != 12 {
		t.Fatalf("a plain rollout did not read: %+v", p)
	}
}

func TestCodexNamesAHardLinkedSessionTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := writeRollout(t, home)

	// A second name for the same inode — what a backup with --link-dest, a
	// de-duplicator, or a test fixture built out of `cp -al` leaves behind.
	// Anywhere else on the same filesystem will do; the count is on the inode,
	// not on the directory it is reached through.
	if err := os.Link(file, filepath.Join(t.TempDir(), "elsewhere.jsonl")); err != nil {
		t.Skipf("this filesystem will not hard link: %v", err)
	}

	p := collectCodex()
	if p.OK {
		t.Fatal("a multiply-named file was read; the guard is gone")
	}
	if p.DetailCode != "sessions-linked" {
		t.Fatalf("detail code %q — a refused tree must not read as an empty one", p.DetailCode)
	}
	if p.DetailArg != "1" {
		t.Errorf("refused count = %q, want 1", p.DetailArg)
	}
	// Refused is not searched: claiming the files hold nothing would be a
	// negative this never established.
	if !p.Truncated {
		t.Error("a tree that was never read must not report a complete scan")
	}
	if !strings.Contains(p.Detail, "more than one name") {
		t.Errorf("sentence does not name the cause: %q", p.Detail)
	}
}

// One refused file among readable ones is not the same finding, and must not
// borrow the message: the reading here is real, only the "and nothing newer
// exists" half is weakened.
func TestCodexKeepsReadingWhenOnlySomeFilesAreRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRollout(t, home)

	dir := filepath.Join(home, ".codex", "sessions", "2026", "07", "23")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(dir, "rollout-2026-07-23T00-00-00-def.jsonl")
	if err := os.WriteFile(newer, []byte(oneReading), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(newer, filepath.Join(t.TempDir(), "elsewhere.jsonl")); err != nil {
		t.Skipf("this filesystem will not hard link: %v", err)
	}

	p := collectCodex()
	if !p.OK || len(p.Limits) != 1 {
		t.Fatalf("the readable file was not used: %+v", p)
	}
	if !p.Truncated {
		t.Error("a skipped candidate must still weaken the freshness claim")
	}
}
