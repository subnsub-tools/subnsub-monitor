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
	"strconv"
	"strings"
	"testing"
)

// A home directory nothing else can be reached from. Both variables, because
// os.UserHomeDir() reads HOME on unix and USERPROFILE on Windows — setting one
// on the other platform leaves the test reading the real user's machine, where
// it would pass or fail by accident.
func testHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

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
	home := testHome(t)
	writeRollout(t, home)

	p := collectCodex()
	if !p.OK || len(p.Limits) != 1 || p.Limits[0].UsedPercent != 12 {
		t.Fatalf("a plain rollout did not read: %+v", p)
	}
	// A healthy machine must not wear the partial-scan mark: it is the whole
	// value of the mark that it means something when it appears.
	if p.Truncated || p.Capped {
		t.Errorf("a complete scan reported itself partial: truncated=%v capped=%v", p.Truncated, p.Capped)
	}
}

func TestCodexNamesAHardLinkedSessionTree(t *testing.T) {
	home := testHome(t)
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

// The count is what was OPENED, and the sentence has to keep saying only that.
// The real incident had 557 files against a 400-file backstop, so a message
// about "all of them" would have been a claim about 157 nobody looked at.
func TestCodexCountsOnlyWhatItOpened(t *testing.T) {
	home := testHome(t)
	dir := filepath.Join(home, ".codex", "sessions", "2026", "07", "22")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	for i := 0; i < maxFiles+1; i++ {
		n := strconv.Itoa(i)
		file := filepath.Join(dir, "rollout-2026-07-22T05-09-06-"+n+".jsonl")
		if err := os.WriteFile(file, []byte(oneReading), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(file, filepath.Join(elsewhere, n+".jsonl")); err != nil {
			t.Skipf("this filesystem will not hard link: %v", err)
		}
	}

	p := collectCodex()
	if p.DetailCode != "sessions-linked" {
		t.Fatalf("detail code %q", p.DetailCode)
	}
	if p.DetailArg != strconv.Itoa(maxFiles) {
		t.Errorf("count = %q, want the %d it opened and not the %d that exist",
			p.DetailArg, maxFiles, maxFiles+1)
	}
	if !p.Capped {
		t.Error("a scan that stopped at the backstop must say so")
	}
	if strings.Contains(p.Detail, "all ") {
		t.Errorf("sentence claims more than it looked at: %q", p.Detail)
	}
}

// One refused file among readable ones is not the same finding, and must not
// borrow the message: the reading here is real, only the "and nothing newer
// exists" half is weakened.
func TestCodexKeepsReadingWhenOnlySomeFilesAreRefused(t *testing.T) {
	home := testHome(t)
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
