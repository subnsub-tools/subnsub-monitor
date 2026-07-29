package main

// Amp: quota costs neither a credential of ours nor a request of ours.
//
// Amp stores its API key at ~/.local/share/amp/secrets.json and records no
// balance on disk anywhere — its thread files carry per-conversation token
// counts and nothing about the account. So on the face of it Amp looks like
// Claude: to get a number you have to hold the key and call the vendor.
//
// It isn't, because Amp ships a CLI that will answer the question itself:
//
//	$ amp usage
//	Signed in as someone@example.com
//	Amp Free: 100% remaining today (resets daily) - https://ampcode.com/...
//	Subscription Megawatt: 19% other usage and 100% orb usage remaining - resets upon renewal in 23 days
//	Individual credits: $8.71 remaining (set up auto-reload ...) - https://...
//
// That makes this a THIRD cost tier, cheaper than the one Claude sits in and
// worth naming: Codex is a local file (no credential, no network), Claude is a
// credential we hold and a request we send, and Amp is a subprocess — the key
// stays inside Amp's own process, we never open secrets.json, and the only
// thing crossing into this program is text on a pipe. CodexBar's Amp provider
// reaches the same numbers three ways (this CLI, an AMP_API_KEY bearer against
// ampcode.com, and scraping the settings page with browser cookies); we
// implement only the first. The bearer path would mean holding a key we
// currently never touch, and the cookie path is outside this helper's stated
// boundary in the most direct way possible.
//
// What that buys, and what it costs:
//
//   - Buys: no Amp credential is ever read, held, or transmitted by us. The
//     strongest form of "no credential leaves the machine" is not touching it.
//   - Costs: this helper now executes another program. That is a genuinely new
//     capability — everything else here only reads files and makes one HTTPS
//     call — so the binary is resolved to an absolute path from a fixed list
//     rather than trusted from PATH, refused if anyone but the owner can write
//     it, run with stdin on the null device and a 15s deadline, and its output
//     is read into a bounded buffer.
//
// ON THE FIRST LINE, WHICH MUST NOT TRAVEL: `amp usage` prints the signed-in
// email address. This collector publishes to a relay and onto other people's
// screens. Nothing below captures that line — the sign-in check deliberately
// matches its shape and throws the address away, and every value that becomes
// part of a Provider comes from a numeric capture group. Same rule as
// system.go: identity is not a quota reading.

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ampBinEnv = "MON_AMP_BIN" // absolute path override, for unusual installs

	// `amp usage` is a live call to Amp's servers made on our behalf, so the
	// same reasoning as claude.go applies: the push loop's 30s would be 2,880
	// invocations a day per machine for a number that moves slowly. Between
	// calls the last answer is served as-is, and recorded_at still says when it
	// was actually fetched, so a cached reading visibly ages on the page.
	ampMinInterval = 120.0

	// How stale a cached reading may get before a failure is reported as a
	// failure. `amp usage` is a network call and will blip; flipping the card
	// to an error and back every half minute is worse than a number that is
	// visibly a few minutes old. Past this, the error is the truth.
	ampStaleMax = 600.0

	ampTimeout = 15 * time.Second
	ampMaxOut  = 64 << 10 // the real output is four lines; this is a runaway guard
)

var ampCache struct {
	sync.Mutex
	last      *Provider
	fetchedAt float64
}

func collectAmp() Provider {
	ampCache.Lock()
	defer ampCache.Unlock()

	t := now()
	if ampCache.last != nil && t-ampCache.fetchedAt < ampMinInterval {
		cached := *ampCache.last
		cached.CapturedAt = t // when we looked; RecordedAt still says when it was fetched
		return cached
	}

	p := fetchAmp()
	if p.OK {
		ampCache.last = &p
		ampCache.fetchedAt = t
		return p
	}
	if ampCache.last != nil && t-ampCache.fetchedAt < ampStaleMax {
		cached := *ampCache.last
		cached.CapturedAt = t
		return cached
	}
	return p
}

func fetchAmp() Provider {
	p := Provider{ID: "amp", Name: "Amp", Source: "cli", CapturedAt: now()}
	fail := func(err, detail string) Provider {
		p.Error, p.Detail = err, detail
		return p
	}

	bin := ampBinary()
	if bin == "" {
		return fail("not-installed", "找不到可用的 amp 命令")
	}
	out, err := runAmpUsage(bin)
	if err != nil {
		// Never err.Error(): exec errors quote the full path being run, which
		// carries the local username straight onto every watcher's screen.
		switch {
		case err == errAmpTimeout:
			return fail("unreachable", "amp usage 超时")
		default:
			return fail("cli-failed", "amp usage 执行失败")
		}
	}
	return parseAmpUsage(p, out, now())
}

