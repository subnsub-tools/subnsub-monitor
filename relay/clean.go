package main

// Field-by-field rebuild of everything a helper pushes, mirrored from the
// hosted relay's admission rules so a helper cannot tell the two apart.
//
// THE RULE: nothing a machine sent reaches storage — or a browser — except by
// being copied, field by known field, into a struct this file owns. A reading
// is parsed, each field is checked against the shape the helper is known to
// emit, and the result is re-serialised. Unknown keys do not survive, strings
// are cut to the length the page was designed around, numbers are clamped into
// the range the page can draw, and anything malformed degrades to the absent
// key the page already handles rather than to an error the operator has to
// notice. The dashboard then treats every string as text, never as markup —
// but that is the second line of defence, not the first.
//
// Sizes and shapes are the hosted relay's, deliberately: a helper that pushes
// here behaves identically against both, and a reading accepted here would
// have been accepted there. Loosening a limit locally would make this relay
// the one place a too-long name "works" — until the operator points the same
// machine back at the hosted one and it stops.

import (
	"encoding/json"
	"net"
	"regexp"
	"strings"
)

const (
	maxProviders = 12 // more than the helper has collectors, with room
	maxLimits    = 8
	maxMissing   = 8
	maxNameRunes = 24
	// The interface list, bounded exactly as the helper bounds it (nics.go):
	// more rows than this is a container host whose list is bridge noise, and
	// eight addresses covers a v4, a global v6, a privacy v6 and aliases.
	maxNICs      = 16
	maxNICAddrs  = 8
	detailArgMax = 120 // a credential path or a dial error, not a paragraph
)

var (
	agentIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,32}$`)
	versionRe = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}\.\d{1,3}$`)
	kernelRe  = regexp.MustCompile(`^\d{1,4}\.\d{1,4}$`)
	// An interface is named by its kernel, and kernels do not put spaces or
	// slashes in one. A character class rather than a length cap, for the
	// reason the platform/arch whitelists give: a cap turns an identifying
	// string into a shorter identifying string.
	nicNameRe = regexp.MustCompile(`^[A-Za-z0-9._@:-]{1,24}$`)
	// Why a provider failed, in machine form — a lookup key, so a closed shape
	// rather than a length cap.
	detailCodeRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// Numbers the helper stamps as "epoch seconds". Anything outside this
	// window is a unit mistake — milliseconds, usually — and a wrong-unit
	// timestamp renders as a countdown of several decades, which looks
	// plausible and is wrong. Absent is better.
	epochMin = 1600000000.0
	epochMax = 4000000000.0
)

// GOOS/GOARCH values a helper build could actually carry. A closed set rather
// than "any string": platform and arch are rendered as chips on the page, and
// an open field that ends up in a browser is an open field someone will
// eventually put something surprising in.
var knownPlatforms = map[string]bool{
	"linux": true, "darwin": true, "windows": true, "freebsd": true,
	"openbsd": true, "netbsd": true, "dragonfly": true, "illumos": true, "solaris": true,
	"aix": true, "android": true, "ios": true, "js": true, "plan9": true, "wasip1": true,
}
var knownArches = map[string]bool{
	"amd64": true, "arm64": true, "386": true, "arm": true,
	"riscv64": true, "ppc64le": true, "ppc64": true, "s390x": true,
	"mips64le": true, "mips64": true, "loong64": true,
	"mips": true, "mipsle": true, "wasm": true, "sparc64": true,
}

var knownMissing = map[string]bool{
	"cpu": true, "memory": true, "swap": true, "disk": true, "load": true, "uptime": true,
	"network": true, "procs": true,
}

// One reading as this relay is willing to hold it. Pointers where absent and
// zero are different claims, exactly as in the helper's own types.
type reading struct {
	OK            bool        `json:"ok"`
	CapturedAt    float64     `json:"captured_at"`
	Providers     []*provider `json:"providers"`
	AgentName     string      `json:"agent_name,omitempty"`
	HelperVersion string      `json:"helper_version,omitempty"`
	Exec          bool        `json:"exec"`
	Upd           bool        `json:"upd"`
	System        *system     `json:"system,omitempty"`
}

