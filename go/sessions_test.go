package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A gzip+tar archive holding one regular file named `name` with `body`, plus
// any extra members the caller wants to slip in (to prove they are ignored).
func makeArchive(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range members {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Serve one archive at the version path the installer builds, 404 elsewhere.
func serveArchive(t *testing.T, version, asset string, archive []byte) *httptest.Server {
	t.Helper()
	want := "/v" + version + "/" + asset
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != want {
			w.WriteHeader(404)
			return
		}
		w.Write(archive)
	}))
}

func cfgFor(t *testing.T, srv *httptest.Server, asset, want, version string, archiveRuns bool) (sessConfig, string, string) {
	t.Helper()
	binDir := t.TempDir()
	envDir := t.TempDir()
	probe := func(_ context.Context, path string) string {
		// The staged file is the extracted "binary"; a real probe runs it. Here
		// the body IS its version line, so a well-formed member reports a real
		// version and a broken one reports nothing.
		b, err := os.ReadFile(path)
		if err != nil || !archiveRuns {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return sessConfig{
		base:    srv.URL,
		binDir:  binDir,
		envDir:  envDir,
		asset:   asset,
		want:    want,
		version: version,
		probe:   probe,
	}, binDir, envDir
}

func TestSessionsInstallHappyPath(t *testing.T) {
	const version = "0.40.1"
	asset := "agentsview_" + version + "_test.tar.gz"
	body := "agentsview v" + version + " (commit deadbeef)"
	archive := makeArchive(t, map[string]string{"agentsview": body})
	srv := serveArchive(t, version, asset, archive)
	defer srv.Close()

	cfg, binDir, envDir := cfgFor(t, srv, asset, sha256hex(archive), version, true)
	res, _ := sessionsInstall(cfg)
	if res.Code != 0 || res.Error != "" {
		t.Fatalf("expected clean install, got code=%d error=%q out=%s", res.Code, res.Error, res.Out)
	}
	// Binary present, executable, and the right bytes.
	got, err := os.ReadFile(filepath.Join(binDir, "agentsview"))
	if err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
	if string(got) != body {
		t.Fatalf("installed wrong bytes: %q", got)
	}
	info, _ := os.Stat(filepath.Join(binDir, "agentsview"))
	if info.Mode()&0o100 == 0 {
		t.Fatalf("binary not executable: %v", info.Mode())
	}
	// Telemetry-off written.
	env, err := os.ReadFile(filepath.Join(envDir, "env"))
	if err != nil || !strings.Contains(string(env), "AGENTSVIEW_TELEMETRY_ENABLED=0") {
		t.Fatalf("telemetry-off not written: %v / %q", err, env)
	}
}

func TestSessionsInstallRefusesChecksumMismatch(t *testing.T) {
	const version = "0.40.1"
	asset := "agentsview_" + version + "_test.tar.gz"
	archive := makeArchive(t, map[string]string{"agentsview": "agentsview v" + version})
	srv := serveArchive(t, version, asset, archive)
	defer srv.Close()

	// A sum that is NOT the archive's — the whole point of the checksum.
	cfg, binDir, _ := cfgFor(t, srv, asset, strings.Repeat("a", 64), version, true)
	res, _ := sessionsInstall(cfg)
	if res.Code == 0 {
		t.Fatalf("expected refusal on checksum mismatch, got success: %s", res.Out)
	}
	if !strings.Contains(res.Out, "checksum mismatch") {
		t.Fatalf("expected a checksum-mismatch message, got: %s", res.Out)
	}
	if _, err := os.Stat(filepath.Join(binDir, "agentsview")); err == nil {
		t.Fatal("a binary was installed despite the checksum mismatch")
	}
}

func TestSessionsInstallRefusesBinaryThatWillNotRun(t *testing.T) {
	const version = "0.40.1"
	asset := "agentsview_" + version + "_test.tar.gz"
	archive := makeArchive(t, map[string]string{"agentsview": "whatever"})
	srv := serveArchive(t, version, asset, archive)
	defer srv.Close()

	// archiveRuns=false makes the probe report "", i.e. it does not execute.
	cfg, binDir, _ := cfgFor(t, srv, asset, sha256hex(archive), version, false)
	res, _ := sessionsInstall(cfg)
	if res.Code == 0 {
		t.Fatalf("expected refusal when the binary does not run, got success: %s", res.Out)
	}
	if _, err := os.Stat(filepath.Join(binDir, "agentsview")); err == nil {
		t.Fatal("a binary that would not run was installed anyway")
	}
}

func TestSessionsInstallSkipsUnrelatedMembers(t *testing.T) {
	const version = "0.40.1"
	asset := "agentsview_" + version + "_test.tar.gz"
	body := "agentsview v" + version
	// An unrelated file alongside the real binary: skipped, real one installs.
	archive := makeArchive(t, map[string]string{
		"README.md":  "docs",
		"agentsview": body,
	})
	srv := serveArchive(t, version, asset, archive)
	defer srv.Close()

	cfg, binDir, _ := cfgFor(t, srv, asset, sha256hex(archive), version, true)
	res, _ := sessionsInstall(cfg)
	if res.Code != 0 {
		t.Fatalf("expected clean install, got: %s", res.Out)
	}
	got, _ := os.ReadFile(filepath.Join(binDir, "agentsview"))
	if string(got) != body {
		t.Fatalf("real binary not installed: %q", got)
	}
}

func TestSessionsInstallRefusesTraversalNamedBinary(t *testing.T) {
	const version = "0.40.1"
	asset := "agentsview_" + version + "_test.tar.gz"
	// A member whose base name is "agentsview" but whose path escapes the dir.
	// Refused outright rather than normalised into a match — the "exactly this
	// file" contract, and the regression the previous filepath.Base let slip.
	archive := makeArchive(t, map[string]string{
		"../../agentsview": "agentsview v" + version,
	})
	srv := serveArchive(t, version, asset, archive)
	defer srv.Close()

	cfg, binDir, _ := cfgFor(t, srv, asset, sha256hex(archive), version, true)
	res, _ := sessionsInstall(cfg)
	if res.Code == 0 {
		t.Fatalf("expected refusal of a path-escaping member, got success: %s", res.Out)
	}
	if _, err := os.Stat(filepath.Join(binDir, "agentsview")); err == nil {
		t.Fatal("installed from an archive whose only agentsview carried a traversal path")
	}
}

func TestSessionsInstallRefusesWrongReportedVersion(t *testing.T) {
	const version = "0.40.1"
	asset := "agentsview_" + version + "_test.tar.gz"
	// The bytes verify against their sum, but the binary reports a DIFFERENT
	// version — a mislabelled asset/checksum row. Refused rather than installed
	// and announced as the pinned version.
	archive := makeArchive(t, map[string]string{"agentsview": "agentsview v9.9.9"})
	srv := serveArchive(t, version, asset, archive)
	defer srv.Close()

	cfg, binDir, _ := cfgFor(t, srv, asset, sha256hex(archive), version, true)
	res, _ := sessionsInstall(cfg)
	if res.Code == 0 {
		t.Fatalf("expected refusal on a version mismatch, got success: %s", res.Out)
	}
	if _, err := os.Stat(filepath.Join(binDir, "agentsview")); err == nil {
		t.Fatal("installed a binary reporting the wrong version")
	}
}

func TestSessionsInstallNoOpWhenAlreadyPinned(t *testing.T) {
	const version = "0.40.1"
	asset := "agentsview_" + version + "_test.tar.gz"
	// No server hit expected — a handler that fails the test if reached.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("installer fetched %s despite an up-to-date binary", r.URL.Path)
		w.WriteHeader(500)
	}))
	defer srv.Close()

	binDir := t.TempDir()
	// Pre-place a binary whose probe reports the pinned version.
	if err := os.WriteFile(filepath.Join(binDir, "agentsview"), []byte("agentsview v"+version), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := sessConfig{
		base:    srv.URL,
		binDir:  binDir,
		envDir:  t.TempDir(),
		asset:   asset,
		want:    "unused",
		version: version,
		probe: func(_ context.Context, path string) string {
			b, _ := os.ReadFile(path)
			return strings.TrimSpace(string(b))
		},
	}
	res, _ := sessionsInstall(cfg)
	if res.Code != 0 || res.Error != "sessions-none" {
		t.Fatalf("expected a no-op (sessions-none), got code=%d error=%q out=%s", res.Code, res.Error, res.Out)
	}
}