// Where the CLI is allowed to be.
//
// Deliberately not exec.LookPath first. PATH is inherited from whatever
// launched the helper, and "run the first thing called amp on PATH" is how a
// service ends up executing a file someone dropped in a directory that got
// prepended by a shell profile three years ago. The fixed candidates are the
// two locations Amp's own installer uses plus the two usual package prefixes;
// LookPath stays as a last resort so an unusual-but-legitimate install still
// works, and MON_AMP_BIN is there for the rest.
func ampBinary() string {
	if v := strings.TrimSpace(os.Getenv(ampBinEnv)); v != "" {
		// An override that does not hold up is a refusal, not a reason to go
		// looking elsewhere: someone pointed us at a specific file on purpose.
		if filepath.IsAbs(v) && usableBinary(v) {
			return v
		}
		return ""
	}

	var cands []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cands = append(cands,
			filepath.Join(home, ".local", "bin", "amp"),
			filepath.Join(home, ".amp", "bin", "amp"))
	}
	cands = append(cands, "/usr/local/bin/amp", "/opt/homebrew/bin/amp")
	for _, c := range cands {
		if usableBinary(c) {
			return c
		}
	}
	if p, err := exec.LookPath("amp"); err == nil && usableBinary(p) {
		return p
	}
	return ""
}

// Stat follows symlinks on purpose — ~/.local/bin/amp is a symlink into
// ~/.amp/bin on a normal install, and the thing whose permissions matter is
// the file that actually runs.
//
// World-writable is refused; group-writable is not. The line is where it is
// because a file anyone on the box may rewrite is never legitimate, while
// group-writable is the ordinary state of a Homebrew prefix on a shared admin
// account, and refusing those would break real installs to defend against an
// attacker who, by construction, can already rewrite the user's own copy of
// the same binary and take their Amp session with it.
func usableBinary(path string) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	perm := st.Mode().Perm()
	return perm&0o111 != 0 && perm&0o002 == 0
}

var errAmpTimeout = &ampErr{"timeout"}

type ampErr struct{ s string }

func (e *ampErr) Error() string { return e.s }

func runAmpUsage(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ampTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "usage")
	// Stdin nil means the null device, which is the point: a CLI that decides
	// to prompt must hit EOF and exit rather than hold the push loop open until
	// the deadline.
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "NO_COLOR=1")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: ampMaxOut}
	cmd.Stderr = &limitedWriter{w: &stderr, n: ampMaxOut}

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", errAmpTimeout
	}
	// A non-zero exit still gets parsed: `amp usage` reports "not signed in" on
	// stderr with a failing status, and that is a state worth rendering rather
	// than a generic execution failure. Only an unusable result is an error.
	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = stderr.String()
	}
	if strings.TrimSpace(out) == "" {
		if err != nil {
			return "", err
		}
		return "", &ampErr{"empty output"}
	}
	return out, nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