type provider struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source,omitempty"`
	OK     bool   `json:"ok"`

	// Why there is no reading. Detail is the helper's own sentence, written in
	// one language; DetailCode is the same thing in machine form, for a
	// dashboard that has more than one. This dashboard shows the sentence — it
	// ships no translations on purpose — but the field has to survive the trip,
	// because a helper cannot tell the two relays apart and must not lose data
	// by choosing one. See helper/go/detail.go.
	Error      string `json:"error,omitempty"`
	Detail     string `json:"detail,omitempty"`
	DetailCode string `json:"detail_code,omitempty"`
	DetailArg  string `json:"detail_arg,omitempty"`

	RecordedAt    *float64 `json:"recorded_at,omitempty"`
	PlanType      string   `json:"plan_type,omitempty"`
	RateLimitTier string   `json:"rate_limit_tier,omitempty"`
	Limits        []*limit `json:"limits,omitempty"`
	Credits       *credits `json:"credits,omitempty"`
	SourceFile    string   `json:"source_file,omitempty"`
	Truncated     bool     `json:"truncated,omitempty"`
	Capped        bool     `json:"capped,omitempty"`
}

type limit struct {
	Key           string   `json:"key"`
	UsedPercent   float64  `json:"used_percent"`
	WindowLabel   string   `json:"window_label,omitempty"`
	WindowMinutes *float64 `json:"window_minutes,omitempty"`
	ResetsAt      *float64 `json:"resets_at,omitempty"`
	Severity      string   `json:"severity,omitempty"`
	Scope         string   `json:"scope,omitempty"`
	Active        *bool    `json:"active,omitempty"`
}

type credits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

// One of the machine's interfaces, as it described itself.
//
// Rebuilt like everything else here rather than passed through: the name goes
// through a character class instead of a length cap (a cap only shortens an
// identifying string), every address has to look like one, and both the row
// count and the addresses per row are bounded. A relay that trusted this list
// would be one strange agent away from a watcher that cannot render.
type nic struct {
	Name    string   `json:"name"`
	IPs     []string `json:"ips,omitempty"`
	RxTotal *float64 `json:"rx_total,omitempty"`
	TxTotal *float64 `json:"tx_total,omitempty"`
	Up      bool     `json:"up,omitempty"`
}

type system struct {
	OK        bool     `json:"ok"`
	Platform  string   `json:"platform"`
	Arch      string   `json:"arch,omitempty"`
	OSVersion string   `json:"os_version,omitempty"`
	CPUCount  int      `json:"cpu_count,omitempty"`
	CPUPct    *float64 `json:"cpu_percent,omitempty"`
	Load1     *float64 `json:"load1,omitempty"`
	Load5     *float64 `json:"load5,omitempty"`
	Load15    *float64 `json:"load15,omitempty"`
	MemTotal  *float64 `json:"mem_total,omitempty"`
	MemPct    *float64 `json:"mem_used_percent,omitempty"`
	SwapTotal *float64 `json:"swap_total,omitempty"`
	SwapPct   *float64 `json:"swap_used_percent,omitempty"`
	DiskTotal *float64 `json:"disk_total,omitempty"`
	DiskUsed  *float64 `json:"disk_used,omitempty"`
	DiskPct   *float64 `json:"disk_used_percent,omitempty"`
	UptimeSec *float64 `json:"uptime_sec,omitempty"`
	NetRxBps  *float64 `json:"net_rx_bps,omitempty"`
	NetTxBps  *float64 `json:"net_tx_bps,omitempty"`
	// Cumulative bytes since the machine booted, and the machine's own
	// interfaces. A rate says whether a box is busy; a total says how much of
	// a metered allowance is gone. The interface list is the only place a
	// dual-stack machine's IPv6 can come from — a relay observes the address
	// of ONE connection and cannot see the other family at all.
	NetRxTotal *float64 `json:"net_rx_total,omitempty"`
	NetTxTotal *float64 `json:"net_tx_total,omitempty"`
	NICs       []nic    `json:"nics,omitempty"`
	Procs     *float64 `json:"procs,omitempty"`
	TempC     *float64 `json:"temp_c,omitempty"`
	Missing   []string `json:"missing,omitempty"`
	Series    *series  `json:"series,omitempty"`
}

