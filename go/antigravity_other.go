//go:build !linux

package main

// The same discovery on platforms without /proc — macOS in practice.
//
// Here there is no way to do it by reading files, so two system tools answer
// the two questions `/proc` answers on Linux: `ps` for which processes exist,
// `lsof` for which ports one of them is listening on. Both go through the same
// discipline the Amp collector established for running another program, and for
// the same reason — this helper's whole claim is that it reads rather than
// executes, so every exception to that is resolved to an absolute path from a
// fixed list, refused if anyone but the owner can write it, given a deadline,
// and read into a bounded buffer.
//
// A machine with neither tool reports nothing found, which is indistinguishable
// from Antigravity not running. That is an acceptable blind spot: on macOS both
// ship with the system, and the Linux build never gets here.

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var agPsPaths = []string{"/bin/ps", "/usr/bin/ps"}
var agLsofPaths = []string{"/usr/sbin/lsof", "/usr/bin/lsof", "/bin/lsof"}

func agTool(paths []string) string {
	for _, p := range paths {
		if usableBinary(p) {
			return p
		}
	}
	return ""
}

func agCandidates() []agCandidate {
	ps := agTool(agPsPaths)
	lsof := agTool(agLsofPaths)
	if ps == "" || lsof == "" {
		return nil
	}
	out, err := agRun(ps, "-axo", "pid=,command=")
	if err != nil {
		return nil
	}
	var cands []agCandidate
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidStr, cmd, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 {
			continue
		}
		// `ps` gives one string, so the argv split is on spaces. Good enough
		// for the two things read out of it: the leading path and whether an
		// Antigravity marker appears.
		argv := strings.Fields(cmd)
		if !agIsServer(argv) {
			continue
		}
		ports := agLsofPorts(lsof, pid)
		if len(ports) == 0 {
			continue
		}
		cands = append(cands, agCandidate{pid: pid, csrf: agCsrf(argv), ports: ports})
		if len(cands) >= 4 {
			break
		}
	}
	return cands
}

// lsof -nP -iTCP -sTCP:LISTEN -a -p <pid>, whose rows end in `*:PORT (LISTEN)`
// or `127.0.0.1:PORT (LISTEN)`.
func agLsofPorts(lsof string, pid int) []int {
	out, err := agRun(lsof, "-nP", "-iTCP", "-sTCP:LISTEN", "-a", "-p", strconv.Itoa(pid))
	if err != nil {
		return nil
	}
	var ports []int
	seen := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "LISTEN") {
			continue
		}
		for _, f := range strings.Fields(line) {
			_, portStr, ok := strings.Cut(f, ":")
			if !ok {
				continue
			}
			n, err := strconv.Atoi(portStr)
			if err != nil || n <= 0 || n > 65535 || seen[n] {
				continue
			}
			seen[n] = true
			ports = append(ports, n)
		}
	}
	return ports
}

func agRun(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &buf, n: 512 * 1024}
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		// A non-zero exit with usable output still counts: `lsof` returns 1
		// when it found nothing, which is not an error to report.
		if buf.Len() == 0 {
			return "", err
		}
	}
	return buf.String(), nil
}
