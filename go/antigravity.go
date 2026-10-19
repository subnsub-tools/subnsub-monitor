package main

// Antigravity: the quota comes from the CLI's own report, or from a server
// already running on this machine.
//
// Antigravity (Google's agentic IDE and its CLI, agy, both built on Codeium's
// language server — hence the `exa.language_server_pb` package name below)
// keeps no quota on disk. Two things on the machine will answer the question,
// and what each reading COSTS is the thing worth naming:
//
//	Codex        a file on disk        no credential, no network, no process
//	Antigravity  a loopback request    no credential, no network off this box
//	             or a subprocess       no credential; the vendor's CLI answers
//	Amp          a subprocess          no credential; the vendor's CLI answers
//	Claude       an HTTPS request      we hold the credential and make the call
//
// The loopback request first: the IDE (and agy before 1.2.2) leaves a
// language server listening on 127.0.0.1 that answers a Connect call, so
// nothing is launched and the answer never leaves the machine except as the
// percentages every other provider here also publishes. Since agy 1.2.2 the
// CLI's server gates that call behind a CSRF token the CLI generates and never
// exposes (CodexBar AntigravityProviderDescriptor.firstCSRFGatedVersion,
// steipete/CodexBar@ed3c650e), and on macOS `ps` shows a CLI started from
// PATH as a bare `agy`, which the matcher below cannot tell from any other
// two-letter process — so where the CLI is installed, its own report is
// asked for instead: `agy -p /usage --output-format json` prints the same
// four buckets as JSON in about six seconds, with no model turn (num_turns
// 0, zero tokens; verified against agy 1.3.4 on 2026-10-14). That is Amp's
// cost rung exactly: the Google credential stays inside agy's own process,
// we never open the keychain item or token file it lives in, and the only
// thing crossing into this program is text on a pipe. The OAuth path CodexBar
// also implements (cloudcode-pa.googleapis.com) remains deliberately NOT
// implemented here, for the same reason the Amp bearer path is not: it would
// mean holding a credential this helper currently never touches.
//
// The protocol was worked out from CodexBar's Antigravity provider, which is
// where the credit for this integration belongs — see the README. It speaks
// Connect (a POST with a JSON body to /<package>.<Service>/<Method>).
//
// ON TLS: the loopback port serves a self-signed certificate, so verification
// is switched off for it. That is not a hole being waved through — there is no
// certificate authority that would ever sign 127.0.0.1 for a local process, so
// there is nothing here that verification could check. What bounds the risk is
// the DIAL, not the handshake: the address is a literal loopback IP, so the
// connection cannot leave this machine no matter what the server presents. The
// insecure setting is scoped to that transport and reachable from nowhere else.

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	agTimeout = 4 * time.Second
	// The CLI's report. `agy -p /usage` boots the CLI's language server, signs
	// in with the token the CLI holds, asks Google and exits — about six
	// seconds on a laptop; --print-timeout is the CLI's own cap on the turn,
	// inside ours. MON_AGY_BIN names the binary for an unusual install, as
	// MON_AMP_BIN does for Amp.
	agyBinEnv       = "MON_AGY_BIN"
	agyTimeout      = 45 * time.Second
	agyPrintTimeout = "30s"
	// A ceiling on the WHOLE collection, discovery included. One request is
	// bounded at agTimeout, but a process may hold any number of listening
	// sockets and they were being tried one after another while the cache
	// mutex was held — so a single mis-matched process could stall this
	// collector, every caller waiting on it, and the providers queued behind
	// it. Two numbers rather than one: a hard deadline, and a cap on how many
	// endpoints any one candidate is worth.
	agBudget       = 8 * time.Second
	agMaxEndpoints = 6
	// The Connect endpoints, in the order they are tried. The first is the one
	// that backs Antigravity's own Model Quota UI.
	agQuotaPath  = "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary"
	agStatusPath = "/exa.language_server_pb.LanguageServerService/GetUserStatus"
	// Bounded read: this is a local server, but "the endpoint is ours" is
	// exactly the assumption worth not making.
	agMaxBody = 512 * 1024
)

