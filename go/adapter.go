package main

// The quota reader behind the CPA rung, driven by a table rather than by code.
//
// Every vendor endpoint this program reads through CLI Proxy API answers in
// JSON, and everything that has ever changed about one of them is DATA: the
// path, a header (Codex wants a `codex-tui/<version>` User-Agent, and the
// version moves), the name of a field, whether a reset is an ISO string or an
// epoch. None of it is logic. So the logic lives here once — resolve a path,
// iterate an array, turn a number into a percentage, turn a timestamp into
// seconds — and the per-vendor knowledge lives in adapters.json, which this
// binary embeds today and (the next step, see cpa.go) will be able to swap for
// a signed newer copy without a rebuild. The table is deliberately NOT a
// programme: it can name fields and formats, and it cannot name a host outside
// the allow-list in cpa.go, run a command, or say where a credential goes.
//
// The vocabulary, kept small on purpose:
//
//	requests[]   what to ask, in order. `{cred.a|b}` and `{jwt:a|b:claim}`
//	             fill headers and bodies from the credential CPA listed.
//	windows[]    how to read a quota window out of a response. `each` walks
//	             an array (`groups[].buckets[]` nests), `base` steps into an
//	             object, and a field path starting with `^` climbs back up one
//	             level per caret; `$.` is the document root.
//	plan / tier  the plan chip, from a field or from the first case that holds.
//	credits      a credit balance and its flags.
//	resets       reset credits: how many are held and when each lapses, and
//	             how many could be used now, what holds the rest back, and
//	             which windows one clears.
//	renews       when the plan renews — a date still ahead, or nothing.
//	ends         when it ends and does not renew, where it does not renew.
//	until        how long its current period runs, where the vendor says
//	             neither; one plan date at most, in that order.
//	limited      whether the vendor says the account is at its limit now:
//	             flags out of the documents, any one of them stated.
//
// A request's URL takes the same placeholders as its headers (query values
// only, escaped; the host is checked again after filling).
//
// Paths accept `a|b` alternatives (snake or camel, whichever the vendor sent
// this week), `[N]` indexes, `*` for "whichever entry" of an object keyed by
// something unknowable, and nothing else — no functions.

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed adapters.json
var builtinAdaptersJSON []byte

type adapterSet struct {
	Version int `json:"version"`
	// The oldest helper that reads this table as it is meant. A key a helper
	// does not know is ignored rather than refused, so a table that leans on
	// vocabulary taught later says which release taught it, and an older
	// helper keeps the table it has. (Helpers before 2026.10.07.2 do not read
	// this key; what keeps them off a table naming hosts they never allowed
	// is that allow-list, which refuses the table whole on those builds.)
	MinHelper string                    `json:"min_helper,omitempty"`
	Vendors   map[string]*vendorAdapter `json:"vendors"`
}

type vendorAdapter struct {
	Provider string           `json:"provider"`
	Name     string           `json:"name"`
	Requests []adapterRequest `json:"requests"`
	Windows  []adapterWindow  `json:"windows"`
	Plan     *adapterValue    `json:"plan,omitempty"`
	Tier     *adapterValue    `json:"tier,omitempty"`
	// The account's credit balance, its reset credits, and when its plan
	// renews. Resets and renews are lists tried in order: the first spec that
	// lands answers (a live count before a summary's, a live date before the
	// one in the credential's own claims).
	Credits *adapterCredits `json:"credits,omitempty"`
	Resets  []adapterResets `json:"resets,omitempty"`
	Renews  []adapterStamp  `json:"renews,omitempty"`
	// The plan's date where the vendor does not say it renews (2026.10.07.6):
	// `ends` where it says the plan ends then, `until` where it says only
	// how long the period runs. Asked in the order renews, ends, until; the
	// first that lands is the plan's one date (adapterPlanDate).
	Ends  []adapterStamp `json:"ends,omitempty"`
	Until []adapterStamp `json:"until,omitempty"`
	// Whether the account is at its limit now, in the vendor's own words
	// (2026.10.07.4): any one flag stated is a yes.
	Limited []adapterFlag `json:"limited,omitempty"`
	// Where the vendor's own tool leaves its credential on a machine, for
	// the local rung (local.go): the same requests and windows, with the
	// token read from this file instead of filled in by a proxy.
	Credential *adapterCredential `json:"credential,omitempty"`
}

type adapterCredential struct {
	// Tried in order; `~` and `$NAME` expand. The first regular file wins.
	Files []string `json:"files"`
	// macOS: the generic-password service the vendor's tool keeps the same
	// document under, read when none of the files exists (local.go).
	Keychain      string        `json:"keychain,omitempty"`
	Token         string        `json:"token"`
	Expires       string        `json:"expires,omitempty"`
	ExpiresFormat string        `json:"expires_format,omitempty"`
	Account       string        `json:"account,omitempty"`
	Plan          *adapterValue `json:"plan,omitempty"`
	Tier          *adapterValue `json:"tier,omitempty"`
	// The command to run once to sign in again, for the error line.
	Signin string `json:"signin,omitempty"`
}

