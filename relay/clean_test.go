package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A reading shaped like the one the helper actually pushes, so the tests are
// about the gate rather than about a fixture nobody ships.
const goodPush = `{
  "ok": true, "captured_at": 1785000000,
  "agent_id": "abc123XY", "agent_name": "build box", "helper_version": "2026.08.01.2",
  "exec": true, "upd": false,
  "providers": [
    {"id":"codex","name":"Codex","ok":true,"source":"local-log","captured_at":1785000000,
     "plan_type":"pro","limits":[{"key":"primary","used_percent":42.5,"window_label":"5h",
     "window_minutes":300,"resets_at":1785003600,"severity":"normal","scope":"session","active":true}]},
    {"id":"claude","name":"Claude Code","ok":false,"source":"api","error":"token-expired",
     "detail":"the stored credential no longer verifies","captured_at":1785000000}
  ],
  "system": {"ok":true,"platform":"linux","arch":"arm64","os_version":"6.8","cpu_count":4,
    "cpu_percent":12.5,"load1":0.4,"mem_total":8589934592,"mem_used_percent":61.2,
    "disk_total":107374182400,"disk_used":42949672960,"disk_used_percent":40,
    "uptime_sec":86400,"missing":["swap"]}
}`

func TestCleanReadingAcceptsAHelperPush(t *testing.T) {
	id, r, ok := cleanReading([]byte(goodPush))
	if !ok {
		t.Fatal("a well-formed push was refused")
	}
	if id != "abc123XY" {
		t.Fatalf("agent id = %q", id)
	}
	if !r.OK || len(r.Providers) != 2 {
		t.Fatalf("ok=%v providers=%d", r.OK, len(r.Providers))
	}
	if !r.Exec || r.Upd {
		t.Fatalf("exec=%v upd=%v", r.Exec, r.Upd)
	}
	if r.AgentName != "build box" || r.HelperVersion != "2026.08.01.2" {
		t.Fatalf("name=%q version=%q", r.AgentName, r.HelperVersion)
	}
	if r.System == nil || r.System.Platform != "linux" || r.System.Arch != "arm64" {
		t.Fatalf("system = %+v", r.System)
	}
	if len(r.System.Missing) != 1 || r.System.Missing[0] != "swap" {
		t.Fatalf("missing = %v", r.System.Missing)
	}
	if r.Providers[1].OK || r.Providers[1].Error != "token-expired" {
		t.Fatalf("failed provider not preserved: %+v", r.Providers[1])
	}
}

