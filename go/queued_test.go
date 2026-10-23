package main

// The 2026-08 additions still shipped, tested at the parse seam. Kiro and Grok also got
// a live run against the real CLI/endpoint on the machine this was built on;
// these pin the shapes that live run cannot cover (older CLI vintages, the
// protobuf scan, the tRPC fallbacks) and that a refactor could break quietly.

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// ── Kiro ──────────────────────────────────────────────────────────────────

func TestKiroResetRollsMMDDForward(t *testing.T) {
	// A bare MM/DD in the past rolls to next year; yyyy-MM-dd is taken as-is.
	iso := kiroReset("2099-01-15")
	if iso == nil {
		t.Fatal("iso date should parse")
	}
	past := time.Now().AddDate(0, 0, -2)
	mmdd := past.Format("01/02")
	got := kiroReset(mmdd)
	if got == nil {
		t.Fatalf("MM/DD %q should parse", mmdd)
	}
	if *got <= float64(time.Now().Unix()) {
		t.Fatal("a past MM/DD must roll forward to the future")
	}
}

func TestKiroDisplayPlan(t *testing.T) {
	for in, want := range map[string]string{
		"KIRO FREE": "Kiro Free", "KIRO PRO": "Kiro Pro", "": "",
	} {
		if got := kiroDisplayPlan(in); got != want {
			t.Errorf("kiroDisplayPlan(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── Kilo ──────────────────────────────────────────────────────────────────

func TestKiloContextsAndMoney(t *testing.T) {
	// A nested payload: money is found two levels down, cents beats plain.
	pay := map[string]any{
		"subscription": map[string]any{
			"usedCents": 250.0, "total": 10.0,
		},
	}
	ctxs := kiloContexts(pay)
	var foundUsed bool
	for _, c := range ctxs {
		if v, ok := kiloMoney(c, []string{"usedCents"}, nil, []string{"used"}); ok {
			if v != 2.5 {
				t.Fatalf("cents should divide by 100: got %v", v)
			}
			foundUsed = true
		}
	}
	if !foundUsed {
		t.Fatal("nested usedCents not reached by the BFS")
	}
}

func TestKiloPayloadUnwrap(t *testing.T) {
	entry := map[string]any{
		"result": map[string]any{
			"data": map[string]any{"json": map[string]any{"x": 1.0}},
		},
	}
	pay, ok := kiloPayload(entry).(map[string]any)
	if !ok || pay["x"] != 1.0 {
		t.Fatalf("result.data.json not unwrapped: %v", kiloPayload(entry))
	}
	// A null json unwraps to nil, not to the wrapper.
	entry2 := map[string]any{"result": map[string]any{"data": map[string]any{"json": nil}}}
	if kiloPayload(entry2) != nil {
		t.Fatal("null json should unwrap to nil")
	}
}

func TestKiloTierNames(t *testing.T) {
	if got := kiloPlanName(map[string]any{"tier": "tier_49"}); got != "Pro" {
		t.Errorf("tier_49 = %q, want Pro", got)
	}
	if got := kiloPlanName(map[string]any{"planName": "Custom"}); got != "Custom" {
		t.Errorf("planName fallback = %q", got)
	}
}

// ── Grok ──────────────────────────────────────────────────────────────────

// Build one grpc-web data frame around a protobuf payload.
func grokFrame(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// field 1, wire 5 (fixed32), a float32.
func protoFixed32(field int, f float32) []byte {
	b := []byte{byte(field<<3 | 5), 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[1:], math.Float32bits(f))
	return b
}

// A length-delimited field: wire type 2, field number `field`, holding `inner`.
// The tag is a varint — field 20's is two bytes.
func protoMsg(field int, inner []byte) []byte {
	var buf [10]byte
	n := binary.PutUvarint(buf[:], uint64(field<<3|2))
	out := append([]byte{}, buf[:n]...)
	n = binary.PutUvarint(buf[:], uint64(len(inner)))
	out = append(out, buf[:n]...)
	return append(out, inner...)
}

func TestGrokParsesUsedPercentFromFrame(t *testing.T) {
	// The real wire: message 1 (the config) whose field 1 is 37.5% used.
	inner := protoFixed32(1, 37.5)
	outer := protoMsg(1, inner)
	bill, err := grokParseBilling(grokFrame(outer), 1000)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if bill.used == nil || math.Abs(*bill.used-37.5) > 0.01 || bill.implicitZero {
		t.Fatalf("used = %v, want 37.5 off the wire", bill.used)
	}
}

func TestGrokPercentIsTheShallowestFirstAndNeverASubnormal(t *testing.T) {
	// An integer field 1 = 1 reads as the subnormal 1.4e-45; it must be
	// rejected, not rounded to 0% and published over the real nested value.
	subnormal := protoFixed32(1, math.Float32frombits(1))
	nested := protoMsg(1, protoFixed32(1, 37.5))
	bill, err := grokParseBilling(grokFrame(append(subnormal, nested...)), 1000)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bill.used == nil || math.Abs(*bill.used-37.5) > 0.01 {
		t.Fatalf("subnormal should be ignored, real 37.5 kept; got %v", bill.used)
	}
	// Two field-1 floats at the same depth: the first in the walk is the
	// one (CodexBar orders by depth, then by position).
	a := protoFixed32(1, 20)
	b := protoFixed32(1, 80)
	bill, err = grokParseBilling(grokFrame(protoMsg(1, append(a, b...))), 1000)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bill.used == nil || *bill.used != 20 {
		t.Fatalf("the first float at the shallowest depth wins, got %v", bill.used)
	}
}

// A length-delimited field the billing descriptor does not declare as a
// message is opaque bytes — a string, say — and bytes inside it that happen
// to parse as a float32 tag must not become the percentage.
func TestGrokDoesNotWalkIntoOpaqueBytes(t *testing.T) {
	// [1,9] is not a declared message; a float32 field 1 inside it is a
	// coincidence of bytes, not a reading.
	opaque := protoMsg(9, protoFixed32(1, 99))
	bill, err := grokParseBilling(grokFrame(protoMsg(1, opaque)), 1000)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bill.used != nil {
		t.Fatalf("a float inside an undeclared message is not a percentage, got %v", *bill.used)
	}
	// Whereas [1,6,3,2] is declared, and is walked.
	deep := protoMsg(6, protoMsg(3, protoMsg(2, protoFixed32(1, 12))))
	bill, _ = grokParseBilling(grokFrame(protoMsg(1, deep)), 1000)
	if bill.used == nil || *bill.used != 12 {
		t.Fatalf("a declared nested message is walked, got %v", bill.used)
	}
}

func TestGrokFramingRejectsOverrunAndBadFlags(t *testing.T) {
	// A length that overruns the body is an error, not a partial success.
	bad := []byte{0x00, 0xff, 0xff, 0xff, 0xff, 0x01}
	if _, err := grokParseBilling(bad, 1000); err == nil {
		t.Fatal("overrunning frame length must error")
	}
	// A compressed/reserved flag byte we cannot read must error, not scan.
	comp := grokFrame(protoFixed32(1, 50))
	comp[0] = 0x01
	if _, err := grokParseBilling(comp, 1000); err == nil {
		t.Fatal("unsupported frame flags must error")
	}
}

func grokTrailer(text string) []byte {
	frame := make([]byte, 5+len(text))
	frame[0] = 0x80
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(text)))
	copy(frame[5:], text)
	return frame
}

func TestGrokTrailerStatusIsClassified(t *testing.T) {
	// A trailer frame (flag 0x80) carrying grpc-status 7 with a credential
	// message is an authentication failure; 16 is one whatever it says
	// (that is the WKE "no-credentials" rejection); 9 "no personal team" is
	// a team account the consumer surface has nothing for; anything else is
	// a failure of its own kind. The message is percent-encoded on the wire.
	cases := []struct {
		trailer string
		auth    bool
		team    bool
	}{
		{"grpc-status:7\r\ngrpc-message:bad-credentials\r\n", true, false},
		{"grpc-status:7\r\ngrpc-message:oauth2%20token%20could%20not%20be%20validated\r\n", true, false},
		{"grpc-status:16\r\ngrpc-message:no-credentials\r\n", true, false},
		{"grpc-status:16\r\n", true, false},
		{"grpc-status:9\r\ngrpc-message:No personal team.\r\n", false, true},
		{"grpc-status:7\r\ngrpc-message:permission denied\r\n", false, false},
		{"grpc-status:13\r\ngrpc-message:Missing request message.\r\n", false, false},
	}
	for _, c := range cases {
		_, err := grokParseBilling(grokTrailer(c.trailer), 1000)
		if err == nil {
			t.Fatalf("%q: non-zero grpc-status must be an error", c.trailer)
		}
		rpc, ok := err.(*grokRPCError)
		if !ok {
			t.Fatalf("%q: want a classified rpc error, got %v", c.trailer, err)
		}
		if rpc.authFailure() != c.auth || rpc.teamUnsupported() != c.team {
			t.Errorf("%q: auth=%v team=%v", c.trailer, rpc.authFailure(), rpc.teamUnsupported())
		}
	}
	// grpc-status 0 in a trailer is the happy path.
	body := append(grokFrame(protoMsg(1, protoFixed32(1, 5))), grokTrailer("grpc-status:0\r\n")...)
	if bill, err := grokParseBilling(body, 1000); err != nil || bill.used == nil || *bill.used != 5 {
		t.Fatalf("status 0 trailer: %v %+v", err, bill)
	}
}

func TestGrokResetPrefersTheDeclaredFieldThenTheSoonest(t *testing.T) {
	now := float64(1_800_000_000)
	soon := uint64(now) + 3600
	later := uint64(now) + 48*3600
	past := uint64(1_700_000_100)
	// [1,5,1] is the reset the descriptor declares: it wins over a sooner
	// date elsewhere in the message.
	payload := protoMsg(1, append(append(protoVarint(6, past), protoVarint(6, soon)...),
		append(protoMsg(5, protoVarint(1, later)), protoFixed32(1, 10)...)...))
	bill, err := grokParseBilling(grokFrame(payload), now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bill.resets == nil || uint64(*bill.resets) != later {
		t.Fatalf("reset = %v, want the declared [1,5,1] %d", bill.resets, later)
	}
	// Without it, the soonest date still ahead.
	payload = protoMsg(1, append(append(protoVarint(6, past), protoVarint(6, later)...),
		append(protoVarint(6, soon), protoFixed32(1, 10)...)...))
	bill, _ = grokParseBilling(grokFrame(payload), now)
	if bill.resets == nil || uint64(*bill.resets) != soon {
		t.Fatalf("reset = %v, want the soonest %d", bill.resets, soon)
	}
}

// proto3 omits a zero scalar: a message with a billing period but no
// percentage at all is 0% used — accepted only when the current period
// ([1,8]: type weekly/monthly, start, end) spans now, from one complete
// payload (CodexBar@e7d0011a83).
func TestGrokImplicitZeroNeedsAnActivePeriod(t *testing.T) {
	now := float64(1_800_000_000)
	start := uint64(now) - 86400
	end := uint64(now) + 6*86400
	period := protoMsg(8, append(append(protoVarint(1, 1), protoMsg(2, protoVarint(1, start))...),
		protoMsg(3, protoVarint(1, end))...))
	bill, err := grokParseBilling(grokFrame(protoMsg(1, period)), now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bill.used == nil || *bill.used != 0 || !bill.implicitZero {
		t.Fatalf("an active period with no usage scalar is 0%% used: %+v", bill)
	}
	if bill.resets == nil || uint64(*bill.resets) != end {
		t.Errorf("the period's end is the reset: %v", bill.resets)
	}
	// A period already over is not the current one: nothing is published.
	stale := protoMsg(8, append(append(protoVarint(1, 1), protoMsg(2, protoVarint(1, start-14*86400))...),
		protoMsg(3, protoVarint(1, uint64(now)-7*86400))...))
	// …but a future date somewhere keeps the message in the no-usage shape.
	stale = append(stale, protoVarint(12, end)...)
	bill, _ = grokParseBilling(grokFrame(protoMsg(1, stale)), now)
	if bill.used != nil {
		t.Fatalf("a lapsed period must not read as 0%%: %+v", bill)
	}
	// Two payloads: the zero cannot be attributed, so nothing.
	two := append(grokFrame(protoMsg(1, period)), grokFrame(protoMsg(1, period))...)
	if bill, _ := grokParseBilling(two, now); bill.used != nil {
		t.Fatalf("two payloads: %+v", bill)
	}
}

// The reset coupons: repeated field 10, each {10: id, 20: granted, 30:
// expires}; only the count and the expiries leave, soonest first.
func TestGrokResetCoupons(t *testing.T) {
	now := float64(1_800_000_000)
	stamp := func(sec uint64) []byte { return protoVarint(1, sec) }
	coupon := func(id string, exp uint64) []byte {
		return protoMsg(10, append(append(protoMsg(10, []byte(id)), protoMsg(20, stamp(uint64(now)-86400))...),
			protoMsg(30, stamp(exp))...))
	}
	body := grokFrame(append(append(coupon("tok-b", uint64(now)+20*86400), coupon("tok-a", uint64(now)+5*86400)...),
		coupon("tok-old", uint64(now)-60)...))
	rc := grokParseResets(body, now)
	if rc == nil || rc.Left != 2 || len(rc.Expires) != 2 || rc.Expires[0] != now+5*86400 || rc.Expires[1] != now+20*86400 {
		t.Fatalf("coupons: %+v", rc)
	}
	// An empty list is no coupons, not a failure to say so; a record with
	// no id or no expiry is a wire this build does not know.
	if rc := grokParseResets(grokFrame(nil), now); rc != nil {
		t.Errorf("empty: %+v", rc)
	}
	broken := grokFrame(protoMsg(10, protoMsg(30, stamp(uint64(now)+86400))))
	if rc := grokParseResets(broken, now); rc != nil {
		t.Errorf("a record without an id must drop the lot: %+v", rc)
	}
}

func TestGrokTokenNormalization(t *testing.T) {
	cases := map[string]string{
		"  abc.def  ":            "abc.def",
		"Bearer abc.def":         "abc.def",
		"bearer   abc.def":       "abc.def",
		"cookie: sso=abc":        "",
		"sso=abc; sso-rw=abc":    "",
		"xai-management-api-key": "",
		"":                       "",
	}
	for in, want := range cases {
		if got := grokNormalizeToken(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestGrokRequestBodyNamesTheField(t *testing.T) {
	// One frame of two bytes: field 1 varint 0 — exclude_legacy_monthly_usage
	// = false. An empty frame is refused with status 13.
	if !bytes.Equal(grokBillingBody, []byte{0, 0, 0, 0, 2, 0x08, 0x00}) {
		t.Fatalf("body: % x", grokBillingBody)
	}
}

func protoVarint(field int, v uint64) []byte {
	out := []byte{byte(field<<3 | 0)}
	var buf [10]byte
	n := binary.PutUvarint(buf[:], v)
	return append(out, buf[:n]...)
}

func TestKiloFallbackTriesCentsAndMicro(t *testing.T) {
	// The production fallback must actually reach the cents family — the bug
	// was passing nil for it, so only plain keys were ever consulted.
	ctx := map[string]any{"remainingCents": 500.0}
	if v, ok := kiloMoney(ctx,
		[]string{"remainingCents"}, []string{"remaining_mUsd"}, []string{"remaining"}); !ok || v != 5 {
		t.Fatalf("cents family: want 5, got %v ok=%v", v, ok)
	}
	ctx = map[string]any{"balance_mUsd": 2_500_000.0}
	if v, ok := kiloMoney(ctx, []string{"balanceCents"}, []string{"balance_mUsd"}, nil); !ok || v != 2.5 {
		t.Fatalf("micro family: want 2.5, got %v ok=%v", v, ok)
	}
}

// The credits window is the calendar month ending on the reset date
// (CodexBar KiroStatusProbe / KiroProviderDescriptor, steipete/CodexBar@3386b0ad),
// as long as that month really is: 31 days to Sep 1, 28 to Mar 31 of a
// common year (Feb 31 is Feb 28), and none at all without a reset.
func TestKiroWindowIsTheCalendarMonth(t *testing.T) {
	days := func(from, to string) float64 {
		a, _ := time.ParseInLocation("2006-01-02", from, time.Local)
		b, _ := time.ParseInLocation("2006-01-02", to, time.Local)
		return b.Sub(a).Minutes()
	}
	for _, c := range []struct{ reset, from string }{
		{"2026-09-01", "2026-08-01"},
		{"2026-03-31", "2026-02-28"},
		{"2026-07-01", "2026-06-01"},
		{"2026-01-15", "2025-12-15"},
	} {
		got := kiroMonthWindow(kiroReset(c.reset))
		if got == nil || *got != days(c.from, c.reset) {
			t.Errorf("%s: want the month from %s (%v min), got %v", c.reset, c.from, days(c.from, c.reset), got)
		}
	}
	if kiroMonthWindow(nil) != nil {
		t.Error("no reset, no window")
	}
	p := parseKiroUsage(Provider{ID: "kiro", Source: "cli"}, "Estimated Usage | resets on 2026-09-01 | KIRO FREE\nCredits (0.00 of 50 covered in plan)\n", false)
	if len(p.Limits) != 1 || p.Limits[0].WindowMinutes == nil || *p.Limits[0].WindowMinutes != days("2026-08-01", "2026-09-01") ||
		p.Limits[0].WindowLabel == nil || *p.Limits[0].WindowLabel != "plan" {
		t.Errorf("the credits window carries its month: %+v", p.Limits)
	}
	p = parseKiroUsage(Provider{ID: "kiro", Source: "cli"}, "Plan: KIRO PRO MAX | 1 usage breakdowns\nCredits (0 of 5000 covered in plan)\n", false)
	if len(p.Limits) != 1 || p.Limits[0].WindowMinutes != nil {
		t.Errorf("no reset date, no window length: %+v", p.Limits)
	}
}

func TestKiroMMDDRejectsImpossibleDates(t *testing.T) {
	if kiroReset("02/31") != nil {
		t.Fatal("02/31 normalises to March under time.Date and must be refused")
	}
	if kiroReset("13/01") != nil {
		t.Fatal("month 13 must be refused")
	}
}

// The newer CLI opens with a summary line; a managed plan may print nothing
// else. The plan is the reading then — not a 0% gauge, not a failure
// (CodexBar KiroStatusProbe, steipete/CodexBar@28b8dcbe8d).
func TestKiroSummaryLineIsAPlanNotAGauge(t *testing.T) {
	parse := func(out string) Provider {
		return parseKiroUsage(Provider{ID: "kiro", Name: "Kiro", Source: "cli", CapturedAt: 1000}, out, false)
	}
	p := parse("\x1b[32mPlan: KIRO PRO MAX | 1 usage breakdowns\x1b[0m\n")
	if !p.OK || len(p.Limits) != 0 || p.Credits != nil {
		t.Fatalf("summary alone: %+v", p)
	}
	if p.PlanType == nil || *p.PlanType != "Kiro Pro Max" {
		t.Errorf("plan stops at the pipe and is title-cased: %v", p.PlanType)
	}
	// With a credits line under it, the gauge is read as before.
	p = parse("Plan: KIRO PRO MAX | 1 usage breakdowns\nCredits (0 of 5000 covered in plan)\n")
	if !p.OK || len(p.Limits) != 1 || p.Limits[0].UsedPercent != 0 || p.Credits == nil || p.Credits.Balance != "5000 / 5000" {
		t.Fatalf("summary with credits: %+v", p)
	}
	// "Credits used" under "Overages" is overage spend, not plan usage: a
	// summary with only that still reads as plan-only; without a summary it
	// is no reading at all.
	p = parse("Plan: KIRO PRO MAX | 1 usage breakdowns\nOverages: Enabled  billed at $0.04 per request\nCredits used: 40.29\nEst. cost: $1.61 USD\n")
	if !p.OK || len(p.Limits) != 0 || p.Credits != nil {
		t.Fatalf("overage spend must not become the plan's usage: %+v", p)
	}
	p = parse("Overages: Enabled\nCredits used: 40.29\n")
	if p.OK || p.DetailCode != "cli-nolines" {
		t.Fatalf("overage spend alone is no reading: %+v", p)
	}
	// The older "Plan: Q Developer Pro" shape still names the plan, and the
	// classic header still wins over both.
	p = parse("Plan: Q Developer Pro\n████████ 40%\n")
	if !p.OK || p.PlanType == nil || *p.PlanType != "Q Developer Pro" || len(p.Limits) != 1 {
		t.Fatalf("plain plan line: %+v", p)
	}
	p = parse("Estimated Usage | resets on 2026-09-01 | KIRO FREE\nPlan: KIRO PRO MAX | 1 usage breakdowns\nCredits (0.00 of 50 covered in plan)\n")
	if p.PlanType == nil || *p.PlanType != "Kiro Free" {
		t.Fatalf("the header's plan wins: %+v", p)
	}
}
