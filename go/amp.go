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
//	Signed in as someone@example.com (team)
//	**Amp Megawatt Tier:** agent usage $19.79 of $20 remaining (99%), orb usage 750h of 750h a1.small orb hours remaining (100%) - period 2026-09-22 to 2026-10-22, resets upon renewal in 15 days
//	**Individual credits:** $8.71 remaining (set up auto-reload ...) - https://...
//
// Those rows are display text Amp's servers render — the same displayText its
// usage API returns; none of it is in the CLI binary — so they change shape
// without a CLI release. Earlier shapes, all still read below:
//
//	Amp Free: 100% remaining today (resets daily) - https://ampcode.com/...
//	Subscription Megawatt: 19% other usage and 100% orb usage remaining - resets upon renewal in 23 days
//	**Amp Megawatt Subscription:** 68% other usage and 97% orb usage remaining - resets upon renewal in 5 days
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
// ON THE FIRST LINE: `amp usage` prints the signed-in e-mail address. It is
// the reading's account (ampAddress): the name the tile is shown by, and,
// through accountkey.go's HMAC, the key that folds this machine's Amp with
// the same account's on another. Every
// figure still comes from a numeric capture group.

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	// America/New_York for the free tier's reset (ampNextFreeReset), on the
	// boxes that ship no zoneinfo — Windows, Android, OpenWrt.
	_ "time/tzdata"
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
	ampStaleMax = 1800.0 // three quota floors, as provStaleMax is

	ampTimeout = 15 * time.Second
	ampMaxOut  = 64 << 10 // the real output is four lines; this is a runaway guard
)

// The floor is on ATTEMPTS, not on successes.
//
// Keying it to the last success looked equivalent and was not: once a cached
// success aged past the interval, every failing collection left the clock
// untouched, so an Amp outage turned the floor off entirely and the push loop
// ran the CLI every 30 seconds for as long as the outage lasted — most work
// done exactly when it is least likely to help. A failure now occupies the
// window the same way a success does, and is served back from cache while it
// holds, so the card keeps saying the same true thing instead of flickering.
var ampCache struct {
	sync.Mutex
	last        *Provider // last SUCCESSFUL reading
	fetchedAt   float64   // when that reading was actually fetched
	fail        *Provider // last failure, served while the floor holds
	attemptedAt float64   // when the CLI last ran at all
	pace        paceMark  // refresh / watched floor, see pace.go
}

func collectAmp() Provider {
	ampCache.Lock()
	defer ampCache.Unlock()
	ampCache.pace.id = "amp"

	t := now()
	serve := func(p *Provider) Provider {
		out := *p
		out.CapturedAt = t // when we looked; RecordedAt still says when it was fetched
		return out
	}

	// Not due — no press, and not watched or the floor holds (pace.go): whatever the
	// last run produced stands, rather than running the CLI again. The last
	// success outlives a later failure only for ampStaleMax.
	if !ampCache.pace.due(t, ampCache.fetchedAt, ampCache.attemptedAt, ampMinInterval, ampCache.last != nil) &&
		(ampCache.last != nil || ampCache.fail != nil) {
		if ampCache.last != nil && (ampCache.fail == nil || t-ampCache.fetchedAt < ampStaleMax) {
			return serve(ampCache.last)
		}
		return serve(ampCache.fail)
	}

	ampCache.attemptedAt = t
	p := fetchAmp()
	if p.OK {
		ampCache.last, ampCache.fetchedAt, ampCache.fail = &p, t, nil
		return p
	}
	ampCache.fail = &p
	// A stale-but-real number beats an error nobody can act on, up to a point;
	// past ampStaleMax the error is the more honest answer.
	if ampCache.last != nil && t-ampCache.fetchedAt < ampStaleMax {
		return serve(ampCache.last)
	}
	ampCache.last = nil
	return p
}

