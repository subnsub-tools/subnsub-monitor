package main

// Wayfinder: a reading from a gateway already running on this machine.
//
// Wayfinder is a self-hosted LLM router; it listens on loopback and answers
// unauthenticated read-only endpoints. That puts it on Antigravity's rung —
// a request that never leaves the box, no credential held:
//
//	GET http://127.0.0.1:8088/healthz        {"status","offline","missing_keys"}
//	GET http://127.0.0.1:8088/v1/savings?period=30d
//	      {"priced","requests","tokens","realized","baseline","saved","saved_pct",…}
//	GET http://127.0.0.1:8088/router/models  {"models":[{"name"}],"dry_run"}
//
// It has NO quota semantics — CodexBar's own provider leaves primary/secondary
// nil and renders savings instead, and this follows suit: no Limit is
// fabricated. What travels is the 30-day saved percentage as a gauge-less
// balance line and the model count as the plan chip. The address is a literal
// loopback IP so the connection cannot leave the machine; MON_WAYFINDER_URL
// overrides for an unusual bind, validated to loopback-or-HTTPS.

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	wayfinderDefault = "http://127.0.0.1:8088"
	wayfinderURLEnv  = "MON_WAYFINDER_URL"
)

var wayfinderCache provCache

func collectWayfinder() Provider {
	return wayfinderCache.collect(fetchWayfinder)
}

// The gateway base URL: the override if it validates, else the default. The
// override must be HTTPS or point at a loopback host over plain HTTP — the
// same bar CodexBar's endpoint validator sets, so this cannot be pointed at
// an arbitrary remote to turn the helper into a request proxy.
func wayfinderBase() string {
	v := strings.TrimSpace(os.Getenv(wayfinderURLEnv))
	if v == "" {
		return wayfinderDefault
	}
	if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
		v = strings.TrimSpace(v[1 : len(v)-1])
	}
	u, err := url.Parse(v)
	// No embedded credentials, and no path/query/fragment: this value has
	// endpoint paths appended to it, so anything past the host is both a
	// correctness bug (the appended path lands in the wrong place) and a leak
	// risk — a token in ?query= would otherwise ride into an error Detail that
	// travels to the relay. A bare scheme://host[:port] only.
	if err != nil || u.Host == "" || u.User != nil ||
		strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	base := u.Scheme + "://" + u.Host
	if u.Scheme == "https" {
		return base
	}
	if u.Scheme == "http" && wayfinderLoopback(u.Hostname()) {
		return base
	}
	return ""
}

func wayfinderLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func fetchWayfinder() Provider {
	p := Provider{ID: "wayfinder", Name: "Wayfinder", Source: "local-api", CapturedAt: now()}
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}

	base := wayfinderBase()
	if base == "" {
		// Deliberately does NOT echo the override value: Detail travels to the
		// relay, and a misconfigured override could carry anything.
		return fail("api-error", "gateway-url")
	}

	// healthz first: it is how we tell "no gateway here" (unreachable) from
	// "gateway up but idle". A failure here is the not-installed signal. The
	// base is not published in the message for the same reason.
	hstatus, hbody, err := provRequest("GET", base+"/healthz", nil, nil)
	if err != nil || hstatus != 200 {
		return fail("not-installed", "gateway-down")
	}
	var health struct {
		Status  string `json:"status"`
		Offline bool   `json:"offline"`
	}
	_ = json.Unmarshal(hbody, &health)

	// savings: the actual reading. Absence is not failure — a gateway that
	// has routed nothing yet has no savings, and that is a live, honest zero.
	sstatus, sbody, err := provRequest("GET", base+"/v1/savings?period=30d", nil, nil)
	if err != nil || sstatus != 200 {
		return fail("no-readings", "no-savings")
	}
	var sav struct {
		Priced   bool    `json:"priced"`
		Requests float64 `json:"requests"`
		Tokens   float64 `json:"tokens"`
		Saved    float64 `json:"saved"`
		SavedPct float64 `json:"saved_pct"`
	}
	if json.Unmarshal(sbody, &sav) != nil {
		return fail("api-error", "http-parse")
	}

	// NOT a Limit. used_percent is a quota gauge: the panel colours a high
	// value red ("about to run out") and folds it into the worst-window and
	// trend calculations. Savings is the opposite polarity — high is GOOD —
	// so publishing 90% savings as 90% used would paint a healthy gateway as
	// an emergency (review finding). It rides as a Credits balance instead,
	// which the panel renders as a neutral figure, with the percentage and
	// window in the text.
	// Always a balance line, so a live-but-idle gateway is a visible card
	// rather than a provider the relay drops for having nothing to show. The
	// figure includes a digit either way, which is what the relay's balance
	// check requires.
	pct := sav.SavedPct
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	bal := trimAmount(round2(pct)) + "% saved (30d)"
	if sav.Priced && sav.Saved > 0 {
		bal = "$" + trimAmount(sav.Saved) + " · " + bal
	}
	p.Credits = &Credits{HasCredits: true, Balance: bal}
	// Activity is useful context for the saving figure, but it is not a quota
	// and therefore never enters a red/amber gauge. Both counters are explicit
	// zero-capable pointers so an idle gateway reports an honest 0 rather than a
	// field that looks unsupported.
	p.Activity = &Activity{
		WindowLabel: sp("30d"),
		Requests:    fp(sav.Requests),
		Tokens:      fp(sav.Tokens),
	}
	if sav.Priced && sav.Saved >= 0 {
		p.Activity.Saved = &Amount{Amount: round2(sav.Saved), Unit: "usd"}
	}

	// The plan chip carries the status and the model count — a local gateway's
	// identity, not a subscription plan.
	label := "local gateway"
	if health.Offline {
		label = "offline mode"
	} else if health.Status == "degraded" {
		label = "degraded"
	}
	if mstatus, mbody, err := provRequest("GET", base+"/router/models", nil, nil); err == nil && mstatus == 200 {
		var models struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if json.Unmarshal(mbody, &models) == nil && len(models.Models) > 0 {
			label = strconv.Itoa(len(models.Models)) + " models · " + label
		}
	}
	p.PlanType = sp(tame(label, 40))

	p.OK = true
	p.RecordedAt = fp(now())
	return p
}
