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
//	       "limits":[{"detail":{…},"window":300}]}]}
//
// FEATURE_CODING's detail is the weekly quota; its limits[] carry the short
// rate window. Every number arrives as a STRING, which is why the parse below
// goes through ampNum rather than a typed struct — Codex's lesson about files
// applies to gateways too.
//
// Claude's cost rung, Claude's rules (claude.go): read two scalars from the
// credential file, never refresh, never write back, publish no transport
// text. Protocol per CodexBar's Kimi provider, credential-file path.
// KIMI_CODE_HOME moves the directory, same as the CLI honours;
// MON_KIMI_TOKEN overrides the token outright.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	kimiUsagesURL = "https://www.kimi.com/apiv2/kimi.gateway.billing.v1.BillingService/GetUsages"
	kimiStatsURL  = "https://www.kimi.com/apiv2/kimi.gateway.membership.v2.MembershipService/GetSubscriptionStats"
	kimiTokenEnv  = "MON_KIMI_TOKEN"
	kimiHomeEnv   = "KIMI_CODE_HOME"
)

var kimiCache provCache

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
	if d == nil {
		return nil
	}
	limit := kimiNum(d.Limit)
	if limit == nil || *limit <= 0 {
		return nil
	}
	used := kimiNum(d.Used)
	if used == nil {
		rem := kimiNum(d.Remaining)
		if rem == nil {
			return nil
		}
		u := *limit - *rem
		used = &u
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
	fail := func(err, detail string) Provider {
		p.Error, p.Detail = err, detail
		return p
	}

	token, exp := kimiToken()
	if token == "" {
		return fail("not-signed-in", "~/.kimi-code/credentials/ 里没有 access_token")
	}
	if exp > 0 && exp < now() {
		return fail("token-expired", "凭据已过期；跑一次 kimi 让它续期。")
	}
	hdr := map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}

	status, body, err := provRequest("POST", kimiUsagesURL, hdr, []byte(`{"scope":["FEATURE_CODING"]}`))
	if err != nil {
		return fail("unreachable", "请求失败")
	}
	switch {
	case status == 401 || status == 403:
		return fail("token-expired", "Kimi 接口返回 "+itoa(status))
	case status != 200:
		return fail("api-error", "Kimi 接口返回 "+itoa(status))
	}

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
		return fail("api-error", "Kimi 接口返回的不是预期结构")
	}

	for _, u := range doc.Usages {
		if u.Scope != "FEATURE_CODING" {
			continue
		}
		if used := kimiUsed(u.Detail); used != nil {
			p.Limits = append(p.Limits, Limit{
				Key: "weekly", UsedPercent: *used,
				WindowLabel: sp("7d"), ResetsAt: kimiReset(u.Detail),
			})
		}
		// The short rate window rides in limits[]; only the first is the
		// governing one, per Kimi's own client.
		if len(u.Limits) > 0 {
			l := u.Limits[0]
			if used := kimiUsed(l.Detail); used != nil {
				lim := Limit{Key: "rate", UsedPercent: *used, ResetsAt: kimiReset(l.Detail)}
				if w := kimiNum(l.Window); w != nil && *w > 0 {
					lim.WindowMinutes = w
					lim.WindowLabel = windowLabel(*w)
				}
				p.Limits = append(p.Limits, lim)
			}
		}
		break
	}
	if len(p.Limits) == 0 {
		return fail("no-readings", "Kimi 接口没有返回可用的额度窗口")
	}

	// Subscription stats are a second, separately-failing request and carry
	// only the plan chip; losing them costs nothing the gauges need.
	if status, body, err := provRequest("POST", kimiStatsURL, hdr, []byte("{}")); err == nil && status == 200 {
		var stats struct {
			SubscriptionBalance *struct {
				Type any `json:"type"`
			} `json:"subscriptionBalance"`
		}
		if json.Unmarshal(body, &stats) == nil && stats.SubscriptionBalance != nil {
			if plan := tame(asStr(stats.SubscriptionBalance.Type, 32), 32); plan != "" {
				p.PlanType = sp(plan)
			}
		}
	}

	p.OK = true
	p.RecordedAt = fp(now())
	return p
}
