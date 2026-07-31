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
	"regexp"
	"strings"
)

const (
	maxProviders = 12 // more than the helper has collectors, with room
	maxLimits    = 8
	maxMissing   = 8
	maxNameRunes = 24
)

var (
	agentIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,32}$`)
	versionRe = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}\.\d{1,3}$`)
	kernelRe  = regexp.MustCompile(`^\d{1,4}\.\d{1,4}$`)
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

	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`

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
	Procs     *float64 `json:"procs,omitempty"`
	TempC     *float64 `json:"temp_c,omitempty"`
	Missing   []string `json:"missing,omitempty"`
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

	if raw.CapturedAt != nil {
		if e := cleanEpoch(raw.CapturedAt); e != nil {
			out.CapturedAt = *e
		}
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
		out.System = cleanSystem(raw.System)
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

func cleanSystem(body json.RawMessage) *system {
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
		Procs    json.RawMessage `json:"procs"`
		TempC    json.RawMessage `json:"temp_c"`
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
