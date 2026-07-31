// Package main — subnsub-monitor, the shipping helper.
//
// Ported from the Python reference implementation, which stays as the
// reference: the Python
// found the file formats, the endpoint, and every trap five review rounds
// turned up.
//
// On "same wire format", which an earlier version of this comment overstated:
// the two produce payloads the relay and the page treat identically, not
// byte-identical JSON. Known differences, all of them absorbed by the relay's
// field-by-field rebuild — Go omits some keys where Python emits an explicit
// null (an omitted key and a null key mean the same thing to every consumer
// here), Go's Claude limits carry window_minutes:null where Python omits the
// key entirely, and Go does not send extra_usage, which the relay drops
// anyway. Anything a consumer actually reads matches.
package main

import "time"

// A quota window. Providers name their windows differently (Codex says
// primary/secondary, Claude says session/weekly/weekly_scoped) so Key is
// passed through rather than forced into one vocabulary.
//
// Pointers where the Python emits null: a missing reset time and a reset time
// of zero mean very different things, and the page checks for null.
type Limit struct {
	Key           string   `json:"key"`
	UsedPercent   float64  `json:"used_percent"`
	WindowLabel   *string  `json:"window_label"`
	WindowMinutes *float64 `json:"window_minutes"`
	// Unix SECONDS. Claude's API speaks ISO-8601 and its credential file
	// speaks milliseconds; both are converted before they get here, because
	// one unit reaching the page unconverted looks entirely plausible and is
	// entirely wrong.
	ResetsAt *float64 `json:"resets_at"`
	Severity *string  `json:"severity"`
	Scope    *string  `json:"scope"`
	Active   *bool    `json:"active"`
}

type Credits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

// One provider's answer. A provider reports its own failure rather than
// sinking the snapshot — an expired Claude token must not blank out Codex.
type Provider struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Source string `json:"source"` // "local-log" or "api"

	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`

	CapturedAt float64  `json:"captured_at"`
	RecordedAt *float64 `json:"recorded_at,omitempty"`

	PlanType      *string  `json:"plan_type,omitempty"`
	RateLimitTier *string  `json:"rate_limit_tier,omitempty"`
	Limits        []Limit  `json:"limits,omitempty"`
	Credits       *Credits `json:"credits,omitempty"`

	SourceFile string `json:"source_file,omitempty"`
	Truncated  bool   `json:"truncated"`
	Capped     bool   `json:"capped"`
}

type Snapshot struct {
	OK         bool       `json:"ok"`
	CapturedAt float64    `json:"captured_at"`
	Providers  []Provider `json:"providers"`
	// Which machine this came from. One token is pasted on every machine an
	// account watches, so without this they all push into one slot and the
	// page shows whichever pushed last. Random, never derived from hardware —
	// see agent.go. The label is optional and operator-set; omitted when
	// unset, so the page falls back to the id rather than inventing a name.
	AgentID    string `json:"agent_id,omitempty"`
	AgentLabel string `json:"agent_name,omitempty"`
	// Which build is reporting. The dashboard is the only place anyone can
	// find out that a machine is running a helper from before some provider
	// existed — nothing else on a box you have no route to will tell you — and
	// without this the page cannot tell a current helper from a year-old one.
	//
	// Not a fingerprint: it is the same string for everyone on a given build,
	// says nothing about the machine, and is the one piece of self-description
	// here that the user can act on.
	HelperVersion string `json:"helper_version,omitempty"`
	// Whether this machine will run a command the dashboard sends it. Always
	// present, never omitted: the page has to be able to tell "this helper
	// says no" from "this helper is too old to have an opinion", and an
	// omitted key reads as the first when it means the second. See console.go
	// for what turns it on — which is a file on this machine and nothing else.
	Console bool `json:"exec"`
	// Whether this machine will replace its own binary when the dashboard asks
	// it to. Always present for the same reason `exec` is: the page has to tell
	// "this helper says no" apart from "this helper predates the question".
	//
	// A separate key rather than a second meaning for `exec`, even though the
	// console implies this one — see update.go. Folding them together would
	// make turning the console off also turn off a switch nobody set, and would
	// leave the console-off machine that DID opt in with no way to say so.
	Update bool `json:"upd"`
	// Machine health. A pointer so a platform that can measure nothing at all
	// omits the key rather than shipping a shell of nulls — see system.go for
	// what each platform can actually read, and why.
	System *System `json:"system,omitempty"`
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func sp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
func fp(f float64) *float64 { return &f }
func bp(b bool) *bool       { return &b }

// Every provider this build ships, cheapest first so the expensive ones
// cannot delay the free one. Codex is a local file — no credential, no
// network. Antigravity is a request to a server already running on this
// machine, so no credential and nothing off the box. Amp is a subprocess that
// talks to Amp on our behalf, so we hold no key. The rest are the credential
// rung: each reads the credential its own CLI left on disk and calls that
// vendor and nobody else. Claude was the first; provhttp.go carries the rules
// they all inherit.
//
// Ids match CodexBar's registry on purpose — the site's coverage endpoint
// diffs the two lists by these exact strings, and a test holds this table and
// that endpoint's SUPPORTED array to being the same list.
var collectors = []struct {
	id string
	fn func() Provider
}{
	{"codex", collectCodex},
	{"antigravity", collectAntigravity},
	{"amp", collectAmp},
	{"claude", collectClaude},
	{"gemini", collectGemini},
	{"copilot", collectCopilot},
	{"factory", collectFactory},
	{"kimi", collectKimi},
}

// Collect every provider. A collector that panics is contained and reported —
// these run on other people's machines against files this program does not
// control, and one bad rollout should not take the helper down.
func collectAll() Snapshot {
	snap := Snapshot{CapturedAt: now(), AgentID: agentID(), AgentLabel: agentLabel(),
		HelperVersion: helperVersion, Console: consoleEnabled(), Update: updateAllowed()}
	for _, c := range collectors {
		snap.Providers = append(snap.Providers, safeCollect(c.id, c.fn))
	}
	// Machine health rides along with the quota. Deliberately NOT part of
	// snap.OK: a box with no AI tooling installed still reports its own health,
	// but "ok" has always meant "at least one provider answered", and widening
	// it here would flip every no-provider machine from a visible failure to a
	// silent success.
	if sys := collectSystem(); sys.OK || len(sys.Missing) > 0 {
		snap.System = &sys
	}
	for _, p := range snap.Providers {
		if p.OK {
			snap.OK = true
		}
	}
	return snap
}

func safeCollect(id string, fn func() Provider) (p Provider) {
	defer func() {
		if r := recover(); r != nil {
			// Type/message of a panic can quote whatever it was handed, and
			// these collectors handle credentials. Say nothing about it.
			p = Provider{ID: id, OK: false, Error: "collector-crashed",
				Detail: "collector panicked", CapturedAt: now()}
		}
	}()
	return fn()
}
