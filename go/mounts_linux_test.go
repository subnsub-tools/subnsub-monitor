//go:build linux

package main

// The one mounts test that is about LINUX rather than about mounts: the
// function it exercises parses /proc/self/mountinfo and is defined in
// system_linux.go, behind that file's own build tag.
//
// Split out rather than left where it was written, because the test file it
// came from has no tag and `GOOS=windows go vet ./...` is what build.sh relies
// on to keep the Windows port from rotting while it is switched off. One
// untagged reference to a Linux-only symbol turns that guard into a failure
// nobody reads, and it had been failing since 2026-08-07.

import "testing"

// The kernel escapes space, tab, newline and backslash in /proc/mounts as
// three-digit octal. A parser that misses that reads one path as two fields
// and measures the wrong filesystem.
func TestUnescapeMountField(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`/mnt/my\040disk`, "/mnt/my disk"},
		{`/mnt/a\011b`, "/mnt/a\tb"},
		{`/mnt/a\012b`, "/mnt/a\nb"},
		{`/mnt/a\134b`, `/mnt/a\b`},
		{"/mnt/plain", "/mnt/plain"},
		// Not an escape this understands: left exactly as found rather than
		// given an invented meaning.
		{`/mnt/a\zzb`, `/mnt/a\zzb`},
		{`/mnt/trailing\`, `/mnt/trailing\`},
		{`/mnt/short\04`, `/mnt/short\04`},
	} {
		if got := unescapeMountField(c.in); got != c.want {
			t.Errorf("unescapeMountField(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
