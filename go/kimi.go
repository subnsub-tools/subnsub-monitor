package main

// Kimi Code: quota costs a credential Kimi's own CLI left on disk.
//
// The kimi-code CLI stores its OAuth access token at
// ~/.kimi-code/credentials/kimi-code.json ({"access_token":…,"expires_at":…}),
// and Kimi's billing gateway answers to it over Connect-style POSTs:
//
//	POST https://www.kimi.com/apiv2/kimi.gateway.billing.v1.BillingService/GetUsages
//	  {"scope":["FEATURE_CODING"]}
//	  → {"usages":[{"scope":"FEATURE_CODING",
//	       "detail":{"limit":"2048","used":"117","resetTime":…},
//	       "limits":[{"detail":{…},"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"}}]}]}
//
// FEATURE_CODING's detail is the weekly quota; its limits[] carry the short
// rate window, whose length is a {duration, timeUnit} pair (CodexBar
// KimiModels.KimiWindow; 300 minutes when absent). Every number arrives as a
// STRING, which is why the parse below goes through ampNum rather than a
// typed struct — Codex's lesson about files applies to gateways too.
//
// The plan chip comes from a second call, GetSubscription (CodexBar
// KimiUsageFetcher, steipete/CodexBar@babfb51da4): the goods title of an
// active subscription. GetSubscriptionStats, which this read before, names
// the balance's TYPE ("SUBSCRIPTION") there, not the plan — a chip reading
// "SUBSCRIPTION" was that field. It is read again now for what it does
// carry, the membership's own windows (kimiWithMembership): the month's
// shared balance and the Code 7-day ratio — the only reading a lapsed Code
// plan has left.
//
// Claude's cost rung, Claude's rules (claude.go): read two scalars from the
// credential file, never refresh, never write back, publish no transport
// text. Protocol per CodexBar's Kimi provider, credential-file path.
// KIMI_CODE_HOME moves the directory, same as the CLI honours;
// MON_KIMI_TOKEN overrides the token outright.
//
// The gateway has a twin at kimi.ai for accounts outside China (CodexBar
// KimiRegion, @7618b4fdda); the CLI's credential file carries no issuing
// host, and CodexBar keeps it China-only, so this does too. The request
// headers are the browser's, as CodexBar sends them: the gateway is the
// web console's, and Origin/Referer are derived from the host.

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	kimiOrigin          = "https://www.kimi.com"
	kimiUsagesURL       = kimiOrigin + "/apiv2/kimi.gateway.billing.v1.BillingService/GetUsages"
	kimiSubscriptionURL = kimiOrigin + "/apiv2/kimi.gateway.membership.v2.MembershipService/GetSubscription"
	kimiStatsURL        = kimiOrigin + "/apiv2/kimi.gateway.membership.v2.MembershipService/GetSubscriptionStats"
	kimiTokenEnv        = "MON_KIMI_TOKEN"
	kimiHomeEnv         = "KIMI_CODE_HOME"
	// The console's own identity, as CodexBar's KimiUsageFetcher sends it.
	kimiUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
	// The rate window's length when the gateway names none.
	kimiDefaultWindowMinutes = 300
)

var kimiCache = provCache{pace: paceMark{id: "kimi"}}

func collectKimi() Provider {
	return kimiCache.collect(fetchKimi)
}

