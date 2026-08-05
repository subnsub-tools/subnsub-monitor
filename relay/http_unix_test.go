//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

// The dist-directory shapes only a Unix filesystem can grow, and the shapes
// the /dl route must refuse UNOPENED: a symlink wearing a listed name lends
// the route's unauthenticated reach to whatever it points at, and a FIFO
// blocks its opener until a writer appears — a request that hangs forever on
// a misconfigured directory is a denial of service an operator handed
// themselves. The build tag lists the platforms that actually have Mkfifo
// rather than saying !windows, which would sweep in plan9 and the wasm
// targets and fail to compile there.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDistRefusesSymlinksAndSpecialFiles(t *testing.T) {
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "state.json")
	if err := os.WriteFile(secretPath, []byte(`{"names":{"a":"leaked-secret"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// A listed name pointing OUTSIDE the directory — the exact shape that
	// turns an unauthenticated download route into a state-file reader.
	if err := os.Symlink(secretPath, filepath.Join(dir, "install.sh")); err != nil {
		t.Fatal(err)
	}
	// A listed name pointing INSIDE: still refused — the route serves plain
	// files, and a link is an indirection nobody stamps checksums for.
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("real sums"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "SHA256SUMS"), filepath.Join(dir, "VERSION")); err != nil {
		t.Fatal(err)
	}
	// A FIFO wearing a listed name. A handler that opened it blocking would
	// hang this request, the server's shutdown, and the whole test binary —
	// so the request runs on a short-deadline client, and the cleanup below
	// unwedges a blocked reader by connecting the write end, so a regression
	// fails in seconds instead of at the go test timeout.
	fifo := filepath.Join(dir, "subnsub-monitor-linux-amd64")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	srv := distServer(t, dir)

	for _, tc := range []struct{ name, path string }{
		{"symlink out of the directory", "/dl/install.sh"},
		{"symlink inside the directory", "/dl/VERSION"},
	} {
		r := do(t, srv, "GET", tc.path, "", "")
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("%s (%s): %d, want 404", tc.name, tc.path, r.StatusCode)
		}
		if body, _ := io.ReadAll(r.Body); strings.Contains(string(body), "leaked-secret") ||
			strings.Contains(string(body), "real sums") {
			t.Errorf("%s (%s) leaked a body", tc.name, tc.path)
		}
	}

	fast := &http.Client{Timeout: 3 * time.Second}
	resp, err := fast.Get(srv.URL + "/dl/subnsub-monitor-linux-amd64")
	if err != nil {
		t.Fatalf("the fifo request did not return — the handler opened it blocking: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("fifo: %d, want 404", resp.StatusCode)
	}

	// The plain file beside them still serves — refusal is per name, not
	// per directory.
	if r := do(t, srv, "GET", "/dl/SHA256SUMS", "", ""); r.StatusCode != 200 {
		t.Errorf("plain file beside the refused ones: %d", r.StatusCode)
	}
}
