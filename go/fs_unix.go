//go:build unix

package main

// The two things this helper needs from a filesystem that the standard library
// does not expose portably: how many names a file has, and whether a file is
// one this process should be willing to execute.
//
// Both are asked in security decisions — the first guards the Codex session
// reader against a hard link planted in the sessions tree, the second guards
// running the Amp CLI — so both fail CLOSED on any platform that cannot answer.

import (
	"os"
	"syscall"
)

// Whether this open file is verifiably the ONLY name for its inode.
//
// Refuses when the link count cannot be established, not just when it is wrong.
// `ok && Nlink != 1` was fail-open: on any platform where the type assertion
// misses, the hard-link guard silently switched itself off.
//
// Takes the open FILE as well as its FileInfo because that is what Windows
// needs; on Unix the stat result already carries the count and the handle is
// unused.
func singleLink(_ *os.File, st os.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && sys.Nlink == 1
}

// The same count by path, for the selftest probe that has to SHOW the shape it
// claims to be testing. 0 means "could not tell", which the probe prints as-is.
func linkCount(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(sys.Nlink)
}

// Stat follows symlinks on purpose — ~/.local/bin/amp is a symlink into
// ~/.amp/bin on a normal install, and the thing whose permissions matter is
// the file that actually runs.
//
// World-writable is refused; group-writable is not. The line is where it is
// because a file anyone on the box may rewrite is never legitimate, while
// group-writable is the ordinary state of a Homebrew prefix on a shared admin
// account, and refusing those would break real installs to defend against an
// attacker who, by construction, can already rewrite the user's own copy of
// the same binary and take their Amp session with it.
func usableBinary(path string) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	perm := st.Mode().Perm()
	return perm&0o111 != 0 && perm&0o002 == 0
}
