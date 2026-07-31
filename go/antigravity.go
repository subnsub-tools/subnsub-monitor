package main

// Antigravity: the quota comes from a server already running on this machine.
//
// Antigravity (Google's agentic IDE, built on Codeium's language server — hence
// the `exa.language_server_pb` package name below) keeps no quota on disk. What
// it does have is a language server listening on loopback that will answer the
// question, which puts it a rung below every other reading here — and what each
// reading COSTS is the thing worth naming:
//
//	Codex        a file on disk        no credential, no network, no process
//	Antigravity  a loopback request    no credential, no network off this box
//	Amp          a subprocess          no credential; the vendor's CLI answers
//	Claude       an HTTPS request      we hold the credential and make the call
//
// This one is cheaper than Amp's: nothing is launched. The server is already
// there because the user is running the IDE, we ask it a question over
// 127.0.0.1, and the answer never leaves the machine except as the percentages
// every other provider here also publishes. No Google credential is opened,
// held, or sent — the OAuth path CodexBar also implements (cloudcode-pa
// .googleapis.com) is deliberately NOT implemented here, for the same reason
// the Amp bearer path is not: it would mean holding a credential this helper
// currently never touches.
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
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Probing means a directory walk and a loopback request. Cheap, but `watch`
	// and the local /events stream can both ask far faster than the 30-second
	// push does, and there is no reason to.
	agMinInterval = 20.0
	// How long a real reading outlives the server that produced it. An IDE that
	// was closed is the ordinary case here, and the last known quota is worth
	// more than an error for a while — but not forever, or a card would show
	// yesterday's percentages as though they were current.
	agStaleMax = 600.0
	agTimeout  = 4 * time.Second
	// The Connect endpoints, in the order they are tried. The first is the one
	// that backs Antigravity's own Model Quota UI.
	agQuotaPath  = "/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary"
	agStatusPath = "/exa.language_server_pb.LanguageServerService/GetUserStatus"
	// Bounded read: this is a local server, but "the endpoint is ours" is
	// exactly the assumption worth not making.
	agMaxBody = 512 * 1024
)

var agCache struct {
	sync.Mutex
	last        *Provider
	fail        *Provider
	fetchedAt   float64
	attemptedAt float64
}

// Same shape as collectAmp: a floor on how often the real work runs, and a
// stale-but-real reading preferred over an error up to a point.
func collectAntigravity() Provider {
	agCache.Lock()
	defer agCache.Unlock()

	t := now()
	serve := func(p *Provider) Provider {
		out := *p
		out.CapturedAt = t // when we looked; RecordedAt still says when it was fetched
		return out
	}

	if agCache.last != nil && t-agCache.fetchedAt < agMinInterval {
		return serve(agCache.last)
	}
	if agCache.attemptedAt > 0 && t-agCache.attemptedAt < agMinInterval {
		if agCache.last != nil && t-agCache.fetchedAt < agStaleMax {
			return serve(agCache.last)
		}
		if agCache.fail != nil {
			return serve(agCache.fail)
		}
	}

	agCache.attemptedAt = t
	p := fetchAntigravity()
	if p.OK {
		agCache.last, agCache.fetchedAt, agCache.fail = &p, t, nil
		return p
	}
	agCache.fail = &p
	if agCache.last != nil && t-agCache.fetchedAt < agStaleMax {
		return serve(agCache.last)
	}
	agCache.last = nil
	return p
}

