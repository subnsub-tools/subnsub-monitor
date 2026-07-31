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

var copilotCache provCache

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

func fetchCopilot() Provider {
	p := Provider{ID: "copilot", Name: "Copilot", Source: "api", CapturedAt: now()}
	fail := func(err, detail string) Provider {
		p.Error, p.Detail = err, detail
		return p
	}

	token := copilotToken()
	if token == "" {
		return fail("not-signed-in", "~/.config/github-copilot/ 里没有可用的 oauth_token")
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
		return fail("unreachable", "请求失败")
	}
	switch {
	case status == 401 || status == 403:
		return fail("token-expired", "Copilot 接口返回 "+itoa(status))
	case status == 404:
		// The endpoint answers 404 for an account with no Copilot at all.
		return fail("not-signed-in", "这个 GitHub 账号没有 Copilot 订阅")
	case status != 200:
		return fail("api-error", "Copilot 接口返回 "+itoa(status))
	}

	var doc struct {
		QuotaSnapshots map[string]json.RawMessage `json:"quota_snapshots"`
		QuotaResetDate string                     `json:"quota_reset_date"`
		CopilotPlan    string                     `json:"copilot_plan"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return fail("api-error", "Copilot 接口返回的不是预期结构")
	}

	// The reset date is a bare YYYY-MM-DD. Day granularity is all upstream
	// gives — same trade as amp.go's "in 23 days", rendered at UTC midnight.
	var resets *float64
	if t, err := time.Parse("2006-01-02", doc.QuotaResetDate); err == nil {
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
		used := copilotUsed(&s)
		if used == nil {
			continue
		}
		label := key
		if key == "premium_interactions" {
			label = "premium"
		}
		p.Limits = append(p.Limits, Limit{
			Key: tame(label, 24), UsedPercent: *used,
			WindowLabel: sp("month"), ResetsAt: resets,
		})
	}
	if unlimited {
		p.Credits = &Credits{HasCredits: true, Unlimited: true}
	}
	if len(p.Limits) == 0 && !unlimited {
		return fail("no-readings", "Copilot 接口没有返回可用的额度窗口")
	}

	p.OK = true
	p.RecordedAt = fp(now())
	if plan := tame(doc.CopilotPlan, 32); plan != "" {
		p.PlanType = sp(plan)
	}
	return p
}