func fetchAmp() Provider {
	p := Provider{ID: "amp", Name: "Amp", Source: "cli", CapturedAt: now()}
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}

	bin := ampBinary()
	if bin == "" {
		return fail("not-installed", "cli-missing", "amp")
	}
	out, exited, err := runAmpUsage(bin)
	if err != nil {
		// Never err.Error(): exec errors quote the full path being run, which
		// carries the local username straight onto every watcher's screen.
		if err == errAmpTimeout {
			return fail("unreachable", "cli-timeout", "amp usage")
		}
		return fail("cli-failed", "cli-failed", "amp usage")
	}
	return parseAmpUsage(p, out, now(), exited)
}

// Where the CLI is allowed to be.
//
// PATH is never consulted. It is inherited from whatever launched the helper,
// and "run the first thing called amp on PATH" is how a service ends up
// executing a file someone dropped in a directory that got prepended by a
// shell profile three years ago — a file that would sail through usableBinary,
// since an attacker's own 0755 binary is not world-writable.
//
// An earlier version kept exec.LookPath as a "last resort for unusual
// installs", which quietly gave back everything the fixed list was for: the
// fallback triggers precisely when the trustworthy locations came up empty,
// which is exactly the situation where PATH is least worth trusting. An
// unusual install sets MON_AMP_BIN and names the file outright.
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
	// Where npm puts a global CLI on Windows. Guarded on the variable being an
	// ABSOLUTE path, not merely set: APPDATA is a Windows thing and empty
	// everywhere else, and joining an empty root would turn these into
	// `npm/amp.cmd` — a RELATIVE path, resolved against whatever directory the
	// service happened to start in. That is the one shape a list of trusted
	// binaries must never contain.
	if appdata := os.Getenv("APPDATA"); filepath.IsAbs(appdata) {
		cands = append(cands,
			filepath.Join(appdata, "npm", "amp.cmd"),
			filepath.Join(appdata, "npm", "amp.exe"))
	}
	for _, c := range cands {
		if usableBinary(c) {
			return c
		}
	}
	return ""
}

// usableBinary — "a regular file this process should be willing to run" — is
// in fs_unix.go and fs_windows.go. It moved there when Windows arrived: the
// Unix test is a permission-bit test, Windows has no permission bits in a mode,
// and the honest Windows answer is a different and weaker check that deserved
// to say so in its own comment rather than hide behind this one.

var errAmpTimeout = &ampErr{"timeout"}

type ampErr struct{ s string }

func (e *ampErr) Error() string { return e.s }

// Returns the output, whether the process exited non-zero, and an error only
// when there is nothing usable to parse at all.
func runAmpUsage(bin string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ampTimeout)
	defer cancel()

	// Not exec.CommandContext directly: on Windows a global npm CLI is a .cmd,
	// which CreateProcess will not load and which therefore needs the command
	// interpreter in front of it. See toolCommand in fs_windows.go.
	cmd := toolCommand(ctx, bin, "usage")
	// Stdin nil means the null device, which is the point: a CLI that decides
	// to prompt must hit EOF and exit rather than hold the push loop open until
	// the deadline.
	cmd.Stdin = nil
	cmd.Env = ampEnv()

	// Without this the deadline is a suggestion. Custom writers make os/exec
	// build pipes and copy them in goroutines, and cancelling the context kills
	// only the direct child — a grandchild that inherited the write end keeps
	// the pipe open, so Wait blocks on the copy forever. It would block holding
	// the collector's mutex, which wedges every later collection, not just
	// Amp's. WaitDelay closes the pipes and lets Wait return.
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	lo := &limitedWriter{w: &stdout, n: ampMaxOut}
	le := &limitedWriter{w: &stderr, n: ampMaxOut}
	cmd.Stdout, cmd.Stderr = lo, le

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", false, errAmpTimeout
	}
	// Four lines of text overflowing a 64 KiB buffer means the thing on the
	// other end is not the CLI we think it is. Refuse the whole reading rather
	// than parse a prefix: the prefix can be perfectly well-formed, and
	// reporting a number scraped off the front of a runaway is worse than
	// reporting nothing.
	if lo.over || le.over {
		return "", false, &ampErr{"output too large"}
	}

	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = stderr.String()
	}
	if strings.TrimSpace(out) == "" {
		if err != nil {
			return "", true, err
		}
		return "", false, &ampErr{"empty output"}
	}
	// A non-zero exit still gets parsed — `amp usage` reports being signed out
	// with a failing status, and that is a state worth rendering — but the
	// status travels with the text so the parser can tell "signed out" from
	// "the service is down", which look alike from the output alone.
	return out, err != nil, nil
}

