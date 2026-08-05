package main

// Kiro: quota via the vendor's own CLI, Amp's cost rung exactly.
//
// kiro-cli answers a non-interactive usage question in plain text (verified
// against the real CLI on this machine, not just against CodexBar's parser):
//
//	$ kiro-cli chat --no-interactive /usage
//	Estimated Usage | resets on 2026-09-01 | KIRO FREE
//	Credits (0.00 of 50 covered in plan)
//	████████████████████ 0%
//	Overages: Disabled
//
// No credential is opened or held — kiro-cli talks to its own service with
// its own login. Same parsing philosophy as amp.go: every pattern is anchored
// and literal-prefixed, spacing is [ \t] never \s, numbers carry the grouping
// grammar, and a line that stops matching contributes nothing rather than a
// wrong number.

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

const kiroBinEnv = "MON_KIRO_BIN"

var kiroCache provCache

func collectKiro() Provider {
	return kiroCache.collect(fetchKiro)
}

var (
	// "Estimated Usage | resets on 2026-09-01 | KIRO FREE" — the reset date is
	// either yyyy-MM-dd or a bare MM/DD (older CLI), and the plan is whatever
	// sits after the last pipe.
	kiroResetRE = regexp.MustCompile(`(?i)resets on[ \t]+(\d{4}-\d{2}-\d{2}|\d{2}/\d{2})`)
	kiroHeadRE  = regexp.MustCompile(`(?im)^[ \t]*Estimated Usage[ \t]*\|[^\r\n|]*\|[ \t]*([A-Za-z][A-Za-z0-9 ]+?)[ \t]*$`)
	// "Plan: Q Developer Pro" — the shape CodexBar records for paid seats.
	kiroPlanRE = regexp.MustCompile(`(?im)^[ \t]*Plan:[ \t]*([^\r\n]+?)[ \t]*$`)
	// "Credits (0.00 of 50 covered in plan)" — anchored to the Credits label
	// (allowing the box-drawing/whitespace the CLI pads with) so a stray
	// "(N of M …)" elsewhere in the output cannot be mistaken for it.
	kiroCreditsOfRE = regexp.MustCompile(`(?im)^[ \t│┃]*Credits\b[^\r\n(]*\([ \t]*` + amount + `[ \t]+of[ \t]+` + amount + `[ \t]+covered`)
	// The gauge line: a run of block characters followed by "N%". Requiring
	// the bar to be a RUN (2+) and to sit at the line start keeps a lone
	// glyph in a download/spinner from being read as a quota gauge.
	kiroPctRE = regexp.MustCompile(`(?im)^[ \t│┃]*[█▉▊▋▌▍▎▏]{2,}[ \t]*(\d+)[ \t]*%`)
	// "Credits used: 12.5" — the older shape CodexBar still parses; kept as a
	// fallback so an older CLI loses nothing.
	kiroCreditsUsedRE = regexp.MustCompile(`(?im)^[ \t]*Credits used:[ \t]*` + amount)
	kiroSignedOutRE   = regexp.MustCompile(`(?i)not (?:signed|logged) in|login required|failed to initialize auth portal|kiro-cli login|oauth error`)
)

// A Kiro reset date: yyyy-MM-dd read as local midnight, or a bare MM/DD taken
// as this year unless that is already past, then next year — the CLI's own
// convention (KiroStatusProbe rolls MM/DD forward). Local, not UTC: the CLI
// prints a local date and a UTC reading would be off by up to a day.
func kiroReset(s string) *float64 {
	if len(s) == 10 {
		if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
			return fp(float64(t.Unix()))
		}
		return nil
	}
	// MM/DD
	moStr, dStr, ok := strings.Cut(s, "/")
	if !ok {
		return nil
	}
	mo, e1 := strconv.Atoi(moStr)
	d, e2 := strconv.Atoi(dStr)
	if e1 != nil || e2 != nil || mo < 1 || mo > 12 || d < 1 || d > 31 {
		return nil
	}
	nowT := time.Now()
	t := time.Date(nowT.Year(), time.Month(mo), d, 0, 0, 0, 0, time.Local)
	// time.Date normalises 02/31 to March 3 rather than rejecting it; a date
	// that changed under construction was never a real date.
	if int(t.Month()) != mo || t.Day() != d {
		return nil
	}
	if t.Before(nowT) {
		t = t.AddDate(1, 0, 0)
	}
	return fp(float64(t.Unix()))
}

