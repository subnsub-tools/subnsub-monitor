//go:build linux

package main

// Finding Antigravity's language server on Linux, WITHOUT running anything.
//
// CodexBar shells out to `lsof -nP -iTCP -sTCP:LISTEN -a -p <pid>` for the
// port. On Linux that is unnecessary: /proc holds both halves of the answer,
// and reading files keeps this collector as cheap as the Codex one —
// no subprocess, no PATH lookup, nothing to be careful about executing. It also
// works on the many minimal server images that do not ship lsof at all, where
// the shell-out approach would simply report nothing and look like "not
// installed".
//
// The two halves:
//
//	/proc/<pid>/cmdline   which process this is, and its --csrf_token
//	/proc/<pid>/fd/*      the socket inodes it holds
//	/proc/net/tcp[6]      inode → listening port
//
// Only this process's OWN processes are visible without privileges, which is
// exactly right: the language server belongs to the user running the IDE, and
// the helper runs as that user.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func agCandidates() []agCandidate {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []agCandidate
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(raw) == 0 {
			continue // gone, or not ours to read
		}
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if !agIsServer(argv) {
			continue
		}
		// It has to be OUR process. On an ordinary install /proc/<pid>/fd is
		// unreadable for anyone else's and this is belt-and-braces — but "the
		// permissions will stop it" is not a check, and shared accounts and
		// root-run servers are exactly where it would not.
		if !agSameUser(e.Name()) {
			continue
		}
		eps := agListening(pid)
		if len(eps) == 0 {
			continue // running but not (yet) listening
		}
		out = append(out, agCandidate{pid: pid, csrf: agCsrf(argv), eps: eps})
		if len(out) >= 4 {
			break // more than this is not a fleet of IDEs, it is a loop
		}
	}
	return out
}

// Does this /proc entry belong to the user this helper runs as?
func agSameUser(pid string) bool {
	st, err := os.Stat("/proc/" + pid)
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Geteuid()
}

// Every TCP endpoint this pid is listening on, address family included.
//
// Keyed on host AND port rather than port alone: the same number on ::1 and on
// 127.0.0.1 are two different places to knock, and collapsing them threw away
// the one that answered on an IPv6-only install.
func agListening(pid int) []agEndpoint {
	inodes := agSocketInodes(pid)
	if len(inodes) == 0 {
		return nil
	}
	var eps []agEndpoint
	seen := map[string]bool{}
	for _, f := range []struct {
		path string
		host string
	}{{"/proc/net/tcp", "127.0.0.1"}, {"/proc/net/tcp6", "::1"}} {
		for _, p := range agListenPortsFor(f.path, inodes) {
			key := f.host + "/" + strconv.Itoa(p)
			if !seen[key] {
				seen[key] = true
				eps = append(eps, agEndpoint{host: f.host, port: p})
			}
		}
	}
	return eps
}

func agSocketInodes(pid int) map[string]bool {
	dir := "/proc/" + strconv.Itoa(pid) + "/fd"
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, e := range ents {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if v, ok := strings.CutPrefix(target, "socket:["); ok {
			out[strings.TrimSuffix(v, "]")] = true
		}
	}
	return out
}

// /proc/net/tcp, one socket per line:
//
//	sl  local_address rem_address st ... inode
//	0: 0100007F:1F90 00000000:0000 0A  ... 1234567
//
// st == 0A is TCP_LISTEN. The local address is hex, and the port is the half
// after the colon.
func agListenPortsFor(path string, inodes map[string]bool) []int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int
	for _, line := range strings.Split(string(raw), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 10 || f[3] != "0A" {
			continue
		}
		if !inodes[f[9]] {
			continue
		}
		_, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(hexPort, 16, 32)
		if err != nil || n == 0 || n > 65535 {
			continue
		}
		out = append(out, int(n))
	}
	return out
}
