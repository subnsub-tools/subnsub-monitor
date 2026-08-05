//go:build windows

package main

// The Windows answers to the two questions in fs_unix.go, and one of them is
// weaker than its Unix counterpart in a way worth stating rather than hiding.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
func openLinkCount(f *os.File, _ os.FileInfo) (uint64, bool) {
	if f == nil {
		return 0, false
	}
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return 0, false
	}
	return uint64(d.NumberOfLinks), true
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

// Which extensions are SCRIPTS for the command interpreter rather than programs
// the kernel can load. Windows has no shebang: a .cmd is a text file, and
// CreateProcess — which is the only thing Go's os/exec calls, with no
// batch-file special case anywhere in the path — refuses it outright.
//
// This is not a hypothetical. npm installs a global CLI on Windows as
// `<name>.cmd`, so the Amp collector's own candidate list ends in one on a
// normal machine. Found-then-cannot-start is the worst of the three possible
// outcomes: it looks like support and behaves like an outage.
var scriptExts = map[string]bool{".cmd": true, ".bat": true}

// How to run a tool this helper found on disk. Through the command interpreter
// when it is a script, directly otherwise.
//
// The command line is built by hand for the same reason console_windows.go
// builds its own: cmd.exe parses one string with its own rules, os/exec would
// assemble that string with the C runtime's, and the two disagree about
// quoting. `/s` makes cmd's rule a single sentence — strip the first and last
// character if both are quotes, run the rest verbatim — which is what makes the
// nested quoting below predictable rather than a guess. `/d` skips the user's
// AutoRun registry command, which would otherwise run before every invocation.
func toolCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	if !scriptExts[strings.ToLower(filepath.Ext(bin))] {
		return exec.CommandContext(ctx, bin, args...)
	}
	shell := winShell()
	// Inner: the script's own quoted path and its arguments. Outer: the wrapper
	// /s strips. Arguments here are this program's own literals, never anything
	// that came off the network.
	inner := `"` + bin + `"`
	for _, a := range args {
		inner += " " + a
	}
	cmd := exec.CommandContext(ctx, shell)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       `"` + shell + `" /d /s /c "` + inner + `"`,
		CreationFlags: createNoWindow,
	}
	return cmd
}
