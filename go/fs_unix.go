//go:build unix

package main

// What this helper needs to know about another program's file, which the
// standard library does not answer portably: how many names it has, whether
// this process should be willing to run it, and how to run it.
//
// The first two are security decisions — one guards the Codex session reader
// against a hard link planted in the sessions tree, the other guards running
// the Amp CLI — so both fail CLOSED on any platform that cannot answer. The
// third is not a judgement at all; it is the plain fact that Unix runs a file
// and Windows sometimes needs an interpreter to.

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// How many names this open file has, and whether that could be established at
// all. The second return is not decoration: a count nobody could obtain and a
// count of two both stop the read, but only one of them is evidence that
// somebody hard-linked the tree, and saying so when it is not known would send
// a reader off to hunt for links that do not exist.
//
// Takes the open FILE as well as its FileInfo because that is what Windows
// needs; on Unix the stat result already carries the count and the handle is
// unused.
func openLinkCount(_ *os.File, st os.FileInfo) (uint64, bool) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(sys.Nlink), true
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

// How to run a tool this helper found on disk. Directly: the kernel reads the
// shebang for a script and the ELF header for anything else, and neither needs
// this program's help.
func toolCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}
