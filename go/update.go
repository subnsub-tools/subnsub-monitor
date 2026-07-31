package main

// Replacing this helper's own binary, when the dashboard asks it to.
//
// One sentence holds the whole design: THE DASHBOARD DECIDES WHEN, THE RELEASE
// BUCKET DECIDES WHAT. Those are deliberately not the same authority. A relay
// that has been taken over can make a machine update — and everything that
// machine will do about it is fetch the version genuinely published at
// updateBase, check it against the manifest published beside it, and run that.
// It cannot be pointed somewhere else, because no byte of the payload comes
// from the relay: the base URL is a constant in this file and there is no
// parameter, header or command field that moves it.
//
// Which is also why there is no timer here, and why adding one would be a
// different feature wearing this one's name. A helper that went looking for
// releases on its own would be a permanent, unattended download-and-execute
// channel on every machine — including every machine whose operator never
// turned the console on, which is strictly more authority than the console
// itself grants and would have been acquired by accident. This runs when
// somebody presses a button, or it does not run.
//
// And why the report goes out BEFORE the process exits: this is the one
// command whose success kills the thing that would have reported it. Swapping
// the binary and then trying to say so would lose the message in exactly the
// case where the swap worked — leaving an operator watching a machine go quiet
// with no way to tell a completed update from a helper that died.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// Where releases live. A constant, never a value taken from the relay or
	// from the command — see the note above about who decides what.
	updateBase = "https://tools.subnsub.com/monitor"
	// Overridable from this machine's own environment, for testing against a
	// local copy. Same precedence rule as every other switch here: the
	// environment belongs to whoever started the process, which is someone who
	// already had a shell on this box.
	updateBaseEnv = "MON_UPDATE_BASE"

	// The opt-in for a machine whose console is OFF. Existence is the setting,
	// like the console file and for the same reason: a switch whose meaning
	// depends on whether it says "0" or "false" is one somebody gets wrong in
	// the direction that leaves it on.
	updateFileName = "update"
	updateEnvVar   = "MON_UPDATE"

	// The whole operation, end to end, and it has to be ONE budget rather than
	// a per-request one.
	//
	// http.Client.Timeout applies to each request separately, so three requests
	// at five minutes each is a fifteen-minute update — which outlives the
	// relay's window for relaying the result (UPD_ISSUED_TTL) and the page's
	// for waiting on it (UPD_WAIT). The update would succeed and be reported to
	// nobody. Every downstream deadline is set against THIS number, so this is
	// the one that must stay the smallest.
	updateBudget = 8 * time.Minute
	// A per-request backstop as well, for a connection that goes silent
	// without the context noticing.
	updateTimeout = 5 * time.Minute
	// The new binary has to prove it runs before it is installed, and a binary
	// that hangs on `version` must not hang this helper.
	updateProbeTimeout = 10 * time.Second
	// Nothing published here is remotely this large. It exists so a replaced or
	// redirected endpoint cannot fill the disk before the checksum gets a say.
	updateMaxBytes = 64 << 20
	// The two small text files beside the binaries.
	updateMaxText = 64 << 10
)

// May the dashboard replace this helper?
//
// The console file counts on its own, and that is not a shortcut. A machine
// with the console on has already granted the dashboard arbitrary /bin/sh as
// this user, and `curl …/install.sh | sh` typed at that prompt IS this
// operation with fewer checks and no audit line. Refusing the narrow version
// of something already permitted in its broadest form would be theatre.
//
// The separate file is for the other machine: console off, but willing to be
// upgraded. That one is a genuinely new grant — a relay that can reach a
// machine which otherwise takes no instruction at all — so it gets its own
// switch, set here, off until then.
func updateAllowed() bool {
	// An explicit environment setting wins outright, including over the
	// console. Note what "off" does and does not buy: it removes the button,
	// not the console's own power to do the same thing by hand.
	if v := strings.TrimSpace(os.Getenv(updateEnvVar)); v != "" {
		return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
	}
	if consoleEnabled() {
		return true
	}
	dir := configDir()
	if dir == "" {
		return false
	}
	// Every error is a no, for the same reason it is in consoleEnabled: there
	// is no failure here that means "probably fine".
	st, err := os.Stat(filepath.Join(dir, updateFileName))
	return err == nil && st.Mode().IsRegular()
}

