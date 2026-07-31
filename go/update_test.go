package main

import (
	"strings"
	"testing"
)

// The comparison that decides whether a machine installs something. Getting it
// wrong in the "older looks newer" direction would walk a fleet backwards onto
// a build somebody withdrew, which is the whole reason the update path refuses
// to go down at all.
func TestVercmpIsNumericPerComponent(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// The failure string comparison actually has: 9 sorts after 30.
		{"2026.07.09.1", "2026.07.30.1", -1},
		{"2026.7.9", "2026.7.30", -1},
		{"2026.07.31.6", "2026.07.31.6", 0},
		{"2026.07.31.7", "2026.07.31.6", 1},
		{"2026.08.01.1", "2026.07.31.9", 1},
		// A missing trailing component is zero, so the first release of a day
		// is newer than the bare date and not the same as it.
		{"2026.07.31", "2026.07.31.1", -1},
		{"2026.07.31.0", "2026.07.31", 0},
		// No version at all is older than everything — the state of every
		// helper built before the field existed.
		{"", "2026.07.31.6", -1},
		{"2026.07.31.6", "", 1},
		{"", "", 0},
		// Garbage components read as zero rather than as an error, because the
		// only thing this function may do with a bad version is refuse to
		// treat it as newer.
		{"2026.xx.31", "2026.07.31", -1},
	}
	for _, c := range cases {
		if got := vercmp(c.a, c.b); got != c.want {
			t.Fatalf("vercmp(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// The shape has to be the one the RELAY enforces, not merely a safe-looking
// subset of it. A version this side accepts and the relay rejects installs
// fine and then reports as unidentifiable, which shows a healthy machine on
// the board as older than everything.
//
// Kept in step with HELPER_VERSION_RE in monitor-worker/src/clean.js:
//
//	/^\d{4}\.\d{2}\.\d{2}\.\d{1,3}$/
func TestValidVersionMatchesTheRelaysShapeExactly(t *testing.T) {
	for _, ok := range []string{"2026.07.31.6", "2026.07.31.123", "0000.00.00.0"} {
		if !validVersion(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	bad := []string{
		"", "..", "v2026.07.31", "2026.07.31-rc1", "2026.07.31 ",
		"../../etc/passwd", "2026.07.31\n", strings.Repeat("1.", 20),
		// Digits and dots, and the exact string a looser check let through.
		"2999..1",
		// Right idea, wrong shape: single-digit month, no release counter,
		// four digits of counter, a fifth component.
		"2026.7.9", "1", "2026.07.31", "2026.07.31.1234", "2026.07.31.1.2",
	}
	for _, b := range bad {
		if validVersion(b) {
			t.Fatalf("%q should be refused", b)
		}
	}
	// The version this build ships as has to pass its own gate, or the first
	// machine to update reports something the relay will not accept.
	if !validVersion(helperVersion) {
		t.Fatalf("this build's own version is refused: %q", helperVersion)
	}
}

// The manifest lookup is the step that decides which bytes are acceptable, so
// the name has to match exactly and the hash has to look like one.
func TestSumForMatchesTheWholeNameOnly(t *testing.T) {
	manifest := "" +
		"aaaa000000000000000000000000000000000000000000000000000000000001  subnsub-monitor-linux-amd64\n" +
		"aaaa000000000000000000000000000000000000000000000000000000000002  subnsub-monitor-linux-arm64\n" +
		"aaaa000000000000000000000000000000000000000000000000000000000003 *subnsub-monitor-darwin-arm64\n" +
		"SHA256SUMS\n"

	if got := sumFor(manifest, "subnsub-monitor-linux-arm64"); !strings.HasSuffix(got, "02") {
		t.Fatalf("exact name: %q", got)
	}
	// The '*' marker sha256sum writes for a binary read is part of the format,
	// not part of the name.
	if got := sumFor(manifest, "subnsub-monitor-darwin-arm64"); !strings.HasSuffix(got, "03") {
		t.Fatalf("binary marker: %q", got)
	}
	// A PREFIX of a listed name must not match. These two differ by one
	// character in the field that decides which bytes get installed.
	if got := sumFor(manifest, "subnsub-monitor-linux-arm"); got != "" {
		t.Fatalf("prefix matched: %q", got)
	}
	if got := sumFor(manifest, "subnsub-monitor-windows-amd64"); got != "" {
		t.Fatalf("absent name matched: %q", got)
	}
}

// A manifest whose hash field is not a hash is a manifest to refuse. Returning
// it would make the comparison downstream fail anyway — but as a checksum
// mismatch, which reads as "someone swapped the binary" rather than "the
// release manifest is broken".
func TestSumForRefusesAHashThatIsNotOne(t *testing.T) {
	for _, m := range []string{
		"nothex  subnsub-monitor-linux-amd64\n",
		"aaaa  subnsub-monitor-linux-amd64\n",
		"gggg000000000000000000000000000000000000000000000000000000000001  subnsub-monitor-linux-amd64\n",
		"aaaa000000000000000000000000000000000000000000000000000000000001 extra field  subnsub-monitor-linux-amd64\n",
	} {
		if got := sumFor(m, "subnsub-monitor-linux-amd64"); got != "" {
			t.Fatalf("accepted %q from %q", got, m)
		}
	}
}

// The switch has to fail closed on every path, and the console has to imply it
// — a machine that already hands out /bin/sh is not made safer by refusing the
// narrower operation.
func TestUpdateAllowedFollowsTheEnvironmentAndTheConsole(t *testing.T) {
	t.Setenv(consoleEnvVar, "")
	t.Setenv(updateEnvVar, "")
	// A config directory that does not exist stands in for a machine with
	// neither switch set; HOME is redirected so the test never reads the
	// developer's own install.
	t.Setenv("HOME", t.TempDir())
	if updateAllowed() {
		t.Fatal("no switch set should be off")
	}

	t.Setenv(consoleEnvVar, "1")
	if !updateAllowed() {
		t.Fatal("the console being on should allow updates")
	}

	// An explicit environment setting wins outright, including over that.
	t.Setenv(updateEnvVar, "off")
	if updateAllowed() {
		t.Fatal("MON_UPDATE=off should win over the console")
	}
	t.Setenv(consoleEnvVar, "")
	t.Setenv(updateEnvVar, "on")
	if !updateAllowed() {
		t.Fatal("MON_UPDATE=on should be enough on its own")
	}
	// Anything that is not an affirmative is a no, rather than "not off".
	t.Setenv(updateEnvVar, "maybe")
	if updateAllowed() {
		t.Fatal("an unrecognised value must not read as on")
	}
}

// The file is the switch for a machine with no console, and its EXISTENCE is
// the setting — writing "off" into it must not turn it off.
func TestSetRemoteUpdateWritesAndRemovesTheFile(t *testing.T) {
	t.Setenv(consoleEnvVar, "")
	t.Setenv(updateEnvVar, "")
	t.Setenv("HOME", t.TempDir())

	if err := setRemoteUpdate(true); err != nil {
		t.Fatalf("on: %v", err)
	}
	if !updateAllowed() {
		t.Fatal("the file was written and is not being read")
	}
	if err := setRemoteUpdate(false); err != nil {
		t.Fatalf("off: %v", err)
	}
	if updateAllowed() {
		t.Fatal("the file was removed and is still reading as on")
	}
	// Removing something already absent is the ordinary case on every machine
	// that never turned it on, and must not be an error.
	if err := setRemoteUpdate(false); err != nil {
		t.Fatalf("off twice: %v", err)
	}
}
