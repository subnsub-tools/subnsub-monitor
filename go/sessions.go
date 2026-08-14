package main

// Installing AgentsView — the per-machine session index the dashboard's fleet
// search asks — on THIS machine, when somebody who already has a shell here
// runs `subnsub-monitor sessions install`.
//
// The whole feature exists because of one ruling: a machine that granted the
// console has already granted arbitrary /bin/sh as this user, so the fleet
// search rides that one channel and nothing more sensitive is exposed by it.
// The dashboard's search command runs `agentsview session search` on the far
// end; this subcommand is how that binary gets there without an operator
// having to hand-place a 100 MB download.
//
// Two things are load-bearing and both are the same discipline update.go
// already follows: WE decide what version, and WE check the bytes. The tool
// is pinned to a reviewed release here in this file, fetched from the vendor's
// own release host at that exact tag, and verified against the checksum THIS
// build carries — not a SHA256SUMS the download points us at, because a
// manifest fetched beside a payload proves only that the payload matches
// itself. Pinning here is what lets a helper upgrade decide when AgentsView
// moves, rather than every machine silently tracking upstream's latest.
//
// Telemetry is turned off in the same stroke: the vendor sends a minimal
// anonymous ping by default, and a tool WE install on somebody's machine
// should not start doing that on our say-so. AGENTSVIEW_TELEMETRY_ENABLED=0
// is written into the environment file the install lands beside.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// The reviewed release. Bumped by hand, in a helper change that also
	// updates the four sums below — the two are one edit, and a version that
	// moved without its sums would fail closed at the checksum rather than
	// install something unverified.
	sessVersion = "0.40.1"

	// The vendor's release host. A constant for the reason updateBase is one:
	// no byte of what gets installed may come from an address the relay or the
	// command could move. There is no environment override — unlike our own
	// update, which tests against a local bucket, this points at a third
	// party's releases and the only honest values are the real ones.
	sessBase = "https://github.com/kenn-io/agentsview/releases/download"

	// The tool, once unpacked. The archive holds exactly this one file.
	sessBinName = "agentsview"

	// One budget over the whole install, enforced this side — the download is
	// ~40 MB, and on the console's 30-second command window an operator will
	// usually kick this off and watch the card rather than hold the terminal,
	// but the deadline still has to be ours.
	sessBudget      = 6 * time.Minute
	sessTimeout     = 5 * time.Minute
	sessProbeBudget = 15 * time.Second
	// The archive is tens of MB; the unpacked binary ~100 MB. The ceiling sits
	// above the latter so a replaced endpoint cannot stream forever, and one
	// byte over lets an at-limit read be told from a truncated one.
	sessMaxArchive = 64 << 20
	sessMaxBinary  = 256 << 20
)

// The checksum of each published archive, for the platforms whose shell can
// run the search command (see fedPlatOk on the dashboard side: linux, darwin,
// freebsd — Windows quoting cannot carry the query safely, and the vendor
// ships no freebsd build regardless). Keyed by GOOS_GOARCH.
//
// These are the vendor's own SHA256SUMS values for 0.40.1, copied in at
// review time. This is the trust root: the download is checked against THIS,
// never against a manifest fetched next to it.
var sessSums = map[string]string{
	"linux_amd64":  "d9a8dc63e6a3a09da8b0b033ca2088225faa542bd241330fdbcf2cb3826874cf",
	"linux_arm64":  "31ea689e88422d8b7b096b8a24749d6a9f4cb3374755707f3b37e06e08db78b8",
	"darwin_amd64": "f384084a95ff732c6bbde51fd6a0672a933c85a67247a7808bbcee78929781d9",
	"darwin_arm64": "cd94f117bc55ce6300b3956b3480ad54bba6926cf9ba2eabfe1674fa08935ba1",
}