// What the child is allowed to see.
//
// An earlier version passed os.Environ() straight through, which handed Amp's
// process this helper's OWN relay bearer token: the installed unit supplies it
// as SUBNSUB_MONITOR_TOKEN through EnvironmentFile, so every invocation leaked
// the credential for the account's dashboard into a third party's address
// space. Nothing suggests Amp would look at it, and that is not the point — a
// program whose central claim is that credentials stay put does not hand one
// to a subprocess because it was convenient.
//
// So the environment is rebuilt from an allowlist, the same way the relay
// rebuilds a payload. The list is generous about what a network client on a
// corporate machine legitimately needs (proxies, CA bundles, locale) and says
// nothing about secrets. AMP_* passes through because those are Amp's own
// settings — including its API key, which is Amp's to hold.
// The Windows names are in the same list rather than behind a build tag: they
// simply do not exist elsewhere, and LookupEnv skips what is not set. They are
// there because an allowlist that names only the Unix ones is not a tighter
// allowlist on Windows, it is a BROKEN one — a process with no SystemRoot
// cannot resolve a DLL or open a socket, and one with no APPDATA cannot find
// the npm and Amp configuration it is being asked about. All of them are paths
// and none is a secret; the rule this list exists to enforce — that this
// helper's own relay token never reaches a third party's address space —
// is untouched.
var ampEnvKeys = []string{
	"HOME", "PATH", "USER", "LOGNAME", "SHELL", "TERM", "TZ", "TMPDIR", "LANG",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMFILES", "PROGRAMDATA",
	"SystemRoot", "SystemDrive", "windir", "COMSPEC", "PATHEXT", "TEMP", "TMP",
}

