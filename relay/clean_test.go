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
}
