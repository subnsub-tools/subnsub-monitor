//go:build !windows

package main

// The two dist-directory shapes only a Unix filesystem can grow, and the two
// the /dl route must refuse UNOPENED: a symlink wearing a listed name lends
// the route's unauthenticated reach to whatever it points at, and a FIFO
// blocks its opener until a writer appears — a request that hangs forever on
// a misconfigured directory is a denial of service an operator handed
// themselves.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
	// A FIFO wearing a listed name. If the handler opens it before asking
	// what it is, this request never returns and the test times out — which
	// is the failure mode, not an inconvenience of the test.
	if err := syscall.Mkfifo(filepath.Join(dir, "subnsub-monitor-linux-amd64"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := distServer(t, dir)

	for _, tc := range []struct{ name, path string }{
		{"symlink out of the directory", "/dl/install.sh"},
		{"symlink inside the directory", "/dl/VERSION"},
		{"fifo", "/dl/subnsub-monitor-linux-amd64"},
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
	// The plain file beside them still serves — refusal is per name, not
	// per directory.
	if r := do(t, srv, "GET", "/dl/SHA256SUMS", "", ""); r.StatusCode != 200 {
		t.Errorf("plain file beside the refused ones: %d", r.StatusCode)
	}
}