// The cache floor and the stale window are provhttp.go's — the CLI's report
// is a process launched and a request to Google made on our behalf, so the
// same reasoning as amp.go applies: a real call when a refresh is pressed or
// on pace.go's floor while watched, the last answer served between. The
// loopback probe is cheaper and could run more often, but two cadences for
// one tile is a tile that changes meaning with how it was read.
var agCache = provCache{pace: paceMark{id: "antigravity"}}

func collectAntigravity() Provider {
	return agCache.collect(fetchAntigravity)
}

func fetchAntigravity() Provider {
	p := Provider{ID: "antigravity", Name: "Antigravity", Source: "local-api", CapturedAt: now()}

	cands := agCandidates()
	var lastErr string
	deadline := time.Now().Add(agBudget)
	for _, c := range cands {
		for i, ep := range c.eps {
			if i >= agMaxEndpoints {
				break
			}
			if time.Now().After(deadline) {
				lastErr = "gave up before every port was tried"
				break
			}
			groups, err := agAsk(ep, c.csrf)
			if err != nil {
				lastErr = err.Error()
				continue
			}
			if lim := agLimits(groups); len(lim) > 0 {
				p.OK = true
				p.Limits = lim
				p.RecordedAt = fp(now())
				return p
			}
			lastErr = "no quota groups in the reply"
		}
	}

	// No server answered — none listening, or the CLI's keeping its token to
	// itself. Where the CLI is installed, its own report is the reading.
	if bin := vendorBinary(agyBinEnv, vendorCandidates("agy")); bin != "" {
		return fetchAgyReport(bin)
	}
	if len(cands) == 0 {
		// Not running is the ordinary state on most machines, and it is not a
		// failure of anything. Same wording as a missing CLI.
		p.failWith("not-installed", "ls-none")
		return p
	}
	if lastErr == "" {
		lastErr = "no listening port found"
	}
	// The server was there and would not answer. Distinct from "not running",
	// because the two want different things done about them.
	p.failWith("unreachable", "ls-no-quota", lastErr)
	return p
}

/* ── the CLI's report ───────────────────────────────────────────────────
   `agy -p /usage --output-format json`: the CLI answers its own /usage
   command in print mode and exits, no model turn made. Amp's cost rung and
   Amp's discipline (clirun.go): a binary from a fixed list or MON_AGY_BIN,
   never PATH; an allow-listed environment; null stdin, bounded output, a
   real deadline. */

func fetchAgyReport(bin string) Provider {
	p := Provider{ID: "antigravity", Name: "Antigravity", Source: "cli", CapturedAt: now()}
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}
	out, exited, err := agyRun(bin)
	if err != nil {
		// Never err.Error(): exec errors quote the path being run, which
		// carries the local username onto every watcher's screen.
		if err == errAmpTimeout {
			return fail("unreachable", "cli-timeout", "agy")
		}
		return fail("cli-failed", "cli-failed", "agy")
	}
	return parseAgyReport(p, out, exited)
}