func fetchKiro() Provider {
	p := Provider{ID: "kiro", Name: "Kiro", Source: "cli", CapturedAt: now()}
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}

	bin := vendorBinary(kiroBinEnv, vendorCandidates("kiro-cli"))
	if bin == "" {
		return fail("not-installed", "cli-missing", "kiro-cli")
	}
	out, exited, err := runVendorCLI(bin, []string{"chat", "--no-interactive", "/usage"},
		[]string{"KIRO_", "AWS_", "Q_"}, 30*time.Second)
	if err != nil {
		if err == errAmpTimeout {
			return fail("unreachable", "cli-timeout", "kiro-cli")
		}
		return fail("cli-failed", "cli-failed", "kiro-cli")
	}
	text := ansiRE.ReplaceAllString(out, "")

	var resets *float64
	if m := kiroResetRE.FindStringSubmatch(text); m != nil {
		resets = kiroReset(m[1])
	}
	var plan string
	if m := kiroHeadRE.FindStringSubmatch(text); m != nil {
		plan = kiroDisplayPlan(m[1])
	}
	if plan == "" {
		if m := kiroPlanRE.FindStringSubmatch(text); m != nil {
			plan = tame(m[1], 32)
		}
	}

	// used% comes from the "N of M covered" line when present (exact), else
	// from the gauge bar's own percent. The two agree; the credits line is
	// preferred because it also yields the balance.
	var pct *float64
	var balance string
	if m := kiroCreditsOfRE.FindStringSubmatch(text); m != nil {
		used, ok1 := ampNum(m[1])
		total, ok2 := ampNum(m[2])
		if ok1 && ok2 && total > 0 {
			v := used / total * 100
			if v < 0 {
				v = 0
			}
			if v > 100 {
				v = 100
			}
			pct = fp(round2(v))
			rem := total - used
			if rem < 0 {
				rem = 0
			}
			balance = trimAmount(rem) + " / " + trimAmount(total)
		}
	}
	if pct == nil {
		if m := kiroPctRE.FindStringSubmatch(text); m != nil {
			if v, ok := ampNum(m[1]); ok && v >= 0 && v <= 100 {
				pct = fp(round2(v))
			}
		}
	}
	if pct != nil {
		p.Limits = append(p.Limits, Limit{
			Key: "credits", UsedPercent: *pct,
			WindowLabel: sp("plan"), ResetsAt: resets,
		})
		if balance != "" {
			p.Credits = &Credits{HasCredits: true, Balance: balance}
		}
	} else if m := kiroCreditsUsedRE.FindStringSubmatch(text); m != nil {
		// The oldest shape names no total, so no gauge — but "how much used"
		// is still a reading, published as a balance rather than invented.
		if v, ok := ampNum(m[1]); ok {
			p.Credits = &Credits{HasCredits: true, Balance: trimAmount(v) + " used"}
		}
	}

	if len(p.Limits) == 0 && p.Credits == nil {
		switch {
		case kiroSignedOutRE.MatchString(text):
			return fail("not-signed-in", "cli-signin", "kiro-cli login")
		case exited:
			return fail("cli-failed", "cli-exit", "kiro-cli")
		}
		return fail("no-readings", "cli-nolines", "kiro-cli")
	}

	p.OK = true
	p.RecordedAt = fp(now())
	if plan != "" {
		p.PlanType = sp(plan)
	}
	return p
}

// "KIRO FREE" -> "Kiro Free": title-case the shouty plan name the CLI prints,
// matching KiroStatusProbe.displayPlanName.
func kiroDisplayPlan(s string) string {
	s = tame(s, 32)
	if s == "" {
		return ""
	}
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
