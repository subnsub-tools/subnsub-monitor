package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The exact bytes `amp usage` produced on a real signed-in machine, address
// replaced. Every other fixture here is a variation on this one; keeping the
// real thing verbatim is the difference between testing the parser and testing
// a guess about what the parser will be handed.
const ampReal = `Signed in as someone@example.com
Amp Free: 100% remaining today (resets daily) - https://ampcode.com/settings#amp-free
Subscription Megawatt: 19% other usage and 100% orb usage remaining - resets upon renewal in 23 days
Individual credits: $8.71 remaining (set up auto-reload to avoid running out) - https://ampcode.com/settings
`

func parseAmpFixture(t *testing.T, out string) Provider {
	t.Helper()
	return parseAmpUsage(Provider{ID: "amp", Name: "Amp", Source: "cli", CapturedAt: 1000}, out, 1000, false)
}

// Same, but for output that came with a failing exit status.
func parseAmpFailed(t *testing.T, out string) Provider {
	t.Helper()
	return parseAmpUsage(Provider{ID: "amp", Name: "Amp", Source: "cli", CapturedAt: 1000}, out, 1000, true)
}

func resetAmpCache() {
	ampCache.Lock()
	defer ampCache.Unlock()
	ampCache.last, ampCache.fail = nil, nil
	ampCache.fetchedAt, ampCache.attemptedAt = 0, 0
}

// A stand-in for the real CLI: records every invocation and its environment,
// then prints whatever the test wants it to print.
func fakeAmp(t *testing.T, script string) (bin, calls, envDump string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "amp")
	calls = filepath.Join(dir, "calls")
	envDump = filepath.Join(dir, "env")
	body := "#!/bin/sh\necho x >> " + calls + "\nenv > " + envDump + "\n" + script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls, envDump
}

func callCount(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "x\n")
}

func limitByKey(p Provider, key string) *Limit {
	for i := range p.Limits {
		if p.Limits[i].Key == key {
			return &p.Limits[i]
		}
	}
	return nil
}

func TestAmpParsesRealOutput(t *testing.T) {
	p := parseAmpFixture(t, ampReal)
	if !p.OK {
		t.Fatalf("want ok, got error=%q detail=%q", p.Error, p.Detail)
	}
	if len(p.Limits) != 3 {
		t.Fatalf("want 3 limits, got %d: %+v", len(p.Limits), p.Limits)
	}

	free := limitByKey(p, "free")
	if free == nil || free.UsedPercent != 0 {
		t.Fatalf("free: want 0%% used (100%% remaining), got %+v", free)
	}
	if free.WindowLabel == nil || *free.WindowLabel != "1d" {
		t.Errorf("free window label = %v, want 1d", free.WindowLabel)
	}
	// "resets daily" names no hour; a countdown here would be a guess.
	if free.ResetsAt != nil {
		t.Errorf("free must carry no reset time, got %v", *free.ResetsAt)
	}

	sub := limitByKey(p, "subscription")
	if sub == nil || sub.UsedPercent != 81 {
		t.Fatalf("subscription: want 81%% used (19%% remaining), got %+v", sub)
	}
	if sub.ResetsAt == nil || *sub.ResetsAt != 1000+23*86400 {
		t.Errorf("subscription reset = %v, want %v", sub.ResetsAt, float64(1000+23*86400))
	}

	orb := limitByKey(p, "orb")
	if orb == nil || orb.UsedPercent != 0 {
		t.Fatalf("orb: want 0%% used, got %+v", orb)
	}

	if p.PlanType == nil || *p.PlanType != "Megawatt" {
		t.Errorf("plan = %v, want Megawatt", p.PlanType)
	}
	if p.Credits == nil || !p.Credits.HasCredits || p.Credits.Balance != "$8.71" {
		t.Errorf("credits = %+v, want $8.71", p.Credits)
	}
	if p.RecordedAt == nil || *p.RecordedAt != 1000 {
		t.Errorf("recorded_at = %v, want the fetch time", p.RecordedAt)
	}
}

// The whole point of the subprocess route is that no Amp credential and no
// account identity passes through this program. `amp usage` prints the
// signed-in email on its first line, so the parser is the one place that could
// leak it — and the snapshot it builds is what gets published.
func TestAmpNeverPublishesTheEmail(t *testing.T) {
	p := parseAmpFixture(t, ampReal)
	blob := string(dump(p))
	for _, needle := range []string{"someone@example.com", "someone", "example.com", "Signed in"} {
		if strings.Contains(blob, needle) {
			t.Fatalf("identity leaked into the published payload (%q): %s", needle, blob)
		}
	}
}

