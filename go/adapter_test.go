package main

// The table-driven reader, tested at the seam that can be wrong quietly: the
// document-to-gauge conversion. The payloads are the ones the CLI Proxy API
// Management Center's own tests pin for each vendor (tests/*Quota*.test.ts at
// cpamcReferenceCommit), so what is asserted here is "this table reads what
// that console reads", not a guess about a vendor.

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func adapterDocs(t *testing.T, pairs ...string) map[string]any {
	t.Helper()
	docs := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		var v any
		if err := json.Unmarshal([]byte(pairs[i+1]), &v); err != nil {
			t.Fatalf("fixture %s: %v", pairs[i], err)
		}
		docs[pairs[i]] = v
	}
	return docs
}

func vendorOf(t *testing.T, vendor string) *vendorAdapter {
	t.Helper()
	ad := currentAdapters().Vendors[vendor]
	if ad == nil {
		t.Fatalf("no built-in adapter for %q", vendor)
	}
	return ad
}

func TestBuiltinAdaptersParse(t *testing.T) {
	set, err := parseAdapters(builtinAdaptersJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"claude", "codex", "antigravity", "kimi", "xai", "devin"} {
		if set.Vendors[v] == nil {
			t.Errorf("built-in table lacks %s", v)
		}
	}
	if set.MinHelper == "" || !versionAtLeast(helperVersion, set.MinHelper) {
		t.Errorf("the embedded table asks for helper %q; this build is %s", set.MinHelper, helperVersion)
	}
	// Every provider id the table emits must be one the registry ships, or
	// the coverage endpoint would be contradicted by a CPA reading — unless
	// it is a vendor read only through CPA, which says so here by name.
	shipped := map[string]bool{"devin": true}
	for _, c := range collectors {
		shipped[c.id] = true
	}
	for vendor, ad := range set.Vendors {
		if !shipped[ad.Provider] {
			t.Errorf("adapter %s emits provider %q, which no collector ships", vendor, ad.Provider)
		}
	}
}

func TestAdapterMinHelper(t *testing.T) {
	table := func(min string) []byte {
		return []byte(`{"version":9,"min_helper":"` + min + `","vendors":{"x":{"provider":"claude","name":"x","requests":[{"id":"u","url":"https://api.anthropic.com/api/oauth/usage"}]}}}`)
	}
	if _, err := parseAdapters(table(helperVersion)); err != nil {
		t.Errorf("a table for this very build was refused: %v", err)
	}
	if _, err := parseAdapters(table("2026.01.01.1")); err != nil {
		t.Errorf("a table for an older build was refused: %v", err)
	}
	for _, min := range []string{"2999.01.01.1", "next week", "2026.10.07"} {
		if _, err := parseAdapters(table(min)); err == nil {
			t.Errorf("min_helper %q was accepted by %s", min, helperVersion)
		}
	}
	// Numbers, not strings: .10 is after .9.
	if !versionAtLeast("2026.10.07.10", "2026.10.07.9") || versionAtLeast("2026.10.07.9", "2026.10.07.10") {
		t.Error("versions compared as strings")
	}
}

func TestAdapterRefusesHostOffList(t *testing.T) {
	raw := `{"version":1,"vendors":{"x":{"provider":"claude","name":"x","requests":[{"id":"u","url":"https://evil.example/usage"}]}}}`
	if _, err := parseAdapters([]byte(raw)); err == nil {
		t.Fatal("a table naming a host off the allow-list was accepted")
	}
	plain := `{"version":1,"vendors":{"x":{"provider":"claude","name":"x","requests":[{"id":"u","url":"http://api.anthropic.com/usage"}]}}}`
	if _, err := parseAdapters([]byte(plain)); err == nil {
		t.Fatal("a plain-http vendor URL was accepted")
	}
	if !cpaHostAllowed("https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary") {
		t.Fatal("a googleapis subdomain was refused")
	}
	if cpaHostAllowed("https://googleapis.com.evil.example/") {
		t.Fatal("a suffix-spoofing host was accepted")
	}
}

func TestClaudeModernScopedLimits(t *testing.T) {
	docs := adapterDocs(t, "usage", `{"limits":[
	  {"kind":"session","percent":22,"resets_at":"2026-07-27T10:00:00.000000+00:00","is_active":true},
	  {"kind":"weekly_all","percent":54,"resets_at":"2026-07-28T10:00:00+00:00"},
	  {"kind":"weekly_scoped","group":"weekly","percent":64,"resets_at":"2026-07-28T10:00:00+00:00","is_active":true,"scope":{"model":{"id":null,"display_name":"Fable"}}}
	]}`, "profile", `{"account":{"has_claude_max":true},"organization":{"rate_limit_tier":"default_claude_max_20x"}}`)
	r := adapterRun(vendorOf(t, "claude"), docs, nil, 1000)
	if len(r.Limits) != 3 {
		t.Fatalf("want 3 windows, got %d: %+v", len(r.Limits), r.Limits)
	}
	if r.Limits[0].Key != "session" || r.Limits[0].UsedPercent != 22 || *r.Limits[0].WindowLabel != "5h" || r.Limits[0].Active == nil || !*r.Limits[0].Active {
		t.Errorf("session window: %+v", r.Limits[0])
	}
	if r.Limits[1].Key != "weekly" || *r.Limits[1].WindowLabel != "7d" {
		t.Errorf("weekly window: %+v", r.Limits[1])
	}
	if r.Limits[2].Key != "weekly_scoped" || r.Limits[2].Scope == nil || *r.Limits[2].Scope != "Fable" || r.Limits[2].UsedPercent != 64 {
		t.Errorf("scoped window: %+v", r.Limits[2])
	}
	if r.Limits[0].ResetsAt == nil || *r.Limits[0].ResetsAt != isoToEpoch("2026-07-27T10:00:00Z") {
		t.Errorf("reset not parsed: %+v", r.Limits[0].ResetsAt)
	}
	if r.Plan != "Max" || r.Tier != "20x" {
		t.Errorf("plan/tier: %q %q", r.Plan, r.Tier)
	}
}

func TestClaudeLegacyWindowsOnlyWhenModernAbsent(t *testing.T) {
	docs := adapterDocs(t, "usage", `{"five_hour":{"utilization":10,"resets_at":null},"seven_day":{"utilization":20,"resets_at":"2026-07-28T10:00:00Z"},"seven_day_opus":{"utilization":41,"resets_at":"2026-07-28T10:00:00Z"}}`)
	r := adapterRun(vendorOf(t, "claude"), docs, nil, 1000)
	if len(r.Limits) != 3 {
		t.Fatalf("want 3 legacy windows, got %+v", r.Limits)
	}
	if r.Limits[2].Scope == nil || *r.Limits[2].Scope != "Opus" || r.Limits[2].UsedPercent != 41 {
		t.Errorf("legacy opus: %+v", r.Limits[2])
	}
	// With a modern scoped row present, the legacy field is not a second window.
	both := adapterDocs(t, "usage", `{"seven_day_opus":{"utilization":41},"limits":[{"kind":"weekly_scoped","percent":64,"scope":{"model":{"display_name":"Opus"}}}]}`)
	r = adapterRun(vendorOf(t, "claude"), both, nil, 1000)
	if len(r.Limits) != 1 || r.Limits[0].UsedPercent != 64 {
		t.Errorf("modern row should win: %+v", r.Limits)
	}
}

// `iguana_necktie` is a promotional cloud-session credit in DOLLARS — not
// cents, not a weekly window, and its `utilization` is not read (CodexBar
// ClaudeCloudCreditsSnapshot, steipete/CodexBar@745bf89a; the shape is the
// one its tests carry, attributed to ItsJazii/pane). A table that read it as
// a "7d Fable" window drew a quota that does not exist.
func TestClaudeCloudCreditsAreDollars(t *testing.T) {
	docs := adapterDocs(t, "usage", `{"five_hour":{"utilization":12},"seven_day":{"utilization":34},
	  "extra_usage":{"is_enabled":true,"monthly_limit":2000,"used_credits":500},
	  "iguana_necktie":{"limit_dollars":100,"used_dollars":25,"remaining_dollars":75,"utilization":25,
	    "resets_at":"2026-11-05T07:59:00+00:00","locked_reason":null}}`)
	r := adapterRun(vendorOf(t, "claude"), docs, nil, v4Now)
	var cloud *Limit
	for i := range r.Limits {
		if r.Limits[i].Scope != nil && *r.Limits[i].Scope == "Fable" {
			t.Fatalf("the cloud credit is not a Fable window: %+v", r.Limits[i])
		}
		if r.Limits[i].Key == "cloud_credits" {
			cloud = &r.Limits[i]
		}
	}
	if cloud == nil || cloud.UsedPercent != 25 || cloud.Unit != "usd" ||
		cloud.UsedAmount == nil || *cloud.UsedAmount != 25 || *cloud.TotalAmount != 100 || *cloud.RemainingAmount != 75 {
		t.Fatalf("cloud credits: %+v", cloud)
	}
	if cloud.ResetsAt == nil || *cloud.ResetsAt != isoToEpoch("2026-11-05T07:59:00+00:00") {
		t.Errorf("the grant's expiry: %v", cloud.ResetsAt)
	}
	// Extra usage beside it is still cents.
	for _, l := range r.Limits {
		if l.Key == "extra" && (l.UsedAmount == nil || *l.UsedAmount != 5 || *l.TotalAmount != 20) {
			t.Errorf("extra usage: %+v", l)
		}
	}
	// No limit, or a limit of zero: no window.
	for _, bad := range []string{`{"limit_dollars":0,"remaining_dollars":0}`, `{"remaining_dollars":75}`} {
		r := adapterRun(vendorOf(t, "claude"), adapterDocs(t, "usage", `{"five_hour":{"utilization":1},"iguana_necktie":`+bad+`}`), nil, v4Now)
		for _, l := range r.Limits {
			if l.Key == "cloud_credits" {
				t.Errorf("%s: %+v", bad, l)
			}
		}
	}
}