// The token and its deadline. expires_at has been seen in both seconds and
// milliseconds; disambiguated by magnitude, the same trap types.go documents.
func kimiToken() (string, float64) {
	if v := strings.TrimSpace(os.Getenv(kimiTokenEnv)); v != "" {
		return v, 0
	}
	dir := strings.TrimSpace(os.Getenv(kimiHomeEnv))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", 0
		}
		dir = filepath.Join(home, ".kimi-code")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "credentials", "kimi-code.json"))
	if err != nil {
		return "", 0
	}
	var doc struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   any    `json:"expires_at"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.AccessToken == "" {
		return "", 0
	}
	exp := 0.0
	if v := asNum(doc.ExpiresAt); v != nil {
		exp = *v
		if exp > 1e12 {
			exp /= 1000
		}
	}
	return doc.AccessToken, exp
}

// A Kimi usage detail: strings wearing numbers. used% needs used and limit;
// a missing used with a present remaining is derived instead. Nil when the
// shape carries no reading either way.
type kimiDetail struct {
	Limit     any `json:"limit"`
	Used      any `json:"used"`
	Remaining any `json:"remaining"`
	ResetTime any `json:"resetTime"`
	ResetAt   any `json:"resetAt"`
}

func kimiNum(v any) *float64 {
	switch x := v.(type) {
	case float64:
		if finite(x) {
			return &x
		}
	case string:
		if n, ok := ampNum(x); ok {
			return &n
		}
	}
	return nil
}

func kimiUsed(d *kimiDetail) *float64 {
	used, limit, _ := kimiAmounts(d)
	if used == nil || limit == nil {
		return nil
	}
	pct := *used / *limit * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return fp(round2(pct))
}

// The amount triplet behind a Kimi detail. The gateway sometimes omits used
// and sometimes omits remaining, so derive only the missing side from the real
// denominator. No unit is attached: the API names FEATURE_CODING but does not
// promise whether the number represents requests, credits, or another counter.
func kimiAmounts(d *kimiDetail) (used, total, remaining *float64) {
	if d == nil {
		return nil, nil, nil
	}
	total = kimiNum(d.Limit)
	if total == nil || *total <= 0 {
		return nil, nil, nil
	}
	used = kimiNum(d.Used)
	remaining = kimiNum(d.Remaining)
	if used != nil && *used < 0 {
		used = fp(0)
	}
	if remaining != nil {
		if *remaining < 0 {
			remaining = fp(0)
		} else if *remaining > *total {
			remaining = fp(*total)
		}
	}
	if used == nil && remaining != nil {
		u := *total - *remaining
		used = &u
	}
	if remaining == nil && used != nil {
		r := *total - *used
		remaining = &r
	}
	if remaining != nil && *remaining < 0 {
		remaining = fp(0)
	}
	if used == nil {
		return nil, nil, nil
	}
	used = fp(round2(*used))
	total = fp(round2(*total))
	if remaining != nil {
		remaining = fp(round2(*remaining))
	}
	return used, total, remaining
}

func kimiReset(d *kimiDetail) *float64 {
	for _, v := range []any{d.ResetTime, d.ResetAt} {
		switch x := v.(type) {
		case string:
			if e := isoToEpoch(x); e > 0 {
				return fp(e)
			}
			if n, ok := ampNum(x); ok && n > 0 {
				if n > 1e12 {
					n /= 1000
				}
				return fp(n)
			}
		case float64:
			if x > 1e12 {
				x /= 1000
			}
			if x > 0 && finite(x) {
				return fp(x)
			}
		}
	}
	return nil
}

func fetchKimi() Provider {
	p := Provider{ID: "kimi", Name: "Kimi Code", Source: "api", CapturedAt: now()}
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}

	token, exp := kimiToken()
	if token == "" {
		return fail("not-signed-in", "creds-missing", "~/.kimi-code/credentials/")
	}
	if exp > 0 && exp < now() {
		return fail("token-expired", "creds-expired", "kimi")
	}
	hdr := map[string]string{
		"Authorization":            "Bearer " + token,
		"Content-Type":             "application/json",
		"Accept":                   "*/*",
		"Accept-Language":          "en-US,en;q=0.9",
		"Origin":                   kimiOrigin,
		"Referer":                  kimiOrigin + "/code/console",
		"User-Agent":               kimiUserAgent,
		"connect-protocol-version": "1",
		"x-language":               "en-US",
		"x-msh-platform":           "web",
	}

	status, body, err := provRequest("POST", kimiUsagesURL, hdr, []byte(`{"scope":["FEATURE_CODING"]}`))
	if err != nil {
		return fail("unreachable", "req-failed")
	}
	switch {
	case status == 401 || status == 403:
		return fail("token-expired", "http-status", itoa(status))
	case status != 200:
		return fail("api-error", "http-status", itoa(status))
	}

	limits, ok := kimiCodingLimits(body)
	if !ok {
		return fail("api-error", "http-shape")
	}
	p.Limits = limits

	// The membership's own windows, a separately-failing request like the
	// plan's below. A lapsed Code plan answers GetUsages with no Code pools
	// at all — `{"totalQuota":{…}}`, no `usages` — and its membership quota
	// is then the only reading there is (CodexBar KimiUsageFetcher,
	// steipete/CodexBar@5a84dca9ba: "keep web usage when a lapsed plan has no
	// Code pools"); with neither, the read fails as it always did.
	if status, body, err := provRequest("POST", kimiStatsURL, hdr, []byte("{}")); err == nil && status == 200 {
		p.Limits = kimiWithMembership(p.Limits, body)
	}
	if len(p.Limits) == 0 {
		return fail("no-readings", "no-window")
	}

	// The subscription is another separately-failing request and carries
	// only the plan chip; losing it costs nothing the gauges need.
	if status, body, err := provRequest("POST", kimiSubscriptionURL, hdr, []byte("{}")); err == nil && status == 200 {
		if plan := kimiPlan(body); plan != "" {
			p.PlanType = sp(plan)
		}
	}

	p.OK = true
	p.RecordedAt = fp(now())
	return p
}

// The FEATURE_CODING pools out of a GetUsages answer: the weekly detail and
// the first of limits[], the short rate window. False only when the body is
// not the gateway's shape at all; an answer with no Code pools — a lapsed
// plan's `{"totalQuota":{…}}`, `usages` absent or null (CodexBar KimiModels,
// steipete/CodexBar@5a84dca9ba made it optional) — is no windows, not a
// broken answer.
func kimiCodingLimits(body []byte) ([]Limit, bool) {
	var doc struct {
		Usages []struct {
			Scope  string      `json:"scope"`
			Detail *kimiDetail `json:"detail"`
			Limits []struct {
				Detail *kimiDetail `json:"detail"`
				Window any         `json:"window"`
			} `json:"limits"`
		} `json:"usages"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil, false
	}
	var out []Limit
	for _, u := range doc.Usages {
		if u.Scope != "FEATURE_CODING" {
			continue
		}
		if used := kimiUsed(u.Detail); used != nil {
			lim := Limit{
				Key: "weekly", UsedPercent: *used,
				WindowLabel: sp("7d"), ResetsAt: kimiReset(u.Detail),
			}
			lim.UsedAmount, lim.TotalAmount, lim.RemainingAmount = kimiAmounts(u.Detail)
			out = append(out, lim)
		}
		// The short rate window rides in limits[]; only the first is the
		// governing one, per Kimi's own client.
		if len(u.Limits) > 0 {
			l := u.Limits[0]
			if used := kimiUsed(l.Detail); used != nil {
				lim := Limit{Key: "rate", UsedPercent: *used, ResetsAt: kimiReset(l.Detail)}
				lim.UsedAmount, lim.TotalAmount, lim.RemainingAmount = kimiAmounts(l.Detail)
				w := kimiWindowMinutes(l.Window)
				lim.WindowMinutes = fp(w)
				lim.WindowLabel = windowLabel(w)
				out = append(out, lim)
			}
		}
		break
	}
	return out, true
}