// Turn the dedicated switch on or off, from the command line on the machine.
func setRemoteUpdate(on bool) error {
	dir := configDir()
	if dir == "" {
		return os.ErrNotExist
	}
	path := filepath.Join(dir, updateFileName)
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 0600, like the console file: the presence of this file is a security
	// decision, and not one another local user should get to make here.
	return os.WriteFile(path, []byte("on\n"), 0o600)
}

// Whether the installed systemd unit will actually let this helper replace its
// own binary, and what to say when it will not.
//
// The same silent-disagreement problem consoleSandboxNote exists for, in a
// worse form. The unit is written once, at install time, with a hole for
// $BINDIR only if the update switch was on THEN. Flipping it afterwards
// succeeds, the dashboard grows a button, and the button fails several minutes
// in with a permission error about a temporary file — after the download, on a
// machine the operator cannot see. Saying so here is the difference between
// one more step and a feature that appears to work.
//
// Returns "" when there is nothing to say: switch off, no unit, a unit that
// already agrees, or launchd (which imposes none of this).
func updateSandboxNote(on bool) string {
	if !on {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "subnsub-monitor.service"))
	if err != nil {
		return ""
	}
	unit := string(b)
	// A relaxed unit — which is what --console writes — confines nothing here.
	if !strings.Contains(unit, "ProtectSystem=strict") {
		return ""
	}
	self, err := os.Executable()
	if err != nil || self == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(self); err == nil && real != "" {
		self = real
	}
	dir := filepath.Dir(self)
	if strings.Contains(unit, "ReadWritePaths=-"+dir) || strings.Contains(unit, "ReadWritePaths="+dir) {
		return ""
	}
	return "note: this machine's service is sandboxed read-only, so it cannot replace\n" +
		"      its own binary. Reinstall to lift that for " + dir + ":\n" +
		"        curl -fsSL https://tools.subnsub.com/monitor/install.sh | sh -s -- <TOKEN> --remote-update"
}

// Compare two YYYY.MM.DD.N versions component by component AS NUMBERS.
//
// The same rule as version.go and as the dashboard's vcmp, and it has to stay
// the same in all three: "2026.7.9" sorts after "2026.7.30" as a string and
// before it in every sense that matters. An empty or unparseable version is
// OLDER than everything rather than unknown — a helper that cannot say what it
// is is one from before the field existed.
func vercmp(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return -1
	}
	if b == "" {
		return 1
	}
	x, y := strings.Split(a, "."), strings.Split(b, ".")
	n := len(x)
	if len(y) > n {
		n = len(y)
	}
	at := func(parts []string, i int) int {
		if i >= len(parts) {
			return 0
		}
		v, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil || v < 0 {
			return 0
		}
		return v
	}
	for i := 0; i < n; i++ {
		p, q := at(x, i), at(y, i)
		if p != q {
			if p < q {
				return -1
			}
			return 1
		}
	}
	return 0
}

