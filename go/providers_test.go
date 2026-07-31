package main

// The credential-rung collectors, tested at the seam that can be wrong
// QUIETLY: the shape-to-gauge conversion. Transport is not mocked — what
// these providers' servers actually say is pinned by CodexBar's
// implementation, which this protocol was worked out from; what OUR code does
// with a payload is the part a refactor can silently break.

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestEnvFile(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".factory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ── Copilot ───────────────────────────────────────────────────────────────

func TestCopilotUsedDropsPlaceholderSnapshots(t *testing.T) {
	// GitHub returns all-zero snapshots for token-based billing and Business
	// seats — sometimes with percent_remaining=100 riding along. Both render
	// as a confident wrong number if believed.
	zero := 0.0
	hundred := 100.0
	if got := copilotUsed(&copilotSnapshot{Entitlement: zero, Remaining: zero, PercentRemaining: hundred}); got != nil {
		t.Fatalf("placeholder with percent must be dropped, got %v", *got)
	}
	if got := copilotUsed(&copilotSnapshot{Entitlement: zero, Remaining: zero}); got != nil {
		t.Fatalf("bare placeholder must be dropped, got %v", *got)
	}
}

func TestCopilotUsedReadsAndDerives(t *testing.T) {
	got := copilotUsed(&copilotSnapshot{Entitlement: 300.0, Remaining: 60.0, PercentRemaining: 20.0})
	if got == nil || *got != 80 {
		t.Fatalf("want 80, got %v", got)
	}
	// percent_remaining missing: derived from remaining/entitlement.
	got = copilotUsed(&copilotSnapshot{Entitlement: 300.0, Remaining: 60.0})
	if got == nil || *got != 80 {
		t.Fatalf("derived: want 80, got %v", got)
	}
	// Overage: remaining percent can go negative; used clamps high, not low.
	neg := -5.0
	got = copilotUsed(&copilotSnapshot{Entitlement: 300.0, Remaining: 1.0, PercentRemaining: neg})
	if got == nil || *got != 105 {
		t.Fatalf("overage: want 105, got %v", got)
	}
	if got := copilotUsed(&copilotSnapshot{Unlimited: true, PercentRemaining: 20.0}); got != nil {
		t.Fatal("unlimited seats carry no percentage")
	}
}

// ── Droid (Factory) ───────────────────────────────────────────────────────

func TestFactoryResetDisambiguatesUnits(t *testing.T) {
	t0 := 1_754_000_000.0
	// secondsRemaining wins over windowEnd.
	w := &factoryWindow{SecondsRemaining: 120.0, WindowEnd: 9_999_999_999_999.0}
	if r := factoryReset(w, t0); r == nil || *r != t0+120 {
		t.Fatalf("secondsRemaining: got %v", r)
	}
	// Epoch milliseconds are folded to seconds.
	w = &factoryWindow{WindowEnd: (t0 + 300) * 1000}
	if r := factoryReset(w, t0); r == nil || *r != t0+300 {
		t.Fatalf("ms windowEnd: got %v", r)
	}
	// ISO-8601 string.
	w = &factoryWindow{WindowEnd: "2100-01-01T00:00:00Z"}
	if r := factoryReset(w, t0); r == nil {
		t.Fatal("iso windowEnd: got nil")
	}
	// A window already over yields no reset.
	w = &factoryWindow{WindowEnd: t0 - 10}
	if r := factoryReset(w, t0); r != nil {
		t.Fatalf("past windowEnd: got %v", *r)
	}
}

func TestFactoryUsedZeroesExpiredWindows(t *testing.T) {
	t0 := 1_754_000_000.0
	// Expired five-hour window still carrying 96%: Factory's own UI treats it
	// as reset, and publishing the stale figure is the bug this pins.
	w := &factoryWindow{UsedPercent: 96.0, WindowEnd: t0 - 10}
	reset := factoryReset(w, t0)
	if got := factoryUsed(w, reset); got == nil || *got != 0 {
		t.Fatalf("expired window: want 0, got %v", got)
	}
	// Live window passes through, clamped.
	w = &factoryWindow{UsedPercent: 104.0, SecondsRemaining: 60.0}
	if got := factoryUsed(w, factoryReset(w, t0)); got == nil || *got != 100 {
		t.Fatalf("live window: want 100, got %v", got)
	}
	// No windowEnd and no secondsRemaining at all — a fresh account shape —
	// is NOT the expired case: the percentage stands on its own.
	w = &factoryWindow{UsedPercent: 12.5}
	if got := factoryUsed(w, factoryReset(w, t0)); got == nil || *got != 12.5 {
		t.Fatalf("bare percent: want 12.5, got %v", got)
	}
}

func TestFactoryKeyParsesDotenvShapes(t *testing.T) {
	// The parser tolerates export, quotes and noise, and never runs a shell.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(factoryKeyEnv, "")
	if got := factoryKey(); got != "" {
		t.Fatalf("no file: want empty, got %q", got)
	}
	writeTestEnvFile(t, dir, "# comment\nOTHER=x\nexport FACTORY_API_KEY=\"fk-abc123\"\n")
	if got := factoryKey(); got != "fk-abc123" {
		t.Fatalf("want fk-abc123, got %q", got)
	}
}

// ── Kimi ──────────────────────────────────────────────────────────────────

func TestKimiUsedHandlesStringNumbers(t *testing.T) {
	d := &kimiDetail{Limit: "2048", Used: "117"}
	if got := kimiUsed(d); got == nil || *got != 5.71 {
		t.Fatalf("want 5.71, got %v", got)
	}
	// used missing, remaining present: derived.
	d = &kimiDetail{Limit: "100", Remaining: "25"}
	if got := kimiUsed(d); got == nil || *got != 75 {
		t.Fatalf("derived: want 75, got %v", got)
	}
	// A zero limit is not a window.
	if got := kimiUsed(&kimiDetail{Limit: "0", Used: "5"}); got != nil {
		t.Fatal("zero limit must yield nothing")
	}
	// Grouped thousands would silently misparse without the amp grammar.
	d = &kimiDetail{Limit: "2,048", Used: "1,024"}
	if got := kimiUsed(d); got == nil || *got != 50 {
		t.Fatalf("grouped: want 50, got %v", got)
	}
}

func TestKimiResetAcceptsEveryShapeSeen(t *testing.T) {
	if r := kimiReset(&kimiDetail{ResetTime: "2100-01-02T03:04:05Z"}); r == nil {
		t.Fatal("iso string")
	}
	if r := kimiReset(&kimiDetail{ResetTime: 4_102_444_800_000.0}); r == nil || *r != 4_102_444_800 {
		t.Fatalf("epoch ms: got %v", r)
	}
	if r := kimiReset(&kimiDetail{ResetAt: "4102444800"}); r == nil || *r != 4_102_444_800 {
		t.Fatalf("numeric string via resetAt: got %v", r)
	}
	if r := kimiReset(&kimiDetail{}); r != nil {
		t.Fatal("no reset fields must yield nil")
	}
}

// ── shared plumbing ───────────────────────────────────────────────────────

func TestProvCacheFloorsOnAttemptsAndServesStale(t *testing.T) {
	var c provCache
	calls := 0
	okFetch := func() Provider {
		calls++
		return Provider{ID: "x", OK: true, RecordedAt: fp(now())}
	}
	failFetch := func() Provider {
		calls++
		return Provider{ID: "x", OK: false, Error: "unreachable"}
	}

	p := c.collect(okFetch)
	if !p.OK || calls != 1 {
		t.Fatalf("first collect should fetch: calls=%d", calls)
	}
	// Inside the floor: served from cache, no second call.
	if p = c.collect(failFetch); !p.OK || calls != 1 {
		t.Fatalf("inside floor: want cached success and 1 call, got ok=%v calls=%d", p.OK, calls)
	}
	// Age the success past the floor but not past staleness: a failing fetch
	// runs once, and the STALE SUCCESS is still what gets served.
	c.fetchedAt -= provMinInterval + 1
	c.attemptedAt -= provMinInterval + 1
	if p = c.collect(failFetch); !p.OK || calls != 2 {
		t.Fatalf("stale-but-real: want cached success and 2 calls, got ok=%v calls=%d", p.OK, calls)
	}
	// Past the staleness bound the error is the truth.
	c.fetchedAt -= provStaleMax + 1
	c.attemptedAt -= provMinInterval + 1
	if p = c.collect(failFetch); p.OK || calls != 3 {
		t.Fatalf("past staleness: want the failure and 3 calls, got ok=%v calls=%d", p.OK, calls)
	}
}

func TestTrimAmount(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{{8.71, "8.71"}, {5, "5"}, {5.5, "5.5"}, {0.1, "0.1"}} {
		if got := trimAmount(tc.in); got != tc.want {
			t.Errorf("trimAmount(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