func TestAmpBalanceShapedFreeTier(t *testing.T) {
	p := parseAmpFixture(t, `Signed in as someone@example.com
Amp Free: $4.00 / $10.00 remaining (replenishes +$0.50/hour)
`)
	if !p.OK {
		t.Fatalf("want ok, got %q", p.Error)
	}
	free := limitByKey(p, "free")
	if free == nil || free.UsedPercent != 60 {
		t.Fatalf("want 60%% used ($4 of $10 left), got %+v", free)
	}
	// $10 refilling at $0.50/hour is a 20-hour window.
	if free.WindowMinutes == nil || *free.WindowMinutes != 1200 {
		t.Fatalf("window minutes = %v, want 1200", free.WindowMinutes)
	}
	if free.WindowLabel == nil || *free.WindowLabel != "20h" {
		t.Errorf("window label = %v, want 20h", free.WindowLabel)
	}
}

// A paid account with no free tier and no subscription still has something
// worth showing. This shape is the reason the relay and the panel had to stop
// requiring at least one percentage row.
func TestAmpCreditsOnlyIsStillAReading(t *testing.T) {
	p := parseAmpFixture(t, `Signed in as someone@example.com
Individual credits: $124.50 remaining
`)
	if !p.OK {
		t.Fatalf("want ok, got %q / %q", p.Error, p.Detail)
	}
	if len(p.Limits) != 0 {
		t.Errorf("want no limits, got %+v", p.Limits)
	}
	if p.Credits == nil || p.Credits.Balance != "$124.50" {
		t.Fatalf("credits = %+v", p.Credits)
	}
}

func TestAmpSignedOut(t *testing.T) {
	// Only an explicit phrase earns this diagnosis, whatever the exit status.
	for _, out := range []string{
		"Not signed in. Run `amp login` to continue.\n",
		"Please log in at https://ampcode.com\n",
	} {
		for _, p := range []Provider{parseAmpFixture(t, out), parseAmpFailed(t, out)} {
			if p.OK {
				t.Fatalf("signed-out output parsed as ok: %q", out)
			}
			if p.Error != "not-signed-in" {
				t.Errorf("error = %q, want not-signed-in (for %q)", p.Error, out)
			}
		}
	}
}

// The failure this separates out: a CLI that is signed in fine but cannot
// reach the service exits non-zero with an unfamiliar message. Reading "no
// sign-in line" as "signed out" told the user to run `amp login`, which cannot
// fix an outage and hides it.
func TestAmpServiceFailureIsNotASignOutPrompt(t *testing.T) {
	p := parseAmpFailed(t, "Error: service unavailable (503)\n")
	if p.Error != "cli-failed" {
		t.Fatalf("error = %q, want cli-failed — a failing exit is not proof of being signed out", p.Error)
	}

	// Same text, but the process claimed success: still not a sign-out claim.
	if e := parseAmpFixture(t, "Error: service unavailable (503)\n").Error; e != "cli-failed" {
		t.Errorf("error = %q, want cli-failed", e)
	}
}

// Signed in, exited cleanly, but every quota line is in a shape we no longer
// recognise. That is a third problem again — the parser needs updating — and
// it must not borrow either of the other two messages.
func TestAmpSignedInButUnrecognised(t *testing.T) {
	p := parseAmpFixture(t, "Signed in as someone@example.com\nSomething entirely new: 42 sprockets\n")
	if p.OK {
		t.Fatal("unrecognised output must not be reported as a reading")
	}
	if p.Error != "no-readings" {
		t.Errorf("error = %q, want no-readings", p.Error)
	}
}

// A renamed row must cost us that row and nothing else.
func TestAmpPartialOutputKeepsWhatItCan(t *testing.T) {
	p := parseAmpFixture(t, `Signed in as someone@example.com
Amp Complimentary Allowance: 40% remaining today
Subscription Megawatt: 19% other usage and 100% orb usage remaining - resets upon renewal in 23 days
`)
	if !p.OK {
		t.Fatalf("want ok, got %q", p.Error)
	}
	if limitByKey(p, "free") != nil {
		t.Error("a row we no longer recognise must contribute nothing, not a wrong number")
	}
	if limitByKey(p, "subscription") == nil {
		t.Error("the rows we still recognise must survive")
	}
}

