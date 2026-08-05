package main

// Augment: quota via the vendor's own CLI, Amp's cost rung.
//
//	$ auggie account status
//	  Plan: Developer
//	  600 credits / month
//	  480 credits remaining
//	  Current period ends 9/1/2026
//
// No credential opened or held; auggie talks to its own login. Parsing
// follows amp.go's rules — anchored patterns, [ \t] never \s, the grouping
// grammar on every number — and the shapes come from CodexBar's Auggie
// probe, which tolerates several vintages of this output; each pattern here
// is one of those vintages, tried most-specific first.

import (
	"regexp"
	"time"
)

const augmentBinEnv = "MON_AUGGIE_BIN"

var augmentCache provCache

func collectAugment() Provider {
	return augmentCache.collect(fetchAugment)
}

var (
	// "480 / 600 credits used" — the one shape that names both numbers.
	augUsedOfRE = regexp.MustCompile(`(?im)^[ \t]*` + amount + `[ \t]*/[ \t]*` + amount + `[ \t]+credits[ \t]+used\b`)
	// "600 credits / month" + "480 credits remaining", the paired shape.
	augMonthRE     = regexp.MustCompile(`(?im)^[ \t]*` + amount + `[ \t]+credits[ \t]*/[ \t]*month\b`)
	augRemainingRE = regexp.MustCompile(`(?im)^[ \t]*` + amount + `[ \t]+(?:credits[ \t]+)?remaining\b`)
	// "Current period ends 9/1/2026" — month/day/year, upstream's spelling.
	augEndsRE = regexp.MustCompile(`(?im)\bends[ \t]+(\d{1,2})/(\d{1,2})/(\d{4})\b`)
	augPlanRE = regexp.MustCompile(`(?im)^[ \t]*Plan:[ \t]*([^\r\n]+?)[ \t]*$`)

	augSignedOutRE = regexp.MustCompile(`(?i)not (?:signed|logged) in|please (?:sign|log) in|auggie login`)
)

func fetchAugment() Provider {
	p := Provider{ID: "augment", Name: "Augment", Source: "cli", CapturedAt: now()}
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}

	bin := vendorBinary(augmentBinEnv, vendorCandidates("auggie"))
	if bin == "" {
		return fail("not-installed", "cli-missing", "auggie")
	}
	out, exited, err := runVendorCLI(bin, []string{"account", "status"},
		[]string{"AUGMENT_"}, 20*time.Second)
	if err != nil {
		if err == errAmpTimeout {
			return fail("unreachable", "cli-timeout", "auggie")
		}
		return fail("cli-failed", "cli-failed", "auggie")
	}
	text := ansiRE.ReplaceAllString(out, "")

	var resets *float64
	if m := augEndsRE.FindStringSubmatch(text); m != nil {
		if t, err := time.Parse("1/2/2006", m[1]+"/"+m[2]+"/"+m[3]); err == nil {
			resets = fp(float64(t.Unix()))
		}
	}

	var used, total *float64
	if m := augUsedOfRE.FindStringSubmatch(text); m != nil {
		if u, ok := ampNum(m[1]); ok {
			if t, ok2 := ampNum(m[2]); ok2 {
				used, total = &u, &t
			}
		}
	} else {
		var monthly, remaining *float64
		if m := augMonthRE.FindStringSubmatch(text); m != nil {
			if v, ok := ampNum(m[1]); ok {
				monthly = &v
			}
		}
		if m := augRemainingRE.FindStringSubmatch(text); m != nil {
			if v, ok := ampNum(m[1]); ok {
				remaining = &v
			}
		}
		if monthly != nil && remaining != nil {
			u := *monthly - *remaining
			used, total = &u, monthly
		} else if remaining != nil {
			// Remaining with no denominator is a balance, not a gauge.
			p.Credits = &Credits{HasCredits: true, Balance: trimAmount(*remaining) + " left"}
		}
	}
	if used != nil && total != nil && *total > 0 {
		pct := *used / *total * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		remaining := *total - *used
		if remaining < 0 {
			remaining = 0
		}
		p.Limits = append(p.Limits, Limit{
			Key: "credits", UsedPercent: round2(pct),
			WindowLabel: sp("month"), ResetsAt: resets,
			UsedAmount: fp(round2(*used)), TotalAmount: fp(round2(*total)),
			RemainingAmount: fp(round2(remaining)), Unit: "credits",
		})
	}

	if len(p.Limits) == 0 && p.Credits == nil {
		switch {
		case augSignedOutRE.MatchString(text):
			return fail("not-signed-in", "cli-signin", "auggie login")
		case exited:
			return fail("cli-failed", "cli-exit", "auggie")
		}
		return fail("no-readings", "cli-nolines", "auggie")
	}

	p.OK = true
	p.RecordedAt = fp(now())
	if m := augPlanRE.FindStringSubmatch(text); m != nil {
		if plan := tame(m[1], 32); plan != "" {
			p.PlanType = sp(plan)
		}
	}
	return p
}