// A failed provider now says why twice: a sentence and a code a dashboard with
// translations can look up. The code is a lookup key, so it is gated on SHAPE —
// while the argument spliced into it is free text out of a vendor's error
// string, and gets the same scrubbing and cap as any other display string.
func TestCleanProviderGatesTheDetailCode(t *testing.T) {
	fail := func(extra string) *provider {
		return cleanProvider([]byte(`{"id":"kilo","name":"Kilo","ok":false,` +
			`"error":"api-error","detail":"the endpoint answered 500"` + extra + `}`))
	}
	p := fail(`,"detail_code":"http-status","detail_arg":"500"`)
	if p == nil || p.DetailCode != "http-status" || p.DetailArg != "500" {
		t.Fatalf("a well-formed code was dropped: %+v", p)
	}
	if p.Detail != "the endpoint answered 500" {
		t.Fatalf("the sentence must survive alongside the code: %q", p.Detail)
	}
	for _, bad := range []string{"Http-Status", "http status", "http_status", "1status",
		strings.Repeat("a", 33), ""} {
		if got := fail(`,"detail_code":` + quote(bad)); got.DetailCode != "" {
			t.Errorf("detail_code %q passed the shape gate as %q", bad, got.DetailCode)
		}
	}
	long := fail(`,"detail_code":"ls-no-quota","detail_arg":` + quote(strings.Repeat("x", 400)))
	if n := len([]rune(long.DetailArg)); n != detailArgMax {
		t.Errorf("argument kept %d runes, cap is %d", n, detailArgMax)
	}
	if got := fail(`,"detail_code":"ls-no-quota","detail_arg":"dial‮tcp"`); strings.ContainsRune(got.DetailArg, 0x202e) {
		t.Errorf("a bidi override rode into the argument: %q", got.DetailArg)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestCleanReadingRefusesFramesWithNothingToShow(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", `{`},
		{"no providers", `{"ok":true,"providers":[]}`},
		{"provider with no limits and no credits",
			`{"providers":[{"id":"codex","ok":true}]}`},
		{"failed provider with no error slug",
			`{"providers":[{"id":"codex","ok":false}]}`},
		{"provider with no id",
			`{"providers":[{"ok":true,"limits":[{"used_percent":1}]}]}`},
	} {
		if _, _, ok := cleanReading([]byte(tc.body)); ok {
			t.Errorf("%s: admitted", tc.name)
		}
	}
}

func TestUnknownKeysDoNotSurvive(t *testing.T) {
	body := `{"providers":[{"id":"codex","ok":true,"limits":[{"used_percent":10}],
	  "surprise":"<script>"}],"surprise":"<script>","system":{"platform":"linux","evil":1}}`
	_, r, ok := cleanReading([]byte(body))
	if !ok {
		t.Fatal("refused")
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "surprise") || strings.Contains(string(out), "evil") {
		t.Fatalf("an unknown key survived the rebuild: %s", out)
	}
}

func TestAgentIDShape(t *testing.T) {
	for _, bad := range []string{"", "abc", strings.Repeat("a", 33), "has space", "semi;colon", "../../etc"} {
		if cleanAgentID(bad) != "" {
			t.Errorf("%q accepted as an agent id", bad)
		}
	}
	for _, good := range []string{"abcd", "a-b_C9", strings.Repeat("z", 32)} {
		if cleanAgentID(good) != good {
			t.Errorf("%q refused", good)
		}
	}
	// A helper too old to send one shares the legacy slot rather than being
	// dropped, and the slot is not a shape any real helper can claim.
	_, _, ok := cleanReading([]byte(`{"providers":[{"id":"codex","ok":true,"limits":[{"used_percent":1}]}]}`))
	if !ok {
		t.Fatal("a helper with no agent_id was refused")
	}
	if cleanAgentID(legacyAgent) != "" {
		t.Fatal("the legacy slot is spoofable by a real agent id")
	}
}

func TestDisplayTextStripsWhatMustNotRender(t *testing.T) {
	// Bidi override, zero width, and a control character walk in.
	got := displayText("a‮b‎cd", 24)
	if got != "abcd" {
		t.Fatalf("got %q", got)
	}
	// Cut by runes, not bytes: a name in any script gets the same 24.
	long := strings.Repeat("机", 40)
	if n := len([]rune(displayText(long, 24))); n != 24 {
		t.Fatalf("kept %d runes, want 24", n)
	}
}

func TestNumbersAreClampedAndUnitMistakesDropped(t *testing.T) {
	body := `{"captured_at":1785000000000,
	  "providers":[{"id":"codex","ok":true,"limits":[
	    {"key":"a","used_percent":-5},{"key":"b","used_percent":4000},
	    {"key":"c","used_percent":240,"resets_at":1785000000000}]}],
	  "system":{"platform":"linux","cpu_percent":900,"mem_used_percent":-1,"uptime_sec":-4}}`
	_, r, ok := cleanReading([]byte(body))
	if !ok {
		t.Fatal("refused")
	}
	// Milliseconds where seconds belong: absent beats a countdown of decades.
	if r.CapturedAt != 0 {
		t.Errorf("captured_at in ms survived: %v", r.CapturedAt)
	}
	l := r.Providers[0].Limits
	if l[0].UsedPercent != 0 || l[1].UsedPercent != 1000 {
		t.Errorf("percent clamp: %v %v", l[0].UsedPercent, l[1].UsedPercent)
	}
	// Real overage is kept — it is the number that explains the refusals.
	if l[2].UsedPercent != 240 {
		t.Errorf("overage flattened to %v", l[2].UsedPercent)
	}
	if l[2].ResetsAt != nil {
		t.Errorf("resets_at in ms survived: %v", *l[2].ResetsAt)
	}
	s := r.System
	if s.CPUPct == nil || *s.CPUPct != 100 {
		t.Errorf("cpu clamp: %v", s.CPUPct)
	}
	if s.MemPct != nil || s.UptimeSec != nil {
		t.Errorf("negatives survived: mem=%v uptime=%v", s.MemPct, s.UptimeSec)
	}
}

func TestSystemNeedsAKnownPlatform(t *testing.T) {
	body := `{"providers":[{"id":"codex","ok":true,"limits":[{"used_percent":1}]}],
	  "system":{"platform":"haiku","arch":"weird","cpu_percent":50}}`
	_, r, _ := cleanReading([]byte(body))
	if r.System != nil {
		t.Fatalf("an unknown platform produced a system block: %+v", r.System)
	}
	body = `{"providers":[{"id":"codex","ok":true,"limits":[{"used_percent":1}]}],
	  "system":{"platform":"windows","arch":"weird"}}`
	_, r, _ = cleanReading([]byte(body))
	if r.System == nil || r.System.Arch != "" {
		t.Fatalf("arch not filtered: %+v", r.System)
	}
}

func TestBalanceMustLookLikeANumber(t *testing.T) {
	body := `{"providers":[
	  {"id":"a","ok":true,"credits":{"balance":"call us"}},
	  {"id":"b","ok":true,"credits":{"balance":"$4.20"}}]}`
	_, r, ok := cleanReading([]byte(body))
	if !ok {
		t.Fatal("refused")
	}
	// Provider "a" has no limits and a balance that is not one, so it never
	// reaches the page at all.
	if len(r.Providers) != 1 || r.Providers[0].ID != "b" {
		t.Fatalf("providers = %+v", r.Providers)
	}
	if r.Providers[0].Credits.Balance != "$4.20" {
		t.Fatalf("balance = %q", r.Providers[0].Credits.Balance)
	}
}

func TestDuplicateProvidersCollapse(t *testing.T) {
	body := `{"providers":[
	  {"id":"codex","ok":true,"limits":[{"used_percent":1}]},
	  {"id":"codex","ok":true,"limits":[{"used_percent":99}]}]}`
	_, r, _ := cleanReading([]byte(body))
	if len(r.Providers) != 1 || r.Providers[0].Limits[0].UsedPercent != 1 {
		t.Fatalf("duplicate id not collapsed to the first: %+v", r.Providers)
	}
}

func TestNewHealthFieldsSurviveWithBounds(t *testing.T) {
	// The 2026-08 additions: network rates, process count, temperature, and
	// their missing-vocabulary words. Mirrored from the hosted relay's rules;
	// this test is what notices the two drifting apart again.
	body := `{"providers":[{"id":"codex","ok":true,"limits":[{"used_percent":1}]}],
	  "system":{"platform":"linux","net_rx_bps":784469.28,"net_tx_bps":5154497.76,
	  "procs":1248.9,"temp_c":52.3,"missing":["network","procs","bogus"]}}`
	_, r, ok := cleanReading([]byte(body))
	if !ok || r.System == nil {
		t.Fatal("frame refused")
	}
	s := r.System
	if s.NetRxBps == nil || *s.NetRxBps != 784469.28 || s.NetTxBps == nil {
		t.Fatalf("net rates: %+v", s)
	}
	if s.Procs == nil || *s.Procs != 1248 {
		t.Fatalf("procs should floor to 1248: %+v", s.Procs)
	}
	if s.TempC == nil || *s.TempC != 52.3 {
		t.Fatalf("temp: %+v", s.TempC)
	}
	if len(s.Missing) != 2 {
		t.Fatalf("bogus missing word survived: %v", s.Missing)
	}

	// Out of bounds: a counter pushed as a rate, a fractional process, a
	// temperature no machine that still boots reports.
	body = `{"providers":[{"id":"codex","ok":true,"limits":[{"used_percent":1}]}],
	  "system":{"platform":"linux","net_rx_bps":1e16,"procs":0.5,"temp_c":-200}}`
	_, r, _ = cleanReading([]byte(body))
	if r.System.NetRxBps != nil || r.System.Procs != nil || r.System.TempC != nil {
		t.Fatalf("out-of-bounds values survived: %+v", r.System)
	}

	// Wrong TYPE in a new field must cost that field, not the block. These
	// keys were unknown — and therefore ignored — on every relay before they
	// existed, so an agent already pushing `temp_c` as a string used to get
	// its cpu reading through; typed decoding would have turned that into a
	// silent full-block regression (review finding, 2026-08-01).
	body = `{"providers":[{"id":"codex","ok":true,"limits":[{"used_percent":1}]}],
	  "system":{"platform":"linux","cpu_percent":40,"temp_c":"52","procs":true,"net_rx_bps":"fast"}}`
	_, r, _ = cleanReading([]byte(body))
	if r.System == nil || r.System.CPUPct == nil {
		t.Fatalf("a malformed new field must not sink the system block: %+v", r.System)
	}
	if r.System.TempC != nil || r.System.Procs != nil || r.System.NetRxBps != nil {
		t.Fatalf("malformed new fields survived: %+v", r.System)
	}
}

// The sampled series: one grid, holes that stay holes, and a malformed lane
// that costs only itself. Every case here is one the hosted cleaner is written
// to answer the same way — a helper must not be able to tell the two apart.
func withSeries(t *testing.T, series string) *system {
	t.Helper()
	push := strings.Replace(goodPush, `"missing":["swap"]`, `"missing":["swap"],"series":`+series, 1)
	_, r, ok := cleanReading([]byte(push))
	if !ok {
		t.Fatal("a push carrying a series was refused outright")
	}
	if r.System == nil {
		t.Fatal("the series took the whole system block with it")
	}
	return r.System
}

func TestSeriesAdmission(t *testing.T) {
	four := `"at":1785000000,"step":1,"cpu":[1,2,3],"mem":[4,5,6],"rx":[7,8,9],"tx":[10,11,12]`

	if s := withSeries(t, "{"+four+"}"); s.Series == nil || len(s.Series.CPU) != 3 ||
		s.Series.CPU[0] == nil || *s.Series.CPU[0] != 1 || s.Series.Step != 1 {
		t.Fatalf("a well-formed series did not survive: %+v", s.Series)
	}

	// A lane that disagrees about the grid is dropped BY ITSELF. The bad one is
	// first on purpose: deciding the grid from whichever lane parsed first let
	// one corrupt lane evict three intact ones.
	s := withSeries(t, `{"at":1785000000,"step":1,"cpu":[1,2,3,4],"mem":[4,5,6],"rx":[7,8,9],"tx":[10,11,12]}`)
	if s.Series == nil || s.Series.CPU != nil || len(s.Series.Mem) != 3 {
		t.Fatalf("the odd lane out was not the one dropped: %+v", s.Series)
	}

	// Two against two says nothing about when these samples were taken.
	if s := withSeries(t, `{"at":1785000000,"step":1,"cpu":[1,2,3,4],"mem":[4,5,6,7],"rx":[7,8,9],"tx":[10,11,12]}`); s.Series != nil {
		t.Fatal("an ambiguous grid was admitted")
	}

	// A lane of the wrong TYPE also costs only itself — decoding all four in one
	// Unmarshal made this drop the series, which the hosted cleaner does not.
	if s := withSeries(t, `{"at":1785000000,"step":1,"cpu":"bad","mem":[4,5,6],"rx":[7,8,9],"tx":[10,11,12]}`); s.Series == nil ||
		s.Series.CPU != nil || len(s.Series.Mem) != 3 {
		t.Fatal("one mistyped lane took the series with it")
	}

	// An element that fails its guard is a hole, never a zero.
	s = withSeries(t, `{"at":1785000000,"step":1,"cpu":[1,"x",3],"mem":[4,5,6],"rx":[7,8,9],"tx":[10,11,12]}`)
	if s.Series == nil || s.Series.CPU[1] != nil || s.Series.CPU[2] == nil {
		t.Fatalf("a bad element did not stay a hole: %+v", s.Series.CPU)
	}

	for _, bad := range []string{
		`{"at":1785000000,"step":0,"cpu":[1,2,3]}`,   // no grid spacing
		`{"at":1785000000,"step":600,"cpu":[1,2,3]}`, // ten minutes a slot
		`{"at":1784000000,"step":1,"cpu":[1,2,3]}`,   // not this frame's series
		`{"step":1,"cpu":[1,2,3]}`,                   // no anchor at all
		`{"at":1785000000,"step":1}`,                 // no lanes
		`{"at":1785000000,"step":1,"cpu":[]}`,        // an empty lane
		`"nope"`,                                     // not an object
	} {
		if s := withSeries(t, bad); s.Series != nil {
			t.Fatalf("admitted %s", bad)
		}
	}

	// The lag is checked against the frame's own captured_at, so a series can
	// only be admitted when that timestamp survived its own epoch guard.
	push := strings.Replace(goodPush, `"captured_at": 1785000000,`, `"captured_at": 0,`, 1)
	push = strings.Replace(push, `"missing":["swap"]`, `"missing":["swap"],"series":{"at":0,"step":1,"cpu":[1,2,3]}`, 1)
	if _, r, ok := cleanReading([]byte(push)); ok && r.System != nil && r.System.Series != nil {
		t.Fatal("a series rode in on an unvalidated captured_at")
	}
}

// A frame the size the helper actually pushes has to fit the ceiling this relay
// enforces, or a fully-populated machine 413s and simply stops appearing.
func TestAFullFrameFitsTheBodyCeiling(t *testing.T) {
	lane := "[" + strings.TrimSuffix(strings.Repeat("99.9,", 72), ",") + "]"
	// Fourteen, not maxProviders: the ceiling is checked against the BODY, and
	// the helper ships every collector it has before this relay trims the list.
	var providers []string
	for i := 0; i < 14; i++ {
		providers = append(providers, `{"id":"prov`+string(rune('a'+i))+`","name":"A Provider With A Name","ok":true,`+
			`"source":"local-log","captured_at":1785000000,"plan_type":"some plan",`+
			`"limits":[{"key":"primary","used_percent":42.5,"window_label":"5h","window_minutes":300,`+
			`"resets_at":1785003600,"severity":"normal","scope":"session","active":true},`+
			`{"key":"secondary","used_percent":12.5,"window_label":"weekly","resets_at":1785003600}],`+
			`"credits":{"has_credits":true,"unlimited":false,"balance":"$123.45"},`+
			`"source_file":"rollout-2026-08-05T00-00-00-0000000000000000.jsonl",`+
			`"detail":"a sentence of the length these collectors actually produce when they explain themselves",`+
			`"recorded_at":1785000000,"rate_limit_tier":"standard","truncated":false,"capped":false}`)
	}
	push := `{"ok":true,"captured_at":1785000000,"agent_id":"abc123XY","agent_name":"build box",` +
		`"helper_version":"2026.08.05.2","exec":true,"upd":true,"providers":[` + strings.Join(providers, ",") + `],` +
		`"system":{"ok":true,"platform":"linux","arch":"arm64","os_version":"6.8","cpu_count":4,` +
		`"cpu_percent":12.5,"load1":0.4,"load5":0.4,"load15":0.4,"mem_total":8589934592,` +
		`"mem_used_percent":61.2,"swap_total":1,"swap_used_percent":1,"disk_total":107374182400,` +
		`"disk_used":42949672960,"disk_used_percent":40,"uptime_sec":86400,"net_rx_bps":1234567,` +
		`"net_tx_bps":1234567,"tcp_estab":100,"tcp_time_wait":50,"tcp_retrans_ps":0.5,"procs":400,` +
		`"temp_c":45,"series":{"at":1785000000,"step":1,"cpu":` + lane + `,"mem":` + lane + `,"rx":` + lane + `,"tx":` + lane + `}}}`
	t.Logf("a full frame is %d bytes against a %d-byte ceiling", len(push), maxPushBody)
	// A frame that fits the OLD 8 KiB ceiling would pass this test without
	// proving anything — that limit is exactly what a real fourteen-provider
	// push with a 72-slot series went past, and nobody noticed because the
	// answer was a 413 in a relay log.
	if len(push) <= 8*1024 {
		t.Fatalf("the fixture is only %d bytes and no longer represents a full push", len(push))
	}
	if len(push) > maxPushBody {
		t.Fatalf("a full frame is %d bytes, past the %d-byte ceiling this relay enforces", len(push), maxPushBody)
	}
	if _, _, ok := cleanReading([]byte(push)); !ok {
		t.Fatal("a full frame was refused")
	}
}