// The download client — and the one place this feature's trust model differs
// from the self-updater's, deliberately.
//
// update.go REFUSES redirects, because its trust root is the constant base
// URL: it fetches the checksum manifest from that same host, so a redirect
// able to move where the payload comes from could move the manifest too, and
// the check would pass on an attacker's bytes. The base URL staying put is
// what secures it.
//
// Here the trust root is different: the expected checksum is PINNED in this
// binary (sessSums), not fetched. A GitHub release URL always 302s to a CDN
// host, and following that is safe precisely because bytes that do not match
// the pinned sum are refused no matter where a redirect led — the host is not
// what is trusted, the 64 hex characters in this file are. Redirects are
// bounded so a redirect loop cannot stand in for the deadline.
func sessClient() *http.Client {
	c := &http.Client{
		Timeout: sessTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			// Only ever downgrade-safe hops: never follow https → http, which
			// would put the download on a cleartext connection a network could
			// rewrite (the pinned sum would still catch it, but there is no
			// reason to fetch bytes we would then throw away).
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing a non-https redirect to %s", req.URL.Scheme)
			}
			return nil
		},
	}
	return c
}

// Where the binary lands, and the env file beside it that turns telemetry off.
// ~/.local/bin because that is the PATH entry the search command adds ahead of
// its lookup (see fedCmd on the dashboard side), and a user-writable location
// the helper can reach without the installer's privileges.
func sessBinDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("no home directory for this account")
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// The archive name the vendor publishes for this platform, e.g.
// agentsview_0.40.1_linux_arm64.tar.gz.
func sessAsset() string {
	return fmt.Sprintf("agentsview_%s_%s_%s.tar.gz", sessVersion, runtime.GOOS, runtime.GOARCH)
}

// Is this a platform we install on at all? Kept in step with the dashboard's
// fedPlatOk, minus freebsd, which the search supports but the vendor does not
// build — a freebsd machine can run the search only against an agentsview put
// there by hand.
func sessPlatformSupported() bool {
	if sessSums[runtime.GOOS+"_"+runtime.GOARCH] == "" {
		return false
	}
	return true
}

// sessConfig is the world this install acts on, passed in rather than looked
// up so every branch below is reachable from a test — including the ones that
// must refuse to install, which are the branches worth being sure about. The
// same shape update.go uses (updateFrom takes its facts as arguments).
type sessConfig struct {
	base    string // release host + "/download", no trailing slash
	binDir  string // where the binary lands (~/.local/bin in production)
	envDir  string // where the telemetry-off env file lands (~/.agentsview)
	asset   string // archive filename to fetch
	want    string // expected sha256 of the archive
	version string // the pinned version, for the download path and no-op check
	// probe returns the --version line of a binary, or "" — injected so a test
	// need not carry a real 100 MB executable.
	probe func(path string) string
}

// Report shape mirrors consoleResult so this can ride back over the same
// channel the console and update paths use. Kept as its own call so the CLI
// (`sessions install`) and any future dashboard trigger share one body. The
// production entry resolves the world; sessionsInstall does the work.
func runSessionsInstall() (consoleResult, bool) {
	if !sessPlatformSupported() {
		res := consoleResult{Code: -1, Error: "sessions-failed"}
		res.Out = fmt.Sprintf("no published agentsview build for %s/%s — nothing to install\n", runtime.GOOS, runtime.GOARCH)
		warnf("sessions: %s", strings.TrimSpace(res.Out))
		return res, false
	}
	binDir, err := sessBinDir()
	if err != nil {
		res := consoleResult{Code: -1, Error: "sessions-failed", Out: err.Error() + "\n"}
		return res, false
	}
	home, _ := os.UserHomeDir()
	return sessionsInstall(sessConfig{
		base:    sessBase,
		binDir:  binDir,
		envDir:  filepath.Join(home, ".agentsview"),
		asset:   sessAsset(),
		want:    sessSums[runtime.GOOS+"_"+runtime.GOARCH],
		version: sessVersion,
		probe:   sessProbe,
	})
}

