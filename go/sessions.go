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
	// need not carry a real 100 MB executable. Deadline-bound through ctx.
	probe func(ctx context.Context, path string) string
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
	// Security assumption, stated rather than enforced: the destination is a
	// user-private directory (~/.local/bin). The stage-then-rename below writes
	// through a 0600 temp file this process created, so the one way another
	// local principal could substitute the binary between the probe and the
	// rename is to be able to write this directory in the first place — which,
	// on a machine whose console is on, is already a larger exposure than this
	// install. Hardening the dir's ownership here would break the ordinary
	// case (a group-writable ~/.local/bin is unusual but legitimate) for a
	// threat the console grant already dwarfs.
	dst := filepath.Join(dir, sessBinName)

	// One deadline over the whole operation — download, extraction AND the
	// probe — so sessBudget means what its comment says. The probe derives a
	// child deadline from it rather than starting a fresh clock.
	ctx, cancel := context.WithTimeout(context.Background(), sessBudget)
	defer cancel()

	// Sweep stale staging files a previous run's SIGKILL or power loss may have
	// left in the destination directory. Best effort: a file we cannot remove
	// is not a reason to refuse the install.
	sessSweepStaging(dir)

	// Already the pinned version? Then the binary is a no-op — but the
	// telemetry-off setting still has to be true, because a first install that
	// wrote the binary and then died before the env file would otherwise be
	// frozen in a "installed but still phoning home" state that every reinstall
	// skipped straight past. So the no-op path ENSURES the setting, and only
	// claims success when it holds. Any error probing the binary just means
	// "go ahead and reinstall", never a hard failure.
	if cur := sessVersionAt(ctx, dst, cfg.probe); cur == cfg.version {
		if err := sessWriteTelemetryOff(cfg.envDir); err != nil {
			say("agentsview %s is already installed, but the telemetry-off setting could not be written: %v", cur, err)
			return fail()
		}
		say("agentsview %s is already installed at %s; telemetry is off", cur, dst)
		return done(0, "sessions-none")
	}

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
	tmpName, err := sessExtractBinary(ctx, arch, dir, say)
	if err != nil {
		return fail()
	}
	defer os.Remove(tmpName)

	if err := os.Chmod(tmpName, 0o755); err != nil {
		say("cannot mark the binary executable: %v", err)
		return fail()
	}

	// It has to prove it runs AND reports the version we think we fetched — the
	// same gate the updater puts on our own new binary, tightened so a
	// mislabelled asset or checksum row cannot install something that runs but
	// is not the pinned build. The checksum already pins the bytes; this is the
	// belt to that suspenders.
	if v := sessVersionFromLine(cfg.probe(ctx, tmpName)); v != cfg.version {
		if v == "" {
			say("the downloaded binary did not run, or did not report a version")
		} else {
			say("the downloaded binary reports %s, not the expected %s", v, cfg.version)
		}
		return fail()
	}
	say("verified it runs and reports %s", cfg.version)

	// Telemetry off BEFORE the binary is published, so there is no window in
	// which agentsview is installed and reachable while still able to phone
	// home. A failure here fails the install rather than being shrugged off:
	// the whole point of installing it ourselves is that it starts quiet.
	if err := sessWriteTelemetryOff(cfg.envDir); err != nil {
		say("could not write the telemetry-off setting, so refusing to publish the binary: %v", err)
		return fail()
	}

	// Durable publish, the sequence update.go's syncFile/syncDir spell out:
	// flush the file's bytes, rename, then flush the directory entry, so a
	// crash cannot leave a half-written binary or a rename that did not stick.
	if err := syncFile(tmpName); err != nil {
		say("could not flush the binary to disk: %v", err)
		return fail()
	}
	if err := os.Rename(tmpName, dst); err != nil {
		say("cannot move the binary into place at %s: %v", dst, err)
		return fail()
	}
	syncDir(dir)

	say("installed agentsview %s at %s", cfg.version, dst)
	say("telemetry is off; the dashboard's fleet search can now reach this machine")
	return done(0, "")
}