// One run of the report, in a private directory of its own.
//
// Signing in, agy opens a browser: `open` or `xdg-open` found on PATH, or
// $BROWSER — in print mode too (CodexBar AntigravityProviderDescriptor
// .runPrintUsage, steipete/CodexBar@ed3c650e). A helper under launchd or
// systemd asking every few minutes would raise Google's sign-in page on the
// user's screen each time the token is gone, so both names resolve to a stub
// that exits 1, first on PATH, and BROWSER names the same stub. The CLI's
// own log goes into the directory as well, one file per run, instead of
// into the user's log folder; the directory is removed with the run.
func agyRun(bin string) (string, bool, error) {
	dir, err := os.MkdirTemp("", "subnsub-agy-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)
	stub := []byte("#!/bin/sh\nexit 1\n")
	for _, name := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(dir, name), stub, 0o700); err != nil {
			return "", false, err
		}
	}
	env := vendorEnv(nil)
	path := "/usr/bin:/bin"
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
			env = append(env[:i], env[i+1:]...)
			break
		}
	}
	env = append(env,
		"PATH="+dir+string(os.PathListSeparator)+path,
		"BROWSER="+filepath.Join(dir, "open"))
	// The whole tree on the deadline: agy's server is a child of its own
	// (runVendorCLITree), and a hung one must not outlive this run.
	return runVendorCLITree(bin, []string{
		"-p", "/usage", "--output-format", "json",
		"--print-timeout", agyPrintTimeout,
		"--log-file", filepath.Join(dir, "agy.log"),
	}, env, agyTimeout)
}

// The report, as agy 1.3.4 prints it with --output-format json:
//
//	{"conversation_id":"","status":"SUCCESS","response":"Gemini Models\tWeekly Limit Remaining\t100%\t…",
//	 "num_turns":0,"usage":{"total_tokens":0,…},
//	 "command":{"name":"usage","data":{"description":"…","groups":[{"name":"Gemini Models",
//	   "buckets":[{"id":"gemini-weekly","name":"Weekly Limit Remaining","window":"weekly",
//	               "remaining_fraction":1,"reset_time":"2026-10-21T01:25:17Z"},…]},…]}}}
//
// `response` is the same table as text and is not read; the buckets are the
// server's, spelled in snake_case, and agBucket knows both spellings
// (CodexBar parseCLIUsageReport decodes them with convertFromSnakeCase).
type agReport struct {
	Status  string `json:"status"`
	Command *struct {
		Name string    `json:"name"`
		Data agSummary `json:"data"`
	} `json:"command"`
}

// What the CLI says when it is not a report — the sign-in state is the one
// worth telling apart, because its fix is `agy` once in a terminal (CodexBar
// AntigravityCLIPrintFailure.signInMarkers; "not logged in" also covers the
// CLI's own "You are not logged into Antigravity").
var agySignedOutRE = regexp.MustCompile(`(?i)not logged in|not signed in|unauthenticated|authentication required|login required|please (?:log|sign) in`)

// What the CLI printed, read into a Provider — split from the run so the
// shapes can be tested without a CLI.
func parseAgyReport(p Provider, out string, exited bool) Provider {
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}
	text := strings.TrimSpace(ansiRE.ReplaceAllString(out, ""))
	// Whatever the CLI printed before the document (a notice, an update
	// banner) is skipped to the first brace; whatever follows it is ignored.
	var rep agReport
	parsed := false
	if i := strings.IndexByte(text, '{'); i >= 0 {
		parsed = json.NewDecoder(strings.NewReader(text[i:])).Decode(&rep) == nil
	}
	failed := parsed && rep.Status != "" && rep.Status != "SUCCESS"
	if !parsed || failed || rep.Command == nil || rep.Command.Name != "usage" {
		// Same ladder as amp.go: only an explicit signed-out phrase claims
		// that diagnosis; a failing exit or a failed status is reported as
		// that; and a clean exit with no report is a parser this build has
		// to update.
		switch {
		case agySignedOutRE.MatchString(text):
			return fail("not-signed-in", "cli-signin", "agy")
		case exited || failed:
			return fail("cli-failed", "cli-exit", "agy")
		}
		return fail("cli-failed", "cli-shape", "agy")
	}
	lims := agLimits(rep.Command.Data.groups())
	if len(lims) == 0 {
		return fail("no-readings", "cli-nolines", "agy")
	}
	p.OK = true
	p.Limits = lims
	// Live: the CLI asks Google every time it runs.
	p.RecordedAt = fp(now())
	return p
}

