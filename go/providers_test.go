package main

// The credential-rung collectors, tested at the seam that can be wrong
// QUIETLY: the shape-to-gauge conversion. Transport is not mocked — what
// these providers' servers actually say is pinned by CodexBar's
// implementation, which this protocol was worked out from; what OUR code does
// with a payload is the part a refactor can silently break.

import "testing"

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

func TestCopilotAmountsKeepTheAbsolutePair(t *testing.T) {
	used, total, remaining := copilotAmounts(&copilotSnapshot{
		Entitlement: 300.0, Remaining: 60.0, PercentRemaining: 20.0,
	})
	if used == nil || total == nil || remaining == nil ||
		*used != 240 || *total != 300 || *remaining != 60 {
		t.Fatalf("amounts = %v / %v / %v, want 240 / 300 / 60", used, total, remaining)
	}
	if u, n, r := copilotAmounts(&copilotSnapshot{Entitlement: 0.0, Remaining: 0.0}); u != nil || n != nil || r != nil {
		t.Fatalf("placeholder must carry no amounts, got %v / %v / %v", u, n, r)
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

func TestKimiAmountsDeriveOnlyTheMissingSide(t *testing.T) {
	used, total, remaining := kimiAmounts(&kimiDetail{Limit: "2048", Used: "117"})
	if used == nil || total == nil || remaining == nil ||
		*used != 117 || *total != 2048 || *remaining != 1931 {
		t.Fatalf("from used = %v / %v / %v", used, total, remaining)
	}
	used, total, remaining = kimiAmounts(&kimiDetail{Limit: "100", Remaining: "25"})
	if used == nil || total == nil || remaining == nil ||
		*used != 75 || *total != 100 || *remaining != 25 {
		t.Fatalf("from remaining = %v / %v / %v", used, total, remaining)
	}
	used, total, remaining = kimiAmounts(&kimiDetail{Limit: "100", Used: "-5"})
	if used == nil || remaining == nil || *used != 0 || *remaining != 100 {
		t.Fatalf("negative used must clamp before deriving remaining, got %v / %v / %v", used, total, remaining)
	}
	used, total, remaining = kimiAmounts(&kimiDetail{Limit: "100", Used: "125"})
	if used == nil || remaining == nil || *used != 125 || *remaining != 0 {
		t.Fatalf("overage must not produce negative remaining, got %v / %v / %v", used, total, remaining)
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

// GetUsages with Code pools reads them; a lapsed plan's answer has none —
// `{"totalQuota":…}` and no `usages`, the fixture of CodexBar's
// KimiLapsedSubscriptionTests (steipete/CodexBar@5a84dca9ba) — which is no
// windows rather than a broken answer, so the membership still gets asked.
func TestKimiCodingLimitsToleratesALapsedPlan(t *testing.T) {
	got, ok := kimiCodingLimits([]byte(`{"usages":[{"scope":"FEATURE_CODING",
	  "detail":{"limit":"2048","used":"117","resetTime":"2026-10-27T00:00:00Z"},
	  "limits":[{"detail":{"limit":"200","used":"50"},"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"}}]}]}`))
	if !ok || len(got) != 2 || got[0].Key != "weekly" || got[0].UsedPercent != 5.71 ||
		got[1].Key != "rate" || got[1].UsedPercent != 25 || got[1].WindowMinutes == nil || *got[1].WindowMinutes != 300 {
		t.Fatalf("active plan: ok=%v %+v", ok, got)
	}
	for _, body := range []string{
		`{"totalQuota":{"limit":"100","used":"36","remaining":"64"}}`,
		`{"usages":null}`,
		`{"usages":[{"scope":"FEATURE_OMNI","detail":{"limit":"100","used":"1"}}]}`,
	} {
		if got, ok := kimiCodingLimits([]byte(body)); !ok || len(got) != 0 {
			t.Errorf("%s: ok=%v %+v", body, ok, got)
		}
	}
	if _, ok := kimiCodingLimits([]byte(`{"usages":{"scope":"FEATURE_CODING"}}`)); ok {
		t.Error("an answer not in the gateway's shape is a broken answer")
	}
}

// The membership's windows: the subscription balance as the month's Total
// usage, renewing at its expireTime (CodexBar's lapsed fixture: 0 used, the
// 22nd of next month), and nothing out of an empty answer — a lapsed plan
// with neither still fails, as CodexBar keeps its scope error.
func TestKimiMembershipKeepsALapsedPlanReading(t *testing.T) {
	stats := []byte(`{"subscriptionBalance":{"feature":"FEATURE_OMNI","type":"SUBSCRIPTION","unit":"UNIT_CREDIT",
	  "amountUsedRatio":0,"expireTime":"2026-11-22T00:00:00Z"}}`)
	got := kimiWithMembership(nil, stats)
	if len(got) != 1 || got[0].Key != "monthly" || got[0].UsedPercent != 0 ||
		got[0].WindowLabel == nil || *got[0].WindowLabel != "month" ||
		got[0].ResetsAt == nil || *got[0].ResetsAt != isoToEpoch("2026-11-22T00:00:00Z") {
		t.Fatalf("lapsed: %+v", got)
	}
	got = kimiWithMembership(nil, []byte(`{"subscriptionBalance":{"feature":"FEATURE_OMNI","type":"SUBSCRIPTION","amountUsedRatio":1.4}}`))
	if len(got) != 1 || got[0].UsedPercent != 100 {
		t.Errorf("an overdrawn pool reads full: %+v", got)
	}
	for _, body := range []string{
		`{}`,
		`not json`,
		// A gift expires rather than renews: not the month's window.
		`{"subscriptionBalance":{"feature":"FEATURE_OMNI","type":"GIFT","amountUsedRatio":0.5}}`,
		`{"subscriptionBalance":{"feature":"FEATURE_CODING","type":"SUBSCRIPTION","amountUsedRatio":0.5}}`,
		`{"subscriptionBalance":{"feature":"FEATURE_OMNI","type":"SUBSCRIPTION"}}`,
		`{"ratelimitCode7d":{"ratio":0.5,"enabled":false}}`,
	} {
		if got := kimiWithMembership(nil, []byte(body)); len(got) != 0 {
			t.Errorf("%s: %+v", body, got)
		}
	}
}

// The Code 7-day ratio is the weekly quota through a second endpoint: the
// weekly reading where FEATURE_CODING has none, nothing more where the two
// agree, and a row of its own, scoped Code, where they do not — the detail
// kept whole, its amounts and reset with it.
func TestKimiMembershipCode7dJoinsTheWeekOnlyWhenItAgrees(t *testing.T) {
	reset := isoToEpoch("2026-10-30T00:00:00Z")
	code7d := func(ratio, at string) []byte {
		return []byte(`{"ratelimitCode7d":{"ratio":` + ratio + `,"enabled":true,"resetTime":"` + at + `"}}`)
	}
	got := kimiWithMembership(nil, code7d("0.25", "2026-10-30T00:00:00Z"))
	if len(got) != 1 || got[0].Key != "weekly" || got[0].UsedPercent != 25 || got[0].Scope != nil ||
		got[0].WindowLabel == nil || *got[0].WindowLabel != "7d" || got[0].ResetsAt == nil {
		t.Fatalf("no detail: %+v", got)
	}
	detail := func(resets *float64) []Limit {
		l := Limit{Key: "weekly", UsedPercent: 40, WindowLabel: sp("7d"), ResetsAt: resets}
		l.UsedAmount, l.TotalAmount, l.RemainingAmount = fp(40), fp(100), fp(60)
		return []Limit{l, {Key: "rate", UsedPercent: 10}}
	}
	got = kimiWithMembership(detail(fp(reset)), code7d("0.405", "2026-10-30T00:01:00Z"))
	if len(got) != 2 || got[0].UsedPercent != 40 || got[0].UsedAmount == nil {
		t.Errorf("an agreeing ratio adds nothing and leaves the detail as it was: %+v", got)
	}
	got = kimiWithMembership(detail(fp(reset)), code7d("0.5", "2026-11-03T00:00:00Z"))
	if len(got) != 3 || got[0].Key != "weekly" || got[0].UsedPercent != 40 || got[0].UsedAmount == nil ||
		got[0].ResetsAt == nil || *got[0].ResetsAt != reset {
		t.Fatalf("a disagreeing ratio leaves the detail whole: %+v", got)
	}
	if c := got[2]; c.Key != "code_weekly" || c.UsedPercent != 50 || c.Scope == nil || *c.Scope != "Code" ||
		c.ResetsAt == nil || *c.ResetsAt != isoToEpoch("2026-11-03T00:00:00Z") || c.UsedAmount != nil {
		t.Errorf("and stands as its own row: %+v", c)
	}
	if got[1].Key != "rate" || got[1].UsedPercent != 10 {
		t.Errorf("the rate window is untouched: %+v", got[1])
	}
	got = kimiWithMembership(detail(nil), code7d("0.4", "2026-10-30T00:00:00Z"))
	if len(got) != 3 || got[0].ResetsAt != nil || got[2].Key != "code_weekly" {
		t.Errorf("without the detail's reset the two cannot be shown to be one week: %+v", got)
	}
}

// ── shared plumbing ───────────────────────────────────────────────────────

func TestProvCacheFloorsOnAttemptsAndServesStale(t *testing.T) {
	// The floors are a reader's: the daemon asks only when pressed (pace.go).
	paceReader(t)
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
	// Age the success past the floor (pace.go's, since 2026-09-12) but not
	// past staleness: a failing fetch runs once, and the STALE SUCCESS is
	// still what gets served.
	c.fetchedAt -= quotaFloor + 1
	c.attemptedAt -= quotaFloor + 1
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

// A placeholder snapshot beside the month's figures: `monthly_quotas` is
// the allowance, `limited_user_quotas` what is left, keyed completions
// (premium) and chat (CodexBar CopilotUsageModels.monthlyQuotaFallback).
func TestCopilotMonthlyQuotasStandInForThePlaceholder(t *testing.T) {
	monthly := map[string]any{"completions": 300.0, "chat": 1000.0}
	limited := map[string]any{"completions": 75.0, "chat": "900"}
	m := copilotMonthly(monthly, limited, "premium_interactions")
	if m == nil || m.UsedPercent != 75 || *m.UsedAmount != 225 || *m.TotalAmount != 300 || *m.RemainingAmount != 75 {
		t.Fatalf("premium from the month: %+v", m)
	}
	if m := copilotMonthly(monthly, limited, "chat"); m == nil || m.UsedPercent != 10 {
		t.Fatalf("chat from the month: %+v", m)
	}
	// No allowance, a remainder above it, or a key with no month: nothing.
	if m := copilotMonthly(map[string]any{"completions": 0.0}, limited, "premium_interactions"); m != nil {
		t.Errorf("zero allowance: %+v", m)
	}
	if m := copilotMonthly(monthly, map[string]any{"completions": 301.0}, "premium_interactions"); m != nil {
		t.Errorf("remainder above the allowance: %+v", m)
	}
	if m := copilotMonthly(monthly, limited, "other"); m != nil {
		t.Errorf("unknown key: %+v", m)
	}
	if m := copilotMonthly(nil, limited, "chat"); m != nil {
		t.Errorf("no month: %+v", m)
	}
}

// ── Kimi ──────────────────────────────────────────────────────────────────

// The rate window's length: a {duration, timeUnit} pair (the gateway's
// shape), a bare number (an older one), or the five-hour default.
func TestKimiWindowMinutes(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{map[string]any{"duration": 300.0, "timeUnit": "TIME_UNIT_MINUTE"}, 300},
		{map[string]any{"duration": "5", "timeUnit": "TIME_UNIT_HOUR"}, 300},
		{map[string]any{"duration": 1.0, "timeUnit": "TIME_UNIT_DAY"}, 1440},
		{map[string]any{"duration": 7.0, "timeUnit": "TIME_UNIT_FORTNIGHT"}, 300},
		{map[string]any{}, 300},
		{300.0, 300},
		{"120", 120},
		{nil, 300},
	}
	for _, c := range cases {
		if got := kimiWindowMinutes(c.in); got != c.want {
			t.Errorf("%v: got %v want %v", c.in, got, c.want)
		}
	}
}

// The plan is the active subscription's goods title; an inactive one, or
// a balance's TYPE ("SUBSCRIPTION", which a chip once showed), is not.
func TestKimiPlanFromSubscription(t *testing.T) {
	if got := kimiPlan([]byte(`{"subscription":{"active":true,"status":"SUBSCRIPTION_STATUS_ACTIVE","goods":{"title":"Allegro"}}}`)); got != "Allegro" {
		t.Errorf("active: %q", got)
	}
	for _, body := range []string{
		`{"subscription":{"active":false,"status":"SUBSCRIPTION_STATUS_ACTIVE","goods":{"title":"Allegro"}}}`,
		`{"subscription":{"active":true,"status":"SUBSCRIPTION_STATUS_EXPIRED","goods":{"title":"Allegro"}}}`,
		`{"subscription":{"active":true,"status":"SUBSCRIPTION_STATUS_ACTIVE"}}`,
		`{"subscriptionBalance":{"type":"SUBSCRIPTION"}}`,
		`not json`,
	} {
		if got := kimiPlan([]byte(body)); got != "" {
			t.Errorf("%s: %q", body, got)
		}
	}
}
