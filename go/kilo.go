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
	"sort"
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

// Every object reachable within two levels of a payload, SHALLOWEST FIRST, so
// an authoritative `total` near the root is consulted before a generic
// `limit` buried in some other branch. A real breadth-first walk with an
// explicit queue, not the recursion an earlier version used: DFS returned
// objects in a branch-then-branch order that, combined with Go's randomised
// map iteration, could pick different numbers for the same JSON on different
// runs (review finding). Map keys are visited in sorted order at each node so
// even siblings are deterministic.
func kiloContexts(v any) []map[string]any {
	var out []map[string]any
	type item struct {
		v     any
		depth int
	}
	queue := []item{{v, 0}}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if it.depth > 2 {
			continue
		}
		switch t := it.v.(type) {
		case map[string]any:
			out = append(out, t)
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				queue = append(queue, item{t[k], it.depth + 1})
			}
		case []any:
			for _, child := range t {
				queue = append(queue, item{child, it.depth + 1})
			}
		}
	}
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
		// Fallback runs whenever ANY of the three is still missing — not only
		// when total and remaining are both absent — and it consults the full
		// cents / micro-USD / plain key families the reference does, in that
		// precedence. The earlier version passed nil for the cents and micro
		// lists, so only the plain keys were ever tried in production.
		if used == nil || total == nil || remaining == nil {
			for _, ctx := range kiloContexts(pay) {
				if used == nil {
					if v, ok := kiloMoney(ctx,
						[]string{"usedCents", "spentCents", "consumedCents", "usedAmountCents", "consumedAmountCents"},
						[]string{"used_mUsd", "spent_mUsd", "consumed_mUsd", "usedAmount_mUsd"},
						[]string{"used", "spent", "consumed", "usage", "creditsUsed", "usedAmount", "consumedAmount"}); ok {
						used = fp(v)
					}
				}
				if total == nil {
					if v, ok := kiloMoney(ctx,
						[]string{"amountCents", "totalCents", "planAmountCents", "monthlyAmountCents", "limitCents", "includedCents", "valueCents"},
						[]string{"amount_mUsd", "total_mUsd", "planAmount_mUsd", "limit_mUsd", "included_mUsd", "value_mUsd"},
						[]string{"amount", "total", "limit", "included", "value", "creditsTotal", "totalCredits", "planAmount"}); ok {
						total = fp(v)
					}
				}
				if remaining == nil {
					if v, ok := kiloMoney(ctx,
						[]string{"remainingCents", "remainingAmountCents", "availableCents", "leftCents", "balanceCents"},
						[]string{"remaining_mUsd", "available_mUsd", "left_mUsd", "balance_mUsd"},
						[]string{"remaining", "available", "left", "balance", "creditsRemaining", "remainingAmount", "availableAmount"}); ok {
						remaining = fp(v)
					}
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
		rem := remaining
		if rem == nil {
			r := *total - *used
			if r < 0 {
				r = 0
			}
			rem = fp(r)
		} else if *rem < 0 {
			rem = fp(0)
		}
		p.Limits = append(p.Limits, Limit{
			Key: "credits", UsedPercent: round2(pct),
			UsedAmount: fp(round2(*used)), TotalAmount: fp(round2(*total)),
			RemainingAmount: fp(round2(*rem)), Unit: "usd",
		})
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
			remaining := *pTotal - *pUsed
			if remaining < 0 {
				remaining = 0
			}
			p.Limits = append(p.Limits, Limit{
				Key: "kilo_pass", UsedPercent: round2(pct),
				WindowLabel: sp("plan"), ResetsAt: pReset,
				UsedAmount: fp(round2(*pUsed)), TotalAmount: fp(round2(*pTotal)),
				RemainingAmount: fp(round2(remaining)), Unit: "usd",
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