/* ── discovery ────────────────────────────────────────────────────────
   One language server worth asking: its pid, the CSRF token out of its command
   line (empty for the CLI, which exposes no such flag and needs none), and the
   TCP ports it is listening on. How the three are found is platform-specific —
   /proc on Linux, ps + lsof everywhere else — but what counts as a match is
   not, so that lives here. */

// One endpoint to try. The HOST travels with the port because the two are
// not interchangeable: a server bound only to ::1 is unreachable at
// 127.0.0.1, and keeping just the number silently turned every IPv6-only
// install into "not running".
type agEndpoint struct {
	host string
	port int
}

type agCandidate struct {
	pid  int
	csrf string
	eps  []agEndpoint
}

// argv[0] basenames Antigravity's server ships under, across platforms and
// builds. Matched on the basename rather than by substring so that a path
// containing the word cannot promote an unrelated process.
var agServerNames = map[string]bool{
	"language_server":           true,
	"language_server_macos":     true,
	"language_server_macos_arm": true,
	"language-server":           true,
	"language_server_linux_x64": true,
	"language_server_linux_arm": true,
}

// Is this argv an Antigravity language server?
//
// The bar is deliberately higher than "mentions the word". argv[0] is a string
// a process chooses for itself and the rest of the command line is whatever
// anyone typed, so a match here decides which local program this helper hands
// a token to and believes about quota — the two failure modes being a Codeium
// editor with a project called "antigravity" open, and a process that named
// itself to be found.
//
// So the marker has to be the VALUE OF A FLAG we expect, not a substring of
// the line: `--app_data_dir <path containing antigravity>` is what the real
// server carries. The CLI shape stays path-anchored, and a bare `agy` is no
// longer enough on its own — a two-letter argv[0] is the easiest thing in the
// world to claim.
//
// This is one of two checks. The other is the caller's: the process must
// belong to the same user this helper runs as.
func agIsServer(argv []string) bool {
	if len(argv) == 0 || argv[0] == "" {
		return false
	}
	base := filepath.Base(argv[0])
	exe := strings.ToLower(argv[0])

	// The CLI, anchored to its own install path.
	if strings.Contains(exe, "antigravity-cli") || strings.Contains(exe, "antigravity_cli") ||
		(base == "agy" && strings.Contains(exe, "antigravity")) {
		return true
	}
	if !agServerNames[base] {
		return false
	}
	// The desktop app has shipped as Gemini.app since 2026-08 (CodexBar
	// AntigravityStatusProbe, steipete/CodexBar@a2444e62ec), and its server
	// runs out of the bundle: the bundle's path is the flag value's equal, a
	// place only that app installs to.
	if strings.Contains(exe, "/gemini.app/") || strings.Contains(exe, "\\gemini.app\\") {
		return true
	}
	return agFlagNames(argv, "antigravity")
}

// Does a flag VALUE — not the line as a whole — contain this marker?
// Accepts both `--flag value` and `--flag=value`, and only for the flags whose
// value legitimately names the product's data directory.
func agFlagNames(argv []string, marker string) bool {
	want := map[string]bool{"--app_data_dir": true, "--app-data-dir": true,
		"--extension_dir": true, "--ide_name": true}
	for i, a := range argv {
		name, inline, hasEq := strings.Cut(a, "=")
		if !want[name] {
			continue
		}
		v := inline
		if !hasEq {
			if i+1 >= len(argv) {
				continue
			}
			v = argv[i+1]
		}
		if strings.Contains(strings.ToLower(v), marker) {
			return true
		}
	}
	return false
}

// `--csrf_token X` or `--csrf_token=X`. Absent for the CLI, which needs none.
func agCsrf(argv []string) string {
	for i, a := range argv {
		if a == "--csrf_token" && i+1 < len(argv) {
			return agCleanToken(argv[i+1])
		}
		if v, ok := strings.CutPrefix(a, "--csrf_token="); ok {
			return agCleanToken(v)
		}
	}
	return ""
}

