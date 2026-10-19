package main

// GitHub Copilot: quota costs a credential Copilot itself left on disk.
//
// Copilot's editor integrations store their OAuth token in
// ~/.config/github-copilot/apps.json (newer builds) or hosts.json (older),
// and GitHub's own quota endpoint answers to it:
//
//	GET https://api.github.com/copilot_internal/user
//	  {"quota_snapshots":{"premium_interactions":{...},"chat":{...}},
//	   "quota_reset_date":"2026-09-01","copilot_plan":"individual"}
//
// A seat whose snapshots are the all-zero placeholder (token-based billing,
// Business) may still carry its month in `monthly_quotas` and what is left
// of it in `limited_user_quotas`, keyed `completions` (premium) and `chat`;
// those are read when the snapshot itself says nothing (CodexBar
// CopilotUsageModels). The same seats report `credits_used`, a count of AI
// credits with no ceiling anywhere in GitHub's API (CodexBar@2d76cd9e9f,
// docs/copilot.md: every billing endpoint was probed and none returns one);
// a count with no denominator has no slot on this wire and is not read.
//
// Same cost rung as Claude — we hold a credential and make one HTTPS call to
// the vendor that issued it — and the same rules travel with it (claude.go):
// read the one field, never refresh, never write back, and no transport error
// text in anything published. The protocol shape follows CodexBar's Copilot
// provider; the credential FILE is this helper's own addition, because a
// headless box has the file and does not have CodexBar's paste-a-token UI.
// MON_COPILOT_TOKEN overrides for the box where the file lives elsewhere.
//
// ON THE FILE: apps.json also names the GitHub user. That name is identity,
// not quota, and is deliberately never decoded — same rule as amp.go's first
// line. Only oauth_token leaves the parse.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	copilotURL      = "https://api.github.com/copilot_internal/user"
	copilotTokenEnv = "MON_COPILOT_TOKEN"
)

var copilotCache = provCache{pace: paceMark{id: "copilot"}}

func collectCopilot() Provider {
	return copilotCache.collect(fetchCopilot)
}

