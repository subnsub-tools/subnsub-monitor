package main

// Droid (Factory): quota costs a credential Factory itself left on disk.
//
// The droid CLI keeps its API key in ~/.factory/.env as FACTORY_API_KEY, in
// dotenv form, and Factory's billing endpoint answers to it directly:
//
//	GET https://api.factory.ai/api/billing/limits
//	  {"usesTokenRateLimitsBilling":true,
//	   "limits":{"standard":{"fiveHour":{"usedPercent":12,...},
//	             "weekly":{...},"monthly":{...}},"core":{...}},
//	   "extraUsageBalanceCents":871}
//
// Claude's cost rung and Claude's rules (claude.go): one field out of the
// file, never refresh, never write back, no transport text published. The
// shape follows CodexBar's Droid provider, API-key path only — its WorkOS
// browser-session and cookie paths are exactly what this helper does not do.
// MON_FACTORY_KEY overrides the file for unusual installs.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	factoryLimitsURL = "https://api.factory.ai/api/billing/limits"
	factoryMeURL     = "https://api.factory.ai/api/app/auth/me"
	factoryKeyEnv    = "MON_FACTORY_KEY"
)

var factoryCache provCache

func collectFactory() Provider {
	return factoryCache.collect(fetchFactory)
}

// FACTORY_API_KEY out of ~/.factory/.env. A dotenv parse, not a shell: lines
// of KEY=VALUE, `export ` tolerated, quotes stripped, everything else — and
// every other variable in the file — ignored.
func factoryKey() string {
	if v := strings.TrimSpace(os.Getenv(factoryKeyEnv)); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".factory", ".env"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		k, v, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(k) != "FACTORY_API_KEY" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		return strings.TrimSpace(v)
	}
	return ""
}

// One rate window as Factory reports it. windowEnd arrives as epoch seconds,
// epoch milliseconds, a numeric string, or ISO-8601 depending on which side
// of their stack produced it; secondsRemaining is the newer, unambiguous
// field and wins when present.
type factoryWindow struct {
	UsedPercent      any `json:"usedPercent"`
	WindowEnd        any `json:"windowEnd"`
	SecondsRemaining any `json:"secondsRemaining"`
}

func factoryReset(w *factoryWindow, t float64) *float64 {
	if s := asNum(w.SecondsRemaining); s != nil && *s > 0 {
		return fp(t + *s)
	}
	var end float64
	switch v := w.WindowEnd.(type) {
	case float64:
		end = v
	case string:
		if e := isoToEpoch(v); e > 0 {
			end = e
		} else if n, ok := ampNum(v); ok {
			end = n
		}
	}
	if end > 1e12 {
		end /= 1000 // milliseconds; same trap types.go documents for Claude
	}
	if end > t {
		return fp(end)
	}
	return nil
}

// A window that has expired but not been re-cut carries its old percentage.
// Factory's own UI renders that state as reset, so this does too — a stale
// 96% on a five-hour window that lapsed overnight is the kind of plausible
// number this file exists to not publish.
func factoryUsed(w *factoryWindow, reset *float64) *float64 {
	u := asNum(w.UsedPercent)
	if u == nil {
		return nil
	}
	if reset == nil && w.WindowEnd != nil && asNum(w.SecondsRemaining) == nil {
		return fp(0)
	}
	v := *u
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return fp(round2(v))
}

func fetchFactory() Provider {
	p := Provider{ID: "factory", Name: "Droid", Source: "api", CapturedAt: now()}
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}

	key := factoryKey()
	if key == "" {
		return fail("not-signed-in", "creds-missing", "~/.factory/.env")
	}
	hdr := map[string]string{
		"Authorization":    "Bearer " + key,
		"x-factory-client": "web-app",
	}

	status, body, err := provRequest("GET", factoryLimitsURL, hdr, nil)
	if err != nil {
		return fail("unreachable", "req-failed")
	}
	switch {
	case status == 401 || status == 403:
		return fail("token-expired", "http-status", itoa(status))
	case status != 200:
		return fail("api-error", "http-status", itoa(status))
	}

	var doc struct {
		Limits *struct {
			Standard *struct {
				FiveHour *factoryWindow `json:"fiveHour"`
				Weekly   *factoryWindow `json:"weekly"`
				Monthly  *factoryWindow `json:"monthly"`
			} `json:"standard"`
			Core *struct {
				FiveHour *factoryWindow `json:"fiveHour"`
				Weekly   *factoryWindow `json:"weekly"`
				Monthly  *factoryWindow `json:"monthly"`
			} `json:"core"`
		} `json:"limits"`
		ExtraUsageBalanceCents any `json:"extraUsageBalanceCents"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return fail("api-error", "http-shape")
	}

	t := now()
	add := func(key, label string, scope string, w *factoryWindow) {
		if w == nil {
			return
		}
		reset := factoryReset(w, t)
		used := factoryUsed(w, reset)
		if used == nil {
			return
		}
		l := Limit{Key: key, UsedPercent: *used, WindowLabel: sp(label), ResetsAt: reset}
		if scope != "" {
			l.Scope = sp(scope)
		}
		p.Limits = append(p.Limits, l)
	}
	if doc.Limits != nil && doc.Limits.Standard != nil {
		add("session", "5h", "", doc.Limits.Standard.FiveHour)
		add("weekly", "7d", "", doc.Limits.Standard.Weekly)
		add("monthly", "30d", "", doc.Limits.Standard.Monthly)
	}
	if doc.Limits != nil && doc.Limits.Core != nil {
		// The premium-model pool. Scoped rather than renamed, the same way
		// Claude's per-model windows arrive.
		add("session", "5h", "Core", doc.Limits.Core.FiveHour)
		add("weekly", "7d", "Core", doc.Limits.Core.Weekly)
		add("monthly", "30d", "Core", doc.Limits.Core.Monthly)
	}
	if cents := asNum(doc.ExtraUsageBalanceCents); cents != nil && *cents > 0 {
		p.Credits = &Credits{HasCredits: true,
			Balance: "$" + trimAmount(*cents/100)}
	}
	if len(p.Limits) == 0 && p.Credits == nil {
		return fail("no-readings", "no-window")
	}

	// The plan name is a second, separately-failing request, and a nicety:
	// losing it costs a chip on the card, not the reading.
	if status, body, err := provRequest("GET", factoryMeURL, hdr, nil); err == nil && status == 200 {
		var me struct {
			Organization *struct {
				Subscription *struct {
					FactoryTier     string `json:"factoryTier"`
					OrbSubscription *struct {
						Plan *struct {
							Name string `json:"name"`
						} `json:"plan"`
					} `json:"orbSubscription"`
				} `json:"subscription"`
			} `json:"organization"`
		}
		if json.Unmarshal(body, &me) == nil && me.Organization != nil && me.Organization.Subscription != nil {
			sub := me.Organization.Subscription
			name := ""
			if sub.OrbSubscription != nil && sub.OrbSubscription.Plan != nil {
				name = sub.OrbSubscription.Plan.Name
			}
			if name == "" {
				name = sub.FactoryTier
			}
			if plan := tame(name, 32); plan != "" {
				p.PlanType = sp(plan)
			}
		}
	}

	p.OK = true
	p.RecordedAt = fp(now())
	return p
}