// The membership's windows out of a GetSubscriptionStats answer, added to
// what GetUsages read (CodexBar KimiUsageSnapshot.toUsageSnapshot):
//
//	{"subscriptionBalance":{"feature":"FEATURE_OMNI","type":"SUBSCRIPTION",
//	   "amountUsedRatio":0.42,"expireTime":"2026-11-22T00:00:00Z"},
//	 "ratelimitCode7d":{"ratio":0.5,"enabled":true,"resetTime":"…"}}
//
// The subscription balance is the month's "Total usage", the pool every
// feature draws on, so its amountUsedRatio and not the Code-only
// kimiCodeUsedRatio; it renews at expireTime, which CodexBar shows as the
// window's reset. Labelled "month" as the table labels the Code API's
// limit_month_total, the same lane (KimiUsageSnapshot prefers that pool
// when it has both). A balance of another feature or type — a GIFT, which
// expires rather than renews — is not this window.
//
// ratelimitCode7d is the Code weekly quota as the membership service counts
// it, FEATURE_CODING's weekly detail through a second endpoint. Where the
// detail is missing (a lapsed plan) it is the weekly reading. Where both are
// there and agree — within a point, and resets within five minutes — it adds
// nothing. Where they disagree it stands as a row of its own, the week scoped
// "Code", rather than one figure hiding the other: a higher ratio on another
// reset is another period, not a worse reading of this one (CodexBar
// KimiUsageSnapshot, kimi-code-7d and isEquivalentToWeeklyWindow). Ratios
// arrive as numbers; strings are taken too, as everything else this gateway
// sends is one.
func kimiWithMembership(limits []Limit, body []byte) []Limit {
	var doc struct {
		SubscriptionBalance *struct {
			Feature         string `json:"feature"`
			Type            string `json:"type"`
			AmountUsedRatio any    `json:"amountUsedRatio"`
			ExpireTime      any    `json:"expireTime"`
		} `json:"subscriptionBalance"`
		RatelimitCode7d *struct {
			Ratio     any   `json:"ratio"`
			Enabled   *bool `json:"enabled"`
			ResetTime any   `json:"resetTime"`
		} `json:"ratelimitCode7d"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return limits
	}
	ratioPct := func(v any) *float64 {
		r := kimiNum(v)
		if r == nil {
			return nil
		}
		return fp(round2(min(max(*r*100, 0), 100)))
	}
	if b := doc.SubscriptionBalance; b != nil &&
		(b.Feature == "" || b.Feature == "FEATURE_OMNI") && (b.Type == "" || b.Type == "SUBSCRIPTION") {
		if used := ratioPct(b.AmountUsedRatio); used != nil && !kimiHasKey(limits, "monthly") {
			limits = append(limits, Limit{
				Key: "monthly", UsedPercent: *used, WindowLabel: sp("month"),
				ResetsAt: kimiReset(&kimiDetail{ResetTime: b.ExpireTime}),
			})
		}
	}
	if c := doc.RatelimitCode7d; c != nil && (c.Enabled == nil || *c.Enabled) {
		if used := ratioPct(c.Ratio); used != nil {
			week := Limit{
				Key: "weekly", UsedPercent: *used, WindowLabel: sp("7d"),
				ResetsAt: kimiReset(&kimiDetail{ResetTime: c.ResetTime}),
			}
			switch i := kimiKeyIndex(limits, "weekly"); {
			case i < 0:
				limits = append(limits, week)
			case !kimiSameWeek(week, limits[i]):
				week.Key, week.Scope = "code_weekly", sp("Code")
				limits = append(limits, week)
			}
		}
	}
	return limits
}

// Two weekly readings of one period: within a point of each other, with
// resets five minutes apart at most. Without both resets there is no telling
// they are the same week, and they are kept apart.
func kimiSameWeek(a, b Limit) bool {
	if a.ResetsAt == nil || b.ResetsAt == nil {
		return false
	}
	return math.Abs(a.UsedPercent-b.UsedPercent) <= 1 && math.Abs(*a.ResetsAt-*b.ResetsAt) <= 300
}

func kimiKeyIndex(limits []Limit, key string) int {
	for i := range limits {
		if limits[i].Key == key {
			return i
		}
	}
	return -1
}

func kimiHasKey(limits []Limit, key string) bool { return kimiKeyIndex(limits, key) >= 0 }

// The rate window's length in minutes: a {duration, timeUnit} pair, a bare
// number of minutes (the shape an earlier gateway sent), or the 5-hour
// default. A unit this build does not know is no length, and the default
// stands rather than a guess at the unit.
func kimiWindowMinutes(v any) float64 {
	switch x := v.(type) {
	case map[string]any:
		d := kimiNum(x["duration"])
		if d == nil || *d <= 0 {
			return kimiDefaultWindowMinutes
		}
		switch strings.ToUpper(asStr(x["timeUnit"], 24)) {
		case "TIME_UNIT_MINUTE", "":
			return *d
		case "TIME_UNIT_HOUR":
			return *d * 60
		case "TIME_UNIT_DAY":
			return *d * 1440
		case "TIME_UNIT_WEEK":
			return *d * 10080
		}
		return kimiDefaultWindowMinutes
	default:
		if w := kimiNum(v); w != nil && *w > 0 {
			return *w
		}
	}
	return kimiDefaultWindowMinutes
}

// The plan out of a GetSubscription answer: the goods title, only while the
// subscription is active — an ended plan's title is not the plan the account
// is on. Through the same sieve as every other published name.
func kimiPlan(body []byte) string {
	var doc struct {
		Subscription *struct {
			Active bool   `json:"active"`
			Status string `json:"status"`
			Goods  *struct {
				Title string `json:"title"`
			} `json:"goods"`
		} `json:"subscription"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Subscription == nil || doc.Subscription.Goods == nil {
		return ""
	}
	sub := doc.Subscription
	if !sub.Active || sub.Status != "SUBSCRIPTION_STATUS_ACTIVE" {
		return ""
	}
	return tame(sub.Goods.Title, 32)
}