func ampEnv() []string {
	out := []string{"NO_COLOR=1"}
	for _, k := range ampEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if k := kv[:i]; strings.HasPrefix(k, "AMP_") || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

type limitedWriter struct {
	w    io.Writer
	n    int
	over bool
}

// Always reports the FULL length as written, whatever it actually kept. A
// short write is an error to os/exec, which would abandon the read and turn a
// capped-output run into a failed one — the opposite of the point. Overflow is
// recorded rather than merely absorbed, because a caller that cannot tell it
// happened will happily parse the truncated prefix and call it a reading.
func (l *limitedWriter) Write(p []byte) (int, error) {
	total := len(p)
	if l.n <= 0 {
		l.over = true
		return total, nil // swallow the rest; the process keeps running
	}
	if len(p) > l.n {
		p, l.over = p[:l.n], true
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
//
// Markdown bold around the labels ("**Amp Free:** 0% remaining today") is
// removed before any of them is matched, the same way colour codes are:
// emphasis is presentation, and a label wrapped in it is still the label.

// Two rules hold every pattern below together, and both were mistakes in the
// first version worth naming so they do not come back.
//
// SPACING IS [ \t], NEVER \s. In Go, \s matches \n, so `^Amp Free:\s*(digits)`
// happily reaches across a line break and pairs the label on one line with a
// number from somewhere further down the output. Restricting the run to
// horizontal whitespace is what actually makes these single-line patterns.
//
// NUMBERS CARRY A GROUPING GRAMMAR. `[0-9][0-9,]*` accepts commas anywhere,
// and the parser then strips them — so "$1,2.34" became $12.34 and "2,3 days"
// became 23 days, both published as confident readings. The alternation below
// accepts either properly grouped thousands or no separators at all, and
// nothing in between; anything else fails to match and contributes no row,
// which is the only acceptable direction to be wrong in.
var (
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

	amount = `([0-9]{1,3}(?:,[0-9]{3})+(?:\.[0-9]+)?|[0-9]+(?:\.[0-9]+)?)`
	days   = `([0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)`

	// "Signed in as someone@example.com" — matched for its shape: whether
	// the CLI is signed in at all.
	ampSignedInRE = regexp.MustCompile(`(?im)^[ \t]*Signed in as[ \t]+\S`)
	// The same line with the address captured — the reading's account
	// (ampAddress), and its key.
	ampSignedInAddrRE = regexp.MustCompile(`(?im)^[ \t]*Signed in as[ \t]+(\S+)`)

	// "Amp Free: 100% remaining today (resets daily)" — the parenthesis is
	// its own capture, because a row that does not say "resets daily" has no
	// reset to name (CodexBar AmpUsageParser, steipete/CodexBar@6fb316165e:
	// a percentage alone does not imply the daily reset).
	ampFreePctRE = regexp.MustCompile(`(?im)^[ \t]*Amp Free:[ \t]*` + amount + `[ \t]*%[ \t]+remaining(?:[ \t]+today)?(?:[ \t]*(\(resets[ \t]+daily\)))?`)

	// "Amp Free: $4.20 / $10.00 remaining (replenishes +$0.42/hour)" — the
	// balance-shaped variant of the same row.
	ampFreeAmtRE = regexp.MustCompile(`(?im)^[ \t]*Amp Free:[ \t]*\$?` + amount +
		`[ \t]*/[ \t]*\$?` + amount + `[ \t]+remaining(?:[ \t]*\(replenishes[ \t]*\+?\$?` +
		amount + `[ \t]*/[ \t]*hour\))?`)

	// The countdown both plan rows end on. A monthly plan counts in months
	// ("resets upon renewal in 1 month"), and a pattern that knew only days
	// dropped its reset.
	renewal = `resets[ \t]+upon[ \t]+renewal[ \t]+in[ \t]+` + days + `[ \t]+(days?|months?)\b`

	// "Subscription Megawatt: 19% other usage and 100% orb usage remaining -
	//  resets upon renewal in 23 days", and the same row as it was spelled
	// next: "Amp Megawatt Subscription: 68% other usage and …".
	ampSubRE = regexp.MustCompile(`(?im)^[ \t]*(?:Subscription[ \t]+(.+?)|Amp[ \t]+(.+?)[ \t]+Subscription):[ \t]*` + amount +
		`[ \t]*%[ \t]+other[ \t]+usage[ \t]+and[ \t]+` + amount +
		`[ \t]*%[ \t]+orb[ \t]+usage[ \t]+remaining(?:[ \t]*-[ \t]*` + renewal + `)?`)

	// "Amp Megawatt Tier: agent usage $19.79 of $20 remaining (99%), orb usage
	//  750h of 750h a1.small orb hours remaining (100%) - period 2026-09-22 to
	//  2026-10-22, resets upon renewal in 15 days" — the plan row since
	// 2026-09, quoting the agent pool in dollars and the orb pool in hours.
	// The row is found by its label and its clauses are read one at a time, so
	// a clause that changes shape costs that clause and not the whole row. The
	// clause shapes follow CodexBar's AmpUsageParser (steipete/CodexBar@9699239f),
	// which read this row first.
	ampTierRE      = regexp.MustCompile(`(?im)^[ \t]*Amp[ \t]+([^:\r\n]+?)[ \t]+Tier:([^\r\n]*)`)
	ampTierAgentRE = regexp.MustCompile(`(?i)\bagent[ \t]+usage[ \t]+\$` + amount + `[ \t]+of[ \t]+\$` + amount + `[ \t]+remaining\b`)
	ampTierOrbRE   = regexp.MustCompile(`(?i)\borb[ \t]+usage[ \t]+` + amount + `h[ \t]+of[ \t]+` + amount +
		`h[ \t]+[A-Za-z0-9][A-Za-z0-9._-]*[ \t]+orb[ \t]+hours[ \t]+remaining\b`)
	ampPeriodRE  = regexp.MustCompile(`(?i)\bperiod[ \t]+([0-9]{4}-[0-9]{2}-[0-9]{2})[ \t]+to[ \t]+([0-9]{4}-[0-9]{2}-[0-9]{2})\b`)
	ampRenewalRE = regexp.MustCompile(`(?i)\b` + renewal)

	// "Individual credits: $8.71 remaining"
	ampCreditsRE = regexp.MustCompile(`(?im)^[ \t]*Individual credits:[ \t]*\$?` + amount + `[ \t]+remaining`)

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

func parseAmpUsage(p Provider, out string, t float64, exited bool) Provider {
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}
	text := strings.ReplaceAll(ansiRE.ReplaceAllString(out, ""), "**", "")

	// The balance-shaped row before the percentage one, as CodexBar reads
	// them (freeUsage ?? freePercentUsage): the dollars carry more than the
	// share does.
	if m := ampFreeAmtRE.FindStringSubmatch(text); m != nil {
		remaining, ok1 := ampNum(m[1])
		quota, ok2 := ampNum(m[2])
		if ok1 && ok2 && quota > 0 {
			used := quota - remaining
			if used < 0 {
				used = 0
			}
			if remaining < 0 {
				remaining = 0
			}
			lim := Limit{
				Key: "free", UsedPercent: ampUsed(remaining / quota * 100),
				UsedAmount: fp(round2(used)), TotalAmount: fp(round2(quota)),
				RemainingAmount: fp(round2(remaining)), Unit: "usd",
			}
			// A balance that refills at a fixed rate has an implied window: how
			// long a full one takes to rebuild. That is the same quantity the
			// percentage rows call a window length, so it is labelled the same;
			// and what has been spent refills by the same clock, which is when
			// this balance is whole again (CodexBar AmpUsageParser: now + used
			// / hourly).
			if hourly, ok := ampNum(m[3]); ok && hourly > 0 {
				lim.WindowMinutes = fp(round2(quota / hourly * 60))
				lim.WindowLabel = windowLabel(*lim.WindowMinutes)
				if used > 0 {
					lim.ResetsAt = fp(math.Round(t + used/hourly*3600))
				}
			}
			p.Limits = append(p.Limits, lim)
		}
	} else if m := ampFreePctRE.FindStringSubmatch(text); m != nil {
		if v, ok := ampNum(m[1]); ok {
			lim := Limit{Key: "free", UsedPercent: ampUsed(v), WindowLabel: sp("1d")}
			// "(resets daily)" is 8 PM New York time, every day (CodexBar
			// AmpUsageSnapshot.nextFreeTierReset, steipete/CodexBar@ad966eb54f;
			// docs/amp.md: "resets at 8:00 PM America/New_York"). Only when the
			// row says so: a percentage alone names no reset.
			if m[2] != "" {
				lim.WindowMinutes = fp(1440)
				lim.ResetsAt = ampNextFreeReset(t)
			}
			p.Limits = append(p.Limits, lim)
		}
	}

	if m := ampTierRE.FindStringSubmatch(text); m != nil {
		ampTier(&p, m[1], m[2], t)
	} else if m := ampSubRE.FindStringSubmatch(text); m != nil {
		// Day granularity is all upstream gives. Rendering it as a to-the-second
		// countdown overstates the precision by less than the alternative
		// (no countdown at all) understates the information, and the same
		// rounding already applies to every other reset on the page.
		resets := ampRenewal(t, m[5], m[6])
		if v, ok := ampNum(m[3]); ok {
			p.Limits = append(p.Limits, Limit{
				Key: "subscription", UsedPercent: ampUsed(v),
				WindowLabel: sp("plan"), ResetsAt: resets})
		}
		if v, ok := ampNum(m[4]); ok {
			p.Limits = append(p.Limits, Limit{
				Key: "orb", UsedPercent: ampUsed(v),
				WindowLabel: sp("orb"), ResetsAt: resets})
		}
		// The plan name comes out of the CLI's output, so it goes through the
		// same sieve as Claude's subscriptionType before being published.
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if name := tame(name, 32); name != "" {
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
		// Three different problems that look alike from an empty parse, and
		// telling them apart is the whole value of the message: the fix for
		// one is `amp login`, the fix for another is to wait, and the third is
		// a parser we need to update.
		//
		// The earlier version treated "no sign-in line" as proof of being
		// signed out, so a CLI printing "service unavailable" and exiting
		// non-zero told the user to log in again — advice that cannot work and
		// hides a transient outage. Only an explicit signed-out phrase claims
		// that diagnosis now; a failing exit is reported as a failing exit.
		switch {
		case ampSignedOutRE.MatchString(text):
			return fail("not-signed-in", "cli-signin", "amp login")
		case exited:
			return fail("cli-failed", "cli-exit", "amp usage")
		case !ampSignedInRE.MatchString(text):
			return fail("cli-failed", "cli-shape", "amp usage")
		}
		return fail("no-readings", "cli-nolines", "amp usage")
	}

	if p.PlanType == nil && ampFreePctRE.MatchString(text) {
		p.PlanType = sp("Amp Free")
	}
	p.OK = true
	p.Account = cleanText(ampAddress(text), 60)
	p.AccountKey = ampAccountKey(text)
	// Live: `amp usage` asks Amp's servers every time it runs, so unlike Codex
	// there is no gap between the number and the last time the tool was used.
	p.RecordedAt = fp(t)
	return p
}

// ampAddress is the address on the sign-in line, or "" without one; a
// trailing "(team)" or punctuation the CLI may append is not part of it.
func ampAddress(text string) string {
	m := ampSignedInAddrRE.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	addr := strings.Trim(m[1], "().,;:<>\"'")
	if !strings.Contains(addr, "@") {
		return ""
	}
	return addr
}

// ampAccountKey is the account key for that address, or "" without one (or
// without a room to key it in).
func ampAccountKey(text string) string {
	if addr := ampAddress(text); addr != "" {
		return accountKey("amp", addr)
	}
	return ""
}

// The Tier row's clauses (ampTierRE): the agent pool, the orb pool, and when
// both renew. They land on the same keys the percentage-shaped row used, so a
// machine's card and history carry straight across the change of shape.
func ampTier(p *Provider, name, rest string, t float64) {
	// The billing period is the steadier answer to "when": the countdown is
	// rounded to whole days and moves with every refresh, the period's end
	// does not. Day precision either way, read as UTC midnight as CodexBar
	// reads it. A period already over is not the current one, and only a real
	// period has a length to call the window — the countdown has none.
	var resets, window *float64
	if start, end, ok := ampPeriod(rest); ok && end > t {
		resets, window = fp(end), fp((end-start)/60)
	} else if m := ampRenewalRE.FindStringSubmatch(rest); m != nil {
		resets = ampRenewal(t, m[1], m[2])
	}

	added := false
	// The pool's own dollars rather than the "(99%)" beside them, which is
	// rounded: $0.21 of $20 is 1.05% used, not 1%. A $0 pool has no share to
	// show, and inventing an empty or a full bar for it would be a reading of
	// nothing.
	if m := ampTierAgentRE.FindStringSubmatch(rest); m != nil {
		remaining, ok1 := ampNum(m[1])
		limit, ok2 := ampNum(m[2])
		if ok1 && ok2 && limit > 0 {
			used := limit - remaining
			if used < 0 {
				used = 0
			}
			p.Limits = append(p.Limits, Limit{
				Key: "subscription", UsedPercent: ampUsed(remaining / limit * 100),
				WindowLabel: sp("plan"), WindowMinutes: window, ResetsAt: resets,
				UsedAmount: fp(round2(used)), TotalAmount: fp(round2(limit)),
				RemainingAmount: fp(round2(remaining)), Unit: "usd",
			})
			added = true
		}
	}
	// Hours of whatever orb size the tier counts in: the size changes what an
	// hour buys, not the share of them left. Hours are not a unit the relay
	// carries (clean.js QUOTA_UNITS), so this pool travels as its share alone.
	if m := ampTierOrbRE.FindStringSubmatch(rest); m != nil {
		remaining, ok1 := ampNum(m[1])
		limit, ok2 := ampNum(m[2])
		if ok1 && ok2 && limit > 0 {
			p.Limits = append(p.Limits, Limit{
				Key: "orb", UsedPercent: ampUsed(remaining / limit * 100),
				WindowLabel: sp("orb"), WindowMinutes: window, ResetsAt: resets,
			})
			added = true
		}
	}
	// A plan chip on a reading that has no pool of that plan's in it would
	// be a label with nothing under it.
	if name := tame(name, 32); added && name != "" {
		p.PlanType = sp(name)
	}
}

// The next 20:00 in America/New_York strictly after t, Unix seconds — the
// free tier's daily reset, following the clock through daylight saving
// (00:00Z in summer, 01:00Z in winter). Nil when the zone cannot be loaded,
// which the tzdata import above makes a build error rather than a runtime
// one on the platforms without a zoneinfo of their own.
func ampNextFreeReset(t float64) *float64 {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil
	}
	at := time.Unix(int64(math.Floor(t)), 0).In(loc)
	next := time.Date(at.Year(), at.Month(), at.Day(), 20, 0, 0, 0, loc)
	if !next.After(at) {
		next = time.Date(at.Year(), at.Month(), at.Day()+1, 20, 0, 0, 0, loc)
	}
	return fp(float64(next.Unix()))
}

// "period 2026-09-22 to 2026-10-22" as Unix seconds, UTC midnight. A date the
// calendar does not have ("2026-02-30") or a period that runs backwards is no
// period at all — time.Parse refuses the first, the order check the second.
func ampPeriod(s string) (start, end float64, ok bool) {
	m := ampPeriodRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	a, errA := time.Parse("2006-01-02", m[1])
	b, errB := time.Parse("2006-01-02", m[2])
	if errA != nil || errB != nil || !b.After(a) {
		return 0, 0, false
	}
	return float64(a.Unix()), float64(b.Unix()), true
}

// "in 23 days" or "in 1 month", counted from t. A month is a calendar month,
// the way a monthly plan renews, not thirty days — and a day the target month
// does not have is its last day: Jan 31 plus a month is Feb 28, where
// time.AddDate would roll on to Mar 3. No count, no reset.
func ampRenewal(t float64, count, unit string) *float64 {
	n, ok := ampNum(count)
	if !ok || n <= 0 {
		return nil
	}
	if strings.HasPrefix(strings.ToLower(unit), "month") {
		if n > 1200 {
			return nil
		}
		from := time.Unix(int64(t), 0).UTC()
		y, mo, d := from.Date()
		// time.Date carries a month past December into the next year.
		first := time.Date(y, mo+time.Month(n), 1, 0, 0, 0, 0, time.UTC)
		if last := first.AddDate(0, 1, -1).Day(); d > last {
			d = last
		}
		at := time.Date(first.Year(), first.Month(), d, from.Hour(), from.Minute(), from.Second(), 0, time.UTC)
		return fp(t + float64(at.Unix()-from.Unix()))
	}
	return fp(t + n*86400)
}