// Reset grants: a grant counts once it has begun and until it lapses, and a
// grant of two is two tickets with one lapse (CodexBar's
// ClaudeRateLimitResetCredits, steipete/CodexBar@05a6a434, for the dates).
// Where CodexBar leaves paused and ineligible grants out of the count, the
// table keeps them as held and says they are not usable (v5's rule, what the
// reset cards show) — so they stay in `left` with `usable` at zero.
func TestClaudeResetGrantsAreFiltered(t *testing.T) {
	usage := `{"five_hour":{"utilization":12},"cedar_ember":{"eligible":true,"grants":[
	  {"id":"redemption-handle","label":"private grant","resets_left":2,"resets_total":2,
	   "starts_at":"2020-01-01T00:00:00Z","ends_at":"2099-01-01T00:00:00Z","paused":false,"usable_now":true},
	  {"resets_left":1,"resets_total":1,"paused":true},
	  {"resets_left":1,"paused":false,"ends_at":"2020-01-01T00:00:00Z"},
	  {"resets_left":1,"paused":false,"starts_at":"2099-01-01T00:00:00Z"}]}}`
	r := adapterRun(vendorOf(t, "claude"), adapterDocs(t, "usage", usage), nil, v4Now)
	if r.Resets == nil || r.Resets.Left != 3 || r.Resets.Total == nil || *r.Resets.Total != 3 {
		t.Fatalf("lapsed and not-yet-begun grants must not count; a paused one is held: %+v", r.Resets)
	}
	if r.Resets.Usable == nil || *r.Resets.Usable != 2 {
		t.Errorf("only the running grant's two are usable: %+v", r.Resets)
	}
	end := isoToEpoch("2099-01-01T00:00:00Z")
	if len(r.Resets.Expires) != 2 || r.Resets.Expires[0] != end || r.Resets.Expires[1] != end {
		t.Errorf("two tickets, one lapse each: %v", r.Resets.Expires)
	}
	// Not eligible: the inventory is still shown, none of it usable.
	doc := `{"five_hour":{"utilization":1},"cedar_ember":{"eligible":false,"ineligible_reason":"surface","grants":[{"resets_left":1,"paused":false}]}}`
	if r := adapterRun(vendorOf(t, "claude"), adapterDocs(t, "usage", doc), nil, v4Now); r.Resets == nil || r.Resets.Left != 1 || r.Resets.Usable == nil || *r.Resets.Usable != 0 {
		t.Errorf("ineligible: %+v", r.Resets)
	}
}

// A workspace seat's monthly credit pool: `individual_limit` at the root,
// under rate_limit, or under spend_control — the first that is there — with
// its figures in credits (CodexBar CodexOAuthUsageFetcher, @5f872ecca9).
func TestCodexMonthlyCreditPool(t *testing.T) {
	team := adapterDocs(t, "usage", `{"plan_type":"team",
	  "rate_limit":{"allowed":false,"limit_reached":true,
	    "primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":45962,"reset_at":1786161204},
	    "secondary_window":null},
	  "credits":{"has_credits":true,"unlimited":false,"balance":null},
	  "spend_control":{"reached":false,"individual_limit":{"source":"workspace_spend_controls",
	    "limit":"1000","used":"36.79748725891113","remaining":"963.2025127410889",
	    "used_percent":4,"remaining_percent":96,"reset_after_seconds":2105558,"reset_at":1788220800}}}`)
	r := adapterRun(vendorOf(t, "codex"), team, nil, v4Now)
	var pool *Limit
	for i := range r.Limits {
		if r.Limits[i].Key == "monthly_credits" {
			pool = &r.Limits[i]
		}
	}
	if pool == nil || pool.UsedPercent != 3.68 || pool.Unit != "credits" ||
		pool.UsedAmount == nil || *pool.UsedAmount != 36.8 || *pool.TotalAmount != 1000 || *pool.RemainingAmount != 963.2 {
		t.Fatalf("team pool: %+v", pool)
	}
	if pool.ResetsAt == nil || *pool.ResetsAt != 1788220800 {
		t.Errorf("reset_at: %v", pool.ResetsAt)
	}
	ent := adapterDocs(t, "usage", `{"plan_type":"enterprise","rate_limit":{"primary_window":null,"secondary_window":null,
	  "individual_limit":{"limit":100000,"used":"7761","remaining_percent":92.239,"resets_at":1782864000}},
	  "credits":{"has_credits":true,"unlimited":false,"balance":"0"}}`)
	r = adapterRun(vendorOf(t, "codex"), ent, nil, v4Now)
	if len(r.Limits) != 1 || r.Limits[0].Key != "monthly_credits" || r.Limits[0].UsedPercent != 7.76 ||
		*r.Limits[0].RemainingAmount != 92239 || *r.Limits[0].ResetsAt != 1782864000 {
		t.Fatalf("enterprise pool: %+v", r.Limits)
	}
	if r.Plan != "Enterprise" {
		t.Errorf("plan: %q", r.Plan)
	}
}

// `active_until` is the renewal date while `will_renew` is true, and the
// plan's END when it is false — a cancelled plan does not renew on it
// (CodexBar OpenAISubscriptionMetadataModels, @5b0b9fa3e3). Without the
// flag the date is only how long the period runs.
func TestCodexRenewalOnlyWhenItWillRenew(t *testing.T) {
	usage := `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`
	want := math.Round(isoToEpoch("2026-10-20T14:30:07.123Z"))
	renewing := adapterDocs(t, "usage", usage, "subscription", `{"active_until":"2026-10-20T14:30:07.123Z","will_renew":true}`)
	r := adapterRun(vendorOf(t, "codex"), renewing, nil, v4Now)
	if r.PlanKind != "renews" || r.PlanAt == nil || *r.PlanAt != want {
		t.Fatalf("renews: %s %v", r.PlanKind, r.PlanAt)
	}
	ending := adapterDocs(t, "usage", usage, "subscription", `{"active_until":"2026-10-20T14:30:07.123Z","will_renew":false}`)
	if r := adapterRun(vendorOf(t, "codex"), ending, nil, v4Now); r.PlanKind != "until" || r.PlanAt == nil || *r.PlanAt != want {
		t.Fatalf("a plan that will not renew only runs until the date: %s %v", r.PlanKind, r.PlanAt)
	}
	unsaid := adapterDocs(t, "usage", usage, "subscription", `{"active_until":"2026-10-20T14:30:07.123Z"}`)
	if r := adapterRun(vendorOf(t, "codex"), unsaid, nil, v4Now); r.PlanKind != "until" || r.PlanAt == nil || *r.PlanAt != want {
		t.Fatalf("without will_renew the date is not known to be a renewal: %s %v", r.PlanKind, r.PlanAt)
	}
	// The entitlement's own renewal date stands on its own.
	ent := adapterDocs(t, "usage", usage, "subscription", `{"entitlement":{"renews_at":"2026-10-20T00:00:00Z"},"active_until":"2026-10-25T00:00:00Z","will_renew":false}`)
	if r := adapterRun(vendorOf(t, "codex"), ent, nil, v4Now); r.PlanKind != "renews" || r.PlanAt == nil || *r.PlanAt != isoToEpoch("2026-10-20T00:00:00Z") {
		t.Fatalf("entitlement.renews_at: %s %v", r.PlanKind, r.PlanAt)
	}
}

// The Codex payload CPAMC pins (tests/codexQuota.test.ts).
const codexUsageFixture = `{
  "plan_type": "pro",
  "rate_limit": {"allowed": true, "limit_reached": false,
    "primary_window": {"used_percent": 1, "limit_window_seconds": 604800, "reset_after_seconds": 601888, "reset_at": 1785902974},
    "secondary_window": null},
  "code_review_rate_limit": null,
  "additional_rate_limits": [{"limit_name": "GPT-5.3-Codex-Spark", "metered_feature": "codex_bengalfox",
    "rate_limit": {"allowed": true, "limit_reached": false,
      "primary_window": {"used_percent": 0, "limit_window_seconds": 604800, "reset_after_seconds": 602111, "reset_at": 1785903197},
      "secondary_window": null}}],
  "rate_limit_reset_credits": {"available_count": 1, "applicable_available_count": 0}
}`