type adapterRequest struct {
	ID       string            `json:"id"`
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Fallback []string          `json:"fallback_urls,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     string            `json:"body,omitempty"`
	// A request the reading can do without: its failure costs the fields it
	// would have filled and nothing else.
	Optional bool `json:"optional,omitempty"`
	// Credential templates that must resolve before the request is worth
	// sending, each named for the error line ("project_id").
	Requires map[string]string `json:"requires,omitempty"`
	// A request for one channel only ("cpa" or "local"); empty is both. The
	// profile call that fills Claude's plan chip is the proxy's — the local
	// rung reads the plan out of the credential file instead.
	Channel string `json:"channel,omitempty"`
	// Headers laid over `headers` for one channel. Found the hard way
	// (2026-09-09): `anthropic-beta: oauth-2025-04-20`, which the console
	// sends through the proxy, is answered 429 when sent with the token
	// Claude Code left on disk — same endpoint, same token, one header.
	HeadersCPA   map[string]string `json:"headers_cpa,omitempty"`
	HeadersLocal map[string]string `json:"headers_local,omitempty"`
}

// The headers a request sends on a channel: the shared ones, then the
// channel's own on top.
func (r *adapterRequest) headersFor(channel string) map[string]string {
	out := make(map[string]string, len(r.Headers)+4)
	for k, v := range r.Headers {
		out[k] = v
	}
	extra := r.HeadersCPA
	if channel == "local" {
		extra = r.HeadersLocal
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

type adapterWindow struct {
	From string `json:"from"`
	Each string `json:"each,omitempty"`
	Base string `json:"base,omitempty"`
	// Only rows the match admits become windows.
	When *adapterMatch `json:"when,omitempty"`
	// Templates. `key` is the wire key; `label` the short human one.
	Key            string            `json:"key"`
	KeyMap         map[string]string `json:"key_map,omitempty"`
	Label          string            `json:"label,omitempty"`
	LabelMap       map[string]string `json:"label_map,omitempty"`
	LabelBySeconds map[string]string `json:"label_by_seconds,omitempty"`
	// The figure. used_kind: percent (default), remaining_percent,
	// remaining_fraction, used_fraction, or ratio (used over limit).
	Used        string  `json:"used,omitempty"`
	UsedKind    string  `json:"used_kind,omitempty"`
	Limit       string  `json:"limit,omitempty"`
	Remaining   string  `json:"remaining,omitempty"`
	AmountScale float64 `json:"amount_scale,omitempty"`
	Unit        string  `json:"unit,omitempty"`
	// Where the amounts' unit is the vendor's own field (2026.10.07.7):
	// `unit_from` names it and `unit_map` maps its values onto the units the
	// relay knows. A value the map does not name is a unit this cannot say,
	// and the window keeps its share without amounts — never a figure in the
	// wrong currency. No field at all is `unit`.
	UnitFrom string            `json:"unit_from,omitempty"`
	UnitMap  map[string]string `json:"unit_map,omitempty"`
	// When the window turns over. resets_format: auto (default), iso,
	// epoch_s, epoch_ms, after_s.
	Resets       string `json:"resets,omitempty"`
	ResetsFormat string `json:"resets_format,omitempty"`
	// How long the window is, by whichever field the vendor offers.
	WindowSeconds string             `json:"window_seconds,omitempty"`
	WindowMinutes string             `json:"window_minutes,omitempty"`
	WindowUnit    string             `json:"window_unit,omitempty"`
	UnitMinutes   map[string]float64 `json:"unit_minutes,omitempty"`
	Active        string             `json:"active,omitempty"`
	Severity      string             `json:"severity,omitempty"`
	Scope         string             `json:"scope,omitempty"`
	// A legacy row that only applies when the modern one produced nothing:
	// skipped when any listed key was already emitted.
	SkipIfAny []string `json:"skip_if_any,omitempty"`
	// A later row for a key already emitted is dropped — the table's order
	// says which reading is trusted. `merge: "max"` lets it stand instead
	// when it reads MORE used than the one before: two readings of one
	// period that disagree, and the more exhausted is the one to believe
	// (CodexBar KimiUsageSnapshot.resolvedRatioWindow,
	// steipete/CodexBar@8ab81e2ebd — Kimi's ratio pool beside its counter,
	// where until then only a zero pool gave way to the counter). The row
	// that stands replaces the other whole, amounts and all, save what it
	// does not say about the period itself: its length and its reset, kept
	// from the row it replaces when it names none — one period, one length
	// (Kimi's 5h pool names no duration; without the counter's 300 minutes
	// the page would draw no pace mark). A helper from before this key
	// drops the later row as it always did.
	Merge string `json:"merge,omitempty"`
}

type adapterMatch struct {
	Path string   `json:"path"`
	Eq   string   `json:"eq,omitempty"`
	Ne   string   `json:"ne,omitempty"`
	In   []string `json:"in,omitempty"`
}

type adapterValue struct {
	From  string            `json:"from"`
	Path  string            `json:"path,omitempty"`
	Map   map[string]string `json:"map,omitempty"`
	AnyOf []adapterCase     `json:"any_of,omitempty"`
	// A value the windows read beside it contradict is not shown (the helper
	// after 2026.10.18.5). Antigravity: Cloud Code can name currentTier
	// free-tier for a paid account, and five-hour quota is paid-only — a Free
	// beside a measured 5h window is the generic tier, not the plan (CodexBar
	// AntigravityRemoteUsageFetcher.resolvePlan, steipete/CodexBar@f50cd6cee4).
	// Only the contradiction goes; no plan is inferred from the window. A
	// helper before this key shows the value it always did.
	UnlessWindow *adapterContradiction `json:"unless_window,omitempty"`
}

// `value` is dropped when a window labelled any of `labels` was read. A
// window is read only when it was measured and not switched off, so a bucket
// the vendor disabled or left without a figure contradicts nothing.
type adapterContradiction struct {
	Value  string   `json:"value"`
	Labels []string `json:"labels"`
}

func (c *adapterContradiction) holds(value string, limits []Limit) bool {
	if c == nil || value == "" || !strings.EqualFold(value, c.Value) {
		return false
	}
	for _, l := range limits {
		if l.WindowLabel == nil {
			continue
		}
		for _, label := range c.Labels {
			if *l.WindowLabel == label {
				return true
			}
		}
	}
	return false
}

type adapterCase struct {
	Path  string `json:"path"`
	Eq    string `json:"eq,omitempty"`
	Value string `json:"value"`
}

// A credit balance: `balance` as the vendor spells it, with its two flags.
type adapterCredits struct {
	From      string `json:"from"`
	Base      string `json:"base,omitempty"`
	Has       string `json:"has,omitempty"`
	Unlimited string `json:"unlimited,omitempty"`
	Balance   string `json:"balance,omitempty"`
}

// Reset credits. With `each`, the matched items are the credits (Codex) or
// the grants (Claude): each item is one credit, unless `item_left` counts
// inside it, and `expires` is read per item that still holds one. `left` and
// `total` at the base, when the vendor states them, stand over the sums.
//
// `gate` names a flag at the base that must be true for the whole block to
// count (Claude's `eligible`); `starts` a date per item before which it is
// not yet held. An item whose `expires` has passed is not held either. All
// three per CodexBar's ClaudeRateLimitResetCredits (steipete/CodexBar@05a6a434).
// A helper from before these keys ignores them and counts as it always did.
type adapterResets struct {
	From          string        `json:"from"`
	Base          string        `json:"base,omitempty"`
	Gate          string        `json:"gate,omitempty"`
	Left          string        `json:"left,omitempty"`
	Total         string        `json:"total,omitempty"`
	Each          string        `json:"each,omitempty"`
	When          *adapterMatch `json:"when,omitempty"`
	ItemLeft      string        `json:"item_left,omitempty"`
	ItemTotal     string        `json:"item_total,omitempty"`
	Starts        string        `json:"starts,omitempty"`
	StartsFormat  string        `json:"starts_format,omitempty"`
	Expires       string        `json:"expires,omitempty"`
	ExpiresFormat string        `json:"expires_format,omitempty"`

	// What can be done with them now (2026.10.07.3). `usable` is a count
	// stated anywhere (Codex: the usage call's applicable count). Without
	// one, an item's credits are usable when the base is `eligible`, every
	// `item_usable` match holds for the item, and — when `item_needs_limit`
	// holds for it — the base is `at_limit`; a credit held back by that
	// alone is what NeedsLimit says. `wait_until` is a cooldown at the base;
	// `item_clears` lists the windows an item clears, each mapped by
	// `clears_map` to a window's length ("5h", "7d") — an unmapped name is
	// not one.
	Usable         *adapterRef       `json:"usable,omitempty"`
	Eligible       *adapterMatch     `json:"eligible,omitempty"`
	ItemUsable     []adapterMatch    `json:"item_usable,omitempty"`
	ItemNeedsLimit *adapterMatch     `json:"item_needs_limit,omitempty"`
	AtLimit        *adapterMatch     `json:"at_limit,omitempty"`
	WaitUntil      string            `json:"wait_until,omitempty"`
	ItemClears     string            `json:"item_clears,omitempty"`
	ClearsMap      map[string]string `json:"clears_map,omitempty"`
}

// A value out of a document by request id (or `cred`), from its root.
type adapterRef struct {
	From string `json:"from"`
	Path string `json:"path"`
}

// A window's length as the windows are labelled: digits and one unit.
var clearsLabelRE = regexp.MustCompile(`^[0-9]{1,3}[mhd]$`)

// A yes the vendor states outright, out of a document by request id (or
// `cred`): the path is there and equals `eq` (or one of `in`). A field that
// is missing is never a yes, and a flag with nothing to equal is no flag.
type adapterFlag struct {
	From string   `json:"from"`
	Path string   `json:"path"`
	Eq   string   `json:"eq,omitempty"`
	In   []string `json:"in,omitempty"`
}

func (f *adapterFlag) holds(docs map[string]any, cred map[string]any) bool {
	ctx := adapterDoc(f.From, docs, cred)
	if ctx == nil || (f.Eq == "" && len(f.In) == 0) {
		return false
	}
	return (&adapterMatch{Path: f.Path, Eq: f.Eq, In: f.In}).holds(ctx)
}

// A date out of a document, or out of the credential (`from: "cred"`).
// `unless` names a field whose being there at all, past or not, means this is
// not the date (2026.10.07.6): an entitlement that names a renewal does not
// end on its expiry, even on renewal day, between the one and the other.
// `when` admits the date only while the match holds: Codex's `active_until`
// is the renewal date when `will_renew` is true and the END of the plan when
// it is false (CodexBar OpenAISubscriptionMetadataModels, @5b0b9fa3e3), and
// a stamp that cannot tell the two apart shows a cancelled plan renewing.
type adapterStamp struct {
	From   string        `json:"from"`
	Path   string        `json:"path"`
	Format string        `json:"format,omitempty"`
	Unless string        `json:"unless,omitempty"`
	When   *adapterMatch `json:"when,omitempty"`
}

// ── the live table ─────────────────────────────────────────────────────────

var adapterState struct {
	sync.Mutex
	set *adapterSet
}

// The embedded table, put in force if nothing is yet. Under the lock.
func ensureBaseLocked() {
	if adapterState.set != nil {
		return
	}
	set, err := parseAdapters(builtinAdaptersJSON)
	if err != nil {
		// The embedded table is checked by adapter_test.go; reaching this
		// line means a build shipped with a broken file, and the honest
		// answer is an empty table (every CPA credential then reports
		// `no-window`) rather than a panic on every push.
		warnf("built-in adapters.json is unusable: %v", err)
		set = &adapterSet{Vendors: map[string]*vendorAdapter{}}
	}
	adapterState.set = set
}

// The table in force: the embedded copy, unless an earlier fetch kept a
// newer one beside the token or a fetch since put one in (adapterfeed.go).
// Never nil.
func currentAdapters() *adapterSet {
	loadPersistedAdapters()
	adapterState.Lock()
	defer adapterState.Unlock()
	ensureBaseLocked()
	return adapterState.set
}

// Put a table in force if it is newer than the one there. The caller has
// checked the signature; this checks the shape, the hosts and the version.
// Returns the version now in force.
func installAdaptersIfNewer(raw []byte) (int, bool, error) {
	set, err := parseAdapters(raw)
	if err != nil {
		return 0, false, err
	}
	adapterState.Lock()
	defer adapterState.Unlock()
	ensureBaseLocked()
	if set.Version <= adapterState.set.Version {
		return adapterState.set.Version, false, nil
	}
	adapterState.set = set
	return set.Version, true, nil
}

func parseAdapters(raw []byte) (*adapterSet, error) {
	var set adapterSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return nil, errors.New("adapters: not valid JSON")
	}
	if set.Version < 1 || len(set.Vendors) == 0 {
		return nil, errors.New("adapters: no version or no vendors")
	}
	if set.MinHelper != "" && (!validVersion(set.MinHelper) || !versionAtLeast(helperVersion, set.MinHelper)) {
		return nil, errors.New("adapters: this table needs helper " + cleanText(set.MinHelper, 16) + " or newer")
	}
	for vendor, ad := range set.Vendors {
		if ad == nil || ad.Provider == "" || len(ad.Requests) == 0 {
			return nil, errors.New("adapters: vendor " + vendor + " is incomplete")
		}
		for _, r := range ad.Requests {
			for _, u := range append([]string{r.URL}, r.Fallback...) {
				if !cpaHostAllowed(u) {
					return nil, errors.New("adapters: " + vendor + " names a host off the allow-list")
				}
			}
		}
	}
	return &set, nil
}

// Whether version `have` is `need` or later, both YYYY.MM.DD.N, compared
// component by component as numbers — "2026.10.07.10" is after ".9".
func versionAtLeast(have, need string) bool {
	h, n := strings.Split(have, "."), strings.Split(need, ".")
	for i := 0; i < len(h) || i < len(n); i++ {
		a, b := 0, 0
		if i < len(h) {
			a, _ = strconv.Atoi(h[i])
		}
		if i < len(n) {
			b, _ = strconv.Atoi(n[i])
		}
		if a != b {
			return a > b
		}
	}
	return true
}

// ── paths ──────────────────────────────────────────────────────────────────

// A resolution context: the document at [0], then one entry per `each` level
// and one for `base`. Field paths read from the top; `^` climbs.
type adapterCtx []any

func (c adapterCtx) top() any {
	if len(c) == 0 {
		return nil
	}
	return c[len(c)-1]
}

// Resolve a path against the context. Alternatives are tried in order and the
// first that lands on a non-nil value wins.
func (c adapterCtx) get(path string) any {
	for _, alt := range strings.Split(path, "|") {
		alt = strings.TrimSpace(alt)
		if alt == "" {
			continue
		}
		if v := c.getOne(alt); v != nil {
			return v
		}
	}
	return nil
}

func (c adapterCtx) getOne(path string) any {
	var root any
	switch {
	case strings.HasPrefix(path, "$."):
		if len(c) == 0 {
			return nil
		}
		root, path = c[0], path[2:]
	case path == "$":
		if len(c) == 0 {
			return nil
		}
		return c[0]
	default:
		up := 0
		for strings.HasPrefix(path, "^") {
			up++
			path = path[1:]
		}
		idx := len(c) - 1 - up
		if idx < 0 {
			return nil
		}
		root = c[idx]
	}
	if path == "" {
		return root
	}
	cur := root
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return nil
		}
		key, index := seg, -1
		if i := strings.IndexByte(seg, '['); i >= 0 && strings.HasSuffix(seg, "]") {
			n, err := strconv.Atoi(seg[i+1 : len(seg)-1])
			if err != nil || n < 0 {
				return nil
			}
			key, index = seg[:i], n
		}
		if key == "*" {
			// Any one entry of an object whose keys are not knowable in
			// advance — grok's auth.json is keyed by issuer and account id.
			// The lowest key, so the choice is the same on every read.
			m, ok := cur.(map[string]any)
			if !ok || len(m) == 0 {
				return nil
			}
			first := ""
			for k := range m {
				if first == "" || k < first {
					first = k
				}
			}
			cur = m[first]
		} else if key != "" {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[key]
		}
		if index >= 0 {
			arr, ok := cur.([]any)
			if !ok || index >= len(arr) {
				return nil
			}
			cur = arr[index]
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

// Every context an `each` path names. `groups[].buckets[]` yields one context
// per bucket, with the bucket on top and its group one level down.
func (c adapterCtx) iterate(each string) []adapterCtx {
	if each == "" {
		return []adapterCtx{c}
	}
	i := strings.Index(each, "[]")
	if i < 0 {
		v := c.get(each)
		if v == nil {
			return nil
		}
		return []adapterCtx{append(append(adapterCtx{}, c...), v)}
	}
	head, rest := each[:i], strings.TrimPrefix(each[i+2:], ".")
	arr, ok := c.get(head).([]any)
	if !ok {
		return nil
	}
	var out []adapterCtx
	for _, item := range arr {
		if item == nil {
			continue
		}
		next := append(append(adapterCtx{}, c...), item)
		out = append(out, next.iterate(rest)...)
		if len(out) >= 64 {
			break // a quota document with more windows than this is not one
		}
	}
	return out
}

// ── values ─────────────────────────────────────────────────────────────────

// A number from whatever the vendor sent: a number, a numeric string, a
// percentage string, or a `{val: n}` wrapper. Strings ending in % are given
// as percent (12% → 12); the caller knows which kind it asked for.
func adapterNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, finite(x)
	case json.Number:
		f, err := x.Float64()
		return f, err == nil && finite(f)
	case string:
		s := strings.TrimSpace(x)
		pct := strings.HasSuffix(s, "%")
		s = strings.TrimSuffix(s, "%")
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || !finite(f) {
			return 0, false
		}
		if pct {
			return f, true
		}
		return f, true
	case map[string]any:
		if inner, ok := x["val"]; ok {
			return adapterNum(inner)
		}
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// A fraction from a value that may be "0.9", 0.9 or "90%".
func adapterFraction(v any) (float64, bool) {
	if s, ok := v.(string); ok && strings.HasSuffix(strings.TrimSpace(s), "%") {
		f, ok := adapterNum(v)
		return f / 100, ok
	}
	return adapterNum(v)
}

func adapterStr(v any, limit int) string {
	switch x := v.(type) {
	case string:
		return cleanText(x, limit)
	case float64:
		return cleanText(trimAmount(x), limit)
	case json.Number:
		return cleanText(x.String(), limit)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}

func adapterBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes", "y", "on":
			return true, true
		case "false", "0", "no", "n", "off":
			return false, true
		}
	case float64:
		return x != 0, true
	}
	return false, false
}

// A reset instant in unix seconds, or 0. `auto` reads the shape: an ISO
// string, an epoch in seconds or milliseconds by magnitude, or a small number
// as seconds from now — which is the one that needs `at`.
func adapterReset(v any, format string, at float64) float64 {
	if v == nil {
		return 0
	}
	if adapterIsBool(v) {
		// Not an instant: `true` would read as one second from now
		// (adapterFigure says why a boolean is never a figure).
		return 0
	}
	if format == "" || format == "auto" {
		if s, ok := v.(string); ok {
			if e := isoToEpoch(strings.TrimSpace(s)); e > 0 {
				return e
			}
			if n, ok := adapterNum(s); ok {
				return adapterReset(n, "auto", at)
			}
			return 0
		}
		n, ok := adapterNum(v)
		if !ok || n <= 0 {
			return 0
		}
		switch {
		case n > 1e11:
			return n / 1000
		case n > 1e9:
			return n
		default:
			return at + n
		}
	}
	switch format {
	case "iso":
		if s, ok := v.(string); ok {
			return isoToEpoch(strings.TrimSpace(s))
		}
		return 0
	case "epoch_s":
		if n, ok := adapterNum(v); ok && n > 0 {
			return n
		}
	case "epoch_ms":
		if n, ok := adapterNum(v); ok && n > 0 {
			return n / 1000
		}
	case "after_s":
		if n, ok := adapterNum(v); ok && n >= 0 {
			return at + n
		}
	case "after_ms":
		if n, ok := adapterNum(v); ok && n >= 0 {
			return at + n/1000
		}
	}
	return 0
}

// ── templates ──────────────────────────────────────────────────────────────

// `{path}` from the context, `{cred.path}` from the credential, `{jwt:field:
// claim}` from a JWT the credential carries (the claim is looked for at the
// top and under the OpenAI namespace, which is where Codex keeps its account
// id). An unresolvable placeholder yields "", and the caller decides what
// an empty value means — a header is dropped, a required body field fails.
func adapterTemplate(tpl string, ctx adapterCtx, cred map[string]any) string {
	if !strings.Contains(tpl, "{") {
		return tpl
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(tpl, '{')
		if i < 0 {
			b.WriteString(tpl)
			break
		}
		j := strings.IndexByte(tpl[i+1:], '}')
		inner := ""
		if j >= 0 {
			inner = tpl[i+1 : i+1+j]
		}
		if j < 0 || !placeholderLike(inner) {
			// A brace that is not a placeholder — a JSON body's own — is
			// text, and the scan moves on past it.
			b.WriteString(tpl[:i+1])
			tpl = tpl[i+1:]
			continue
		}
		b.WriteString(tpl[:i])
		b.WriteString(adapterPlaceholder(inner, ctx, cred))
		tpl = tpl[i+1+j+1:]
	}
	return b.String()
}

// A request URL with its placeholders filled the way a header's are — an
// unresolvable one gives "", and the request's `requires` is what refuses to
// send it half-filled — except that each value is escaped as a query
// component, so nothing a credential holds can reach past where it was put.
// The caller checks the filled URL's host against the allow-list again.
func adapterTemplateURL(tpl string, ctx adapterCtx, cred map[string]any) string {
	if !strings.Contains(tpl, "{") {
		return tpl
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(tpl, '{')
		if i < 0 {
			b.WriteString(tpl)
			break
		}
		j := strings.IndexByte(tpl[i+1:], '}')
		inner := ""
		if j >= 0 {
			inner = tpl[i+1 : i+1+j]
		}
		if j < 0 || !placeholderLike(inner) {
			b.WriteString(tpl[:i+1])
			tpl = tpl[i+1:]
			continue
		}
		b.WriteString(tpl[:i])
		b.WriteString(url.QueryEscape(adapterPlaceholder(inner, ctx, cred)))
		tpl = tpl[i+1+j+1:]
	}
	return b.String()
}

// `{…}` is a placeholder only when its inside could be one: a path, with no
// quotes, braces or whitespace. `{"project":"…"}` is JSON, not a template.
func placeholderLike(inner string) bool {
	if inner == "" || strings.ContainsAny(inner, "{}\" \t\r\n") {
		return false
	}
	return true
}

func adapterPlaceholder(ph string, ctx adapterCtx, cred map[string]any) string {
	switch {
	case strings.HasPrefix(ph, "cred."):
		return adapterStr(adapterCtx{cred}.get(ph[5:]), 200)
	case strings.HasPrefix(ph, "jwt:"):
		parts := strings.SplitN(ph[4:], ":", 2)
		if len(parts) != 2 {
			return ""
		}
		raw, _ := adapterCtx{cred}.get(parts[0]).(string)
		claims := jwtClaims(raw)
		if claims == nil {
			return ""
		}
		if v := adapterStr(claims[parts[1]], 200); v != "" {
			return v
		}
		if ns, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
			return adapterStr(ns[parts[1]], 200)
		}
		return ""
	default:
		return adapterStr(ctx.get(ph), 200)
	}
}

// The payload of a JWT, unverified — it is read for an account id to put in a
// header, never for anything that would be trusted.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 || len(parts[1]) > 16*1024 {
		return nil
	}
	seg := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.StdEncoding.DecodeString(seg)
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