// A version string this program is willing to act on.
//
// THE SAME SHAPE THE RELAY ENFORCES, character for character — see
// HELPER_VERSION_RE in monitor-worker/src/clean.js. The two are separated by a
// network and cannot share code, so the only thing keeping them honest is that
// they are written to look identical and are tested against the same cases.
//
// A looser rule here is not merely untidy: "2999..1" is digits and dots, so it
// would pass a laxer check, get installed, and then be REFUSED by the relay
// when the new helper reported it — leaving a machine that runs fine, cannot
// name itself, and shows on the board as older than everything. An update that
// makes a healthy machine look broken is worse than one that refused to start.
var versionShape = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}\.\d{1,3}$`)

func validVersion(s string) bool { return versionShape.MatchString(s) }

// The old binary, kept under a second name.
//
// Only reached when the filesystem refuses a hard link. The destination is
// opened O_TRUNC rather than written beside and renamed, because this file is
// a spare and not the one anything runs — a half-written .prev is a worse
// spare, not a broken install.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	// A copy that failed must not leave the file it had already created. The
	// destination here is the .prev backup, and an empty or half-written one is
	// worse than none at all: it is exactly the thing somebody reaches for when
	// the update went wrong, and it would look like a helper they could restore.
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return nil
}

// The extension this platform needs on an executable.
//
// Windows will not run a file without one, and neither will Go's own exec
// resolution — a staged binary named `.subnsub-monitor-1234` downloads,
// verifies, and then cannot be probed, which would fail the update at the last
// check before the swap for a reason that has nothing to do with the release.
// The published asset carries the same suffix for the same reason: the file a
// browser downloads has to be one Windows will let the user run.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// Force this file's contents out to the device.
//
// Opened for WRITING although nothing is written: fsync on a read-only
// descriptor is fine on Unix, and the Windows equivalent — FlushFileBuffers —
// requires write access and fails with access denied without it. The file is
// this process's own temporary one, so asking for the stronger mode costs
// nothing anywhere.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// The same for a directory, which is what makes a rename itself durable.
// Attempted and not checked: several platforms and filesystems refuse it, and
// none of those refusals mean the install did not happen.
func syncDir(path string) {
	if f, err := os.Open(path); err == nil {
		f.Sync()
		f.Close()
	}
}

func updateClient() *http.Client {
	return &http.Client{
		Timeout: updateTimeout,
		// Refused for the same reason every other client here refuses: not the
		// token this time — these requests carry none — but because a redirect
		// is the one way the constant base URL above could still end up
		// fetching bytes from somewhere nobody chose.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func fetchText(ctx context.Context, c *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "subnsub-monitor")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return "", errStatus(resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, updateMaxText))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// The checksum SHA256SUMS gives for one file, or "".
//
// Matched on the exact name. A prefix match would accept the line for
// subnsub-monitor-linux-arm64 when asked about subnsub-monitor-linux-arm, and
// the two differ by one character in a field that decides which bytes are
// acceptable.
func sumFor(manifest, name string) string {
	for _, line := range strings.Split(manifest, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) != 2 {
			continue
		}
		// `sha256sum` marks binary reads with a leading '*' on the name.
		if strings.TrimPrefix(f[1], "*") != name {
			continue
		}
		h := strings.ToLower(f[0])
		if len(h) != 64 {
			return ""
		}
		for _, c := range h {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return ""
			}
		}
		return h
	}
	return ""
}

// Do it. Returns what to report, and whether the caller should now exit so the
// service manager can start the version that is already on disk.
//
// Everything that can fail happens before anything irreversible, and the one
// irreversible step — the rename — happens before the report rather than
// after, so a swap that failed is still something the operator gets told
// about. The only thing left for the caller is the exit.
func runUpdate(id string) (consoleResult, bool) {
	self, err := os.Executable()
	if err != nil || self == "" {
		return consoleResult{ID: id, Code: -1, Error: "update-failed",
			Out: "cannot work out this program's own path\n"}, false
	}
	// Through any symlink, so an install behind one replaces the real file
	// rather than turning the link into a regular binary and orphaning what it
	// pointed at.
	if real, err := filepath.EvalSymlinks(self); err == nil && real != "" {
		self = real
	}
	base := updateBase
	if v := strings.TrimSpace(os.Getenv(updateBaseEnv)); v != "" {
		base = strings.TrimRight(v, "/")
	}
	return updateFrom(id, self, base)
}

// The same thing with the two facts about the world passed in rather than
// looked up, which is what makes every branch below reachable from a test —
// including the ones that must not install anything, which are the branches
// worth being sure about.
func updateFrom(id, self, base string) (consoleResult, bool) {
	res := consoleResult{ID: id, Code: -1}
	started := time.Now()

	// Said twice on purpose: once into this machine's own log, which is the
	// record that does not pass through us, and once into the report that goes
	// back to whoever pressed the button.
	var log []string
	say := func(format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		log = append(log, s)
		warnf("update: %s", s)
	}
	done := func(code int, errStr string) (consoleResult, bool) {
		res.Code = code
		res.Error = errStr
		res.Ms = time.Since(started).Milliseconds()
		res.Out = strings.Join(log, "\n") + "\n"
		return res, false
	}
	fail := func() (consoleResult, bool) { return done(-1, "update-failed") }

	// One deadline over the whole thing, so the numbers everything downstream
	// waits on are bounded by a figure this side actually enforces.
	ctx, cancel := context.WithTimeout(context.Background(), updateBudget)
	defer cancel()
	client := updateClient()

	target, err := fetchText(ctx, client, base+"/VERSION")
	if err != nil {
		say("could not read the published version: %v", err)
		return fail()
	}
	target = strings.TrimSpace(target)
	if !validVersion(target) {
		say("the published version is not a version this build will act on")
		return fail()
	}
	say("running %s; published %s", helperVersion, target)

	// Only forward. A bucket serving an OLDER version than the one running is
	// either a rollback somebody performed deliberately — in which case
	// reinstalling is the honest way to do it — or an attempt to walk a fleet
	// back onto a build with a known hole. Neither is something to do because
	// a button was pressed.
	if vercmp(target, helperVersion) <= 0 {
		say("already up to date — nothing to install")
		// Marked so the page can say THIS rather than "done, the machine is
		// restarting". Both end successfully and neither is an error, but only
		// one of them is followed by a machine going quiet for half a minute,
		// and telling somebody to expect that when it will not happen is how a
		// working feature gets reported as broken. Release skew, a double
		// press, and a rolled-back bucket all land here.
		return done(0, "update-none")
	}

	asset := "subnsub-monitor-" + runtime.GOOS + "-" + runtime.GOARCH + exeSuffix()

	manifest, err := fetchText(ctx, client, base+"/SHA256SUMS")
	if err != nil {
		say("could not read the release manifest: %v", err)
		return fail()
	}
	want := sumFor(manifest, asset)
	if want == "" {
		// The realistic cause is a platform nobody publishes for. Saying which
		// one saves the operator from reading a release script to find out.
		say("the manifest lists no %s — this platform has no published build", asset)
		return fail()
	}

	// Staged in the SAME DIRECTORY as the binary being replaced, because the
	// swap below is a rename and a rename does not cross filesystems. /tmp is
	// its own mount often enough that using it would work everywhere it was
	// tested and fail on somebody's server.
	tmp, err := os.CreateTemp(filepath.Dir(self), ".subnsub-monitor-*"+exeSuffix())
	if err != nil {
		say("cannot write next to %s: %v", self, err)
		// The overwhelmingly likely cause on Linux, and worth naming rather
		// than leaving as a bare permission error: a helper installed WITHOUT
		// --console runs under ProtectHome=read-only, so its own binary is not
		// writable by it. That is the sandbox working, not a bug, and the way
		// through it is the installer.
		say("if this is a sandboxed service, re-run the installer with --remote-update")
		return fail()
	}
	tmpName := tmp.Name()
	// Harmless after a successful rename: the name no longer exists, and
	// removing something that is not there is not an error worth branching on.
	defer os.Remove(tmpName)

	req, err := http.NewRequestWithContext(ctx, "GET", base+"/"+asset, nil)
	if err != nil {
		tmp.Close()
		say("cannot build the download request")
		return fail()
	}
	req.Header.Set("User-Agent", "subnsub-monitor")
	resp, err := client.Do(req)
	if err != nil {
		tmp.Close()
		say("download failed: %v", err)
		return fail()
	}
	h := sha256.New()
	// Bounded by one byte more than the ceiling, so a body AT the limit is
	// distinguishable from one that was cut off at it.
	n, cerr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, updateMaxBytes+1))
	resp.Body.Close()
	closeErr := tmp.Close()
	if resp.StatusCode != 200 {
		say("download failed: http %d", resp.StatusCode)
		return fail()
	}
	if cerr != nil || closeErr != nil {
		say("download did not complete")
		return fail()
	}
	if n > updateMaxBytes {
		say("the published binary is larger than this build will accept")
		return fail()
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		// The whole point of the exercise. Nothing has been installed and
		// nothing will be.
		say("checksum mismatch — refusing to install")
		say("  expected %s", want)
		say("  got      %s", got)
		return fail()
	}
	// Rounded UP, so anything that arrived at all reads as something. A
	// truncating divide printed "downloaded 0 KiB" beside "checksum matches",
	// which is a sentence that reads as a bug in the line above it.
	say("downloaded %d KiB; checksum matches the manifest", (n+1023)/1024)

	if err := os.Chmod(tmpName, 0o755); err != nil {
		say("could not make the new binary executable: %v", err)
		return fail()
	}

	// Run it before trusting it. The checksum proves the bytes are the ones
	// that were published; this proves they are a program THIS machine can
	// execute and that it is the version the manifest claimed. A binary for the
	// wrong architecture passes every check above and fails here — which is
	// much better than failing after the rename, as a service that no longer
	// starts on a machine nobody has a route to.
	// Its own, shorter deadline, but still inside the overall budget: a binary
	// that hangs must not be able to spend what is left of it.
	pctx, pcancel := context.WithTimeout(ctx, updateProbeTimeout)
	defer pcancel()
	out, err := exec.CommandContext(pctx, tmpName, "version").Output()
	if err != nil {
		say("the new binary would not run here: %v", err)
		return fail()
	}
	if reported := strings.TrimSpace(string(out)); reported != target {
		say("the new binary calls itself %q, not %q — refusing to install", reported, target)
		return fail()
	}
	say("the new binary runs and reports %s", target)

	// ── from here it is real ────────────────────────────────────────────────
	// The bytes, before the entry that will point at them.
	//
	// A rename is atomic with respect to other processes, not with respect to
	// power. Without this a crash after the rename can leave the service path
	// pointing at a file whose contents were never written back — and the
	// machine comes up to a truncated binary that will not start, having been
	// told the update succeeded.
	if err := syncFile(tmpName); err != nil {
		say("could not flush the new binary to disk: %v", err)
		return fail()
	}
	// The outgoing binary is kept under a name that is not on anyone's PATH. It
	// costs a few megabytes and it is the difference between "restore the
	// previous helper" being a move and being another download on a machine
	// whose helper is the thing that just broke.
	//
	// HOW the swap is done is the one genuinely platform-shaped step in this
	// file — a running program's file can be replaced in place on Unix and
	// cannot on Windows — so it lives in update_unix.go and update_windows.go,
	// where each can explain the guarantee it does and does not offer. Both
	// promise the same thing on failure: nothing was replaced, and the helper
	// the service starts is the one that is running now.
	prev := self + ".prev"
	if err := installOver(tmpName, self, prev); err != nil {
		say("%v", err)
		return fail()
	}
	// And the directory entry itself. Best effort: not every platform allows
	// it, and a failure here is not a reason to unpick an install that has
	// already happened.
	syncDir(filepath.Dir(self))
	say("installed %s over %s", target, helperVersion)
	say("restarting; the service manager starts the new one within about a minute")

	res.Code = 0
	res.Ms = time.Since(started).Milliseconds()
	res.Out = strings.Join(log, "\n") + "\n"
	return res, true
}