// Always reports the FULL length as written, whatever it actually kept. A
// short write is an error to os/exec, which would abandon the read and turn a
// capped-output run into a failed one — the opposite of the point.
func (l *limitedWriter) Write(p []byte) (int, error) {
	total := len(p)
	if l.n <= 0 {
		return total, nil // swallow the overflow; the process keeps running
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.w.Write(p)
	l.n -= n
	return total, err
}

// ── parsing ───────────────────────────────────────────────────────────────
//
// The shapes below are Amp's own display text, which is a human-facing string
// and therefore subject to change without notice. Every pattern is anchored to
// the start of a line and to a literal prefix, and a line that no longer
// matches contributes nothing rather than contributing a wrong number. If Amp
// renames a row we lose that row and keep the others; the failure mode is a
// gauge going missing, never a gauge going wrong.

var (
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

	amount = `([0-9][0-9,]*(?:\.[0-9]+)?)`

	// "Signed in as someone@example.com" — matched for its shape only. The
	// address is deliberately outside every capture group.
	ampSignedInRE = regexp.MustCompile(`(?im)^\s*Signed in as\s+\S`)

	// "Amp Free: 100% remaining today (resets daily)"
	ampFreePctRE = regexp.MustCompile(`(?im)^\s*Amp Free:\s*` + amount + `\s*%\s+remaining`)

	// "Amp Free: $4.20 / $10.00 remaining (replenishes +$0.42/hour)" — the
	// balance-shaped variant of the same row.
	ampFreeAmtRE = regexp.MustCompile(`(?im)^\s*Amp Free:\s*\$?` + amount +
		`\s*/\s*\$?` + amount + `\s+remaining(?:\s*\(replenishes\s*\+?\$?` + amount + `\s*/\s*hour\))?`)

	// "Subscription Megawatt: 19% other usage and 100% orb usage remaining -
	//  resets upon renewal in 23 days"
	ampSubRE = regexp.MustCompile(`(?im)^\s*Subscription\s+(.+?):\s*` + amount +
		`\s*%\s+other\s+usage\s+and\s+` + amount +
		`\s*%\s+orb\s+usage\s+remaining(?:\s*-\s*resets\s+upon\s+renewal\s+in\s+([0-9][0-9,]*)\s+days?)?`)

	// "Individual credits: $8.71 remaining"
	ampCreditsRE = regexp.MustCompile(`(?im)^\s*Individual credits:\s*\$?` + amount + `\s+remaining`)

	ampSignedOutRE = regexp.MustCompile(`(?i)not (?:signed|logged) in|please (?:sign|log) in|run ['"` + "`" + `]?amp login`)
)

func ampNum(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", ""), 64)
	if err != nil || !finite(f) {
		return 0, false
	}
	return f, true
}

// remaining% -> used%, which is what every gauge on the page renders. Clamped
// because a vendor rounding 100.4% remaining down to nothing used is not worth
// a negative bar.
func ampUsed(remaining float64) float64 {
	used := 100 - remaining
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return round2(used)
}

func parseAmpUsage(p Provider, out string, t float64) Provider {
	fail := func(err, detail string) Provider {
		p.Error, p.Detail = err, detail
		return p
	}
	text := ansiRE.ReplaceAllString(out, "")

	if m := ampFreePctRE.FindStringSubmatch(text); m != nil {
		if v, ok := ampNum(m[1]); ok {
			// "resets daily" names no time of day, and inventing one would put
			// a countdown on the page that is wrong by up to 24 hours. The row
			// carries its length and no reset.
			p.Limits = append(p.Limits, Limit{
				Key: "free", UsedPercent: ampUsed(v), WindowLabel: sp("1d")})
		}
	} else if m := ampFreeAmtRE.FindStringSubmatch(text); m != nil {
		remaining, ok1 := ampNum(m[1])
		quota, ok2 := ampNum(m[2])
		if ok1 && ok2 && quota > 0 {
			lim := Limit{Key: "free", UsedPercent: ampUsed(remaining / quota * 100)}
			// A balance that refills at a fixed rate has an implied window: how
			// long a full one takes to rebuild. That is the same quantity the
			// percentage rows call a window length, so it is labelled the same.
			if hourly, ok := ampNum(m[3]); ok && hourly > 0 {
				lim.WindowMinutes = fp(round2(quota / hourly * 60))
				lim.WindowLabel = windowLabel(*lim.WindowMinutes)
			}
			p.Limits = append(p.Limits, lim)
		}
	}

	if m := ampSubRE.FindStringSubmatch(text); m != nil {
		// Day granularity is all upstream gives. Rendering it as a to-the-second
		// countdown overstates the precision by less than the alternative
		// (no countdown at all) understates the information, and the same
		// rounding already applies to every other reset on the page.
		var resets *float64
		if days, ok := ampNum(m[4]); ok && days > 0 {
			resets = fp(t + days*86400)
		}
		if v, ok := ampNum(m[2]); ok {
			p.Limits = append(p.Limits, Limit{
				Key: "subscription", UsedPercent: ampUsed(v),
				WindowLabel: sp("plan"), ResetsAt: resets})
		}
		if v, ok := ampNum(m[3]); ok {
			p.Limits = append(p.Limits, Limit{
				Key: "orb", UsedPercent: ampUsed(v),
				WindowLabel: sp("orb"), ResetsAt: resets})
		}
		// The plan name comes out of the CLI's output, so it goes through the
		// same sieve as Claude's subscriptionType before being published.
		if name := tame(m[1], 32); name != "" {
			p.PlanType = sp(name)
		}
	}

	if m := ampCreditsRE.FindStringSubmatch(text); m != nil {
		if v, ok := ampNum(m[1]); ok {
			p.Credits = &Credits{
				HasCredits: true,
				Balance:    "$" + strconv.FormatFloat(v, 'f', 2, 64),
			}
		}
	}

	if len(p.Limits) == 0 && p.Credits == nil {
		if ampSignedOutRE.MatchString(text) || !ampSignedInRE.MatchString(text) {
			return fail("not-signed-in", "amp 未登录；跑一次 amp login。")
		}
		return fail("no-readings", "amp usage 没有输出可识别的额度行")
	}

	if p.PlanType == nil && ampFreePctRE.MatchString(text) {
		p.PlanType = sp("Amp Free")
	}
	p.OK = true
	// Live: `amp usage` asks Amp's servers every time it runs, so unlike Codex
	// there is no gap between the number and the last time the tool was used.
	p.RecordedAt = fp(t)
	return p
}