func TestAmpSubscriptionWithoutRenewalDate(t *testing.T) {
	p := parseAmpFixture(t,
		"Signed in as someone@example.com\nSubscription Free: 55% other usage and 90% orb usage remaining\n")
	sub := limitByKey(p, "subscription")
	if sub == nil || sub.UsedPercent != 45 {
		t.Fatalf("want 45%% used, got %+v", sub)
	}
	if sub.ResetsAt != nil {
		t.Errorf("no renewal date in the text means no reset time, got %v", *sub.ResetsAt)
	}
}

func TestAmpStripsANSI(t *testing.T) {
	p := parseAmpFixture(t, "Signed in as someone@example.com\n\x1b[1mAmp Free:\x1b[0m \x1b[32m75\x1b[0m% remaining today\n")
	free := limitByKey(p, "free")
	if free == nil || free.UsedPercent != 25 {
		t.Fatalf("colour codes broke the parse: %+v", free)
	}
}

func TestAmpThousandsSeparator(t *testing.T) {
	p := parseAmpFixture(t, "Signed in as someone@example.com\nIndividual credits: $1,240.00 remaining\n")
	if p.Credits == nil || p.Credits.Balance != "$1240.00" {
		t.Fatalf("credits = %+v, want the separator stripped", p.Credits)
	}
}

// The first version accepted commas anywhere and then stripped them, so
// "$1,2.34" was published as $12.34 and "2,3 days" as a 23-day renewal — both
// as confident readings. Misgrouped digits must produce no row at all.
func TestAmpRejectsMalformedGrouping(t *testing.T) {
	p := parseAmpFixture(t, "Signed in as someone@example.com\nIndividual credits: $1,2.34 remaining\n")
	if p.Credits != nil {
		t.Fatalf("misgrouped digits must not become a balance, got %+v", p.Credits)
	}

	q := parseAmpFixture(t, "Signed in as someone@example.com\n"+
		"Subscription Megawatt: 19% other usage and 100% orb usage remaining - resets upon renewal in 2,3 days\n")
	sub := limitByKey(q, "subscription")
	if sub == nil {
		t.Fatal("the percentages are still well formed and must survive")
	}
	if sub.ResetsAt != nil {
		t.Errorf("an unparseable day count must yield no reset time, got %v", *sub.ResetsAt)
	}

	// Properly grouped and ungrouped forms both still work.
	if v := parseAmpFixture(t, "Signed in as x@y.z\nIndividual credits: $1,240,500.25 remaining\n"); v.Credits == nil || v.Credits.Balance != "$1240500.25" {
		t.Errorf("grouped thousands broke: %+v", v.Credits)
	}
	if v := parseAmpFixture(t, "Signed in as x@y.z\nIndividual credits: $1240500.25 remaining\n"); v.Credits == nil || v.Credits.Balance != "$1240500.25" {
		t.Errorf("ungrouped digits broke: %+v", v.Credits)
	}
}

// \s matches a newline in Go, so the label on one line used to be able to pair
// with a number from further down the output. These patterns are single-line.
func TestAmpPatternsDoNotCrossLines(t *testing.T) {
	p := parseAmpFixture(t, "Signed in as someone@example.com\nAmp Free:\nsome diagnostic\n40% remaining today\n")
	if limitByKey(p, "free") != nil {
		t.Fatal("a label and a number on different lines must not be spliced into a reading")
	}

	q := parseAmpFixture(t, "Signed in as someone@example.com\nIndividual credits:\n$99.00 remaining\n")
	if q.Credits != nil {
		t.Fatalf("same for the balance, got %+v", q.Credits)
	}
}

// Percentages arriving out of range must not produce a negative or >100 bar.
func TestAmpClampsPercentages(t *testing.T) {
	p := parseAmpFixture(t, "Signed in as someone@example.com\nAmp Free: 140% remaining today\n")
	free := limitByKey(p, "free")
	if free == nil || free.UsedPercent != 0 {
		t.Fatalf("want clamp to 0, got %+v", free)
	}
}

// ── binary resolution ─────────────────────────────────────────────────────