// It goes into a header, so anything that could split one is refused rather
// than trimmed — a token with a newline in it is not a token.
func agCleanToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 200 {
		return ""
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return ""
		}
	}
	return s
}

/* ── the wire ─────────────────────────────────────────────────────────── */

type agRemaining struct {
	RemainingFraction *float64 `json:"remainingFraction"`
}

// One bucket, in every spelling the three sources of this summary use —
// the server's Connect reply, Cloud Code's, and the CLI's own report
// (CodexBar AntigravityQuotaSummaryParser, steipete/CodexBar@62acfd8ead):
// `bucketId` or `id`, `displayName` or `name` — and the CLI's print report
// spells the same fields in snake_case (`bucket_id`, `remaining_fraction`,
// `reset_time`), which encoding/json does not fold into the camelCase names.
type agBucket struct {
	BucketID      string `json:"bucketId"`
	ID            string `json:"id"`
	BucketIDSnake string `json:"bucket_id"`
	// Two shapes seen for the same number; the nested one is current.
	Remaining              *agRemaining `json:"remaining"`
	RemainingFraction      *float64     `json:"remainingFraction"`
	RemainingFractionSnake *float64     `json:"remaining_fraction"`
	// The period, stated: "weekly" or "5h". When the server says it, the id
	// is not consulted — a bucket named gemini-5h with window "weekly" is
	// weekly — and a window this build does not know is no period at all
	// rather than a guess from the id.
	Window string `json:"window"`
	// A bucket switched off carries no reading, whatever its number says.
	Disabled bool `json:"disabled"`
	// ISO-8601 in current builds, epoch seconds in older ones. Decoded as
	// `any` so a number does not fail the whole document.
	ResetTime      any `json:"resetTime"`
	ResetTimeSnake any `json:"reset_time"`
}

type agGroup struct {
	DisplayName string     `json:"displayName"`
	Name        string     `json:"name"`
	Buckets     []agBucket `json:"buckets"`
}

func (g *agGroup) label() string {
	if g.DisplayName != "" {
		return g.DisplayName
	}
	return g.Name
}

// Connect returns the response message at the top level; some builds wrap it
// in `response`, the CLI's report in `summary`. All three are accepted
// rather than guessed at.
type agSummary struct {
	Groups   []agGroup `json:"groups"`
	Response *struct {
		Groups []agGroup `json:"groups"`
	} `json:"response"`
	Summary *struct {
		Groups []agGroup `json:"groups"`
	} `json:"summary"`
}

// The groups, from whichever wrapper holds them.
func (s *agSummary) groups() []agGroup {
	if len(s.Groups) > 0 {
		return s.Groups
	}
	if s.Response != nil && len(s.Response.Groups) > 0 {
		return s.Response.Groups
	}
	if s.Summary != nil && len(s.Summary.Groups) > 0 {
		return s.Summary.Groups
	}
	return nil
}

