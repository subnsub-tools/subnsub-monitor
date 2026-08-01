package main

// Kilo: quota costs a credential Kilo's own CLI left on disk.
//
// The kilo CLI stores its token at ~/.local/share/kilo/auth.json
// ({"kilo":{"access":"…"}}), and Kilo's tRPC endpoint answers to it. Three
// procedures batched into one GET:
//
//	GET https://app.kilo.ai/api/trpc/user.getCreditBlocks,kiloPass.getState,
//	    user.getAutoTopUpPaymentMethod?batch=1&input={"0":{"json":null},…}
//
// Claude's cost rung and Claude's rules (claude.go / provhttp.go): one field
// out of the file, never write back, publish no transport text. The response
// is a tRPC batch whose shape CodexBar itself probes with layered fallbacks —
// so this decodes defensively (codex.go's `any`-everywhere approach) and
// looks for the numbers under every key CodexBar knows, rather than trusting
// one schema. Money is micro-USD (…_mUsd / 1e6) or cents (…Cents / 100) or a
// plain number, in that order, matching the reference.
//
// MON_KILO_TOKEN overrides the file.

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	kiloTRPCBase = "https://app.kilo.ai/api/trpc"
	kiloTokenEnv = "MON_KILO_TOKEN"
)

var kiloCache provCache

func collectKilo() Provider {
	return kiloCache.collect(fetchKilo)
}