func TestSessTelemetryOffIsIdempotentAndPreservesOtherLines(t *testing.T) {
	dir := t.TempDir()
	// A user already put something in the env file.
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("FOO=bar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := sessWriteTelemetryOff(dir); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "env"))
	s := string(b)
	if !strings.Contains(s, "FOO=bar") {
		t.Fatalf("clobbered the user's line: %q", s)
	}
	if n := strings.Count(s, "AGENTSVIEW_TELEMETRY_ENABLED=0"); n != 1 {
		t.Fatalf("expected exactly one telemetry line after 3 writes, got %d: %q", n, s)
	}
}

func TestSessTelemetryOffReplacesAnExistingSetting(t *testing.T) {
	dir := t.TempDir()
	// A prior ON setting, plus other lines that must survive. If this were
	// appended rather than replaced, two assignments to one key would leave the
	// winner undefined and telemetry not reliably off.
	initial := "FOO=bar\nAGENTSVIEW_TELEMETRY_ENABLED=1\nexport BAZ=qux\n"
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sessWriteTelemetryOff(dir); err != nil {
		t.Fatal(err)
	}
	s := string(mustRead(t, filepath.Join(dir, "env")))
	if strings.Contains(s, "AGENTSVIEW_TELEMETRY_ENABLED=1") {
		t.Fatalf("left the ON setting in place: %q", s)
	}
	if n := strings.Count(s, "AGENTSVIEW_TELEMETRY_ENABLED"); n != 1 {
		t.Fatalf("expected exactly one assignment of the key, got %d: %q", n, s)
	}
	if !strings.Contains(s, "FOO=bar") || !strings.Contains(s, "export BAZ=qux") {
		t.Fatalf("dropped a user line: %q", s)
	}
}

func TestSessTelemetryOffRefusesUnreadableEnv(t *testing.T) {
	dir := t.TempDir()
	// A file that exists but cannot be read must NOT be treated as empty and
	// overwritten with only our line.
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte("SECRET=keepme\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	// Root ignores the mode bits; skip there rather than assert a false pass.
	if b, err := os.ReadFile(path); err == nil {
		t.Skipf("cannot make a file unreadable in this environment (read returned %q)", b)
	}
	if err := sessWriteTelemetryOff(dir); err == nil {
		t.Fatal("expected a refusal on an unreadable env file, got nil")
	}
	// Restore perms and confirm the original content is untouched.
	os.Chmod(path, 0o600)
	if got := string(mustRead(t, path)); !strings.Contains(got, "SECRET=keepme") {
		t.Fatalf("clobbered an unreadable env file: %q", got)
	}
}

func TestSessVersionFromLine(t *testing.T) {
	// sessProbe hands this a single line (it truncates at the first newline).
	cases := map[string]string{
		"agentsview v0.40.1 (commit 9ef48912, built …)": "0.40.1",
		"agentsview version v0.40.1":                    "0.40.1", // not "ersion"
		"agentsview v1.2.3":                             "1.2.3",
		"no version here":                               "",
		"v":                                             "",
		"vabc":                                          "", // 'v' not followed by a digit
	}
	for line, want := range cases {
		if got := sessVersionFromLine(line); got != want {
			t.Errorf("sessVersionFromLine(%q) = %q, want %q", line, got, want)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSessAssetNameMatchesVendorScheme(t *testing.T) {
	// Guards the download URL: the vendor publishes
	// agentsview_<ver>_<goos>_<goarch>.tar.gz, and a drift here is a 404.
	a := sessAsset()
	if !strings.HasPrefix(a, "agentsview_"+sessVersion+"_") || !strings.HasSuffix(a, ".tar.gz") {
		t.Fatalf("asset name does not match the vendor scheme: %s", a)
	}
}