// Staging files this feature leaves in the destination directory all share a
// prefix; a crash between create and remove can strand one. Swept on the next
// run so they do not accumulate, and only ours (the prefixes below) — never a
// file somebody else put there.
func sessSweepStaging(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".agentsview-") || strings.HasPrefix(n, ".agentsview-dl-") {
			os.Remove(filepath.Join(dir, n))
		}
	}
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
// temp file beside the destination. Deadline-bound through ctx so extraction
// cannot outlast the whole-operation budget, and the total number of bytes
// read across ALL members is capped, so an archive padded with junk members
// (which are refused below anyway) cannot decompress without bound.
func sessExtractBinary(ctx context.Context, arch *os.File, dir string, say func(string, ...any)) (string, error) {
	gz, err := gzip.NewReader(arch)
	if err != nil {
		say("the download is not a gzip archive: %v", err)
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := ""
	var budget int64 = sessMaxBinary + 1 // shared across every member read
	for {
		if err := ctx.Err(); err != nil {
			if found != "" {
				os.Remove(found)
			}
			say("ran out of time unpacking the archive")
			return "", err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if found != "" {
				os.Remove(found)
			}
			say("the archive could not be read: %v", err)
			return "", err
		}
		// EXACTLY the file we expect — the whole path, not its base name, so an
		// archive carrying "../../agentsview" or "/tmp/agentsview" is refused
		// rather than normalised into a match. There is one file to want, and
		// the loop keeps scanning to EOF so a second copy or a trailing member
		// is a reason to refuse, not something silently skipped.
		clean := strings.TrimPrefix(hdr.Name, "./")
		if clean != sessBinName || hdr.Typeflag != tar.TypeReg {
			// A NAMED-but-wrong member (an agentsview under a path, a second
			// regular file) is suspicious enough to reject outright; anything
			// truly unrelated is skipped but still counts against the budget.
			if filepath.Base(hdr.Name) == sessBinName {
				if found != "" {
					os.Remove(found)
				}
				say("the archive carried an unexpected %q; refusing it", hdr.Name)
				return "", fmt.Errorf("unexpected archive member")
			}
			continue
		}
		if found != "" {
			os.Remove(found)
			say("the archive contained more than one %s; refusing it", sessBinName)
			return "", fmt.Errorf("duplicate archive member")
		}
		tmpBin, err := os.CreateTemp(dir, ".agentsview-*")
		if err != nil {
			say("cannot stage the binary in %s: %v", dir, err)
			return "", err
		}
		tmpName := tmpBin.Name()
		n, cpErr := io.Copy(tmpBin, io.LimitReader(tr, budget))
		budget -= n
		closeErr := tmpBin.Close()
		if cpErr != nil {
			os.Remove(tmpName)
			say("could not write the binary out: %v", cpErr)
			return "", cpErr
		}
		if closeErr != nil {
			os.Remove(tmpName)
			say("could not finish writing the binary: %v", closeErr)
			return "", closeErr
		}
		if budget <= 0 {
			os.Remove(tmpName)
			say("the unpacked binary was larger than expected; refusing it")
			return "", fmt.Errorf("oversize binary")
		}
		found = tmpName
	}
	if found == "" {
		say("the archive did not contain an %s binary", sessBinName)
		return "", fmt.Errorf("binary not found in archive")
	}
	return found, nil
}

// What `agentsview --version` reports, or "" if it will not run. Telemetry off
// for the probe too, so a version check never phones home. Bounded by the
// smaller of the shared deadline and a per-probe backstop.
func sessProbe(ctx context.Context, path string) string {
	pctx, cancel := context.WithTimeout(ctx, sessProbeBudget)
	defer cancel()
	cmd := exec.CommandContext(pctx, path, "--version")
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

// The version a --version line reports, or "" — the token that is a leading
// 'v' followed by a DIGIT, so "version" (a word starting with v) and a stray
// "v" are not mistaken for it. agentsview prints "agentsview v0.40.1 (commit
// …)"; this pulls "0.40.1".
func sessVersionFromLine(line string) string {
	for _, tok := range strings.Fields(line) {
		if len(tok) >= 2 && tok[0] == 'v' && tok[1] >= '0' && tok[1] <= '9' {
			return tok[1:]
		}
	}
	return ""
}

// The version already installed at path, or "" if there is none or it will not
// answer. Used only to skip a reinstall, so any doubt resolves toward
// installing. The prober is injected so a test can stand in for a real binary.
func sessVersionAt(ctx context.Context, path string, probe func(context.Context, string) string) string {
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return sessVersionFromLine(probe(ctx, path))
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

	// A file that EXISTS but will not read is not an empty file: treating it as
	// one would let us replace an unreadable env (or an unreadable symlink
	// target) with a file holding only our line, wiping whatever was there. So
	// only a genuine "not there" is an empty start; any other read error stops.
	existing, rerr := os.ReadFile(path)
	if rerr != nil && !os.IsNotExist(rerr) {
		return rerr
	}

	// Rebuild the file line by line: any prior assignment to this variable —
	// `=1`, spaced, or `export`-prefixed — is REPLACED in place with the off
	// setting, and every other line is kept verbatim. Appending instead would
	// leave two assignments to one key whose winner agentsview does not
	// define, so "telemetry off" could not be guaranteed.
	var out []string
	replaced := false
	for _, l := range strings.Split(string(existing), "\n") {
		if sessIsTelemetryAssignment(l) {
			if !replaced {
				out = append(out, line)
				replaced = true
			}
			// Drop any further assignments — one canonical line is enough.
			continue
		}
		out = append(out, l)
	}
	if !replaced {
		// No prior assignment: add ours. Trailing "" from the final newline is
		// dropped so the join does not grow a blank line each run.
		if n := len(out); n > 0 && out[n-1] == "" {
			out[n-1] = line
			out = append(out, "")
		} else {
			out = append(out, line)
		}
	}
	body := strings.Join(out, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}

	// Nothing to do if the file is already exactly this — avoids a needless
	// rewrite (and a needless temp file) on every reinstall.
	if rerr == nil && string(existing) == body {
		return nil
	}

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
	if err := tmp.Sync(); err != nil {
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
	syncDir(dir)
	return nil
}

// Whether a line assigns AGENTSVIEW_TELEMETRY_ENABLED at all, in any of the
// forms a shell-style env file uses: bare, `export`-prefixed, and with spaces
// around the name. Value-agnostic, because the point is to find and replace
// whatever it was set to.
func sessIsTelemetryAssignment(l string) bool {
	s := strings.TrimSpace(l)
	s = strings.TrimPrefix(s, "export ")
	s = strings.TrimSpace(s)
	const key = "AGENTSVIEW_TELEMETRY_ENABLED"
	if !strings.HasPrefix(s, key) {
		return false
	}
	rest := strings.TrimSpace(s[len(key):])
	return strings.HasPrefix(rest, "=")
}
