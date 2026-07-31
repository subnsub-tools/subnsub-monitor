//go:build windows

package main

// The Windows answers to the two questions in fs_unix.go, and one of them is
// weaker than its Unix counterpart in a way worth stating rather than hiding.

import (
	"os"
	"strings"
	"syscall"
)

// NTFS keeps a link count and Windows will hand it over, but only for an OPEN
// HANDLE — there is no stat that carries it. Which is why singleLink takes the
// file rather than the FileInfo alone: on Unix the count arrives with the stat
// and the handle is spare, here it is the other way round.
//
// A filesystem that is not NTFS (FAT32 on a removable disk, most network
// redirectors) reports 1 whether or not that is true, because it cannot make
// more than one name for a file in the first place. That is the correct answer
// for the guard's purpose: the attack it exists to stop cannot be built there.
func singleLink(f *os.File, st os.FileInfo) bool {
	if f == nil {
		return false
	}
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return false
	}
	return d.NumberOfLinks == 1
}

func linkCount(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return 0
	}
	return uint64(d.NumberOfLinks)
}

// WHAT THIS CANNOT CHECK, said plainly: Windows has no execute bit and no
// world-writable bit. os.Stat reports 0666 or 0444 depending on one attribute
// — the read-only flag — so the Unix test (`some execute bit set, no write bit
// for others`) would reject every file on the system if it were reused here.
//
// Permission on Windows lives in an ACL, and reading one means
// GetSecurityInfo, an SID lookup and an access check — several hundred lines
// of Win32 to answer a question that, on the path this guards, has a much
// shorter answer: is it a regular file with a name Windows will execute?
//
// So the guard here is genuinely weaker, and the thing that carries the weight
// instead is where the candidates come from — a fixed list of absolute paths
// under the user's own profile, never PATH, never the working directory. An
// attacker who can write to those has the user's account already.
func usableBinary(path string) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	// PATHEXT is the machine's own list of what counts as runnable. Consulted
	// rather than hardcoded, because a shop that adds .ps1 to it has said so.
	exts := os.Getenv("PATHEXT")
	if exts == "" {
		exts = ".COM;.EXE;.BAT;.CMD"
	}
	lower := strings.ToLower(path)
	for _, e := range strings.Split(exts, ";") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" && e != "." && strings.HasSuffix(lower, e) {
			return true
		}
	}
	return false
}