func TestUsableBinaryRefusesWorldWritable(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "amp")
	if err := os.WriteFile(good, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !usableBinary(good) {
		t.Fatal("0755 executable should be usable")
	}

	bad := filepath.Join(dir, "amp-open")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o777); err != nil {
		t.Fatal(err)
	}
	if usableBinary(bad) {
		t.Fatal("a world-writable binary is one anybody on the box can swap; it must be refused")
	}

	notExec := filepath.Join(dir, "amp-plain")
	if err := os.WriteFile(notExec, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if usableBinary(notExec) {
		t.Fatal("a non-executable file is not a binary")
	}

	if usableBinary(dir) {
		t.Fatal("a directory is not a binary")
	}
	if usableBinary(filepath.Join(dir, "nope")) {
		t.Fatal("a missing path is not a binary")
	}
}

func TestAmpBinaryHonoursOverride(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "amp")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv(ampBinEnv, bin)
	if got := ampBinary(); got != bin {
		t.Fatalf("override ignored: got %q, want %q", got, bin)
	}

	// A pointed-at file that does not hold up is a refusal. Falling through to
	// the search would run a DIFFERENT binary than the one named, which is the
	// one outcome an explicit override must never produce.
	t.Setenv(ampBinEnv, filepath.Join(dir, "missing"))
	if got := ampBinary(); got != "" {
		t.Fatalf("a bad override must refuse, not fall back; got %q", got)
	}

	t.Setenv(ampBinEnv, "amp")
	if got := ampBinary(); got != "" {
		t.Fatalf("a relative override must be refused; got %q", got)
	}
}

// PATH is not a source of binaries to execute. The fallback that used to exist
// fired precisely when the trusted locations came up empty — the situation
// where PATH deserves the least trust — and an attacker's own 0755 file passes
// every permission check we can make on it.
func TestAmpBinaryIgnoresPATH(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "amp")
	if err := os.WriteFile(planted, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv(ampBinEnv, "")
	t.Setenv("HOME", t.TempDir()) // so the real machine's install cannot answer

	if got := ampBinary(); got == planted {
		t.Fatal("a binary found only on PATH must never be executed")
	}
}

// ── the environment handed to the child ───────────────────────────────────

// The helper's own relay token arrives as SUBNSUB_MONITOR_TOKEN through the
// unit's EnvironmentFile. Passing os.Environ() through handed it to a third
// party's process on every collection.
func TestAmpEnvWithholdsOurCredentials(t *testing.T) {
	t.Setenv("SUBNSUB_MONITOR_TOKEN", "relay-bearer-do-not-share")
	t.Setenv("MON_AMP_BIN", "/nonexistent")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "unrelated-but-still-not-amps")
	t.Setenv("AMP_API_KEY", "amps-own-key")
	t.Setenv("HOME", "/home/somebody")
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:8080")

	env := ampEnv()
	joined := strings.Join(env, "\n")
	for _, leaked := range []string{"relay-bearer-do-not-share", "unrelated-but-still-not-amps", "MON_AMP_BIN"} {
		if strings.Contains(joined, leaked) {
			t.Errorf("%q must not reach the child: %v", leaked, env)
		}
	}
	// …while everything the CLI legitimately needs still gets through.
	for _, kept := range []string{"AMP_API_KEY=amps-own-key", "HOME=/home/somebody",
		"HTTPS_PROXY=http://proxy.internal:8080", "NO_COLOR=1"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("%q should have been passed through: %v", kept, env)
		}
	}
}

// End to end, through a real subprocess: what the child actually sees.
func TestAmpChildProcessNeverSeesTheRelayToken(t *testing.T) {
	bin, _, envDump := fakeAmp(t, "echo 'Signed in as x@y.z'; echo 'Individual credits: $1.00 remaining'")
	t.Setenv("SUBNSUB_MONITOR_TOKEN", "relay-bearer-do-not-share")
	t.Setenv(ampBinEnv, bin)
	resetAmpCache()

	if p := fetchAmp(); !p.OK {
		t.Fatalf("fixture should parse: %q / %q", p.Error, p.Detail)
	}
	b, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "relay-bearer-do-not-share") {
		t.Fatal("the child process saw this helper's relay token")
	}
	if !strings.Contains(string(b), "NO_COLOR=1") {
		t.Error("NO_COLOR should be set for the child")
	}
}

// ── how often the CLI is allowed to run ───────────────────────────────────