// ── reading a document set ─────────────────────────────────────────────────

func (m *adapterMatch) holds(ctx adapterCtx) bool {
	if m == nil {
		return true
	}
	v := adapterStr(ctx.get(m.Path), 200)
	if m.Eq != "" && !strings.EqualFold(v, m.Eq) {
		return false
	}
	if m.Ne != "" && strings.EqualFold(v, m.Ne) {
		return false
	}
	if len(m.In) > 0 {
		hit := false
		for _, want := range m.In {
			if strings.EqualFold(v, want) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return v != "" || (m.Eq == "" && len(m.In) == 0)
}

type adapterReading struct {
	Limits  []Limit
	Plan    string
	Tier    string
	Credits *Credits
	Resets  *ResetCredits
	// The plan's one date and what it is ("renews", "ends", "until"); the
	// date came from the credential's own claim, not a live answer.
	PlanAt        *float64
	PlanKind      string
	PlanFromClaim bool
	Limited       bool
}

// What makes two windows the same one: the wire key, and the scope where
// there is one — a 7d Opus window beside the 7d window is two windows.
func limitDedupeKey(l Limit) string {
	if l.Scope != nil {
		return l.Key + "\x00" + *l.Scope
	}
	return l.Key
}

// Every window the table can read out of the documents. Window order is the
// table's order; a window with no usable figure is simply not a window.
func adapterRun(ad *vendorAdapter, docs map[string]any, cred map[string]any, at float64) adapterReading {
	var out adapterReading
	emitted := map[string]bool{}
	for _, w := range ad.Windows {
		doc, ok := docs[w.From]
		if !ok || doc == nil {
			continue
		}
		if len(w.SkipIfAny) > 0 {
			skip := false
			for _, k := range w.SkipIfAny {
				if emitted[k] {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
		}
		for _, item := range (adapterCtx{doc}).iterate(w.Each) {
			ctx := item
			if w.Base != "" {
				b := ctx.get(w.Base)
				if b == nil {
					continue
				}
				ctx = append(append(adapterCtx{}, ctx...), b)
			}
			if !w.When.holds(ctx) {
				continue
			}
			lim, ok := adapterLimit(&w, ctx, cred, at)
			if !ok {
				continue
			}
			dedupe := limitDedupeKey(lim)
			if emitted[dedupe] {
				if w.Merge == "max" {
					for i := range out.Limits {
						if limitDedupeKey(out.Limits[i]) == dedupe {
							if lim.UsedPercent > out.Limits[i].UsedPercent {
								if lim.WindowMinutes == nil {
									lim.WindowMinutes = out.Limits[i].WindowMinutes
								}
								if lim.ResetsAt == nil {
									lim.ResetsAt = out.Limits[i].ResetsAt
								}
								out.Limits[i] = lim
							}
							break
						}
					}
				}
				continue
			}
			emitted[dedupe] = true
			emitted[lim.Key] = true
			out.Limits = append(out.Limits, lim)
			if len(out.Limits) >= 8 {
				break
			}
		}
	}
	if ad.Plan != nil {
		out.Plan = adapterPick(ad.Plan, docs, cred)
		if ad.Plan.UnlessWindow.holds(out.Plan, out.Limits) {
			out.Plan = ""
		}
	}
	if ad.Tier != nil {
		out.Tier = adapterPick(ad.Tier, docs, cred)
	}
	if ad.Credits != nil {
		out.Credits = adapterCreditsRun(ad.Credits, docs, cred)
	}
	out.Resets = adapterResetsRun(ad.Resets, docs, cred, at)
	out.PlanAt, out.PlanKind, out.PlanFromClaim = adapterPlanDate(ad, docs, cred, at)
	for i := range ad.Limited {
		if ad.Limited[i].holds(docs, cred) {
			out.Limited = true
			break
		}
	}
	return out
}

// The document a spec reads: a response by request id, or the credential.
func adapterDoc(from string, docs map[string]any, cred map[string]any) adapterCtx {
	if from == "cred" {
		if cred == nil {
			return nil
		}
		return adapterCtx{cred}
	}
	if doc := docs[from]; doc != nil {
		return adapterCtx{doc}
	}
	return nil
}

// The base step a spec names, or the document itself; nil when the base is
// not there.
func adapterBase(ctx adapterCtx, base string) adapterCtx {
	if ctx == nil || base == "" {
		return ctx
	}
	b := ctx.get(base)
	if b == nil {
		return nil
	}
	return append(append(adapterCtx{}, ctx...), b)
}

var creditBalanceRE = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,6})?$`)

// A credit balance, when there is one worth a row: unlimited, or flagged as
// held, or a positive figure. A team account answering "0, no credits" has
// no balance to show, and a row saying 0 on every such card is noise.
func adapterCreditsRun(c *adapterCredits, docs map[string]any, cred map[string]any) *Credits {
	ctx := adapterBase(adapterDoc(c.From, docs, cred), c.Base)
	if ctx == nil {
		return nil
	}
	unlimited, has := false, false
	if c.Unlimited != "" {
		unlimited, _ = adapterBool(ctx.get(c.Unlimited))
	}
	if c.Has != "" {
		has, _ = adapterBool(ctx.get(c.Has))
	}
	balance := ""
	if c.Balance != "" {
		// The vendor's own spelling of a plain number, or nothing: this text
		// is published and rendered, and a figure is all it may carry.
		if s := adapterStr(ctx.get(c.Balance), 24); creditBalanceRE.MatchString(s) {
			balance = s
		}
	}
	positive := false
	if n, ok := adapterNum(balance); ok && n > 0 {
		positive = true
	}
	if !unlimited && !has && !positive {
		return nil
	}
	return &Credits{HasCredits: has || positive, Unlimited: unlimited, Balance: balance}
}

// Whether the first array an `each` path walks is present as an array.
func adapterListThere(ctx adapterCtx, each string) bool {
	head := each
	if i := strings.Index(each, "[]"); i >= 0 {
		head = each[:i]
	}
	_, ok := ctx.get(head).([]any)
	return ok
}

// The first reset-credit spec that can say how many are held. A spec that
// answers zero answers: an exhausted count is not a reason to believe a
// staler one further down the list, and zero is shown as nothing.
func adapterResetsRun(specs []adapterResets, docs map[string]any, cred map[string]any, at float64) *ResetCredits {
	for _, s := range specs {
		ctx := adapterBase(adapterDoc(s.From, docs, cred), s.Base)
		if ctx == nil {
			continue
		}
		if s.Gate != "" {
			// A block whose flag is not the boolean true — false, null,
			// missing, "yes" — holds nothing, whatever its list says.
			if b, ok := ctx.get(s.Gate).(bool); !ok || !b {
				continue
			}
		}
		var left, total, usable float64
		haveLeft, haveTotal, haveUsable, needsLimit := false, false, false, false
		var expires []float64
		var clears []string
		// The account-wide halves of "usable now", read once at the base.
		// A missing matcher is no condition — except at_limit, which an
		// account only is when the vendor says so.
		eligible := s.Eligible.holds(ctx)
		atLimit := s.AtLimit != nil && s.AtLimit.holds(ctx)
		itemRules := len(s.ItemUsable) > 0 || s.ItemNeedsLimit != nil
		if s.Each != "" {
			// A list that is there — even empty — is an answer ("none
			// held"); a list that is not there is no answer at all, and an
			// answer of `{}` must not stand in front of a summary that has
			// one.
			if adapterListThere(ctx, s.Each) {
				haveLeft = true
			}
			for _, item := range ctx.iterate(s.Each) {
				if !s.When.holds(item) {
					continue
				}
				n := 1.0
				if s.ItemLeft != "" {
					v, ok := adapterNum(item.get(s.ItemLeft))
					if !ok || v < 0 {
						continue
					}
					n = v
				}
				// A grant not yet begun, or already lapsed, is not held: a
				// bound that is there and has passed drops the item; a bound
				// that is absent is no bound.
				if s.Starts != "" {
					if v := item.get(s.Starts); v != nil {
						if b := adapterReset(v, s.StartsFormat, at); b > at {
							continue
						}
					}
				}
				var lapse float64
				if s.Expires != "" {
					if v := item.get(s.Expires); v != nil {
						lapse = adapterReset(v, s.ExpiresFormat, at)
						if lapse > 0 && lapse <= at {
							continue
						}
					}
				}
				left += n
				if s.ItemTotal != "" {
					if v, ok := adapterNum(item.get(s.ItemTotal)); ok && v >= 0 {
						total, haveTotal = total+v, true
					}
				}
				// Each credit the item holds lapses when the item does — a
				// grant of two is two tickets with one date — and only a
				// lapse still ahead is one.
				if lapse > at {
					for k := 0.0; k < n && k < 8; k++ {
						expires = append(expires, math.Round(lapse))
					}
				}
				if n <= 0 {
					continue
				}
				if itemRules {
					haveUsable = true
					ok := eligible
					for i := range s.ItemUsable {
						if !s.ItemUsable[i].holds(item) {
							ok = false
						}
					}
					if ok && s.ItemNeedsLimit.holdsSet(item) && !atLimit {
						// Usable in every way but one: the window it clears
						// has not reached its limit yet.
						ok, needsLimit = false, true
					}
					if ok {
						usable += n
					}
				}
				if s.ItemClears != "" {
					clears = adapterClears(item.get(s.ItemClears), s.ClearsMap, clears)
				}
			}
		}
		if s.Usable != nil {
			if v, ok := adapterNum(adapterDoc(s.Usable.From, docs, cred).get(s.Usable.Path)); ok && v >= 0 {
				usable, haveUsable, needsLimit = v, true, false
			}
		}
		if s.Left != "" {
			if v, ok := adapterNum(ctx.get(s.Left)); ok && v >= 0 {
				left, haveLeft = v, true
			}
		}
		if s.Total != "" {
			if v, ok := adapterNum(ctx.get(s.Total)); ok && v >= 0 {
				total, haveTotal = v, true
			}
		}
		if !haveLeft {
			continue
		}
		left = math.Round(left)
		if left <= 0 || left > 999 {
			return nil
		}
		out := &ResetCredits{Left: left}
		if haveTotal && total >= left && total <= 999 {
			out.Total = fp(math.Round(total))
		}
		sort.Float64s(expires)
		if len(expires) > 8 {
			expires = expires[:8]
		}
		out.Expires = expires
		if haveUsable {
			out.Usable = fp(math.Min(math.Round(usable), left))
			out.NeedsLimit = needsLimit && *out.Usable < left
		}
		if s.WaitUntil != "" {
			if e := adapterReset(ctx.get(s.WaitUntil), "auto", at); e > at {
				out.WaitUntil = fp(math.Round(e))
			}
		}
		out.Clears = clears
		return out
	}
	return nil
}

// A matcher that is set and holds; an absent one is no claim at all here
// (holds() reads an absent matcher as "no condition", which is right for a
// filter and wrong for a flag).
func (m *adapterMatch) holdsSet(ctx adapterCtx) bool {
	return m != nil && m.holds(ctx)
}

// The windows a credit clears, added to `have` in the order first named:
// each vendor name mapped to a window's length, at most four.
func adapterClears(v any, names map[string]string, have []string) []string {
	list, ok := v.([]any)
	if !ok {
		return have
	}
	for _, x := range list {
		name, ok := x.(string)
		if !ok {
			continue
		}
		label := names[strings.ToLower(strings.TrimSpace(name))]
		if !clearsLabelRE.MatchString(label) {
			continue
		}
		seen := false
		for _, h := range have {
			if h == label {
				seen = true
				break
			}
		}
		if !seen && len(have) < 4 {
			have = append(have, label)
		}
	}
	return have
}

// What the optional calls answered last, per credential: the plan's date and
// what it is, and the lapse dates behind a count of reset credits. Those
// calls fail now and then — chatgpt.com answered its subscription call with a
// bot challenge one time in two on 2026-10-07 — and a card that drops its
// renewal date on every other refresh says less than one that keeps the date
// it was told, for exactly as long as that date is still ahead. Lapse dates
// are kept only while the count they belong to has not moved.
var adapterSticky struct {
	sync.Mutex
	plan    map[string]stickyPlan
	expires map[string]stickyExpires
}

type stickyPlan struct {
	at   float64
	kind string
}

type stickyExpires struct {
	left  float64
	dates []float64
}

func adapterKeep(key string, p *Provider, at float64) {
	if key == "" || p == nil || !p.OK {
		return
	}
	adapterSticky.Lock()
	defer adapterSticky.Unlock()
	if adapterSticky.plan == nil {
		adapterSticky.plan = map[string]stickyPlan{}
		adapterSticky.expires = map[string]stickyExpires{}
	}
	// Only a live answer is remembered, what it said of the date with it, and
	// a remembered live date still ahead outranks the credential's own claim:
	// a token minted before the last renewal carries the date before it, and
	// reading that when the live call failed would move the card's date
	// backwards — or call a plan the vendor said renews
	// one that merely runs until then.
	live, haveLive := adapterSticky.plan[key]
	date, kind := p.planDate()
	switch {
	case date != nil && !p.planClaim:
		adapterSticky.plan[key] = stickyPlan{*date, kind}
	case haveLive && live.at > at:
		p.setPlanDate(fp(live.at), live.kind)
	case haveLive:
		delete(adapterSticky.plan, key)
	}
	switch {
	case p.Resets == nil:
		delete(adapterSticky.expires, key)
	case len(p.Resets.Expires) > 0:
		adapterSticky.expires[key] = stickyExpires{left: p.Resets.Left, dates: append([]float64(nil), p.Resets.Expires...)}
	default:
		s, ok := adapterSticky.expires[key]
		if !ok {
			break
		}
		if s.left != p.Resets.Left {
			// A count that moved is not the set of credits those dates
			// belonged to — and must not hand them back if it moves home
			// again.
			delete(adapterSticky.expires, key)
			break
		}
		var ahead []float64
		for _, d := range s.dates {
			if d > at {
				ahead = append(ahead, d)
			}
		}
		p.Resets.Expires = ahead
	}
}

// The first date a spec finds that is still ahead. A renewal date in the
// past is not one — it is a stale claim (a token minted before the last
// renewal still carries the one before it), and showing it would date the
// plan wrong.
// The plan's one date and what the vendor says of it: the first of renews,
// ends and until that lands. A plan that renews has no end to show, and a date
// the vendor says neither of is only how long the period runs.
func adapterPlanDate(ad *vendorAdapter, docs map[string]any, cred map[string]any, at float64) (*float64, string, bool) {
	for _, k := range []struct {
		kind  string
		specs []adapterStamp
	}{{"renews", ad.Renews}, {"ends", ad.Ends}, {"until", ad.Until}} {
		if d, claim := adapterStampRun(k.specs, docs, cred, at); d != nil {
			return d, k.kind, claim
		}
	}
	return nil, "", false
}

func adapterStampRun(specs []adapterStamp, docs map[string]any, cred map[string]any, at float64) (*float64, bool) {
	for _, s := range specs {
		ctx := adapterDoc(s.From, docs, cred)
		if ctx == nil || !s.When.holds(ctx) {
			continue
		}
		if s.Unless != "" {
			if v := ctx.get(s.Unless); v != nil && v != "" {
				continue
			}
		}
		if e := adapterReset(ctx.get(s.Path), s.Format, at); e > at {
			return fp(math.Round(e)), s.From == "cred"
		}
	}
	return nil, false
}

func adapterLimit(w *adapterWindow, ctx adapterCtx, cred map[string]any, at float64) (Limit, bool) {
	var lim Limit
	used, ok := adapterUsed(w, ctx)
	if !ok {
		return lim, false
	}
	lim.UsedPercent = round2(math.Max(0, math.Min(1000, used)))
	adapterAmounts(w, ctx, &lim)

	// Window length, from seconds, minutes, or a count in a named unit.
	minutes := 0.0
	if w.WindowSeconds != "" {
		if s, ok := adapterNum(ctx.get(w.WindowSeconds)); ok && s > 0 {
			minutes = s / 60
		}
	}
	if minutes == 0 && w.WindowMinutes != "" {
		if m, ok := adapterNum(ctx.get(w.WindowMinutes)); ok && m > 0 {
			minutes = m
			if w.WindowUnit != "" {
				unit := adapterStr(ctx.get(w.WindowUnit), 40)
				if mult, ok := w.UnitMinutes[unit]; ok && mult > 0 {
					minutes = m * mult
				}
			}
		}
	}
	if minutes > 0 {
		lim.WindowMinutes = fp(minutes)
	}
	seconds := strconv.FormatInt(int64(math.Round(minutes*60)), 10)

	// The key: `{seconds}` first (it is not a document field), then the
	// template, then the map over the result.
	key := adapterTemplate(strings.ReplaceAll(w.Key, "{seconds}", seconds), ctx, cred)
	if mapped, ok := w.KeyMap[key]; ok {
		key = mapped
	} else if mapped, ok := w.KeyMap[strings.ToLower(key)]; ok {
		key = mapped
	}
	if key == "" {
		key = seconds
	}
	key = cleanText(key, 24)
	if key == "" {
		return lim, false
	}
	lim.Key = key

	// The label: by window length when the table knows it, else the template,
	// else derived from the length.
	label := ""
	if l, ok := w.LabelBySeconds[seconds]; ok {
		label = l
	} else if w.Label != "" {
		label = adapterTemplate(strings.ReplaceAll(w.Label, "{seconds}", seconds), ctx, cred)
		if mapped, ok := w.LabelMap[label]; ok {
			label = mapped
		} else if mapped, ok := w.LabelMap[strings.ToLower(label)]; ok {
			label = mapped
		}
	}
	label = cleanText(strings.TrimSpace(label), 12)
	if label == "" && minutes > 0 {
		if l := windowLabel(minutes); l != nil {
			label = *l
		}
	}
	if label != "" {
		lim.WindowLabel = sp(label)
	}

	if w.Resets != "" {
		if e := adapterReset(ctx.get(w.Resets), w.ResetsFormat, at); e > 0 {
			lim.ResetsAt = fp(e)
		}
	}
	if w.Active != "" {
		if b, ok := adapterBool(ctx.get(w.Active)); ok {
			lim.Active = bp(b)
		}
	}
	if w.Severity != "" {
		switch s := strings.ToLower(adapterStr(ctx.get(w.Severity), 16)); s {
		case "normal", "warning", "critical":
			lim.Severity = sp(s)
		}
	}
	if w.Scope != "" {
		if s := cleanText(adapterTemplate(w.Scope, ctx, cred), 40); s != "" {
			lim.Scope = sp(s)
		}
	}
	return lim, true
}

// A window's figure — used, limit, remaining. adapterNum reads a JSON boolean
// as 1 or 0, which a count or a flag may want; as a figure it is a reading
// nobody sent ("percent": true would be 1% used). CodexBar stopped taking
// booleans as percentages on both Claude paths (ClaudeWebExtraRateWindowParser
// .percentValue, steipete/CodexBar@e1325bfd66), and here a boolean is no
// figure at all: the row is skipped, never drawn at 1% or 0%.
func adapterFigure(v any) (float64, bool) {
	if adapterIsBool(v) {
		return 0, false
	}
	return adapterNum(v)
}

func adapterFigureFraction(v any) (float64, bool) {
	if adapterIsBool(v) {
		return 0, false
	}
	return adapterFraction(v)
}

// Whether v is a JSON boolean, looking through the {"val": …} wrapper that
// adapterNum reads through: a wrapped true is no figure either, or Grok's
// {"monthlyLimit": {"val": true}} would be a one-cent limit (review
// 2026-10-24).
func adapterIsBool(v any) bool {
	for range 8 {
		switch x := v.(type) {
		case bool:
			return true
		case map[string]any:
			inner, ok := x["val"]
			if !ok {
				return false
			}
			v = inner
			continue
		}
		return false
	}
	return false
}

// The percentage used, by the kind the table names. `ratio` also fills the
// absolute figures, scaled (cents → dollars) when the table says so.
func adapterUsed(w *adapterWindow, ctx adapterCtx) (float64, bool) {
	switch w.UsedKind {
	case "", "percent":
		return adapterFigure(ctx.get(w.Used))
	case "remaining_percent":
		r, ok := adapterFigure(ctx.get(w.Used))
		if !ok {
			return 0, false
		}
		return 100 - r, true
	case "remaining_fraction":
		f, ok := adapterFigureFraction(ctx.get(w.Used))
		if !ok {
			return 0, false
		}
		return 100 * (1 - f), true
	case "used_fraction":
		// A helper that predates this kind skips the row rather than misread
		// it (the default below), which is what lets a table carry it.
		f, ok := adapterFigureFraction(ctx.get(w.Used))
		if !ok {
			return 0, false
		}
		return 100 * f, true
	case "ratio":
		used, uok := adapterFigure(ctx.get(w.Used))
		limit, lok := adapterFigure(ctx.get(w.Limit))
		if !lok || limit <= 0 {
			return 0, false
		}
		if !uok {
			// Only the remainder was given: the used share is what is left over.
			rem, rok := adapterFigure(ctx.get(w.Remaining))
			if !rok {
				return 0, false
			}
			used = limit - rem
		}
		return used / limit * 100, true
	}
	return 0, false
}

// The absolute figures behind a ratio window, when the table names a unit.
func adapterAmounts(w *adapterWindow, ctx adapterCtx, lim *Limit) {
	unit := w.Unit
	if w.UnitFrom != "" {
		if v := adapterStr(ctx.get(w.UnitFrom), 16); v != "" {
			unit = ""
			for k, u := range w.UnitMap {
				if strings.EqualFold(k, v) {
					unit = u
				}
			}
		}
	}
	if w.UsedKind != "ratio" || unit == "" {
		return
	}
	scale := w.AmountScale
	if scale == 0 {
		scale = 1
	}
	if u, ok := adapterFigure(ctx.get(w.Used)); ok {
		lim.UsedAmount = fp(round2(u * scale))
	}
	if l, ok := adapterFigure(ctx.get(w.Limit)); ok {
		lim.TotalAmount = fp(round2(l * scale))
	}
	if r, ok := adapterFigure(ctx.get(w.Remaining)); ok {
		lim.RemainingAmount = fp(round2(r * scale))
	} else if lim.UsedAmount != nil && lim.TotalAmount != nil {
		lim.RemainingAmount = fp(round2(*lim.TotalAmount - *lim.UsedAmount))
	}
	lim.Unit = unit
}

// A single value — the plan, the tier — from a field, a map over it, or the
// first case whose condition holds.
func adapterPick(v *adapterValue, docs map[string]any, cred map[string]any) string {
	doc := docs[v.From]
	if doc == nil && v.From != "cred" {
		return ""
	}
	ctx := adapterCtx{doc}
	if v.From == "cred" {
		ctx = adapterCtx{cred}
	}
	if len(v.AnyOf) > 0 {
		for _, c := range v.AnyOf {
			got := adapterStr(ctx.get(c.Path), 60)
			if c.Eq != "" {
				if strings.EqualFold(got, c.Eq) {
					return cleanText(adapterTemplate(c.Value, ctx, cred), 40)
				}
				continue
			}
			if got != "" {
				return cleanText(adapterTemplate(c.Value, ctx, cred), 40)
			}
		}
		return ""
	}
	got := adapterStr(ctx.get(v.Path), 60)
	if got == "" {
		return ""
	}
	if mapped, ok := v.Map[strings.ToLower(got)]; ok {
		return mapped
	}
	if len(v.Map) > 0 {
		// A map that does not know the value passes it through rather than
		// hiding it: a new plan name should show up as itself.
		return cleanText(got, 40)
	}
	return cleanText(got, 40)
}