func kiloToken() string {
	if v := strings.TrimSpace(os.Getenv(kiloTokenEnv)); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, ".local", "share", "kilo", "auth.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Kilo *struct {
			Access string `json:"access"`
		} `json:"kilo"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Kilo == nil {
		return ""
	}
	v := strings.TrimSpace(doc.Kilo.Access)
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = strings.TrimSpace(v[1 : len(v)-1])
	}
	return v
}

// ── loose extraction over the batch, codex.go style ──────────────────────

// A number out of an arbitrary JSON value, tolerating the string form Kilo
// sometimes uses.
func kiloAsNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		if finite(x) {
			return x, true
		}
	case string:
		if n, ok := ampNum(x); ok {
			return n, true
		}
	}
	return 0, false
}

// Money under a family of keys: cents first, then micro-USD, then plain, the
// exact precedence KiloUsageFetcher.moneyAmount uses.
func kiloMoney(ctx map[string]any, cents, micro, plain []string) (float64, bool) {
	for _, k := range cents {
		if n, ok := kiloAsNum(ctx[k]); ok {
			return n / 100, true
		}
	}
	for _, k := range micro {
		if n, ok := kiloAsNum(ctx[k]); ok {
			return n / 1_000_000, true
		}
	}
	for _, k := range plain {
		if n, ok := kiloAsNum(ctx[k]); ok {
			return n, true
		}
	}
	return 0, false
}

// Every object reachable within two levels of a payload, so a value under
// `data.json.subscription` is found the same way as one at the root — the
// BFS KiloUsageFetcher.dictionaryContexts performs.
func kiloContexts(v any) []map[string]any {
	var out []map[string]any
	var walk func(x any, depth int)
	walk = func(x any, depth int) {
		if depth > 2 {
			return
		}
		switch t := x.(type) {
		case map[string]any:
			out = append(out, t)
			for _, child := range t {
				walk(child, depth+1)
			}
		case []any:
			for _, child := range t {
				walk(child, depth+1)
			}
		}
	}
	walk(v, 0)
	return out
}

// The result payload of one batch entry, unwrapped through tRPC's
// result.data.json → result.data → result.json nesting.
func kiloPayload(entry any) any {
	m, ok := entry.(map[string]any)
	if !ok {
		return nil
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		return nil
	}
	if data, ok := res["data"].(map[string]any); ok {
		if j, has := data["json"]; has {
			return j
		}
		return data
	}
	if j, has := res["json"]; has {
		return j
	}
	return nil
}

func fetchKilo() Provider {
	p := Provider{ID: "kilo", Name: "Kilo", Source: "api", CapturedAt: now()}
	fail := func(err, detail string) Provider {
		p.Error, p.Detail = err, detail
		return p
	}

	token := kiloToken()
	if token == "" {
		return fail("not-signed-in", "~/.local/share/kilo/auth.json 里没有 access token")
	}

	input := `{"0":{"json":null},"1":{"json":null},"2":{"json":null}}`
	reqURL := kiloTRPCBase +
		"/user.getCreditBlocks,kiloPass.getState,user.getAutoTopUpPaymentMethod" +
		"?batch=1&input=" + url.QueryEscape(input)
	status, body, err := provRequest("GET", reqURL, map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/json",
	}, nil)
	if err != nil {
		return fail("unreachable", "请求失败")
	}
	switch {
	case status == 401 || status == 403:
		return fail("token-expired", "Kilo 接口返回 "+itoa(status))
	case status != 200:
		return fail("api-error", "Kilo 接口返回 "+itoa(status))
	}

	// The batch is an array of entries, or a single object for one procedure.
	var raw any
	if json.Unmarshal(body, &raw) != nil {
		return fail("api-error", "Kilo 接口返回的不是预期结构")
	}
	entries := map[int]any{}
	switch t := raw.(type) {
	case []any:
		for i, e := range t {
			if i < 3 {
				entries[i] = e
			}
		}
	case map[string]any:
		entries[0] = t
	}

	// getCreditBlocks: sum amount/balance across blocks (micro-USD), or fall
	// back to the named used/total/remaining keys anywhere in the payload.
	var used, total, remaining *float64
	if pay := kiloPayload(entries[0]); pay != nil {
		for _, ctx := range kiloContexts(pay) {
			if blocks, ok := ctx["creditBlocks"].([]any); ok {
				var t, r float64
				var sawT, sawR bool
				for _, b := range blocks {
					bm, _ := b.(map[string]any)
					if bm == nil {
						continue
					}
					if n, ok := kiloAsNum(bm["amount_mUsd"]); ok {
						t += n / 1_000_000
						sawT = true
					}
					if n, ok := kiloAsNum(bm["balance_mUsd"]); ok {
						r += n / 1_000_000
						sawR = true
					}
				}
				if sawT {
					total = fp(t)
				}
				if sawR {
					remaining = fp(r)
				}
				break
			}
		}
		if total == nil && remaining == nil {
			for _, ctx := range kiloContexts(pay) {
				if v, ok := kiloMoney(ctx, nil, nil, []string{"used", "usedCredits", "creditsUsed", "consumed", "spent"}); ok && used == nil {
					used = fp(v)
				}
				if v, ok := kiloMoney(ctx, nil, nil, []string{"total", "totalCredits", "creditsTotal", "limit"}); ok && total == nil {
					total = fp(v)
				}
				if v, ok := kiloMoney(ctx, nil, nil, []string{"remaining", "remainingCredits", "creditsRemaining"}); ok && remaining == nil {
					remaining = fp(v)
				}
			}
		}
	}
	if total == nil && used != nil && remaining != nil {
		total = fp(*used + *remaining)
	}
	if used == nil && total != nil && remaining != nil {
		u := *total - *remaining
		if u < 0 {
			u = 0
		}
		used = fp(u)
	}

	if total != nil && *total > 0 && used != nil {
		pct := *used / *total * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		p.Limits = append(p.Limits, Limit{Key: "credits", UsedPercent: round2(pct)})
	} else if total != nil && *total == 0 {
		// Explicitly exhausted, the reference's (0,0,0) shape.
		p.Limits = append(p.Limits, Limit{Key: "credits", UsedPercent: 100})
	}
	if remaining != nil {
		p.Credits = &Credits{HasCredits: true, Balance: "$" + trimAmount(*remaining)}
	}

	// kiloPass.getState: a second gauge, in USD already (no /1e6).
	var plan string
	if pay := kiloPayload(entries[1]); pay != nil {
		var pUsed, pTotal, pReset *float64
		for _, ctx := range kiloContexts(pay) {
			if v, ok := kiloAsNum(ctx["currentPeriodUsageUsd"]); ok && pUsed == nil {
				pUsed = fp(v)
			}
			if base, ok := kiloAsNum(ctx["currentPeriodBaseCreditsUsd"]); ok && pTotal == nil {
				bonus, _ := kiloAsNum(ctx["currentPeriodBonusCreditsUsd"])
				pTotal = fp(base + bonus)
			}
			for _, k := range []string{"nextBillingAt", "nextRenewalAt", "renewsAt", "renewAt", "currentPeriodEnd"} {
				if pReset == nil {
					if e := kiloEpoch(ctx[k]); e != nil {
						pReset = e
					}
				}
			}
			if name := kiloPlanName(ctx); name != "" && plan == "" {
				plan = name
			}
		}
		if pTotal != nil && *pTotal > 0 && pUsed != nil {
			pct := *pUsed / *pTotal * 100
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			p.Limits = append(p.Limits, Limit{
				Key: "kilo_pass", UsedPercent: round2(pct),
				WindowLabel: sp("plan"), ResetsAt: pReset,
			})
		}
	}

	if len(p.Limits) == 0 && p.Credits == nil {
		return fail("no-readings", "Kilo 接口没有返回可用的额度")
	}

	p.OK = true
	p.RecordedAt = fp(now())
	if plan != "" {
		p.PlanType = sp(plan)
	}
	return p
}

// A tRPC epoch: number (ms if huge), or numeric/ISO string.
func kiloEpoch(v any) *float64 {
	switch x := v.(type) {
	case float64:
		if x <= 0 {
			return nil
		}
		if x > 1e10 {
			x /= 1000
		}
		return fp(x)
	case string:
		if e := isoToEpoch(x); e > 0 {
			return fp(e)
		}
		if n, ok := ampNum(x); ok && n > 0 {
			if n > 1e10 {
				n /= 1000
			}
			return fp(n)
		}
	}
	return nil
}

var kiloTierNames = map[string]string{
	"tier_19": "Starter", "tier_49": "Pro", "tier_199": "Expert",
}

func kiloPlanName(ctx map[string]any) string {
	if tier, ok := ctx["tier"].(string); ok && tier != "" {
		if n, mapped := kiloTierNames[tier]; mapped {
			return n
		}
		return tame(tier, 32)
	}
	for _, k := range []string{"planName", "tierName", "passName", "subscriptionName"} {
		if s, ok := ctx[k].(string); ok && s != "" {
			return tame(s, 32)
		}
	}
	return ""
}