func TestCodexWindows(t *testing.T) {
	docs := adapterDocs(t, "usage", codexUsageFixture)
	r := adapterRun(vendorOf(t, "codex"), docs, nil, 1000)
	if len(r.Limits) != 2 {
		t.Fatalf("want primary + one extra, got %+v", r.Limits)
	}
	p := r.Limits[0]
	if p.Key != "primary" || p.UsedPercent != 1 || p.WindowMinutes == nil || *p.WindowMinutes != 10080 || *p.WindowLabel != "7d" {
		t.Errorf("primary: %+v", p)
	}
	if p.ResetsAt == nil || *p.ResetsAt != 1785902974 {
		t.Errorf("primary reset should be the epoch, got %v", p.ResetsAt)
	}
	x := r.Limits[1]
	if x.Key != "extra_primary" || x.Scope == nil || *x.Scope != "GPT-5.3-Codex-Spark" || x.UsedPercent != 0 {
		t.Errorf("extra: %+v", x)
	}
	if r.Plan != "Pro" {
		t.Errorf("plan: %q", r.Plan)
	}
}

func TestCodexRelativeResetAndCamelCase(t *testing.T) {
	docs := adapterDocs(t, "usage", `{"rateLimit":{"primaryWindow":{"usedPercent":"37","limitWindowSeconds":18000,"resetAfterSeconds":900},"secondaryWindow":{"usedPercent":5,"limitWindowSeconds":604800}}}`)
	r := adapterRun(vendorOf(t, "codex"), docs, nil, 5000)
	if len(r.Limits) != 2 {
		t.Fatalf("got %+v", r.Limits)
	}
	if r.Limits[0].UsedPercent != 37 || *r.Limits[0].WindowLabel != "5h" || r.Limits[0].ResetsAt == nil || *r.Limits[0].ResetsAt != 5900 {
		t.Errorf("primary: %+v", r.Limits[0])
	}
	if r.Limits[1].Key != "secondary" || r.Limits[1].ResetsAt != nil {
		t.Errorf("secondary: %+v", r.Limits[1])
	}
}

func TestAntigravityGroupsAndBuckets(t *testing.T) {
	docs := adapterDocs(t, "quota", `{"groups":[
	  {"displayName":"Gemini models","buckets":[
	    {"bucketId":"gemini-five-hour","displayName":"Five Hour Limit Remaining","window":"five_hour","remainingFraction":1,"resetTime":"2026-09-09T20:00:00Z"},
	    {"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","remainingFraction":"0.96","resetTime":"2026-09-11T02:00:00Z"}]},
	  {"display_name":"Claude and GPT models","buckets":[
	    {"bucket_id":"claude-weekly","window":"weekly","remaining_fraction":"99%","reset_time":"2026-09-11T20:00:00Z"}]}
	]}`)
	r := adapterRun(vendorOf(t, "antigravity"), docs, nil, 1000)
	if len(r.Limits) != 3 {
		t.Fatalf("got %+v", r.Limits)
	}
	if r.Limits[0].Key != "gemini-five-hour" || r.Limits[0].UsedPercent != 0 || *r.Limits[0].WindowLabel != "5h" || *r.Limits[0].Scope != "Gemini models" {
		t.Errorf("first: %+v", r.Limits[0])
	}
	if r.Limits[1].UsedPercent != 4 || *r.Limits[1].WindowLabel != "7d" {
		t.Errorf("second: %+v", r.Limits[1])
	}
	if r.Limits[2].UsedPercent != 1 || *r.Limits[2].Scope != "Claude and GPT models" {
		t.Errorf("third: %+v", r.Limits[2])
	}
}

func TestKimiRowsAndWindows(t *testing.T) {
	docs := adapterDocs(t, "usage", `{"usage":{"used":"1","limit":"100","remaining":"99","resetTime":"2099-08-06T13:59:23.136523Z"},
	  "limits":[{"detail":{"used":"2","limit":"100","remaining":"98","resetTime":"2099-07-31T06:59:23.136523Z"},"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"}}]}`)
	r := adapterRun(vendorOf(t, "kimi"), docs, nil, 1000)
	if len(r.Limits) != 2 {
		t.Fatalf("got %+v", r.Limits)
	}
	rate := r.Limits[0]
	if rate.Key != "rate" || rate.UsedPercent != 2 || rate.WindowMinutes == nil || *rate.WindowMinutes != 300 || *rate.WindowLabel != "5h" {
		t.Errorf("rate: %+v", rate)
	}
	if rate.ResetsAt == nil || *rate.ResetsAt != isoToEpoch("2099-07-31T06:59:23.136523Z") {
		t.Errorf("rate reset: %v", rate.ResetsAt)
	}
	weekly := r.Limits[1]
	if weekly.Key != "weekly" || weekly.UsedPercent != 1 || *weekly.WindowLabel != "7d" {
		t.Errorf("weekly: %+v", weekly)
	}
}

func TestXaiBillingWindows(t *testing.T) {
	docs := adapterDocs(t,
		"weekly", `{"config":{"currentPeriod":{"type":"WEEKLY","start":"2026-09-08T00:00:00Z","end":"2026-09-15T00:00:00Z"},"creditUsagePercent":12.5}}`,
		"monthly", `{"config":{"monthlyLimit":{"val":3000},"used":{"val":450},"billingPeriodEnd":"2026-10-01T00:00:00Z"}}`)
	r := adapterRun(vendorOf(t, "xai"), docs, nil, 1000)
	if len(r.Limits) != 2 {
		t.Fatalf("got %+v", r.Limits)
	}
	if r.Limits[0].Key != "credits" || r.Limits[0].UsedPercent != 12.5 || r.Limits[0].ResetsAt == nil {
		t.Errorf("credits: %+v", r.Limits[0])
	}
	m := r.Limits[1]
	if m.Key != "monthly" || m.UsedPercent != 15 || m.Unit != "usd" || m.UsedAmount == nil || *m.UsedAmount != 4.5 || *m.TotalAmount != 30 || *m.RemainingAmount != 25.5 {
		t.Errorf("monthly: %+v", m)
	}
}

func TestAdapterTemplatesReadTheCredential(t *testing.T) {
	// The Codex account id rides inside the id_token JWT, under OpenAI's
	// namespace; the Antigravity project id sits wherever CPA put it.
	jwt := "h." + "eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdC0xIn19" + ".s"
	cred := map[string]any{"id_token": jwt, "attributes": map[string]any{"gemini_virtual_project": "proj-9"}}
	ctx := adapterCtx{cred}
	if got := adapterTemplate("{jwt:id_token|metadata.id_token:chatgpt_account_id}", ctx, cred); got != "acct-1" {
		t.Errorf("jwt claim: %q", got)
	}
	if got := adapterTemplate(`{"project":"{cred.project_id|attributes.gemini_virtual_project}"}`, ctx, cred); got != `{"project":"proj-9"}` {
		t.Errorf("body template: %q", got)
	}
	if got := adapterTemplate("{cred.nothing|missing}", ctx, cred); got != "" {
		t.Errorf("missing should be empty, got %q", got)
	}
}

func TestAdapterPathClimbsAndIndexes(t *testing.T) {
	var doc any
	json.Unmarshal([]byte(`{"a":{"b":[{"c":1},{"c":2}]},"top":"T"}`), &doc)
	ctx := adapterCtx{doc}
	items := ctx.iterate("a.b[]")
	if len(items) != 2 {
		t.Fatalf("iterate: %d", len(items))
	}
	if v, _ := adapterNum(items[1].get("c")); v != 2 {
		t.Errorf("item field: %v", v)
	}
	if v := adapterStr(items[1].get("^top"), 10); v != "T" {
		t.Errorf("climb to root: %q", v)
	}
	if items[1].get("^^top") != nil {
		t.Error("climbing past the root must be nil, not the root")
	}
	if v := adapterStr(items[1].get("$.top"), 10); v != "T" {
		t.Errorf("root prefix: %q", v)
	}
	if v, _ := adapterNum(ctx.get("a.b[0].c")); v != 1 {
		t.Errorf("index: %v", v)
	}
	if ctx.get("a.b[5].c") != nil || ctx.get("a.x.y") != nil {
		t.Error("missing paths must be nil")
	}
}

func TestParseCPAListing(t *testing.T) {
	body := `{"files":[
	  {"name":"codex-b.json","provider":"codex","auth_index":"7","email":"b@example.com"},
	  {"name":"claude-a.json","type":"claude","auth_index":3,"email":"a@example.com"},
	  {"name":"grok.json","provider":"x_ai","auth_index":"9"},
	  {"name":"off.json","provider":"claude","auth_index":"1","disabled":true},
	  {"name":"unknown.json","provider":"vertex","auth_index":"2"},
	  {"name":"noindex.json","provider":"claude"}
	]}`
	creds, ok := parseCPAListing([]byte(body))
	if !ok {
		t.Fatal("listing refused")
	}
	if len(creds) != 4 {
		t.Fatalf("want 4 (unknown vendors ride along, disabled and unindexed do not), got %+v", creds)
	}
	if creds[0].vendor != "vertex" || creds[1].vendor != "claude" || creds[1].account != "a@example.com" || creds[2].vendor != "codex" || creds[3].vendor != "xai" {
		t.Errorf("order/vendor: %+v", creds)
	}
	if creds[3].account != "grok" {
		t.Errorf("account falls back to the file stem: %q", creds[3].account)
	}
	if _, ok := parseCPAListing([]byte(`{"nope":1}`)); ok {
		t.Error("a document without files is not a listing")
	}
	if _, ok := parseCPAListing([]byte(`[]`)); !ok {
		t.Error("a bare array is a listing")
	}
}

