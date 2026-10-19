package main

// Host identity — which INSTALLATION of an operating system this process is
// on, kept beside the state so the next start can tell whether the state
// was carried to a different machine.
//
// The agent id is deliberately not derived from hardware (agent.go): it is
// what the dashboard's card is keyed by, and a rebuilt box that gets its
// state directory restored keeps its card. That is the point of the restore
// — and it is also why the outage that rebuild left could only ever read as
// `down`: the process was not running, the stamp says for how long, and
// nothing said the machine underneath had been replaced. A VM the platform
// wipes and re-provisions is not a reboot (operator ruling 2026-09-26: the two must be told apart),
// so the state carries one more file, `host-id`, holding the OS install's
// own identity. A start that finds it different from the running system is a
// start on a NEW machine with OLD state, and the stretch the stamp shows is
// filed as `wipe` rather than `down` (outages.go).
//
// The value is read, never generated: /etc/machine-id is written once by
// systemd-machine-id-setup on the install's first boot (a cloud image ships
// it empty so every instance gets its own; an image cloned WITH one makes
// two instances read alike, which only ever costs a wipe reading as a down),
// FreeBSD writes /etc/hostid at first boot, and on macOS the root volume's
// UUID is the one an erase-and-reinstall changes — the hardware's
// IOPlatformUUID survives that, so it would say "same machine" of a Mac
// wiped to the metal (review 2026-09-26). A platform with none of these
// reports "" and the comparison is simply never made — a blank never
// counts as a change in either direction.
//
// Never sent anywhere: it stays in the state directory and is compared there.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const hostIDFile = "host-id"

func hostIDPath() string {
	dir := configDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, hostIDFile)
}

// hostIdentity is the running system's install identity, or "" where the
// platform offers none this build knows how to read. Bounded and cleaned:
// the file is root-owned system state, but it is still read into a
// comparison and written back out, so only the characters an id is made of
// are kept and only up to a length no real id reaches.
func hostIdentity() string {
	switch runtime.GOOS {
	case "linux":
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if id := hostIDClean(hostReadSmall(p)); id != "" {
				return id
			}
		}
	case "freebsd":
		return hostIDClean(hostReadSmall("/etc/hostid"))
	case "darwin":
		return hostIDClean(macRootVolumeUUID())
	}
	return ""
}

func hostReadSmall(p string) string {
	b, err := os.ReadFile(p)
	if err != nil || len(b) > 4096 {
		return ""
	}
	return string(b)
}

// hostIDClean keeps the first line's id characters, at most 64 of them; a
// value with fewer than 8 (an empty or placeholder file) is no identity.
func hostIDClean(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
			b.WriteRune(r)
		default:
			return ""
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() < 8 {
		return ""
	}
	return b.String()
}

// macRootVolumeUUID is the "Volume UUID" line of `diskutil info /` — the
// APFS volume the system was installed onto, made anew by an erase. Bounded
// in time through the command's own context: a start must not wait on a
// stuck diskutil, and a command that outlives the wait is killed with it
// rather than left running (review 2026-09-26).
func macRootVolumeUUID() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", "info", "/").Output()
	if err != nil {
		return ""
	}
	return macFieldUUID(string(out), "Volume UUID:")
}

// macFieldUUID picks the value after `label` on its line of a diskutil-style
// listing ("   Volume UUID:   0A1B…"), or "".
func macFieldUUID(listing, label string) string {
	for _, line := range strings.Split(listing, "\n") {
		i := strings.Index(line, label)
		if i < 0 {
			continue
		}
		return strings.TrimSpace(line[i+len(label):])
	}
	return ""
}

// hostIDStored is the identity the state was last seen on; "" when the file
// is absent (a state directory from before this file existed, or a first
// run) or unreadable.
func hostIDStored() string {
	p := hostIDPath()
	if p == "" {
		return ""
	}
	return hostIDClean(hostReadSmall(p))
}

// hostIDWrite records the running identity, write-then-rename like the
// stamp. A failure is silent for the same reason the stamp's is: the next
// start simply cannot make the comparison.
func hostIDWrite(id string) {
	p := hostIDPath()
	if p == "" || id == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".new"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
	}
}

// hostChanged is the one question outageBoot asks: the state carries an
// identity, the system has one, and they differ. `cur` is passed in rather
// than read here so a test can stand in for the platform.
func hostChanged(stored, cur string) bool {
	return stored != "" && cur != "" && stored != cur
}