func sessionsInstall(cfg sessConfig) (consoleResult, bool) {
	res := consoleResult{Code: -1}
	started := time.Now()
	var log []string
	say := func(format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		log = append(log, s)
		warnf("sessions: %s", s)
	}
	done := func(code int, errStr string) (consoleResult, bool) {
		res.Code = code
		res.Error = errStr
		res.Ms = time.Since(started).Milliseconds()
		res.Out = strings.Join(log, "\n") + "\n"
		return res, false
	}
	fail := func() (consoleResult, bool) { return done(-1, "sessions-failed") }

	want := cfg.want
	dir := cfg.binDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		say("cannot create %s: %v", dir, err)
		return fail()
	}
	dst := filepath.Join(dir, sessBinName)

	// Already the pinned version? Then this is a no-op, said as one — the same
	// distinction update.go draws, because a reinstall that changed nothing
	// should not read as a fresh install. Any error probing just means "go
	// ahead and install", never a hard failure: a corrupt or older binary is
	// exactly what a reinstall is for.
	if cur := sessVersionAt(dst, cfg.probe); cur == cfg.version {
		say("agentsview %s is already installed at %s", cur, dst)
		return done(0, "sessions-none")
	}

	ctx, cancel := context.WithTimeout(context.Background(), sessBudget)
	defer cancel()
	client := sessClient()

	url := cfg.base + "/v" + cfg.version + "/" + cfg.asset
	say("downloading agentsview %s", cfg.version)

	// Stream to a temp file in the SAME directory as the destination, so the
	// final move is a rename and not a cross-filesystem copy — the discipline
	// update.go spells out. The gzip archive is verified whole against the
	// pinned sum BEFORE anything is unpacked: a checksum computed after
	// extraction would already have run the vendor's bytes through a
	// decompressor on trust.
	arch, archName, err := sessDownloadVerified(ctx, client, url, want, dir, say)
	if err != nil {
		return fail()
	}
	defer os.Remove(archName)
	defer arch.Close()

	// Unpack the single binary to its own temp file beside the destination.
	tmpName, err := sessExtractBinary(arch, dir, say)
	if err != nil {
		return fail()
	}
	defer os.Remove(tmpName)

	if err := os.Chmod(tmpName, 0o755); err != nil {
		say("cannot mark the binary executable: %v", err)
		return fail()
	}

	// It has to prove it runs before it is installed — the same gate the
	// updater puts on our own new binary. A download that unpacks but does not
	// execute (wrong libc, truncated-yet-somehow-matching, an unexpected
	// format) is caught here instead of at the first search.
	if v := cfg.probe(tmpName); v == "" {
		say("the downloaded binary did not run on this machine")
		return fail()
	} else {
		say("verified it runs: reports %s", v)
	}

	if err := os.Rename(tmpName, dst); err != nil {
		say("cannot move the binary into place at %s: %v", dst, err)
		return fail()
	}

	// Telemetry off, written where agentsview reads its environment. Best
	// effort and said either way: a failure here does not undo a good install,
	// it just means the operator should export the variable themselves.
	if err := sessWriteTelemetryOff(cfg.envDir); err != nil {
		say("installed, but could not write the telemetry-off setting: %v", err)
	}

	say("installed agentsview %s at %s", cfg.version, dst)
	say("telemetry is off; the dashboard's fleet search can now reach this machine")
	return done(0, "")
}