func agClient() *http.Client {
	return &http.Client{
		Timeout: agTimeout,
		Transport: &http.Transport{
			// The dial is pinned to loopback by the URL this transport is only
			// ever used with; see the note at the top of this file for why
			// verification is off and why that is bounded rather than blind.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext:     (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			// One request, then done. A keep-alive pool to a server that comes
			// and goes with an IDE window is a pool of dead connections.
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// The metadata every Connect call to this server carries. Deliberately fixed
// strings: this identifies the CLIENT, and inventing a field to put the machine
// or the user in would be the one thing this helper does not do.
const agMeta = `{"metadata":{"ideName":"antigravity","extensionName":"antigravity",` +
	`"locale":"en","ideVersion":"unknown"}}`

func agAsk(ep agEndpoint, csrf string) ([]agGroup, error) {
	client := agClient()
	// The summary endpoint first; GetUserStatus is the older shape and is only
	// asked when the first is absent, not when it merely says nothing.
	for _, path := range []string{agQuotaPath, agStatusPath} {
		body, err := agPost(client, ep, path, csrf)
		if err != nil {
			// A 404 means this build does not have that method; anything else
			// means the server is there and unhappy, and trying the next path
			// will not help.
			if strings.Contains(err.Error(), "404") {
				continue
			}
			return nil, err
		}
		var s agSummary
		if err := json.Unmarshal(body, &s); err != nil {
			continue
		}
		if groups := s.groups(); len(groups) > 0 {
			return groups, nil
		}
	}
	return nil, fmt.Errorf("no quota in the reply")
}

func agPost(client *http.Client, ep agEndpoint, path, csrf string) ([]byte, error) {
	// JoinHostPort, so an IPv6 host is bracketed rather than producing an
	// address that parses as something else entirely.
	url := "https://" + net.JoinHostPort(ep.host, strconv.Itoa(ep.port)) + path
	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte(agMeta)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if csrf != "" {
		// The app and IDE servers require it; the CLI's exposes no such flag
		// and needs none, which is why an empty token is not an error here.
		req.Header.Set("X-Codeium-Csrf-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		// Never the URL or the error's own text: this string is published, and
		// a transport error can quote whatever it was handed.
		return nil, fmt.Errorf("no answer on the local port")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == 401 && csrf == "" {
			// agy 1.2.2 and later gate their local server behind a CSRF
			// token the CLI generates and never exposes (CodexBar
			// AntigravityProviderDescriptor.firstCSRFGatedVersion,
			// steipete/CodexBar@7d7c730185): a 401 from a tokenless ask is
			// that, not a server that would not answer.
			return nil, fmt.Errorf("local server answered 401 (agy 1.2.2+ keeps its token to itself)")
		}
		return nil, fmt.Errorf("local server answered %d", resp.StatusCode)
	}
	// One byte past the cap, so "too big" is DETECTED rather than silently
	// becoming a shorter document. A prefix of a reply is not the reply, and a
	// server that can make the first 512 KiB parse cleanly should not be able
	// to hide the rest behind a truncation nobody noticed.
	body, err := io.ReadAll(io.LimitReader(resp.Body, agMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("could not read the reply")
	}
	if len(body) > agMaxBody {
		return nil, fmt.Errorf("reply was too large")
	}
	return body, nil
}

/* ── the reading ──────────────────────────────────────────────────────── */

// Antigravity's own Model Quota UI shows two groups — "Gemini Models" and
// "Claude and GPT models" — each with a weekly and a five-hour bucket. That is
// four rows, which is why the group name becomes the row's SCOPE rather than a
// separate provider: the card already has a shape for "same window, different
// model family" (Claude's weekly_scoped uses it for Opus).
func agLimits(groups []agGroup) []Limit {
	var out []Limit
	for _, g := range groups {
		for _, b := range g.Buckets {
			if b.Disabled {
				continue
			}
			frac := b.RemainingFraction
			if frac == nil {
				frac = b.RemainingFractionSnake
			}
			if b.Remaining != nil && b.Remaining.RemainingFraction != nil {
				frac = b.Remaining.RemainingFraction
			}
			// A fraction is between nothing and everything. Anything else is
			// a protocol change or a bad reading, and clamping it would turn
			// both into a plausible number: -1 saturates to "100% used", which
			// is indistinguishable from a genuinely exhausted quota. Dropped
			// instead, on the same rule the empty bucket follows — a missing
			// reading must never render as a confident one.
			if frac == nil || *frac < -0.001 || *frac > 1.001 {
				continue
			}
			// The server reports what is LEFT; every other provider here
			// reports what is used, and the card draws a bar that fills up.
			used := (1 - *frac) * 100
			if used < 0 {
				used = 0
			}
			if used > 100 {
				used = 100
			}
			id := b.BucketID
			if id == "" {
				id = b.ID
			}
			if id == "" {
				id = b.BucketIDSnake
			}
			lim := Limit{Key: agKey(id), UsedPercent: round2(used)}
			if lbl := agWindow(b.Window, id); lbl != "" {
				lim.WindowLabel = sp(lbl)
			}
			if name := g.label(); name != "" {
				lim.Scope = sp(agScope(name))
			}
			reset := b.ResetTime
			if reset == nil {
				reset = b.ResetTimeSnake
			}
			if e := agReset(reset); e > 0 {
				lim.ResetsAt = fp(e)
			}
			out = append(out, lim)
			if len(out) >= 8 {
				// The relay keeps eight per provider; sending more is a
				// payload nobody will render.
				return out
			}
		}
	}
	return out
}

// The bucket id becomes this row's key, and the key is rendered in every
// browser watching this account. What answers on that loopback port is a
// server this helper authenticated in no way at all, so the id is accepted
// only in the shape a real one has — letters, digits and separators — and
// anything else becomes the generic word rather than travelling.
func agKey(bucketID string) string {
	k := strings.ToLower(strings.TrimSpace(bucketID))
	if k == "" || len(k) > 24 {
		return "quota"
	}
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return "quota"
		}
	}
	return k
}

// The group name becomes the row's scope, and is free text from the same
// unauthenticated source. Repaired rather than refused — it is the one field
// here whose whole job is to be read by a human — but repaired against what
// text can DO on a page rather than what it says: control characters, bidi
// overrides that can make a label render as something it does not spell, and
// invisible padding. Same rule, and the same list, the relay applies to a
// machine's name.
func agScope(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20, r >= 0x7f && r <= 0x9f:
		case r >= 0x202a && r <= 0x202e:
		case r >= 0x2066 && r <= 0x2069:
		case r == 0xfeff:
		default:
			b.WriteRune(r)
		}
	}
	return trimTo(b.String(), 40)
}

// The window this bucket covers, in the same vocabulary the other providers
// use — "5h" and "7d" are what Codex and Claude already put in this column, and
// a fourth spelling would make the card read as four different things.
//
// A stated `window` is the answer, known or not; the id is read only when
// none is stated (CodexBar AntigravityStatusProbe, @62acfd8ead — "window":
// "unknown" on gemini-5h is no period, not five hours).
func agWindow(window, bucketID string) string {
	if w := strings.TrimSpace(window); w != "" {
		switch strings.ToLower(strings.ReplaceAll(strings.TrimSuffix(strings.ToLower(w), " limit"), "_", "-")) {
		case "weekly", "week":
			return "7d"
		case "session", "5h", "5-hour", "five hour", "five-hour":
			return "5h"
		case "daily", "day":
			return "1d"
		}
		return ""
	}
	u := strings.ToUpper(bucketID)
	switch {
	case strings.Contains(u, "WEEK"):
		return "7d"
	case strings.Contains(u, "FIVE_HOUR"), strings.Contains(u, "5_HOUR"),
		strings.Contains(u, "5H"), strings.Contains(u, "HOUR"):
		return "5h"
	case strings.Contains(u, "DAY"), strings.Contains(u, "DAILY"):
		return "1d"
	case strings.Contains(u, "MONTH"):
		return "30d"
	}
	return ""
}

// ISO-8601 in current builds, epoch seconds in older ones, and milliseconds
// from anything that got the unit wrong. All three land on unix seconds,
// because a reset time in the wrong unit renders as a countdown that is
// plausible and completely false.
func agReset(v any) float64 {
	switch x := v.(type) {
	case string:
		if e := isoToEpoch(x); e > 0 {
			return e
		}
	case float64:
		if x > 1e11 { // milliseconds
			return x / 1000
		}
		if x > 1e9 {
			return x
		}
	}
	return 0
}

func trimTo(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > n {
		return strings.TrimSpace(string(r[:n]))
	}
	return s
}