func fetchAntigravity() Provider {
	p := Provider{ID: "antigravity", Name: "Antigravity", Source: "local-api", CapturedAt: now()}

	cands := agCandidates()
	if len(cands) == 0 {
		// Not running is the ordinary state on most machines, and it is not a
		// failure of anything. Same wording as a missing CLI.
		p.Error = "not-installed"
		p.Detail = "no Antigravity language server is listening"
		return p
	}

	var lastErr string
	for _, c := range cands {
		for _, port := range c.ports {
			groups, err := agAsk(port, c.csrf)
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

	if lastErr == "" {
		lastErr = "no listening port found"
	}
	// The server was there and would not answer. Distinct from "not running",
	// because the two want different things done about them.
	p.Error = "unreachable"
	p.Detail = "the Antigravity language server did not return a quota: " + lastErr
	return p
}

/* ── discovery ────────────────────────────────────────────────────────
   One language server worth asking: its pid, the CSRF token out of its command
   line (empty for the CLI, which exposes no such flag and needs none), and the
   TCP ports it is listening on. How the three are found is platform-specific —
   /proc on Linux, ps + lsof everywhere else — but what counts as a match is
   not, so that lives here. */

type agCandidate struct {
	pid   int
	csrf  string
	ports []int
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
// Two accepted shapes, and both need more than a name: the app/IDE server is a
// `language_server*` binary that also names antigravity somewhere in its
// arguments (`--app_data_dir .../antigravity`), and the CLI is the `agy` binary
// or something under an antigravity-cli path. A bare `language_server` with no
// Antigravity marker belongs to some other Codeium-derived editor and is not
// ours to interrogate.
func agIsServer(argv []string) bool {
	if len(argv) == 0 || argv[0] == "" {
		return false
	}
	base := filepath.Base(argv[0])
	joined := strings.ToLower(strings.Join(argv, " "))

	if agServerNames[base] && strings.Contains(joined, "antigravity") {
		return true
	}
	if base == "agy" {
		return true
	}
	// Path-anchored, so a command that merely mentions the word in an argument
	// (a grep, an editor opening this file) cannot match.
	return strings.Contains(strings.ToLower(argv[0]), "antigravity-cli") ||
		strings.Contains(strings.ToLower(argv[0]), "antigravity_cli")
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

type agBucket struct {
	BucketID string `json:"bucketId"`
	// Two shapes seen for the same number; the nested one is current.
	Remaining         *agRemaining `json:"remaining"`
	RemainingFraction *float64     `json:"remainingFraction"`
	// ISO-8601 in current builds, epoch seconds in older ones. Decoded as
	// `any` so a number does not fail the whole document.
	ResetTime any `json:"resetTime"`
}

type agGroup struct {
	DisplayName string     `json:"displayName"`
	Buckets     []agBucket `json:"buckets"`
}

// Connect returns the response message at the top level; some builds wrap it
// in `response`. Both are accepted rather than guessed at.
type agSummary struct {
	Groups   []agGroup `json:"groups"`
	Response *struct {
		Groups []agGroup `json:"groups"`
	} `json:"response"`
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

func agAsk(port int, csrf string) ([]agGroup, error) {
	client := agClient()
	// The summary endpoint first; GetUserStatus is the older shape and is only
	// asked when the first is absent, not when it merely says nothing.
	for _, path := range []string{agQuotaPath, agStatusPath} {
		body, err := agPost(client, port, path, csrf)
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
		if len(s.Groups) > 0 {
			return s.Groups, nil
		}
		if s.Response != nil && len(s.Response.Groups) > 0 {
			return s.Response.Groups, nil
		}
	}
	return nil, fmt.Errorf("no quota in the reply")
}

func agPost(client *http.Client, port int, path, csrf string) ([]byte, error) {
	url := fmt.Sprintf("https://127.0.0.1:%d%s", port, path)
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
		return nil, fmt.Errorf("local server answered %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, agMaxBody))
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
			frac := b.RemainingFraction
			if b.Remaining != nil && b.Remaining.RemainingFraction != nil {
				frac = b.Remaining.RemainingFraction
			}
			if frac == nil {
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
			lim := Limit{Key: agKey(b.BucketID), UsedPercent: round2(used)}
			if lbl := agWindow(b.BucketID); lbl != "" {
				lim.WindowLabel = sp(lbl)
			}
			if g.DisplayName != "" {
				lim.Scope = sp(trimTo(g.DisplayName, 40))
			}
			if e := agReset(b.ResetTime); e > 0 {
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

func agKey(bucketID string) string {
	k := strings.ToLower(strings.TrimSpace(bucketID))
	if k == "" {
		return "quota"
	}
	return trimTo(k, 24)
}

// The window this bucket covers, in the same vocabulary the other providers
// use — "5h" and "7d" are what Codex and Claude already put in this column, and
// a fourth spelling would make the card read as four different things.
func agWindow(bucketID string) string {
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