func TestMergeCPAPrecedence(t *testing.T) {
	native := []Provider{{ID: "codex", OK: true}, {ID: "claude", OK: true}, {ID: "amp", OK: true}}
	cpa := cpaResult{configured: true, providers: []Provider{
		{ID: "claude", OK: true, Source: "cpa", Account: "a@example.com"},
		{ID: "claude@2", OK: false, Source: "cpa", Account: "b@example.com", Error: "token-expired"},
	}}
	got := mergeCPA(native, cpa)
	ids := []string{}
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	want := "codex,claude,claude@2,amp"
	if s := joinIDs(ids); s != want {
		t.Errorf("order: %s, want %s", s, want)
	}
	if got[1].Source != "cpa" || got[1].Collector == nil || got[1].Collector.Method != "cpa" || len(got[1].Collector.References) != 1 || got[1].Collector.References[0].ID != "cpamc" {
		t.Errorf("cpa reading lacks its provenance: %+v", got[1].Collector)
	}
	// CPA broken: natives untouched, and nothing added — the proxy is a
	// channel, its trouble is for the log.
	fail := Provider{ID: "cpa", OK: false, Error: "unreachable"}
	got = mergeCPA(native, cpaResult{configured: true, status: &fail})
	if len(got) != 3 || got[0].ID != "codex" || got[2].ID != "amp" {
		t.Errorf("a broken proxy must add nothing: %+v", got)
	}
	// Not configured: nothing changes, not even the order.
	got = mergeCPA(native, cpaResult{})
	if len(got) != 3 || got[0].ID != "codex" {
		t.Errorf("unconfigured: %+v", got)
	}
}

func joinIDs(ids []string) string {
	s := ""
	for i, id := range ids {
		if i > 0 {
			s += ","
		}
		s += id
	}
	return s
}

func TestLoadCPAConfigRules(t *testing.T) {
	t.Setenv(cpaURLEnv, "")
	t.Setenv(cpaKeyEnv, "")
	if c, code := loadCPAConfig(); c.base != nil || code != "" {
		t.Errorf("unset should be unconfigured: %+v %q", c, code)
	}
	t.Setenv(cpaKeyEnv, "k")
	if c, code := loadCPAConfig(); code != "" || c.base == nil || c.base.String() != cpaDefaultURL {
		t.Errorf("key alone takes the default loopback URL: %+v %q", c, code)
	}
	t.Setenv(cpaURLEnv, "http://10.0.0.5:8317")
	if _, code := loadCPAConfig(); code != "cpa-url" {
		t.Errorf("plain http off loopback must be refused, got %q", code)
	}
	t.Setenv(cpaURLEnv, "https://cpa.example.net/")
	if c, code := loadCPAConfig(); code != "" || c.base.Path != "" {
		t.Errorf("https anywhere, trailing slash trimmed: %+v %q", c, code)
	}
	t.Setenv(cpaURLEnv, "http://127.0.0.1:8317")
	t.Setenv(cpaKeyEnv, "")
	if _, code := loadCPAConfig(); code != "cpa-url" {
		t.Errorf("a URL without a key is unusable, got %q", code)
	}
}

// ── v4: credits, reset credits, renewal, plans (CPAMC 6abace9f) ────────────
//
// The payloads below are the shapes the vendors answered with on 2026-10-07,
// read through CPA on a real machine; ids, emails and free text replaced.

// 2026-10-07 12:00:00 UTC — "now" for the date checks below.
const v4Now = 1791374400

const claudeCedarFixture = `{
  "five_hour": {"utilization": 9.0, "resets_at": "2026-10-07T14:40:00+00:00"},
  "seven_day": {"utilization": 46.0, "resets_at": "2026-10-12T08:00:00+00:00"},
  "limits": [
    {"kind": "session", "percent": 9, "severity": "normal", "resets_at": "2026-10-07T14:40:00+00:00", "is_active": false},
    {"kind": "weekly_all", "percent": 46, "severity": "normal", "resets_at": "2026-10-12T08:00:00+00:00", "is_active": false}
  ],
  "cedar_ember": {
    "eligible": true, "ineligible_reason": null, "at_limit": false, "exhausted": [],
    "grants": [{"id": "g_launch", "label": "one usage-limit reset", "resets_total": 1, "resets_left": 1,
      "starts_at": "2026-09-22T16:00:00+00:00", "ends_at": "2026-10-22T16:00:00+00:00",
      "clears": ["five_hour", "seven_day"], "paused": false, "usable_now": true, "use_requires_limit": false}],
    "next_grant_id": "g_launch", "weekly_resets_at": "2026-10-12T08:00:00+00:00", "cooldown_until": null
  },
  "extra_usage": {"is_enabled": true, "monthly_limit": 5000, "used_credits": 1234, "utilization": 24.68, "currency": "USD"}
}`

func TestClaudeResetGrantsAndExtraUsage(t *testing.T) {
	r := adapterRun(vendorOf(t, "claude"), adapterDocs(t, "usage", claudeCedarFixture), nil, v4Now)
	if r.Resets == nil || r.Resets.Left != 1 || r.Resets.Total == nil || *r.Resets.Total != 1 {
		t.Fatalf("resets: %+v", r.Resets)
	}
	if len(r.Resets.Expires) != 1 || r.Resets.Expires[0] != 1792684800 {
		t.Errorf("the grant's end should be its lapse: %v", r.Resets.Expires)
	}
	var extra *Limit
	for i := range r.Limits {
		if r.Limits[i].Key == "extra" {
			extra = &r.Limits[i]
		}
	}
	if extra == nil || extra.UsedPercent != 24.68 || extra.Unit != "usd" ||
		extra.UsedAmount == nil || *extra.UsedAmount != 12.34 || *extra.TotalAmount != 50 || *extra.RemainingAmount != 37.66 {
		t.Errorf("extra usage, cents to dollars: %+v", extra)
	}
	// Its unit is the vendor's currency (2026.10.07.7): an account billed in
	// euros keeps its share and shows no dollars it was never charged; one
	// whose answer names no currency is read in dollars, as it always was.
	extraOf := func(currency string) *Limit {
		docs := adapterDocs(t, "usage", `{"limits":[{"kind":"session","percent":8}],
		  "extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":1234,"utilization":24.68`+currency+`}}`)
		for _, l := range adapterRun(vendorOf(t, "claude"), docs, nil, v4Now).Limits {
			if l.Key == "extra" {
				return &l
			}
		}
		return nil
	}
	if e := extraOf(`,"currency":"EUR"`); e == nil || e.UsedPercent != 24.68 || e.Unit != "" || e.UsedAmount != nil || e.TotalAmount != nil {
		t.Errorf("euros are not dollars: %+v", e)
	}
	if e := extraOf(`,"currency":"usd"`); e == nil || e.Unit != "usd" || e.UsedAmount == nil || *e.UsedAmount != 12.34 {
		t.Errorf("usd, any case: %+v", e)
	}
	if e := extraOf(""); e == nil || e.Unit != "usd" || e.TotalAmount == nil || *e.TotalAmount != 50 {
		t.Errorf("no currency named: %+v", e)
	}

	// An account the program does not reach, and extra usage switched off:
	// nothing to say about either.
	off := adapterDocs(t, "usage", `{"limits":[{"kind":"session","percent":8,"resets_at":"2026-10-07T14:40:00+00:00"}],
	  "cedar_ember":{"eligible":false,"ineligible_reason":"surface","grants":[]},
	  "extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null}}`)
	r = adapterRun(vendorOf(t, "claude"), off, nil, v4Now)
	if r.Resets != nil {
		t.Errorf("no grants is no reset credits: %+v", r.Resets)
	}
	for _, l := range r.Limits {
		if l.Key == "extra" {
			t.Errorf("extra usage switched off must not be a window: %+v", l)
		}
	}
	// A grant spent is not a credit held, and a lapse already passed is none.
	spent := adapterDocs(t, "usage", `{"limits":[{"kind":"session","percent":8}],"cedar_ember":{"eligible":true,"grants":[
	  {"id":"a","resets_total":1,"resets_left":0,"ends_at":"2026-10-22T16:00:00+00:00"}]}}`)
	if r := adapterRun(vendorOf(t, "claude"), spent, nil, v4Now); r.Resets != nil {
		t.Errorf("a spent grant still showed: %+v", r.Resets)
	}
}

const codexUsageV4Fixture = `{
  "plan_type": "team",
  "rate_limit": {"primary_window": {"used_percent": 0, "limit_window_seconds": 18000, "reset_at": 1791400000},
                 "secondary_window": {"used_percent": 44, "limit_window_seconds": 604800, "reset_at": 1791800000}},
  "credits": {"has_credits": true, "unlimited": false, "balance": "12.5"},
  "rate_limit_reset_credits": {"available_count": 2, "applicable_available_count": 0}
}`

