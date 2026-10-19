package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// The exact bytes `amp usage` printed on 2026-10-07, address and account
// replaced: bold labels, and the plan row as a Tier quoting dollars and hours.
// Every row of it read as unrecognised until this shape was taught, so the
// card showed no Amp at all.
const ampTierReal = `Signed in as someone@example.com (someorg)
**Amp Megawatt Tier:** agent usage $19.79 of $20 remaining (99%), orb usage 750h of 750h a1.small orb hours remaining (100%) - period 2026-09-22 to 2026-10-22, resets upon renewal in 15 days
**Individual credits:** $8.71 remaining (set up auto-reload to avoid running out) - https://ampcode.com/settings

# Run ` + "`amp usage --details`" + ` for more detailed information.
`

func utcEpoch(y int, mo time.Month, d, h, mi int) float64 {
	return float64(time.Date(y, mo, d, h, mi, 0, 0, time.UTC).Unix())
}

func parseAmpAt(out string, t float64) Provider {
	return parseAmpUsage(Provider{ID: "amp", Name: "Amp", Source: "cli", CapturedAt: t}, out, t, false)
}

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
	// "(resets daily)" is 8 PM New York time: from t=1000 (1970-01-01
	// 00:16Z, 19:16 EST on 1969-12-31) that is 20:00 EST the same evening,
	// 01:00Z on 1970-01-01.
	if free.ResetsAt == nil || *free.ResetsAt != utcEpoch(1970, time.January, 1, 1, 0) {
		t.Errorf("free reset = %v, want 1970-01-01T01:00Z", free.ResetsAt)
	}
	if free.WindowMinutes == nil || *free.WindowMinutes != 1440 {
		t.Errorf("a daily reset is a 24h window: %v", free.WindowMinutes)
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

// `amp usage` prints the signed-in e-mail on its first line: it is the
// reading's account, the name its tile is shown by (operator ruling 2026-10-18: that
// privacy rule no longer exists). The line itself is not a figure and goes nowhere.
func TestAmpNamesTheAccount(t *testing.T) {
	p := parseAmpFixture(t, ampReal)
	if p.Account != "someone@example.com" {
		t.Fatalf("account = %q, want the signed-in address", p.Account)
	}
	if blob := string(dump(p)); strings.Contains(blob, "Signed in") {
		t.Fatalf("the sign-in line is not a value: %s", blob)
	}
}

func TestAmpParsesTierOutput(t *testing.T) {
	now := utcEpoch(2026, 10, 7, 1, 50)
	p := parseAmpAt(ampTierReal, now)
	if !p.OK {
		t.Fatalf("want ok, got error=%q detail=%q", p.Error, p.Detail)
	}
	if len(p.Limits) != 2 {
		t.Fatalf("want 2 limits, got %d: %+v", len(p.Limits), p.Limits)
	}
	end := utcEpoch(2026, 10, 22, 0, 0)

	// The dollars, not the rounded "(99%)": $0.21 of $20 is 1.05% used.
	sub := limitByKey(p, "subscription")
	if sub == nil || sub.UsedPercent != 1.05 {
		t.Fatalf("subscription: want 1.05%% used, got %+v", sub)
	}
	if sub.UsedAmount == nil || sub.TotalAmount == nil || sub.RemainingAmount == nil ||
		*sub.UsedAmount != 0.21 || *sub.TotalAmount != 20 || *sub.RemainingAmount != 19.79 || sub.Unit != "usd" {
		t.Errorf("agent pool amounts = %+v, want $0.21 used / $20 / $19.79 left", sub)
	}
	// The period's end, not now + the rounded countdown: a reset that moved
	// with every refresh would make the pace mark wander.
	if sub.ResetsAt == nil || *sub.ResetsAt != end {
		t.Errorf("subscription reset = %v, want the period end %v", sub.ResetsAt, end)
	}
	if sub.WindowMinutes == nil || *sub.WindowMinutes != 30*1440 {
		t.Errorf("subscription window = %v, want the 30-day period", sub.WindowMinutes)
	}
	if sub.WindowLabel == nil || *sub.WindowLabel != "plan" {
		t.Errorf("subscription label = %v, want plan (the key the old row used)", sub.WindowLabel)
	}

	orb := limitByKey(p, "orb")
	if orb == nil || orb.UsedPercent != 0 {
		t.Fatalf("orb: want 0%% used (750h of 750h left), got %+v", orb)
	}
	if orb.ResetsAt == nil || *orb.ResetsAt != end {
		t.Errorf("orb reset = %v, want the period end %v", orb.ResetsAt, end)
	}
	// Hours are not a unit the relay carries; the share travels alone.
	if orb.Unit != "" || orb.UsedAmount != nil || orb.TotalAmount != nil || orb.RemainingAmount != nil {
		t.Errorf("orb must carry no amounts, got %+v", orb)
	}

	if p.PlanType == nil || *p.PlanType != "Megawatt" {
		t.Errorf("plan = %v, want Megawatt", p.PlanType)
	}
	if p.Credits == nil || !p.Credits.HasCredits || p.Credits.Balance != "$8.71" {
		t.Errorf("credits = %+v, want $8.71", p.Credits)
	}

	if p.Account != "someone@example.com" {
		t.Fatalf("account = %q: the address without the org after it", p.Account)
	}
	blob := string(dump(p))
	for _, needle := range []string{"someorg", "Signed in", "**"} {
		if strings.Contains(blob, needle) {
			t.Fatalf("%q leaked into the published payload: %s", needle, blob)
		}
	}
}

// The shape in between, which CodexBar recorded: bold labels, and the plan
// row spelt "Amp <name> Subscription:".
func TestAmpBoldLabelsAndAmpSubscriptionRow(t *testing.T) {
	p := parseAmpFixture(t, `Signed in as someone@example.com (someorg)
**Amp Free:** 0% remaining today (resets daily) - https://ampcode.com/settings
**Amp Megawatt Subscription:** 68% other usage and 97% orb usage remaining - resets upon renewal in 5 days
**Individual credits:** $3.23 remaining (set up auto-reload to avoid running out) - https://ampcode.com/settings
`)
	if !p.OK {
		t.Fatalf("want ok, got error=%q detail=%q", p.Error, p.Detail)
	}
	if free := limitByKey(p, "free"); free == nil || free.UsedPercent != 100 {
		t.Errorf("free: want 100%% used, got %+v", free)
	}
	sub := limitByKey(p, "subscription")
	if sub == nil || sub.UsedPercent != 32 {
		t.Fatalf("subscription: want 32%% used, got %+v", sub)
	}
	if sub.ResetsAt == nil || *sub.ResetsAt != 1000+5*86400 {
		t.Errorf("subscription reset = %v, want %v", sub.ResetsAt, float64(1000+5*86400))
	}
	if sub.WindowMinutes != nil {
		t.Errorf("a countdown is no period; want no window, got %v", *sub.WindowMinutes)
	}
	if orb := limitByKey(p, "orb"); orb == nil || orb.UsedPercent != 3 {
		t.Errorf("orb: want 3%% used, got %+v", orb)
	}
	if p.PlanType == nil || *p.PlanType != "Megawatt" {
		t.Errorf("plan = %v, want Megawatt", p.PlanType)
	}
	if p.Credits == nil || p.Credits.Balance != "$3.23" {
		t.Errorf("credits = %+v, want $3.23", p.Credits)
	}

	// The balance-shaped free row in bold too.
	q := parseAmpFixture(t, "Signed in as x@y.z\n**Amp Free:** $6/$10 remaining (replenishes +$0.5/hour)\n")
	if free := limitByKey(q, "free"); free == nil || free.UsedPercent != 40 {
		t.Errorf("bold balance-shaped free row: want 40%% used, got %+v", free)
	}
}

// A monthly plan counts its renewal in months, which the day-only pattern
// dropped. A month is a calendar month.
func TestAmpRenewalInMonths(t *testing.T) {
	now := utcEpoch(2026, 8, 18, 12, 0)
	want := utcEpoch(2026, 9, 18, 12, 0)

	p := parseAmpAt("Signed in as x@y.z\n**Amp Example Tier:** agent usage $250 of $1,000 remaining - resets upon renewal in 1 month\n", now)
	sub := limitByKey(p, "subscription")
	if sub == nil || sub.UsedPercent != 75 {
		t.Fatalf("subscription: want 75%% used, got %+v", sub)
	}
	if sub.ResetsAt == nil || *sub.ResetsAt != want {
		t.Errorf("reset = %v, want %v", sub.ResetsAt, want)
	}
	if limitByKey(p, "orb") != nil {
		t.Error("no orb clause, no orb row")
	}
	if p.PlanType == nil || *p.PlanType != "Example" {
		t.Errorf("plan = %v, want Example", p.PlanType)
	}

	q := parseAmpAt("Signed in as x@y.z\nAmp Megawatt Subscription: 100% other usage and 100% orb usage remaining - resets upon renewal in 1 month\n", now)
	if sub := limitByKey(q, "subscription"); sub == nil || sub.ResetsAt == nil || *sub.ResetsAt != want {
		t.Errorf("percentage-shaped row in months: got %+v, want reset %v", sub, want)
	}

	// A day the target month does not have is its last day, not a roll into
	// the month after: Jan 31 plus a month is Feb 28 (29 in a leap year).
	for _, c := range []struct {
		from, want float64
		months     string
	}{
		{utcEpoch(2026, 1, 31, 12, 0), utcEpoch(2026, 2, 28, 12, 0), "1 month"},
		{utcEpoch(2028, 1, 31, 12, 0), utcEpoch(2028, 2, 29, 12, 0), "1 month"},
		{utcEpoch(2026, 3, 31, 9, 30), utcEpoch(2026, 4, 30, 9, 30), "1 month"},
		{utcEpoch(2026, 12, 31, 23, 59), utcEpoch(2027, 1, 31, 23, 59), "1 month"},
		{utcEpoch(2026, 11, 30, 0, 0), utcEpoch(2027, 2, 28, 0, 0), "3 months"},
	} {
		r := parseAmpAt("Signed in as x@y.z\nAmp Example Tier: agent usage $5 of $20 remaining - resets upon renewal in "+c.months+"\n", c.from)
		sub := limitByKey(r, "subscription")
		if sub == nil || sub.ResetsAt == nil || *sub.ResetsAt != c.want {
			got := "none"
			if sub != nil && sub.ResetsAt != nil {
				got = time.Unix(int64(*sub.ResetsAt), 0).UTC().Format(time.RFC3339)
			}
			t.Errorf("%s from %s: reset %s, want %s", c.months,
				time.Unix(int64(c.from), 0).UTC().Format(time.RFC3339), got,
				time.Unix(int64(c.want), 0).UTC().Format(time.RFC3339))
		}
	}
}

func TestAmpTierWithoutAUsablePoolOrPeriod(t *testing.T) {
	// $0 of $0 has no share to show; the credits still make a reading, and no
	// plan chip stands over a pool that is not there.
	p := parseAmpFixture(t, "Signed in as x@y.z\nAmp Megawatt Tier: agent usage $0 of $0 remaining - resets upon renewal in 27 days\nIndividual credits: $12 remaining\n")
	if !p.OK || p.Credits == nil || p.Credits.Balance != "$12.00" {
		t.Fatalf("want a credits-only reading, got ok=%v credits=%+v error=%q", p.OK, p.Credits, p.Error)
	}
	if len(p.Limits) != 0 || p.PlanType != nil {
		t.Errorf("a $0 pool must yield no row and no plan, got limits=%+v plan=%v", p.Limits, p.PlanType)
	}

	// A date the calendar does not have, a period that runs backwards, or none
	// at all: the countdown speaks, and no window length is claimed.
	for _, period := range []string{"period 2026-02-30 to 2026-03-30", "period 2026-10-13 to 2026-09-13", "period 2026-09-13 to 2026-09-13", ""} {
		q := parseAmpFixture(t, "Signed in as x@y.z\nAmp Example Tier: agent usage $18 of $20 remaining - "+period+", resets upon renewal in 27 days\n")
		sub := limitByKey(q, "subscription")
		if sub == nil || sub.UsedPercent != 10 {
			t.Fatalf("%q: want 10%% used, got %+v", period, sub)
		}
		if sub.WindowMinutes != nil {
			t.Errorf("%q: want no window, got %v", period, *sub.WindowMinutes)
		}
		if sub.ResetsAt == nil || *sub.ResetsAt != 1000+27*86400 {
			t.Errorf("%q: reset = %v, want the countdown", period, sub.ResetsAt)
		}
	}

	// A period that has already ended is not the current one.
	later := utcEpoch(2026, 11, 1, 0, 0)
	r := parseAmpAt("Signed in as x@y.z\nAmp Megawatt Tier: agent usage $18 of $20 remaining - period 2026-09-22 to 2026-10-22, resets upon renewal in 15 days\n", later)
	sub := limitByKey(r, "subscription")
	if sub == nil || sub.ResetsAt == nil || *sub.ResetsAt != later+15*86400 || sub.WindowMinutes != nil {
		t.Errorf("an ended period: want the countdown and no window, got %+v", sub)
	}

	// Neither: the pools still read, with no reset.
	s := parseAmpFixture(t, "Signed in as x@y.z\nAmp Megawatt Tier: agent usage $18 of $20 remaining\n")
	if sub := limitByKey(s, "subscription"); sub == nil || sub.ResetsAt != nil {
		t.Errorf("no renewal text: want a row with no reset, got %+v", sub)
	}
}

// Each clause of the Tier row stands alone: one changing shape costs that
// pool, never the other.
func TestAmpTierClausesStandAlone(t *testing.T) {
	for _, orb := range []string{"", "orb usage unavailable", "orb usage 0h of 0h a1.small orb hours remaining"} {
		p := parseAmpFixture(t, "Signed in as x@y.z\nAmp Example Tier: agent usage $3 of $20 remaining (15%), "+orb+" - resets upon renewal in 2 days\n")
		if sub := limitByKey(p, "subscription"); sub == nil || sub.UsedPercent != 85 {
			t.Errorf("%q: agent pool must survive, got %+v", orb, sub)
		}
		if o := limitByKey(p, "orb"); o != nil {
			t.Errorf("%q: want no orb row, got %+v", orb, o)
		}
	}

	p := parseAmpFixture(t, "Signed in as x@y.z\nAmp Example Tier: agent usage unavailable, orb usage 12h of 50h a1.large orb hours remaining - resets upon renewal in 2 days\n")
	if sub := limitByKey(p, "subscription"); sub != nil {
		t.Errorf("an agent clause in another shape must contribute nothing, got %+v", sub)
	}
	if o := limitByKey(p, "orb"); o == nil || o.UsedPercent != 76 {
		t.Errorf("the orb pool's own share must survive (12h of 50h left), got %+v", o)
	}
	if p.PlanType == nil || *p.PlanType != "Example" {
		t.Errorf("plan = %v, want Example", p.PlanType)
	}

	// Grouped hours, fractional hours.
	q := parseAmpFixture(t, "Signed in as x@y.z\nAmp Example Tier: agent usage $18.57 of $20 remaining (93%), orb usage 375.75h of 1,500h a1.small orb hours remaining (99%) - resets upon renewal in 2 days\n")
	if o := limitByKey(q, "orb"); o == nil || o.UsedPercent != 74.95 {
		t.Errorf("orb: want 74.95%% used, got %+v", o)
	}
	if sub := limitByKey(q, "subscription"); sub == nil || sub.UsedPercent != 7.15 {
		t.Errorf("agent: want 7.15%% used, got %+v", sub)
	}

	// More left than the allowance (restored credits) is nothing used, not a
	// negative bar — and the amounts say what Amp said.
	r := parseAmpFixture(t, "Signed in as x@y.z\nAmp Example Tier: agent usage $1,100 of $1,000 remaining\n")
	sub := limitByKey(r, "subscription")
	if sub == nil || sub.UsedPercent != 0 || sub.UsedAmount == nil || *sub.UsedAmount != 0 ||
		sub.RemainingAmount == nil || *sub.RemainingAmount != 1100 {
		t.Errorf("over-full pool: got %+v", sub)
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
	if free.UsedAmount == nil || free.TotalAmount == nil || free.RemainingAmount == nil ||
		*free.UsedAmount != 6 || *free.TotalAmount != 10 || *free.RemainingAmount != 4 || free.Unit != "usd" {
		t.Errorf("absolute amounts = %+v, want $6 used / $10 total / $4 left", free)
	}
	// $6 spent refills at $0.50/hour: whole again in 12 hours.
	if free.ResetsAt == nil || *free.ResetsAt != 1000+12*3600 {
		t.Errorf("reset = %v, want the balance whole again in 12h", free.ResetsAt)
	}
	// Nothing spent: nothing to wait for.
	p = parseAmpFixture(t, "Signed in as x@y.z\nAmp Free: $10.00 / $10.00 remaining (replenishes +$0.50/hour)\n")
	if free := limitByKey(p, "free"); free == nil || free.ResetsAt != nil {
		t.Errorf("a full balance has no reset: %+v", free)
	}
}

// The free tier's daily reset follows New York's clock through daylight
// saving, and a percentage row that does not say "(resets daily)" names no
// reset at all (CodexBar AmpUsageSnapshotTests).
func TestAmpFreeTierResetIsEightPMNewYork(t *testing.T) {
	cases := []struct{ now, want float64 }{
		{utcEpoch(2026, time.August, 3, 23, 59) + 59, utcEpoch(2026, time.August, 4, 0, 0)},
		{utcEpoch(2026, time.August, 4, 0, 0), utcEpoch(2026, time.August, 5, 0, 0)},           // on the boundary: the next one
		{utcEpoch(2026, time.January, 16, 0, 59) + 59, utcEpoch(2026, time.January, 16, 1, 0)}, // winter: 01:00Z
	}
	for _, c := range cases {
		p := parseAmpAt("Signed in as x@y.z\nAmp Free: 61% remaining today (resets daily) - https://ampcode.com/settings#amp-free\n", c.now)
		free := limitByKey(p, "free")
		if free == nil || free.ResetsAt == nil || *free.ResetsAt != c.want {
			t.Errorf("at %v: reset = %v, want %v", c.now, free, c.want)
		}
	}
	p := parseAmpFixture(t, "Signed in as x@y.z\nAmp Free: 61% remaining\n")
	if free := limitByKey(p, "free"); free == nil || free.UsedPercent != 39 || free.ResetsAt != nil || free.WindowMinutes != nil {
		t.Errorf("a percentage alone does not imply the daily reset: %+v", free)
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

	// Same grammar in the Tier row's dollars, hours and countdown.
	r := parseAmpFixture(t, "Signed in as someone@example.com\n"+
		"Amp Megawatt Tier: agent usage $1,2.34 of $20 remaining, orb usage 7,50h of 750h a1.small orb hours remaining - resets upon renewal in 2,3 days\n")
	if len(r.Limits) != 0 {
		t.Errorf("misgrouped dollars or hours must yield no row, got %+v", r.Limits)
	}
	s := parseAmpFixture(t, "Signed in as someone@example.com\n"+
		"Amp Megawatt Tier: agent usage $18 of $20 remaining - resets upon renewal in 2,3 days\n")
	if sub := limitByKey(s, "subscription"); sub == nil || sub.ResetsAt != nil {
		t.Errorf("an unparseable countdown must yield no reset time, got %+v", sub)
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

	// And for the Tier row: its clauses are read from its own line only.
	r := parseAmpFixture(t, "Signed in as someone@example.com\nAmp Megawatt Tier:\nagent usage $1 of $20 remaining\nperiod 2026-09-22 to 2026-10-22\n")
	if len(r.Limits) != 0 || r.PlanType != nil {
		t.Fatalf("clauses on other lines must not be read into the Tier row, got limits=%+v plan=%v", r.Limits, r.PlanType)
	}
	if r.OK || r.Error != "no-readings" {
		t.Errorf("a Tier label with nothing on its line is an unrecognised output, got ok=%v error=%q", r.OK, r.Error)
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
