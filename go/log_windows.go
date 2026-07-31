//go:build windows

package main

// The machine's own record, on the one platform that does not already keep one.
//
// console.go promises that every command sent from the dashboard is written to
// this machine's log BEFORE it runs, so that a box keeps an account of what was
// done to it that does not pass through us. On Linux that is the systemd
// journal and on macOS the launchd log — both free, both outside this program.
// Task Scheduler has no such thing: a task's stdout and stderr are discarded.
// Without this file the promise would be true on two platforms out of three and
// nowhere would say which.
//
// A FILE, not the Windows event log. The event log would mean an installer that
// registers an event source, which needs administrator rights the rest of this
// install deliberately does not ask for. The file sits beside the token and the
// machine id, is removed by --uninstall with them, and can be read with
// anything.
//
// Rotation is two files and no timer: at the cap the current log becomes
// `log.1` and a new one starts. A helper with the console off writes almost
// nothing — a startup line, and an error when the relay is unreachable — so the
// cap is there for the pathological case rather than the ordinary one.

import (
	"io"
	"os"
	"path/filepath"
)

const (
	machineLogName = "log"
	// Past this the current log is rolled aside. Two megabytes of a file that
	// normally holds a few hundred bytes.
	machineLogMax = 2 << 20
)

// Both destinations, and errors from neither are fatal.
//
// io.MultiWriter is the wrong tool here: it stops at the first error, and under
// Task Scheduler os.Stderr is a handle that fails on write — which would mean
// the console's audit line reached NOTHING because the useless stream was tried
// first. This writes the file first and treats both as best effort.
type teeWriter struct{ file, term io.Writer }

func (t teeWriter) Write(p []byte) (int, error) {
	if t.file != nil {
		t.file.Write(p)
	}
	if t.term != nil {
		t.term.Write(p)
	}
	// Always the full count: a short write makes callers retry into a stream
	// that already failed, and there is nothing useful for warnf to do about it
	// either way.
	return len(p), nil
}

// Opened at startup, before anything can call warnf. A failure here leaves
// stderr exactly as it was rather than taking the process down: a machine with
// an unwritable home directory should still report its quota.
func init() {
	dir := configDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, machineLogName)
	if st, err := os.Stat(path); err == nil && st.Size() >= machineLogMax {
		// Replaces the previous roll. Keeping more would need a policy for how
		// much of somebody's disk this may take, and two files is enough to
		// survive the rotation that happens while you are reading.
		os.Remove(path + ".1")
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	stderr = teeWriter{file: f, term: os.Stderr}
}