const codexResetCreditsFixture = `{
  "credits": [
    {"id": "rc_2", "reset_type": "codex_rate_limits", "status": "available", "granted_at": "2026-09-29T19:15:12Z", "expires_at": "2026-10-29T19:15:12Z", "title": "Full reset"},
    {"id": "rc_1", "reset_type": "codex_rate_limits", "status": "available", "granted_at": "2026-09-22T20:48:34Z", "expires_at": "2026-10-22T20:48:34Z", "title": "Full reset"},
    {"id": "rc_0", "reset_type": "codex_rate_limits", "status": "redeemed", "granted_at": "2026-09-01T00:00:00Z", "expires_at": "2026-10-01T00:00:00Z"}
  ],
  "available_count": 2, "total_earned_count": 0
}`

// What can be done with reset credits now (2026.10.07.3): how many are usable
// at once, whether the rest wait on a window reaching its limit, a cooldown,
// and which windows one clears — Claude's from the grants' own flags, as the
// console's claim button decides (CPAMC anthropicResetGrantBlocker), Codex's
// from the count its usage call states.
func TestResetCreditsSayWhatCanBeDoneNow(t *testing.T) {
	claude := vendorOf(t, "claude")
	grant := func(extra, top string) map[string]any {
		return adapterDocs(t, "usage", `{"limits":[{"kind":"session","percent":9}],"cedar_ember":{"eligible":true,"at_limit":false`+top+`,
		  "grants":[{"id":"g","resets_total":2,"resets_left":2,"ends_at":"2026-10-22T16:00:00+00:00",
		    "clears":["five_hour","seven_day","seven_day_overage_included","some_new_window"],"usable_now":true,"paused":false`+extra+`}]}}`)
	}
	usable := func(r *ResetCredits) float64 {
		if r == nil || r.Usable == nil {
			return -1
		}
		return *r.Usable
	}

	// The fixture the console pins: usable at once, clearing both windows.
	r := adapterRun(claude, adapterDocs(t, "usage", claudeCedarFixture), nil, v4Now).Resets
	if usable(r) != 1 || r.NeedsLimit || r.WaitUntil != nil || strings.Join(r.Clears, ",") != "5h,7d" {
		t.Errorf("a usable grant: %+v", r)
	}
	// A grant that only works at a limit, on an account not at one: held,
	// none usable, and that is the reason. The overage window maps onto 7d
	// and a name nobody mapped is not a window.
	r = adapterRun(claude, grant(`,"use_requires_limit":true`, ""), nil, v4Now).Resets
	if r == nil || r.Left != 2 || usable(r) != 0 || !r.NeedsLimit || strings.Join(r.Clears, ",") != "5h,7d" {
		t.Errorf("waiting on a limit: %+v", r)
	}
	// …and at one, usable.
	r = adapterRun(claude, grant(`,"use_requires_limit":true`, `,"at_limit":true`), nil, v4Now).Resets
	if usable(r) != 2 || r.NeedsLimit {
		t.Errorf("at the limit: %+v", r)
	}
	// Paused, or the vendor not saying it is usable: held, not usable, and
	// not for want of a limit.
	for name, extra := range map[string]string{"paused": `,"paused":true`, "not usable": `,"usable_now":false`} {
		r = adapterRun(claude, grant(extra, ""), nil, v4Now).Resets
		if usable(r) != 0 || r.NeedsLimit {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// A grant with no word on it either way is no claim against it.
	r = adapterRun(claude, adapterDocs(t, "usage", `{"limits":[{"kind":"session","percent":9}],"cedar_ember":{"eligible":true,
	  "grants":[{"id":"g","resets_total":1,"resets_left":1,"ends_at":"2026-10-22T16:00:00+00:00"}]}}`), nil, v4Now).Resets
	if usable(r) != 1 || r.NeedsLimit || len(r.Clears) != 0 {
		t.Errorf("unstated: %+v", r)
	}
	// An account that is not eligible uses none of what it holds.
	r = adapterRun(claude, grant("", `,"eligible":false`), nil, v4Now).Resets
	if r == nil || usable(r) != 0 {
		t.Errorf("ineligible: %+v", r)
	}
	// A cooldown still ahead is one; one already over is none.
	r = adapterRun(claude, grant("", `,"cooldown_until":"2026-10-07T16:00:00+00:00"`), nil, v4Now).Resets
	if r.WaitUntil == nil || *r.WaitUntil != 1791388800 {
		t.Errorf("cooldown: %+v", r.WaitUntil)
	}
	if r = adapterRun(claude, grant("", `,"cooldown_until":"2026-10-01T12:00:00+00:00"`), nil, v4Now).Resets; r.WaitUntil != nil {
		t.Errorf("a cooldown over is none: %v", *r.WaitUntil)
	}

	// Codex states the count itself: two held, none applicable now — with
	// the detail call, and with the summary alone.
	codex := vendorOf(t, "codex")
	r = adapterRun(codex, adapterDocs(t, "usage", codexUsageV4Fixture, "reset_credits", codexResetCreditsFixture), nil, v4Now).Resets
	if r == nil || r.Left != 2 || usable(r) != 0 || r.NeedsLimit || len(r.Clears) != 0 {
		t.Errorf("codex applicable: %+v", r)
	}
	r = adapterRun(codex, adapterDocs(t, "usage", codexUsageV4Fixture), nil, v4Now).Resets
	if r == nil || r.Left != 2 || usable(r) != 0 {
		t.Errorf("codex summary applicable: %+v", r)
	}
	// A vendor that says nothing about it: no claim either way.
	r = adapterRun(codex, adapterDocs(t, "usage", `{"rate_limit":{"primary_window":{"used_percent":1}},"rate_limit_reset_credits":{"available_count":1}}`), nil, v4Now).Resets
	if r == nil || r.Usable != nil {
		t.Errorf("no count stated is no claim: %+v", r)
	}
	// And never more usable than held.
	r = adapterRun(codex, adapterDocs(t, "usage", `{"rate_limit":{"primary_window":{"used_percent":1}},"rate_limit_reset_credits":{"available_count":1,"applicable_available_count":5}}`), nil, v4Now).Resets
	if usable(r) != 1 {
		t.Errorf("usable is at most held: %+v", r)
	}
}

// Whether the account is at its limit now (2026.10.07.4): the vendor's own
// word, any one flag stated — Codex's account-wide limit_reached or allowed,
// never an additional model's; Claude's cedar_ember.at_limit. Only ever a
// yes: a field that is not there says nothing.
func TestAdapterLimitedIsTheVendorsOwnWord(t *testing.T) {
	codex, claude := vendorOf(t, "codex"), vendorOf(t, "claude")
	limited := func(ad *vendorAdapter, body string) bool {
		return adapterRun(ad, adapterDocs(t, "usage", body), nil, v4Now).Limited
	}
	if limited(codex, codexUsageFixture) || limited(codex, codexUsageV4Fixture) {
		t.Error("allowed and not reached is not at the limit")
	}
	for name, body := range map[string]string{
		"limit_reached": `{"rate_limit":{"allowed":true,"limit_reached":true,"primary_window":{"used_percent":100}}}`,
		"not allowed":   `{"rate_limit":{"allowed":false,"limit_reached":false,"primary_window":{"used_percent":97}}}`,
		"camelCase":     `{"rateLimit":{"limitReached":true,"primaryWindow":{"usedPercent":100}}}`,
	} {
		if !limited(codex, body) {
			t.Errorf("%s: should be at the limit", name)
		}
	}
	// A model's own limit spent is that model's, not the account's.
	if limited(codex, `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":3}},
	  "additional_rate_limits":[{"limit_name":"Spark","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100}}}]}`) {
		t.Error("an additional limit reached is not the account at its limit")
	}
	if limited(claude, claudeCedarFixture) {
		t.Error("Claude's fixture says at_limit false")
	}
	if !limited(claude, `{"limits":[{"kind":"session","percent":100}],"cedar_ember":{"eligible":true,"at_limit":true,"grants":[]}}`) {
		t.Error("Claude at_limit true")
	}
	if limited(claude, `{"limits":[{"kind":"session","percent":100}]}`) {
		t.Error("no cedar_ember block says nothing")
	}
	// A flag with nothing to equal never holds, whatever is there.
	f := adapterFlag{From: "usage", Path: "rate_limit.allowed"}
	if f.holds(adapterDocs(t, "usage", `{"rate_limit":{"allowed":false}}`), nil) {
		t.Error("a flag with no eq/in is no flag")
	}
}

func TestCodexCreditsResetsAndRenewal(t *testing.T) {
	ad := vendorOf(t, "codex")
	docs := adapterDocs(t, "usage", codexUsageV4Fixture, "reset_credits", codexResetCreditsFixture,
		"subscription", `{"id":"s","entitlement":{"has_active_subscription":true,"renews_at":"2026-10-10T09:03:46+00:00","expires_at":"2026-10-10T15:03:46+00:00"}}`)
	r := adapterRun(ad, docs, nil, v4Now)
	if r.Credits == nil || r.Credits.Balance != "12.5" || !r.Credits.HasCredits || r.Credits.Unlimited {
		t.Errorf("credits: %+v", r.Credits)
	}
	if r.Resets == nil || r.Resets.Left != 2 || r.Resets.Total != nil {
		t.Fatalf("resets: %+v", r.Resets)
	}
	// Soonest first, and the redeemed one is not a lapse anybody holds.
	if len(r.Resets.Expires) != 2 || r.Resets.Expires[0] != 1792702114 || r.Resets.Expires[1] != 1793301312 {
		t.Errorf("expiries: %v", r.Resets.Expires)
	}
	if r.PlanKind != "renews" || r.PlanAt == nil || *r.PlanAt != 1791623026 {
		t.Errorf("renews: %s %v", r.PlanKind, r.PlanAt)
	}

	// The detail call failed: the summary's count stands, with no dates.
	r = adapterRun(ad, adapterDocs(t, "usage", codexUsageV4Fixture), nil, v4Now)
	if r.Resets == nil || r.Resets.Left != 2 || len(r.Resets.Expires) != 0 {
		t.Errorf("summary fallback: %+v", r.Resets)
	}
	// …but a detail call that says none are held answers, whatever the
	// summary still claims.
	none := adapterDocs(t, "usage", codexUsageV4Fixture, "reset_credits", `{"credits":[],"available_count":0}`)
	if r := adapterRun(ad, none, nil, v4Now); r.Resets != nil {
		t.Errorf("zero held must show nothing: %+v", r.Resets)
	}

	// A team account with no credits to its name has no balance row.
	zero := adapterDocs(t, "usage", `{"rate_limit":{"primary_window":{"used_percent":1}},"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`)
	if r := adapterRun(ad, zero, nil, v4Now); r.Credits != nil {
		t.Errorf("a zero balance with no credits is not a row: %+v", r.Credits)
	}

	// No live subscription answer: the credential's claim, but only while it
	// is still ahead — a token minted before the last renewal is stale — and
	// as what it says: how long the period runs, not that it renews.
	stale := map[string]any{"id_token": map[string]any{"chatgpt_subscription_active_until": "2026-09-10T09:03:46+00:00"}}
	if r := adapterRun(ad, adapterDocs(t, "usage", codexUsageV4Fixture), stale, v4Now); r.PlanAt != nil {
		t.Errorf("a plan date in the past was shown: %v", *r.PlanAt)
	}
	fresh := map[string]any{"id_token": map[string]any{"chatgpt_subscription_active_until": "2026-11-10T09:03:46+00:00"}}
	if r := adapterRun(ad, adapterDocs(t, "usage", codexUsageV4Fixture), fresh, v4Now); r.PlanKind != "until" || r.PlanAt == nil || *r.PlanAt != 1794301426 {
		t.Errorf("the claim's date, still ahead: %s %v", r.PlanKind, r.PlanAt)
	}
}

// The plan's date says what it is (2026.10.07.6): renews where the vendor
// says so, ends where it says the plan ends and names no renewal at all, and
// on renewal day — the renewal just past, the expiry hours ahead — neither.
func TestPlanDateSaysWhatItIs(t *testing.T) {
	ad := vendorOf(t, "codex")
	sub := func(entitlement string) adapterReading {
		return adapterRun(ad, adapterDocs(t, "usage", codexUsageV4Fixture,
			"subscription", `{"id":"s","entitlement":`+entitlement+`}`), nil, v4Now)
	}
	if r := sub(`{"has_active_subscription":true,"renews_at":null,"expires_at":"2026-10-20T09:03:46+00:00"}`); r.PlanKind != "ends" || r.PlanAt == nil || *r.PlanAt != 1792487026 {
		t.Errorf("cancelled, it ends: %s %v", r.PlanKind, r.PlanAt)
	}
	if r := sub(`{"has_active_subscription":true,"expires_at":"2026-10-20T09:03:46+00:00"}`); r.PlanKind != "ends" {
		t.Errorf("no renewal named, it ends: %s %v", r.PlanKind, r.PlanAt)
	}
	if r := sub(`{"has_active_subscription":true,"renews_at":"2026-10-07T09:03:46+00:00","expires_at":"2026-10-07T15:03:46+00:00"}`); r.PlanAt != nil {
		t.Errorf("renewal day is not an end: %s %v", r.PlanKind, *r.PlanAt)
	}

	// What the live call said is kept as it said it: an end stays an end
	// when the next call fails and only the claim is left.
	key := "cpa:test-ends"
	claim := map[string]any{"id_token": map[string]any{"chatgpt_subscription_active_until": "2026-10-20T09:03:46+00:00"}}
	r := adapterRun(ad, adapterDocs(t, "usage", codexUsageV4Fixture,
		"subscription", `{"entitlement":{"expires_at":"2026-10-20T09:03:46+00:00"}}`), claim, v4Now)
	first := Provider{OK: true, planClaim: r.PlanFromClaim}
	first.setPlanDate(r.PlanAt, r.PlanKind)
	adapterKeep(key, &first, v4Now)
	r = adapterRun(ad, adapterDocs(t, "usage", codexUsageV4Fixture), claim, v4Now+60)
	next := Provider{OK: true, planClaim: r.PlanFromClaim}
	next.setPlanDate(r.PlanAt, r.PlanKind)
	adapterKeep(key, &next, v4Now+60)
	if next.EndsAt == nil || *next.EndsAt != 1792487026 || next.ActiveUntil != nil || next.RenewsAt != nil {
		t.Errorf("the kept end became something else: renews %v ends %v until %v", next.RenewsAt, next.EndsAt, next.ActiveUntil)
	}
}

// The account id CPA lists is a decoded object today and a raw JWT on older
// builds; the header and the subscription URL take whichever is there, and
// never both at once.
func TestCodexAccountIdEitherForm(t *testing.T) {
	ad := vendorOf(t, "codex")
	var usage, sub *adapterRequest
	for i := range ad.Requests {
		switch ad.Requests[i].ID {
		case "usage":
			usage = &ad.Requests[i]
		case "subscription":
			sub = &ad.Requests[i]
		}
	}
	if usage == nil || sub == nil {
		t.Fatal("codex adapter lacks its usage or subscription request")
	}
	jwt := "h." + "eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdC0xIn19" + ".s"
	for _, cred := range []map[string]any{
		{"id_token": map[string]any{"chatgpt_account_id": "acct-1"}},
		{"id_token": jwt},
	} {
		ctx := adapterCtx{cred}
		if got := adapterTemplate(usage.Headers["Chatgpt-Account-Id"], ctx, cred); got != "acct-1" {
			t.Errorf("header from %T: %q", cred["id_token"], got)
		}
		if got := adapterTemplateURL(sub.URL, ctx, cred); got != "https://chatgpt.com/backend-api/subscriptions?account_id=acct-1" {
			t.Errorf("subscription URL from %T: %q", cred["id_token"], got)
		}
	}
}

// A value reaching a URL is escaped where it lands and cannot move the host.
func TestAdapterURLTemplateEscapes(t *testing.T) {
	cred := map[string]any{"a": "x&y=1/@evil.example#z"}
	got := adapterTemplateURL("https://chatgpt.com/backend-api/subscriptions?account_id={cred.a}", adapterCtx{cred}, cred)
	if got != "https://chatgpt.com/backend-api/subscriptions?account_id=x%26y%3D1%2F%40evil.example%23z" {
		t.Errorf("escaped: %q", got)
	}
	if !cpaHostAllowed(got) || hostOf(got) != "chatgpt.com" {
		t.Errorf("the host moved: %q", hostOf(got))
	}
}

func TestAntigravityPlanFromCodeAssist(t *testing.T) {
	ad := vendorOf(t, "antigravity")
	for _, c := range []struct{ assist, want string }{
		{`{"currentTier":{"id":"free-tier","name":"Antigravity"},"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`, "Pro"},
		{`{"currentTier":{"id":"free-tier"}}`, "Free"},
		{`{"paidTier":{"id":"g1-ultra-tier"}}`, "Ultra"},
		{`{"currentTier":{"id":"standard-tier"}}`, "standard-tier"},
	} {
		docs := adapterDocs(t, "quota", `{"groups":[{"displayName":"Gemini","buckets":[{"window":"5h","remainingFraction":0.9}]}]}`, "assist", c.assist)
		if r := adapterRun(ad, docs, nil, v4Now); r.Plan != c.want {
			t.Errorf("%s: plan %q, want %q", c.assist, r.Plan, c.want)
		}
	}
}

func TestKimiMonthlyWindow(t *testing.T) {
	docs := adapterDocs(t, "usage", `{"usage":{"limit":"2048","used":"117","resetTime":"2026-10-12T00:00:00Z"},
	  "usages":{"limit_month_total":{"used_ratio":0.42,"reset_time":"2026-11-01T00:00:00Z"}}}`)
	r := adapterRun(vendorOf(t, "kimi"), docs, nil, v4Now)
	var month *Limit
	for i := range r.Limits {
		if r.Limits[i].Key == "monthly" {
			month = &r.Limits[i]
		}
	}
	// Labelled "month", not "30d": a calendar month is not thirty days, and
	// the table does not pretend to know the length (CodexBar
	// KimiUsageSnapshot, steipete/CodexBar@51ed16bdd3: "Total usage").
	if month == nil || month.UsedPercent != 42 || month.ResetsAt == nil || *month.WindowLabel != "month" {
		t.Errorf("monthly: %+v", month)
	}
}

// The Code API's ratio pools, `usages.limit_7d` / `limit_5h` (CodexBar
// KimiModels, @51ed16bdd3), when the counters are not there; and when both
// are, the counter — a 0 ratio beside a counter of 19 is a placeholder
// (@fd2414d295), and the table has no way to say that except by order.
func TestKimiRatioPools(t *testing.T) {
	pools := adapterDocs(t, "usage", `{"usages":{"limit_7d":{"used_ratio":0.125,"reset_time":"2026-09-20T00:00:00Z"},"limit_5h":{"used_ratio":0.625}}}`)
	r := adapterRun(vendorOf(t, "kimi"), pools, nil, v4Now)
	by := map[string]Limit{}
	for _, l := range r.Limits {
		by[l.Key] = l
	}
	if w := by["weekly"]; w.UsedPercent != 12.5 || w.WindowLabel == nil || *w.WindowLabel != "7d" || w.ResetsAt == nil {
		t.Errorf("weekly pool: %+v", w)
	}
	if s := by["rate"]; s.UsedPercent != 62.5 || s.WindowLabel == nil || *s.WindowLabel != "5h" {
		t.Errorf("session pool: %+v", s)
	}
	mixed := adapterDocs(t, "usage", `{"usage":{"limit":"100","used":"19","remaining":"81"},
	  "usages":{"limit_7d":{"used_ratio":0,"reset_time":"2026-09-19T16:45:58Z"}}}`)
	r = adapterRun(vendorOf(t, "kimi"), mixed, nil, v4Now)
	if len(r.Limits) != 1 || r.Limits[0].Key != "weekly" || r.Limits[0].UsedPercent != 19 {
		t.Errorf("the counter stands over the zero placeholder: %+v", r.Limits)
	}
}

// The plan is the membership level the Code API names, in the names Kimi
// gives its tiers (CodexBar KimiModels, @babfb51da4).
func TestKimiMembershipPlan(t *testing.T) {
	docs := adapterDocs(t, "usage", `{"user":{"membership":{"level":"LEVEL_ADVANCED"}},"version":"GOODS_VERSION_V1",
	  "usage":{"limit":"100","used":"1","remaining":"99"}}`)
	if r := adapterRun(vendorOf(t, "kimi"), docs, nil, v4Now); r.Plan != "Allegro" {
		t.Errorf("plan: %q", r.Plan)
	}
	docs = adapterDocs(t, "usage", `{"user":{"membership":{"level":"LEVEL_UNSPECIFIED"}},"usage":{"limit":"100","used":"1","remaining":"99"}}`)
	if r := adapterRun(vendorOf(t, "kimi"), docs, nil, v4Now); r.Plan != "" {
		t.Errorf("unspecified is no plan: %q", r.Plan)
	}
}

// When the proxy sends no percentage, the on-demand cap and what is used of
// it are the share; and the period's end is `billingPeriodEnd` when there
// is no `currentPeriod` (CodexBar GrokCreditsProxyFetcher, @79077dd72a).
// A cap of zero is no share at all — not 0% (@0b715a4f6f).
func TestXaiOnDemandFallback(t *testing.T) {
	docs := adapterDocs(t, "weekly", `{"config":{"billingPeriodEnd":"2026-08-13T00:00:00Z","onDemandCap":{"val":1000},"onDemandUsed":{"val":250}}}`)
	r := adapterRun(vendorOf(t, "xai"), docs, nil, v4Now)
	if len(r.Limits) != 1 || r.Limits[0].Key != "credits" || r.Limits[0].UsedPercent != 25 ||
		r.Limits[0].ResetsAt == nil || *r.Limits[0].ResetsAt != isoToEpoch("2026-08-13T00:00:00Z") {
		t.Fatalf("on-demand share: %+v", r.Limits)
	}
	if r.Limits[0].Unit != "" || r.Limits[0].UsedAmount != nil {
		t.Errorf("the on-demand figures are in no unit this wire names: %+v", r.Limits[0])
	}
	zero := adapterDocs(t, "weekly", `{"config":{"onDemandCap":{"val":0},"onDemandUsed":{"val":0}},"subscriptionTier":"SuperGrok Heavy"}`)
	if r := adapterRun(vendorOf(t, "xai"), zero, nil, v4Now); len(r.Limits) != 0 {
		t.Errorf("a zero cap is unknown, not 0%%: %+v", r.Limits)
	}
	// A reported percentage still wins over the ratio.
	both := adapterDocs(t, "weekly", `{"config":{"creditUsagePercent":12.5,"onDemandCap":{"val":1000},"onDemandUsed":{"val":250}}}`)
	if r := adapterRun(vendorOf(t, "xai"), both, nil, v4Now); len(r.Limits) != 1 || r.Limits[0].UsedPercent != 12.5 {
		t.Errorf("reported percent: %+v", r.Limits)
	}
}

// The quota summary in every shape its three sources send it (CodexBar
// AntigravityQuotaSummaryParser, @62acfd8ead): a `response` wrapper, the
// fraction nested under `remaining`, the period stated as `window`, a bucket
// `disabled`; and the CLI report's `id` / `name` spelling.
func TestAntigravitySummaryShapes(t *testing.T) {
	wrapped := adapterDocs(t, "quota", `{"response":{"description":"Within each group…","groups":[
	  {"displayName":"Gemini Models","buckets":[
	    {"bucketId":"gemini-weekly","displayName":"Weekly Limit","remaining":{"remainingFraction":0.82},"resetTime":"2026-06-19T08:45:39Z"},
	    {"bucketId":"gemini-5h","displayName":"Five Hour Limit","remaining":{"remainingFraction":0.91},"resetTime":"2026-06-15T11:39:34Z"}]},
	  {"displayName":"Claude and GPT models","buckets":[
	    {"bucketId":"3p-weekly","remaining":{"remainingFraction":0.5}},
	    {"bucketId":"3p-5h","remaining":{"remainingFraction":1},"disabled":true}]}]}}`)
	r := adapterRun(vendorOf(t, "antigravity"), wrapped, nil, v4Now)
	if len(r.Limits) != 3 {
		t.Fatalf("three live buckets (one disabled): %+v", r.Limits)
	}
	if l := r.Limits[0]; l.Key != "gemini-weekly" || l.UsedPercent != 18 || l.WindowLabel == nil || *l.WindowLabel != "7d" ||
		l.Scope == nil || *l.Scope != "Gemini Models" || l.ResetsAt == nil {
		t.Errorf("weekly: %+v", l)
	}
	if l := r.Limits[1]; l.Key != "gemini-5h" || l.UsedPercent != 9 || *l.WindowLabel != "5h" {
		t.Errorf("5h: %+v", l)
	}
	// Flat fields with the window stated: the window names the period, the
	// id notwithstanding.
	flat := adapterDocs(t, "quota", `{"groups":[{"name":"Gemini Models","buckets":[
	  {"id":"gemini-5h","name":"Five Hour","window":"weekly","remaining_fraction":0.95,"reset_time":"2026-09-13T03:48:04Z"}]}]}`)
	r = adapterRun(vendorOf(t, "antigravity"), flat, nil, v4Now)
	if len(r.Limits) != 1 || r.Limits[0].Key != "gemini-5h" || *r.Limits[0].WindowLabel != "7d" || *r.Limits[0].Scope != "Gemini Models" || r.Limits[0].UsedPercent != 5 {
		t.Errorf("stated window: %+v", r.Limits)
	}
}

func TestXaiPlanFromSettings(t *testing.T) {
	docs := adapterDocs(t, "weekly", `{"config":{"creditUsagePercent":3}}`,
		"settings", `{"subscription_tier_display":"SuperGrok Heavy","subscription_tier":"SUPERGROK_HEAVY"}`)
	if r := adapterRun(vendorOf(t, "xai"), docs, nil, v4Now); r.Plan != "SuperGrok Heavy" {
		t.Errorf("plan: %q", r.Plan)
	}
}

func TestDevinStatus(t *testing.T) {
	docs := adapterDocs(t, "status", `{"userStatus":{"planStatus":{
	  "planInfo":{"planName":"Pro"},
	  "dailyQuotaRemainingPercent":80,"dailyQuotaResetAtUnix":"1791417600",
	  "weeklyQuotaRemainingPercent":"35.5","weeklyQuotaResetAtUnix":1791849600,
	  "planStart":"2026-10-01T00:00:00Z","planEnd":"2026-11-01T00:00:00Z"}}}`)
	r := adapterRun(vendorOf(t, "devin"), docs, nil, v4Now)
	if len(r.Limits) != 2 || r.Limits[0].Key != "daily" || r.Limits[0].UsedPercent != 20 ||
		r.Limits[0].ResetsAt == nil || *r.Limits[0].ResetsAt != 1791417600 ||
		r.Limits[1].Key != "weekly" || r.Limits[1].UsedPercent != 64.5 {
		t.Errorf("windows: %+v", r.Limits)
	}
	// planEnd is the period's end, renewing or not: only how long it runs.
	if r.Plan != "Pro" || r.PlanKind != "until" || r.PlanAt == nil || *r.PlanAt != 1793491200 {
		t.Errorf("plan %q, %s %v", r.Plan, r.PlanKind, r.PlanAt)
	}
}

// End to end through the CPA rung's fetch: which requests go out, what they
// carry, and that the reading takes the credits, the resets and the date.
func TestCodexFetchCarriesTheNewFields(t *testing.T) {
	ad := vendorOf(t, "codex")
	cred := map[string]any{"id_token": map[string]any{"chatgpt_account_id": "acct-9"}}
	asked := map[string]map[string]string{}
	send := func(method, target string, header map[string]string, data string) (int, []byte, *Provider) {
		asked[target] = header
		switch {
		case strings.HasSuffix(target, "/wham/usage"):
			return 200, []byte(codexUsageV4Fixture), nil
		case strings.HasSuffix(target, "/wham/rate-limit-reset-credits"):
			return 200, []byte(codexResetCreditsFixture), nil
		case strings.Contains(target, "/subscriptions?account_id=acct-9"):
			return 403, []byte("<html>challenge</html>"), nil // Cloudflare, as seen
		}
		return 404, nil, nil
	}
	p := adapterFetch(Provider{ID: "codex", Source: "cpa"}, ad, cred, "cpa", send)
	if !p.OK {
		t.Fatalf("a failed optional call sank the reading: %+v", p)
	}
	if h := asked["https://chatgpt.com/backend-api/wham/usage"]; h == nil || h["Chatgpt-Account-Id"] != "acct-9" {
		t.Errorf("usage header: %v", h)
	}
	if h := asked["https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"]; h == nil || h["OpenAI-Beta"] != "codex-1" {
		t.Errorf("reset-credits header: %v", h)
	}
	if p.Credits == nil || p.Resets == nil || p.Resets.Left != 2 || len(p.Resets.Expires) != 2 {
		t.Errorf("fields: credits %+v resets %+v", p.Credits, p.Resets)
	}
	if d, kind := p.planDate(); d != nil {
		t.Errorf("a refused subscription call cannot give a date: %s %v", kind, *d)
	}
}

// An optional call that failed this time does not take back what it said
// last time — while the date is still ahead, and while the count it dated
// has not moved.
func TestAdapterKeepsWhatTheOptionalCallsSaid(t *testing.T) {
	key := "cpa:test-keep"
	first := Provider{OK: true, RenewsAt: fp(v4Now + 86400), Resets: &ResetCredits{Left: 2, Expires: []float64{v4Now + 3600, v4Now + 7200}}}
	adapterKeep(key, &first, v4Now)

	later := Provider{OK: true, Resets: &ResetCredits{Left: 2}}
	adapterKeep(key, &later, v4Now+5000)
	if later.RenewsAt == nil || *later.RenewsAt != v4Now+86400 {
		t.Errorf("the renewal date was dropped: %v", later.RenewsAt)
	}
	if len(later.Resets.Expires) != 1 || later.Resets.Expires[0] != v4Now+7200 {
		t.Errorf("only the lapse still ahead should be kept: %v", later.Resets.Expires)
	}

	spent := Provider{OK: true, Resets: &ResetCredits{Left: 1}}
	adapterKeep(key, &spent, v4Now+5000)
	if len(spent.Resets.Expires) != 0 {
		t.Errorf("a count that moved cannot keep the old dates: %v", spent.Resets.Expires)
	}

	past := Provider{OK: true}
	adapterKeep(key, &past, v4Now+90000)
	if past.RenewsAt != nil {
		t.Errorf("a renewal date that has passed was kept: %v", *past.RenewsAt)
	}
	failed := Provider{OK: false}
	adapterKeep(key, &failed, v4Now)
	if failed.RenewsAt != nil {
		t.Error("a failed reading must not be dressed with kept fields")
	}
}

// Review 2026-10-07, one test per finding.

// An optional request whose channel fails — CPA timing out on a slow vendor —
// costs its own fields, not the reading the required ones already made.
func TestOptionalRequestChannelFailureKeepsTheReading(t *testing.T) {
	ad := vendorOf(t, "codex")
	cred := map[string]any{"id_token": map[string]any{"chatgpt_account_id": "acct-9"}}
	send := func(method, target string, header map[string]string, data string) (int, []byte, *Provider) {
		switch {
		case strings.HasSuffix(target, "/wham/usage"):
			return 200, []byte(codexUsageV4Fixture), nil
		}
		q := Provider{ID: "codex"}
		q.failWith("unreachable", "cpa-down")
		return 0, nil, &q
	}
	p := adapterFetch(Provider{ID: "codex", Source: "cpa"}, ad, cred, "cpa", send)
	if !p.OK || len(p.Limits) == 0 {
		t.Fatalf("an optional call's channel failure sank the reading: %+v", p)
	}
	if p.Resets == nil || p.Resets.Left != 2 || len(p.Resets.Expires) != 0 {
		t.Errorf("the summary's count should stand, undated: %+v", p.Resets)
	}
	// The required call failing that way is still the reading's failure.
	down := func(method, target string, header map[string]string, data string) (int, []byte, *Provider) {
		q := Provider{ID: "codex"}
		q.failWith("unreachable", "cpa-down")
		return 0, nil, &q
	}
	if p := adapterFetch(Provider{ID: "codex", Source: "cpa"}, ad, cred, "cpa", down); p.OK || p.DetailCode != "cpa-down" {
		t.Errorf("a required call's channel failure must stand: %+v", p)
	}
}

// A detail answer with no list and no count is no answer; the summary's
// count behind it stands.
func TestResetCreditsEmptyAnswerFallsThrough(t *testing.T) {
	docs := adapterDocs(t, "usage", codexUsageV4Fixture, "reset_credits", `{}`)
	r := adapterRun(vendorOf(t, "codex"), docs, nil, v4Now)
	if r.Resets == nil || r.Resets.Left != 2 {
		t.Errorf("`{}` blocked the summary's count: %+v", r.Resets)
	}
}

// 2 dated → 1 undated → 2 undated: the dates of the first two must not come
// back for whichever two are held at the end.
func TestKeptLapsesForgetAMovedCount(t *testing.T) {
	key := "cpa:test-moved"
	a := Provider{OK: true, Resets: &ResetCredits{Left: 2, Expires: []float64{v4Now + 3600, v4Now + 7200}}}
	adapterKeep(key, &a, v4Now)
	b := Provider{OK: true, Resets: &ResetCredits{Left: 1}}
	adapterKeep(key, &b, v4Now+10)
	c := Provider{OK: true, Resets: &ResetCredits{Left: 2}}
	adapterKeep(key, &c, v4Now+20)
	if len(c.Resets.Expires) != 0 {
		t.Errorf("dates of another set of credits came back: %v", c.Resets.Expires)
	}
}

// A live date remembered outranks the credential's own claim when the live
// call fails — the claim can be a renewal behind, and still in the future.
func TestKeptLiveRenewalOutranksTheClaim(t *testing.T) {
	ad := vendorOf(t, "codex")
	key := "cpa:test-claim"
	claim := map[string]any{"id_token": map[string]any{"chatgpt_subscription_active_until": "2026-10-10T09:03:46+00:00"}}

	live := adapterDocs(t, "usage", codexUsageV4Fixture,
		"subscription", `{"entitlement":{"renews_at":"2026-11-10T09:03:46+00:00"}}`)
	r := adapterRun(ad, live, claim, v4Now)
	first := Provider{OK: true, planClaim: r.PlanFromClaim}
	first.setPlanDate(r.PlanAt, r.PlanKind)
	adapterKeep(key, &first, v4Now)
	if first.RenewsAt == nil || *first.RenewsAt != 1794301426 || r.PlanFromClaim {
		t.Fatalf("live date: %v (claim %v)", first.RenewsAt, r.PlanFromClaim)
	}

	r = adapterRun(ad, adapterDocs(t, "usage", codexUsageV4Fixture), claim, v4Now+60)
	if r.PlanAt == nil || !r.PlanFromClaim || r.PlanKind != "until" {
		t.Fatalf("without the live call the claim answers, as how long the period runs: %s %v %v", r.PlanKind, r.PlanAt, r.PlanFromClaim)
	}
	next := Provider{OK: true, planClaim: r.PlanFromClaim}
	next.setPlanDate(r.PlanAt, r.PlanKind)
	adapterKeep(key, &next, v4Now+60)
	if next.RenewsAt == nil || *next.RenewsAt != 1794301426 || next.ActiveUntil != nil {
		t.Errorf("the claim moved the date backwards, or unsaid the renewal: renews %v until %v", next.RenewsAt, next.ActiveUntil)
	}
}

// An answer with no window still names the plan: the native road that runs
// next (local.go collectLocalOr) keeps the chip the table did read, while
// the gauge stays the native road's own.
func TestAdapterFetchNamesThePlanEvenWithNoWindow(t *testing.T) {
	ad := vendorOf(t, "xai")
	send := func(method, target string, header map[string]string, data string) (int, []byte, *Provider) {
		switch {
		case strings.Contains(target, "/v1/billing?format=credits"):
			return 200, []byte(`{"config":{"onDemandCap":{"val":0},"onDemandUsed":{"val":0}}}`), nil
		case strings.HasSuffix(target, "/v1/settings"):
			return 200, []byte(`{"subscription_tier_display":"SuperGrok Heavy"}`), nil
		}
		return 404, nil, nil
	}
	p := adapterFetch(Provider{ID: "grok", Source: "api"}, ad, nil, "local", send)
	if p.OK || p.DetailCode != "no-window" {
		t.Fatalf("no percentage anywhere is no window: %+v", p)
	}
	if p.PlanType == nil || *p.PlanType != "SuperGrok Heavy" {
		t.Errorf("the plan the table read should ride on the failure: %+v", p.PlanType)
	}
}