func TestAmpFloorAppliesToFailuresToo(t *testing.T) {
	// Exits non-zero with an unfamiliar message: a failure, and one that must
	// not be retried on every push for as long as the outage lasts.
	bin, calls, _ := fakeAmp(t, "echo 'Error: service unavailable' >&2; exit 1")
	t.Setenv(ampBinEnv, bin)
	resetAmpCache()

	first := collectAmp()
	if first.OK || first.Error != "cli-failed" {
		t.Fatalf("want cli-failed, got ok=%v error=%q", first.OK, first.Error)
	}
	for i := 0; i < 5; i++ {
		if p := collectAmp(); p.Error != "cli-failed" {
			t.Fatalf("cached failure changed shape on call %d: %q", i, p.Error)
		}
	}
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("the CLI ran %d times inside one interval; the floor is on attempts, not successes", n)
	}
}

func TestAmpSuccessIsCachedNotRerun(t *testing.T) {
	bin, calls, _ := fakeAmp(t, "echo 'Signed in as x@y.z'; echo 'Amp Free: 60% remaining today'")
	t.Setenv(ampBinEnv, bin)
	resetAmpCache()

	for i := 0; i < 4; i++ {
		p := collectAmp()
		if !p.OK {
			t.Fatalf("call %d failed: %q", i, p.Error)
		}
		if l := limitByKey(p, "free"); l == nil || l.UsedPercent != 40 {
			t.Fatalf("call %d returned %+v", i, l)
		}
	}
	if n := callCount(t, calls); n != 1 {
		t.Fatalf("the CLI ran %d times; a fresh success should be served from cache", n)
	}
}

// Output that will not stop is not output to parse a prefix of, however
// well-formed that prefix looks.
func TestAmpRefusesRunawayOutput(t *testing.T) {
	bin, _, _ := fakeAmp(t, `echo 'Signed in as x@y.z'
echo 'Individual credits: $5.00 remaining'
i=0; while [ $i -lt 3000 ]; do echo 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; i=$((i+1)); done`)
	t.Setenv(ampBinEnv, bin)
	resetAmpCache()

	p := collectAmp()
	if p.OK {
		t.Fatalf("a runaway must not be reported as a reading, got %+v", p.Credits)
	}
	if p.Error != "cli-failed" {
		t.Errorf("error = %q, want cli-failed", p.Error)
	}
}

// The collector must survive a machine with no Amp at all — that is the common
// case, not the edge case.
func TestAmpNotInstalledIsAReportableState(t *testing.T) {
	t.Setenv(ampBinEnv, filepath.Join(t.TempDir(), "definitely-not-here"))
	p := fetchAmp()
	if p.OK {
		t.Fatal("want a failure when there is no binary")
	}
	if p.Error != "not-installed" {
		t.Errorf("error = %q, want not-installed", p.Error)
	}
	if p.ID != "amp" || p.Name != "Amp" {
		t.Errorf("a failing provider still has to name itself: %+v", p)
	}
}

// Whatever goes wrong, the detail line is published — so it must never contain
// a path, which on a normal install carries the local username.
func TestAmpErrorsCarryNoPaths(t *testing.T) {
	home, _ := os.UserHomeDir()
	t.Setenv(ampBinEnv, filepath.Join(t.TempDir(), "definitely-not-here"))
	p := fetchAmp()
	blob := string(dump(p))
	for _, needle := range []string{"/", "definitely-not-here"} {
		if strings.Contains(p.Detail, needle) {
			t.Fatalf("detail leaks a path: %q", p.Detail)
		}
	}
	if home != "" && strings.Contains(blob, home) {
		t.Fatalf("payload leaks the home directory: %s", blob)
	}
}

func TestLimitedWriterStopsAtCap(t *testing.T) {
	var buf strings.Builder
	w := &limitedWriter{w: &buf, n: 5}
	n, err := w.Write([]byte("abcdefghij"))
	if err != nil || n != 10 {
		t.Fatalf("a capped writer must still report the full write: n=%d err=%v", n, err)
	}
	if buf.String() != "abcde" {
		t.Fatalf("kept %q, want the first 5 bytes", buf.String())
	}
	if _, err := w.Write([]byte("more")); err != nil {
		t.Fatalf("writes past the cap must be swallowed, not fail: %v", err)
	}
	if buf.String() != "abcde" {
		t.Fatalf("kept %q after overflow", buf.String())
	}
}