// One frame's worth of samples for the readings that move fast enough to draw
// as a line. The helper samples every second and ships the lot with its push,
// so this is thirty points arriving at once rather than a faster push — the
// same cadence, the same write count, thirty times the resolution.
//
// The last slot of every lane is the snapshot this rides on, so a watcher's
// gauges and the end of its line are one measurement. Slots with no sample are
// null: on a CPU line "not measured" and "idle" are opposite claims.
type series struct {
	// When the last slot was taken, on the MACHINE's clock — meaningful only
	// against the frame's own captured_at, which is the same clock.
	At   float64    `json:"at"`
	Step float64    `json:"step"`
	CPU  []*float64 `json:"cpu,omitempty"`
	Mem  []*float64 `json:"mem,omitempty"`
	Rx   []*float64 `json:"rx,omitempty"`
	Tx   []*float64 `json:"tx,omitempty"`
}

// The loosely-typed shapes the wire actually carries. Decoded with UseNumber
// off — float64 is what every consumer here wants — and every field optional,
// because the whole point is to decide what survives.
type rawReading struct {
	CapturedAt    *float64          `json:"captured_at"`
	Providers     []json.RawMessage `json:"providers"`
	AgentID       *string           `json:"agent_id"`
	AgentName     *string           `json:"agent_name"`
	HelperVersion *string           `json:"helper_version"`
	Exec          *bool             `json:"exec"`
	Upd           *bool             `json:"upd"`
	System        json.RawMessage   `json:"system"`
}

// Strip what must never reach a page: control characters, and the invisible
// bidi/zero-width marks that can make one string render as another. Cut by
// RUNES, not bytes — a name in any script is 24 characters, not 8.
func displayText(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		// 0x7f–0x9f takes the C1 controls with DEL, matching the hosted
		// relay's filter exactly — this was one of the places the two
		// implementations had quietly diverged.
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			continue
		}
		if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) ||
			r == 0xfeff || r == 0x200e || r == 0x200f {
			continue
		}
		if n >= maxRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

func cleanEpoch(v *float64) *float64 {
	if v == nil || *v < epochMin || *v > epochMax {
		return nil
	}
	x := *v
	return &x
}

func clampPct(v float64, ceiling float64) float64 {
	if v < 0 {
		return 0
	}
	if v > ceiling {
		return ceiling
	}
	return v
}