// Download to a staged file and verify the whole archive against the pinned
// sum before returning it, rewound to the start for the caller to read.
func sessDownloadVerified(ctx context.Context, client *http.Client, url, want, dir string, say func(string, ...any)) (*os.File, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		say("cannot build the download request")
		return nil, "", err
	}
	req.Header.Set("User-Agent", "subnsub-monitor")
	resp, err := client.Do(req)
	if err != nil {
		say("download failed: %v", err)
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		say("the release host answered %d for %s", resp.StatusCode, sessAsset())
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(dir, ".agentsview-dl-*.tar.gz")
	if err != nil {
		say("cannot stage the download in %s: %v", dir, err)
		return nil, "", err
	}
	tmpName := tmp.Name()

	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, sessMaxArchive+1))
	if cerr != nil {
		tmp.Close()
		os.Remove(tmpName)
		say("download interrupted: %v", cerr)
		return nil, "", cerr
	}
	if n > sessMaxArchive {
		tmp.Close()
		os.Remove(tmpName)
		say("the download was larger than any published build; refusing it")
		return nil, "", fmt.Errorf("oversize")
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		tmp.Close()
		os.Remove(tmpName)
		// The one failure worth naming precisely: the bytes are not the bytes
		// this build vouches for, and installing them anyway is the whole thing
		// this checksum exists to prevent.
		say("checksum mismatch — got %s, expected %s; refusing to install", got, want)
		return nil, "", fmt.Errorf("checksum mismatch")
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		say("cannot rewind the staged download: %v", err)
		return nil, "", err
	}
	return tmp, tmpName, nil
}

// Pull the single agentsview binary out of the verified gzip archive into a
// temp file beside the destination.
func sessExtractBinary(arch *os.File, dir string, say func(string, ...any)) (string, error) {
	gz, err := gzip.NewReader(arch)
	if err != nil {
		say("the download is not a gzip archive: %v", err)
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			say("the archive could not be read: %v", err)
			return "", err
		}
		// The base name only, and it must be the one file we expect — an
		// archive that carried a path (../, /etc/…) or an unexpected member is
		// refused rather than followed. There is exactly one file to want.
		if filepath.Base(hdr.Name) != sessBinName || hdr.Typeflag != tar.TypeReg {
			continue
		}
		tmpBin, err := os.CreateTemp(dir, ".agentsview-*")
		if err != nil {
			say("cannot stage the binary in %s: %v", dir, err)
			return "", err
		}
		tmpName := tmpBin.Name()
		n, err := io.Copy(tmpBin, io.LimitReader(tr, sessMaxBinary+1))
		tmpBin.Close()
		if err != nil {
			os.Remove(tmpName)
			say("could not write the binary out: %v", err)
			return "", err
		}
		if n > sessMaxBinary {
			os.Remove(tmpName)
			say("the unpacked binary was larger than expected; refusing it")
			return "", fmt.Errorf("oversize binary")
		}
		return tmpName, nil
	}
	say("the archive did not contain an %s binary", sessBinName)
	return "", fmt.Errorf("binary not found in archive")
}

// What `agentsview --version` reports, or "" if it will not run. Telemetry off
// for the probe too, so a version check never phones home.
func sessProbe(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), sessProbeBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = append(os.Environ(), "AGENTSVIEW_TELEMETRY_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return line
}

// The version already installed at path, parsed from its --version line, or ""
// if there is none or it will not answer. Used only to skip a reinstall, so
// any doubt resolves toward installing. The prober is injected so a test can
// stand in for a real binary.
func sessVersionAt(path string, probe func(string) string) string {
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	line := probe(path)
	// The line looks like "agentsview v0.40.1 (commit …)". Pull the token
	// after a leading 'v' that matches our version shape rather than trusting
	// a fixed offset.
	for _, tok := range strings.Fields(line) {
		if strings.HasPrefix(tok, "v") && len(tok) > 1 {
			return strings.TrimPrefix(tok, "v")
		}
	}
	return ""
}

// AGENTSVIEW_TELEMETRY_ENABLED=0 in the env file agentsview reads. Written
// atomically (temp + rename) and only if it is not already set there, so a
// re-run neither duplicates the line nor clobbers other settings a user put in
// the file.
func sessWriteTelemetryOff(dir string) error {
	if dir == "" {
		return fmt.Errorf("no config directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "env")
	const line = "AGENTSVIEW_TELEMETRY_ENABLED=0"

	existing, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(l) == line {
			return nil // already set; leave the file untouched
		}
	}
	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"

	tmp, err := os.CreateTemp(dir, ".env-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