// The token, from wherever Copilot's own tooling put it. apps.json maps
// "github.com:<client-id>" to an entry carrying oauth_token; hosts.json is the
// same idea keyed by bare host. Both are tried, newer file first, and only
// github.com entries are considered — an enterprise host's token belongs to
// that enterprise's endpoint, which this collector does not speak.
func copilotToken() string {
	if v := strings.TrimSpace(os.Getenv(copilotTokenEnv)); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, name := range []string{"apps.json", "hosts.json"} {
		raw, err := os.ReadFile(filepath.Join(home, ".config", "github-copilot", name))
		if err != nil {
			continue
		}
		var doc map[string]struct {
			OauthToken string `json:"oauth_token"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		for key, entry := range doc {
			if entry.OauthToken == "" {
				continue
			}
			if key == "github.com" || strings.HasPrefix(key, "github.com:") {
				return entry.OauthToken
			}
		}
	}
	return ""
}

// One snapshot as GitHub reports it. Numbers arrive as numbers, but every
// field is decoded loose — same reasoning as codex.go: a surprise type in a
// field we do not read must not sink the row that carries the one we do.
type copilotSnapshot struct {
	Entitlement      any  `json:"entitlement"`
	Remaining        any  `json:"remaining"`
	PercentRemaining any  `json:"percent_remaining"`
	Unlimited        bool `json:"unlimited"`
}

// used% out of one snapshot, or nil for the shapes that carry no reading:
// unlimited seats, and the all-zero placeholder GitHub returns for
// token-based-billing and Business seats — 100-0=100% used on an idle seat,
// or 100-100=0% on one whose usage is simply not counted this way, both
// plausible and both wrong (CodexBar learned this one for everybody).
func copilotUsed(s *copilotSnapshot) *float64 {
	if s == nil || s.Unlimited {
		return nil
	}
	pct := asNum(s.PercentRemaining)
	ent := asNum(s.Entitlement)
	rem := asNum(s.Remaining)
	if ent != nil && rem != nil && *ent == 0 && *rem == 0 {
		return nil // placeholder, whatever percent_remaining claims
	}
	if pct == nil {
		// Derive when both inputs exist; refuse otherwise.
		if ent == nil || rem == nil || *ent <= 0 {
			return nil
		}
		d := *rem / *ent * 100
		pct = &d
	}
	used := 100 - *pct
	if used < 0 {
		used = 0
	}
	return fp(round2(used))
}

// The exact figures behind the percentage, when GitHub supplied both sides.
// Keep this separate from copilotUsed: percent_remaining can stand on its own,
// so a useful gauge must not disappear merely because the absolute pair did.
func copilotAmounts(s *copilotSnapshot) (used, total, remaining *float64) {
	if s == nil || s.Unlimited {
		return nil, nil, nil
	}
	ent := asNum(s.Entitlement)
	rem := asNum(s.Remaining)
	if ent == nil || rem == nil || *ent <= 0 {
		return nil, nil, nil
	}
	u := *ent - *rem
	if u < 0 {
		u = 0
	}
	used, total = fp(round2(u)), fp(round2(*ent))
	if *rem >= 0 {
		remaining = fp(round2(*rem))
	}
	return used, total, remaining
}

func fetchCopilot() Provider {
	p := Provider{ID: "copilot", Name: "Copilot", Source: "api", CapturedAt: now()}
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}

	token := copilotToken()
	if token == "" {
		return fail("not-signed-in", "creds-missing", "~/.config/github-copilot/")
	}

	status, body, err := provRequest("GET", copilotURL, map[string]string{
		// "token", not "Bearer": this is the scheme GitHub's own integrations
		// use for these OAuth tokens, and the one CodexBar ships.
		"Authorization":         "token " + token,
		"Editor-Version":        "vscode/1.96.2",
		"Editor-Plugin-Version": "copilot-chat/0.26.7",
		"X-Github-Api-Version":  "2025-04-01",
	}, nil)
	if err != nil {
		return fail("unreachable", "req-failed")
	}
	switch {
	case status == 401 || status == 403:
		return fail("token-expired", "http-status", itoa(status))
	case status == 404:
		// The endpoint answers 404 for an account with no Copilot at all.
		return fail("not-signed-in", "no-subscription")
	case status != 200:
		return fail("api-error", "http-status", itoa(status))
	}

	var doc struct {
		QuotaSnapshots    map[string]json.RawMessage `json:"quota_snapshots"`
		QuotaResetDate    string                     `json:"quota_reset_date"`
		CopilotPlan       string                     `json:"copilot_plan"`
		MonthlyQuotas     map[string]any             `json:"monthly_quotas"`
		LimitedUserQuotas map[string]any             `json:"limited_user_quotas"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return fail("api-error", "http-shape")
	}

	// The reset date: a full timestamp when GitHub sends one, else the bare
	// YYYY-MM-DD it usually does — day granularity, same trade as amp.go's
	// "in 23 days", rendered at UTC midnight.
	var resets *float64
	if e := isoToEpoch(doc.QuotaResetDate); e > 0 {
		resets = fp(e)
	} else if t, err := time.Parse("2006-01-02", doc.QuotaResetDate); err == nil {
		resets = fp(float64(t.Unix()))
	}

	// premium_interactions is the gauge Copilot's own UI leads with; chat is
	// second. Other snapshot keys GitHub may add pass through as themselves,
	// bounded — same choice claude.go makes for an unrecognised window kind.
	var unlimited bool
	order := []string{"premium_interactions", "chat"}
	seen := map[string]bool{"premium_interactions": true, "chat": true}
	for key := range doc.QuotaSnapshots {
		if !seen[key] {
			order = append(order, key)
		}
	}
	for _, key := range order {
		raw, okKey := doc.QuotaSnapshots[key]
		if !okKey {
			continue
		}
		var s copilotSnapshot
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		if s.Unlimited {
			unlimited = true
			continue
		}
		label := key
		if key == "premium_interactions" {
			label = "premium"
		}
		lim := Limit{Key: tame(label, 24), WindowLabel: sp("month"), ResetsAt: resets}
		if used := copilotUsed(&s); used != nil {
			lim.UsedPercent = *used
			lim.UsedAmount, lim.TotalAmount, lim.RemainingAmount = copilotAmounts(&s)
		} else if m := copilotMonthly(doc.MonthlyQuotas, doc.LimitedUserQuotas, key); m != nil {
			// The placeholder snapshot said nothing; the month's figures do.
			lim.UsedPercent = m.UsedPercent
			lim.UsedAmount, lim.TotalAmount, lim.RemainingAmount = m.UsedAmount, m.TotalAmount, m.RemainingAmount
		} else {
			continue
		}
		p.Limits = append(p.Limits, lim)
	}
	if unlimited {
		p.Credits = &Credits{HasCredits: true, Unlimited: true}
	}
	if len(p.Limits) == 0 && !unlimited {
		return fail("no-readings", "no-window")
	}

	p.OK = true
	p.RecordedAt = fp(now())
	if plan := tame(doc.CopilotPlan, 32); plan != "" {
		p.PlanType = sp(plan)
	}
	return p
}

// The month's figures for a snapshot key, out of `monthly_quotas` (the
// allowance) and `limited_user_quotas` (what is left of it): premium
// interactions are `completions` there, chat is `chat`. Both sides must be
// present and the allowance positive; a remainder above it is the same
// placeholder trouble copilotUsed refuses, and reads as nothing. Per
// CodexBar CopilotUsageModels.monthlyQuotaFallback.
func copilotMonthly(monthly, limited map[string]any, key string) *Limit {
	field := ""
	switch key {
	case "premium_interactions":
		field = "completions"
	case "chat":
		field = "chat"
	default:
		return nil
	}
	if monthly == nil || limited == nil {
		return nil
	}
	total, okT := adapterNum(monthly[field])
	left, okL := adapterNum(limited[field])
	if !okT || !okL || total <= 0 || left < 0 || left > total {
		return nil
	}
	used := total - left
	return &Limit{
		UsedPercent: round2(used / total * 100),
		UsedAmount:  fp(round2(used)), TotalAmount: fp(round2(total)), RemainingAmount: fp(round2(left)),
	}
}