// A finite float64 out of a raw JSON value, or nil for anything else — a
// string, a bool, an absent key, a number too large for float64. Field-local
// tolerance for the fields that need it; see the rawReading note.
// The interface list, rebuilt row by row. Every bound here mirrors the
// helper's own (nics.go): sixteen rows, eight addresses in one, a kernel-shaped
// name, one row per name. A row that fails any of it is dropped on its own —
// one malformed interface must never cost a machine the rest of its list.
func cleanNICs(raw []json.RawMessage) []nic {
	if len(raw) == 0 {
		return nil
	}
	out := make([]nic, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		if len(out) >= maxNICs {
			break
		}
		var r struct {
			Name    string          `json:"name"`
			IPs     []string        `json:"ips"`
			RxTotal json.RawMessage `json:"rx_total"`
			TxTotal json.RawMessage `json:"tx_total"`
			Up      *bool           `json:"up"`
		}
		if json.Unmarshal(item, &r) != nil {
			continue
		}
		if !nicNameRe.MatchString(r.Name) || seen[r.Name] {
			continue
		}
		seen[r.Name] = true
		n := nic{Name: r.Name, Up: r.Up != nil && *r.Up}
		for _, a := range r.IPs {
			if len(n.IPs) >= maxNICAddrs {
				break
			}
			// net.ParseIP is the shape check: it accepts exactly the textual
			// forms an address has, and nothing that merely looks like one.
			if ip := net.ParseIP(a); ip != nil && !contains(n.IPs, ip.String()) {
				n.IPs = append(n.IPs, ip.String())
			}
		}
		n.RxTotal = cleanNonNeg(looseNum(r.RxTotal), 1e18)
		n.TxTotal = cleanNonNeg(looseNum(r.TxTotal), 1e18)
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func looseNum(raw json.RawMessage) *float64 {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	return &f
}

func cleanNonNeg(v *float64, ceiling float64) *float64 {
	if v == nil || *v < 0 || *v > ceiling {
		return nil
	}
	x := *v
	return &x
}

func cleanPctPtr(v *float64) *float64 {
	if v == nil || *v < 0 {
		return nil
	}
	x := clampPct(*v, 100)
	return &x
}

// cleanAgentID is the identity gate for every path that names a machine.
// Empty means "a helper too old to send one"; those all share one legacy slot
// rather than being rejected, so an old install still shows up somewhere.
func cleanAgentID(s string) string {
	if agentIDRe.MatchString(s) {
		return s
	}
	return ""
}

const legacyAgent = "~legacy"

// cleanReading rebuilds one pushed frame. Returns the machine id (or the
// legacy slot), the rebuilt reading, and whether the frame was admissible at
// all — a frame with no usable provider is not a reading, it is noise.
func cleanReading(body []byte) (string, *reading, bool) {
	var raw rawReading
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", nil, false
	}

	out := &reading{}

	// Kept as the cleaned POINTER, not just copied into the output: the system
	// series is admitted by comparing its machine-clock timestamp against this
	// field, and handing the raw one down would let `captured_at: 0` pass a lag
	// check the hosted cleaner fails (epoch(0) is null there).
	capturedAt := cleanEpoch(raw.CapturedAt)
	if capturedAt != nil {
		out.CapturedAt = *capturedAt
	}

	seen := map[string]bool{}
	for _, rp := range raw.Providers {
		if len(out.Providers) >= maxProviders {
			break
		}
		p := cleanProvider(rp)
		if p == nil || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		out.Providers = append(out.Providers, p)
		if p.OK {
			out.OK = true
		}
	}
	if len(out.Providers) == 0 {
		return "", nil, false
	}

	id := legacyAgent
	if raw.AgentID != nil {
		if v := cleanAgentID(*raw.AgentID); v != "" {
			id = v
		}
	}
	if raw.AgentName != nil {
		out.AgentName = displayText(*raw.AgentName, maxNameRunes)
	}
	if raw.HelperVersion != nil && versionRe.MatchString(*raw.HelperVersion) {
		out.HelperVersion = *raw.HelperVersion
	}
	// === true, in Go clothing: only an explicit boolean turns these on.
	out.Exec = raw.Exec != nil && *raw.Exec
	out.Upd = raw.Upd != nil && *raw.Upd
	if len(raw.System) > 0 {
		out.System = cleanSystem(raw.System, capturedAt)
	}
	return id, out, true
}

func cleanProvider(body json.RawMessage) *provider {
	var raw struct {
		ID            *string           `json:"id"`
		Name          *string           `json:"name"`
		Source        *string           `json:"source"`
		OK            *bool             `json:"ok"`
		Error         *string           `json:"error"`
		Detail        *string           `json:"detail"`
		DetailCode    *string           `json:"detail_code"`
		DetailArg     *string           `json:"detail_arg"`
		RecordedAt    *float64          `json:"recorded_at"`
		PlanType      *string           `json:"plan_type"`
		RateLimitTier *string           `json:"rate_limit_tier"`
		Limits        []json.RawMessage `json:"limits"`
		Credits       json.RawMessage   `json:"credits"`
		SourceFile    *string           `json:"source_file"`
		Truncated     *bool             `json:"truncated"`
		Capped        *bool             `json:"capped"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.ID == nil {
		return nil
	}
	id := displayText(*raw.ID, 24)
	if id == "" {
		return nil
	}
	p := &provider{ID: id, Name: id}
	if raw.Name != nil {
		if v := displayText(*raw.Name, 40); v != "" {
			p.Name = v
		}
	}
	if raw.Source != nil {
		p.Source = displayText(*raw.Source, 16)
	}

	if raw.OK == nil || !*raw.OK {
		// A failed provider is still a report — the page turns the error slug
		// into a sentence. But it must carry an error to be one.
		if raw.Error == nil {
			return nil
		}
		p.Error = displayText(*raw.Error, 40)
		if p.Error == "" {
			return nil
		}
		if raw.Detail != nil {
			p.Detail = displayText(*raw.Detail, 400)
		}
		// A closed shape, not a length cap: this is a lookup key on a
		// dashboard, so anything that is not a slug can only ever miss. The
		// argument spliced into it is free text — a vendor's error string can
		// reach it — and gets the same scrubbing as any other display string.
		if raw.DetailCode != nil && detailCodeRE.MatchString(*raw.DetailCode) {
			p.DetailCode = *raw.DetailCode
		}
		if raw.DetailArg != nil {
			p.DetailArg = displayText(*raw.DetailArg, detailArgMax)
		}
		return p
	}

	p.OK = true
	p.RecordedAt = cleanEpoch(raw.RecordedAt)
	if raw.PlanType != nil {
		p.PlanType = displayText(*raw.PlanType, 40)
	}
	if raw.RateLimitTier != nil {
		p.RateLimitTier = displayText(*raw.RateLimitTier, 40)
	}
	for _, rl := range raw.Limits {
		if len(p.Limits) >= maxLimits {
			break
		}
		if l := cleanLimit(rl); l != nil {
			p.Limits = append(p.Limits, l)
		}
	}
	if len(raw.Credits) > 0 {
		p.Credits = cleanCredits(raw.Credits)
	}
	if raw.SourceFile != nil {
		p.SourceFile = displayText(*raw.SourceFile, 120)
	}
	p.Truncated = raw.Truncated != nil && *raw.Truncated
	p.Capped = raw.Capped != nil && *raw.Capped

	// Admission: an OK provider with nothing to show is not a reading.
	if len(p.Limits) == 0 && (p.Credits == nil || (!p.Credits.Unlimited && p.Credits.Balance == "")) {
		return nil
	}
	return p
}

func cleanLimit(body json.RawMessage) *limit {
	var raw struct {
		Key           *string  `json:"key"`
		UsedPercent   *float64 `json:"used_percent"`
		WindowLabel   *string  `json:"window_label"`
		WindowMinutes *float64 `json:"window_minutes"`
		ResetsAt      *float64 `json:"resets_at"`
		Severity      *string  `json:"severity"`
		Scope         *string  `json:"scope"`
		Active        *bool    `json:"active"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.UsedPercent == nil {
		return nil
	}
	l := &limit{Key: "primary",
		// Up to 1000, not 100: providers genuinely report overage past the
		// window, and flattening 240% to 100% hides the one number that
		// explains why everything is refusing to run.
		UsedPercent: clampPct(*raw.UsedPercent, 1000)}
	if raw.Key != nil {
		if v := displayText(*raw.Key, 24); v != "" {
			l.Key = v
		}
	}
	if raw.WindowLabel != nil {
		l.WindowLabel = displayText(*raw.WindowLabel, 12)
	}
	l.WindowMinutes = cleanNonNeg(raw.WindowMinutes, 1e9)
	l.ResetsAt = cleanEpoch(raw.ResetsAt)
	if raw.Severity != nil {
		switch *raw.Severity {
		case "normal", "warning", "critical":
			l.Severity = *raw.Severity
		}
	}
	if raw.Scope != nil {
		l.Scope = displayText(*raw.Scope, 40)
	}
	if raw.Active != nil && *raw.Active {
		t := true
		l.Active = &t
	}
	return l
}

func cleanCredits(body json.RawMessage) *credits {
	var raw struct {
		HasCredits *bool   `json:"has_credits"`
		Unlimited  *bool   `json:"unlimited"`
		Balance    *string `json:"balance"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	c := &credits{
		HasCredits: raw.HasCredits != nil && *raw.HasCredits,
		Unlimited:  raw.Unlimited != nil && *raw.Unlimited,
	}
	if raw.Balance != nil {
		v := displayText(*raw.Balance, 24)
		// A balance is a number wearing a currency. Text with no digit in it
		// is not one, and the page prints this string verbatim.
		if strings.ContainsAny(v, "0123456789") {
			c.Balance = v
		}
	}
	return c
}

// Slot and lag ceilings for a sampled series. The helper sends at most 72
// slots; the extra room is for a third-party agent sampling finer, and the
// ceiling is what keeps a hostile one from posting a megabyte of line.
const (
	seriesSlots  = 128
	seriesMaxLag = 300.0 // seconds between the last slot and captured_at
)

// Rebuilt lane by lane, with two rules a series needs and a scalar does not.
//
// ONE GRID: every lane present must be the same length, because a watcher
// places slot i at at−(n−1−i)·step and nothing else says when a sample was
// taken. A lane whose length disagrees is dropped BY ITSELF — one malformed
// field must never cost a machine its whole system block, the same promise the
// json.RawMessage fields above make.
//
// A HOLE IS NOT A ZERO: an element that fails its guard stays nil.
func cleanSeries(body json.RawMessage, capturedAt *float64) *series {
	if len(body) == 0 || capturedAt == nil {
		return nil
	}
	// Every lane lands as one RawMessage and is decoded on its own. Declaring
	// them []json.RawMessage instead put all four inside a single Unmarshal, so
	// `"cpu": "bad"` failed the whole call and dropped a series the hosted
	// cleaner would have kept minus one lane — the two admission rules have to
	// agree, or a machine draws differently depending on which relay it is
	// pointed at.
	var raw struct {
		At   json.RawMessage `json:"at"`
		Step json.RawMessage `json:"step"`
		CPU  json.RawMessage `json:"cpu"`
		Mem  json.RawMessage `json:"mem"`
		Rx   json.RawMessage `json:"rx"`
		Tx   json.RawMessage `json:"tx"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	at, step := looseNum(raw.At), looseNum(raw.Step)
	if at == nil || step == nil || *step < 0.1 || *step > 60 {
		return nil
	}
	// The machine's clock is checked against the machine's own captured_at,
	// never against this relay's: a box an hour out still draws correctly,
	// because a watcher anchors the series by the LAG between these two. What
	// this rejects is a series that does not belong to the frame carrying it.
	if lag := *capturedAt - *at; lag > seriesMaxLag || lag < -seriesMaxLag {
		return nil
	}
	slots := func(in json.RawMessage) []json.RawMessage {
		if len(in) == 0 {
			return nil
		}
		var out []json.RawMessage
		if err := json.Unmarshal(in, &out); err != nil || len(out) == 0 || len(out) > seriesSlots {
			return nil
		}
		return out
	}
	cpuIn, memIn := slots(raw.CPU), slots(raw.Mem)
	rxIn, txIn := slots(raw.Rx), slots(raw.Tx)

	// Which length IS the grid is decided before any lane is admitted, by vote.
	// Letting the first well-shaped lane define it made the answer depend on
	// field order: one corrupted 31-slot cpu lane ahead of three intact 30-slot
	// ones would seat itself and evict all three. A tie is a frame that cannot
	// say when its own samples were taken, so nothing is admitted.
	var lengths []int
	for _, in := range [][]json.RawMessage{cpuIn, memIn, rxIn, txIn} {
		if len(in) > 0 {
			lengths = append(lengths, len(in))
		}
	}
	n, best := 0, 0
	for _, l := range lengths {
		votes := 0
		for _, m := range lengths {
			if m == l {
				votes++
			}
		}
		switch {
		case votes > best:
			n, best = l, votes
		case votes == best && l != n:
			n = 0
		}
	}
	if n == 0 {
		return nil
	}
	lane := func(in []json.RawMessage, pct bool) []*float64 {
		if len(in) != n {
			return nil
		}
		out := make([]*float64, len(in))
		for i, e := range in {
			if pct {
				out[i] = cleanPctPtr(looseNum(e))
			} else {
				out[i] = cleanNonNeg(looseNum(e), 1e15)
			}
		}
		return out
	}
	s := &series{At: *at, Step: *step}
	s.CPU = lane(cpuIn, true)
	s.Mem = lane(memIn, true)
	s.Rx = lane(rxIn, false)
	s.Tx = lane(txIn, false)
	if s.CPU == nil && s.Mem == nil && s.Rx == nil && s.Tx == nil {
		return nil
	}
	return s
}

func cleanSystem(body json.RawMessage, capturedAt *float64) *system {
	var raw struct {
		OK        *bool    `json:"ok"`
		Platform  *string  `json:"platform"`
		Arch      *string  `json:"arch"`
		OSVersion *string  `json:"os_version"`
		CPUCount  *float64 `json:"cpu_count"`
		CPUPct    *float64 `json:"cpu_percent"`
		Load1     *float64 `json:"load1"`
		Load5     *float64 `json:"load5"`
		Load15    *float64 `json:"load15"`
		MemTotal  *float64 `json:"mem_total"`
		MemPct    *float64 `json:"mem_used_percent"`
		SwapTotal *float64 `json:"swap_total"`
		SwapPct   *float64 `json:"swap_used_percent"`
		DiskTotal *float64 `json:"disk_total"`
		DiskUsed  *float64 `json:"disk_used"`
		DiskPct   *float64 `json:"disk_used_percent"`
		UptimeSec *float64 `json:"uptime_sec"`
		// RawMessage, not *float64, and the difference is a compatibility
		// promise: these keys were UNKNOWN to every deployed relay until
		// 2026-08, so any third-party agent already pushing them — as a
		// string, say — had them ignored. Typed decoding would turn that
		// same push into an Unmarshal error and throw away the WHOLE system
		// block; per-field decoding keeps rejection field-local, which is
		// also what the hosted relay's cleaner does.
		NetRxBps json.RawMessage `json:"net_rx_bps"`
		NetTxBps json.RawMessage `json:"net_tx_bps"`
		// Same RawMessage treatment and for the same compatibility promise:
		// these keys were unknown to every deployed relay until 2026-08-06,
		// so a rejection here has to stay field-local rather than costing the
		// whole system block.
		NetRxTotal json.RawMessage   `json:"net_rx_total"`
		NetTxTotal json.RawMessage   `json:"net_tx_total"`
		NICs       []json.RawMessage `json:"nics"`
		Procs    json.RawMessage `json:"procs"`
		TempC    json.RawMessage `json:"temp_c"`
		Series   json.RawMessage `json:"series"`
		Missing  []string        `json:"missing"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	// No recognised platform, no system block. The platform names the closed
	// vocabulary everything else is rendered under.
	if raw.Platform == nil || !knownPlatforms[*raw.Platform] {
		return nil
	}
	s := &system{OK: raw.OK != nil && *raw.OK, Platform: *raw.Platform}
	if raw.Arch != nil && knownArches[*raw.Arch] {
		s.Arch = *raw.Arch
	}
	if raw.OSVersion != nil && kernelRe.MatchString(*raw.OSVersion) {
		s.OSVersion = *raw.OSVersion
	}
	if raw.CPUCount != nil && *raw.CPUCount >= 1 && *raw.CPUCount <= 4096 {
		s.CPUCount = int(*raw.CPUCount)
	}
	s.CPUPct = cleanPctPtr(raw.CPUPct)
	s.Load1 = cleanNonNeg(raw.Load1, 1e18)
	s.Load5 = cleanNonNeg(raw.Load5, 1e18)
	s.Load15 = cleanNonNeg(raw.Load15, 1e18)
	s.MemTotal = cleanNonNeg(raw.MemTotal, 1e18)
	s.MemPct = cleanPctPtr(raw.MemPct)
	s.SwapTotal = cleanNonNeg(raw.SwapTotal, 1e18)
	s.SwapPct = cleanPctPtr(raw.SwapPct)
	s.DiskTotal = cleanNonNeg(raw.DiskTotal, 1e18)
	s.DiskUsed = cleanNonNeg(raw.DiskUsed, 1e18)
	s.DiskPct = cleanPctPtr(raw.DiskPct)
	s.UptimeSec = cleanNonNeg(raw.UptimeSec, 3.2e9)
	// Rates in bytes/second: a petabyte a second is already past any link, and
	// the tighter ceiling catches a cumulative counter pushed as a rate.
	s.NetRxBps = cleanNonNeg(looseNum(raw.NetRxBps), 1e15)
	s.NetTxBps = cleanNonNeg(looseNum(raw.NetTxBps), 1e15)
	// Cumulative counters, so the rate ceiling would be exactly wrong: an
	// exabyte is past anything that has moved bytes, and the same bound every
	// other absolute quantity here takes.
	s.NetRxTotal = cleanNonNeg(looseNum(raw.NetRxTotal), 1e18)
	s.NetTxTotal = cleanNonNeg(looseNum(raw.NetTxTotal), 1e18)
	s.NICs = cleanNICs(raw.NICs)
	if v := looseNum(raw.Procs); v != nil && *v >= 1 && *v <= 1e7 {
		n := float64(int64(*v))
		s.Procs = &n
	}
	// Celsius; wider than the helper's own 0–150 band on purpose, for
	// third-party agents that are honest but colder.
	if v := looseNum(raw.TempC); v != nil && *v >= -100 && *v <= 300 {
		n := *v
		s.TempC = &n
	}
	s.Series = cleanSeries(raw.Series, capturedAt)
	seen := map[string]bool{}
	for _, m := range raw.Missing {
		if len(s.Missing) >= maxMissing {
			break
		}
		if knownMissing[m] && !seen[m] {
			seen[m] = true
			s.Missing = append(s.Missing, m)
		}
	}
	return s
}
