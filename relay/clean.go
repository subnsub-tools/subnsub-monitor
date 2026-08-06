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
	"sort"
	"strings"
)

const (
	maxProviders = 12 // more than the helper has collectors, with room
	maxLimits    = 8
	// Sized to the vocabulary itself: a helper that could measure nothing at
	// all names every category, and a cap below that count would drop the last
	// row on exactly the machine with the most to say.
	maxMissing   = 9
	maxNameRunes = 24
	// The non-root filesystems, bounded exactly as the helper bounds them
	// (mounts.go): eight rows, and a folded path of at most three short
	// segments.
	maxMounts     = 8
	maxMountPath  = 40
	maxMountDepth = 3
	// The interface list, bounded exactly as the helper bounds it (nics.go):
	// more rows than this is a container host whose list is bridge noise, and
	// eight addresses covers a v4, a global v6, a privacy v6 and aliases.
	maxNICs     = 16
	maxNICAddrs = 8
	maxNICName  = 40
	// A cumulative byte counter is not a capacity: uint64 is the range the
	// kernel keeps these in, and an exabyte cap would null a real reading.
	counterMax   = 1.9e19
	detailArgMax = 120 // a credential path or a dial error, not a paragraph
)

var (
	agentIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,32}$`)
	versionRe = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}\.\d{1,3}$`)
	kernelRe  = regexp.MustCompile(`^\d{1,4}\.\d{1,4}$`)
	// Why a provider failed, in machine form — a lookup key, so a closed shape
	// rather than a length cap.
	detailCodeRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// A command's signature, checked for SHAPE only — this relay holds no key
	// and could not verify one if it wanted to, which is the property the
	// whole scheme buys. Ed25519 signatures are 64 bytes: 88 base64
	// characters ending in one '='. Exported for main.go's op handler.
	sigRe = regexp.MustCompile(`^[A-Za-z0-9+/]{86}==$`)
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
	"cpu": true, "memory": true, "swap": true, "disk": true, "diskio": true,
	"load": true, "uptime": true, "network": true, "procs": true,
}

// Whole PREFIXES whose next segment is an identity rather than a disk: the
// login directories, the roots desktop Linux automounts under
// (/media/<user>/<label>), and the macOS folder where every external disk lands
// under whatever label its owner typed. The helper's own list (mounts.go).
//
// Prefixes rather than segment names, because /System/Volumes/Data is where
// macOS keeps its data volume on every machine ever made and names nobody.
var mountAnchors = map[string]bool{
	"/home": true, "/Users": true, "/root": true, "/export/home": true,
	"/media": true, "/run/media": true, "/Volumes": true,
}

// `D:` or `D:\` and nothing else — deliberately not "a letter and a colon",
// which `C:\Users\alice` also satisfies.
var volumeRootRe = regexp.MustCompile(`^[A-Za-z]:[\\/]?$`)

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
	// Where that percentage went. iowait is a CPU idle on a disk, and steal is
	// time the hypervisor gave to somebody else — the two readings that explain
	// a machine which is slow while reporting spare capacity. Admitted one by
	// one: a platform reports the buckets it keeps, and an absent one must stay
	// absent rather than become a zero that claims a measurement.
	CPUUser   *float64 `json:"cpu_user,omitempty"`
	CPUSys    *float64 `json:"cpu_system,omitempty"`
	CPUIOWait *float64 `json:"cpu_iowait,omitempty"`
	CPUSteal  *float64 `json:"cpu_steal,omitempty"`
	Load1     *float64 `json:"load1,omitempty"`
	Load5     *float64 `json:"load5,omitempty"`
	Load15    *float64 `json:"load15,omitempty"`
	MemTotal  *float64 `json:"mem_total,omitempty"`
	MemPct    *float64 `json:"mem_used_percent,omitempty"`
	// The two figures behind that percentage: what the kernel would hand back
	// under pressure, and what it promises it can. One percentage cannot say
	// whether the missing memory is a process or a page cache.
	MemAvail  *float64 `json:"mem_available,omitempty"`
	MemCached *float64 `json:"mem_cached,omitempty"`
	SwapTotal *float64 `json:"swap_total,omitempty"`
	SwapPct   *float64 `json:"swap_used_percent,omitempty"`
	// Pages MOVING, which is thrashing. Swap written days ago and never touched
	// costs nothing, so the percentage alone cannot tell a healthy box at 80%
	// swap from one that is grinding.
	SwapInBps  *float64 `json:"swap_in_bps,omitempty"`
	SwapOutBps *float64 `json:"swap_out_bps,omitempty"`
	DiskTotal  *float64 `json:"disk_total,omitempty"`
	DiskUsed   *float64 `json:"disk_used,omitempty"`
	DiskPct    *float64 `json:"disk_used_percent,omitempty"`
	// The other filesystems. On a server with a data disk, "is it about to run
	// out of room" is nearly always about the data disk, and a card showing a
	// comfortable root is confidently wrong. Paths arrive folded and are folded
	// AGAIN here — see cleanMountPath for what that means and why.
	Mounts []mount `json:"mounts,omitempty"`
	// Disk throughput, the one fast-moving reading these cards used to lack:
	// a machine pinned by its disk looked idle in every field it sent.
	DiskReadBps  *float64 `json:"disk_read_bps,omitempty"`
	DiskWriteBps *float64 `json:"disk_write_bps,omitempty"`
	UptimeSec    *float64 `json:"uptime_sec,omitempty"`
	NetRxBps     *float64 `json:"net_rx_bps,omitempty"`
	NetTxBps     *float64 `json:"net_tx_bps,omitempty"`
	// Cumulative bytes since the machine booted, and the machine's own
	// interfaces. A rate says whether a box is busy; a total says how much of
	// a metered allowance is gone. The interface list is the only place a
	// dual-stack machine's IPv6 can come from — a relay observes the address
	// of ONE connection and cannot see the other family at all.
	NetRxTotal *float64 `json:"net_rx_total,omitempty"`
	NetTxTotal *float64 `json:"net_tx_total,omitempty"`
	// Packets, errors and drops over the same interfaces. A link that is losing
	// traffic keeps its byte rate looking healthy right up until somebody asks
	// why everything feels slow; the packet totals ride along so a drop count
	// can be read as a proportion rather than as a bare number.
	NetRxPackets *float64 `json:"net_rx_packets,omitempty"`
	NetTxPackets *float64 `json:"net_tx_packets,omitempty"`
	NetRxErrs    *float64 `json:"net_rx_errs,omitempty"`
	NetTxErrs    *float64 `json:"net_tx_errs,omitempty"`
	NetRxDrops   *float64 `json:"net_rx_drops,omitempty"`
	NetTxDrops   *float64 `json:"net_tx_drops,omitempty"`
	NICs         []nic    `json:"nics,omitempty"`
	Procs        *float64 `json:"procs,omitempty"`
	TempC        *float64 `json:"temp_c,omitempty"`
	Missing      []string `json:"missing,omitempty"`
	Series       *series  `json:"series,omitempty"`
}

// One filesystem other than root, and the only place in this block where a
// STRING comes off the machine. See cleanMountPath.
type mount struct {
	Path    string   `json:"path"`
	Total   *float64 `json:"total,omitempty"`
	Used    *float64 `json:"used,omitempty"`
	UsedPct *float64 `json:"used_percent,omitempty"`
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
	Sgn           *bool             `json:"sgn"`
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

// Fold a mount point down to the disk it describes, and refuse anything that
// is not one.
//
// The helper already does this before it sends (helper/go/mounts.go): at most
// three segments, and anything under a home directory collapses to the home
// itself, so `/mnt/backup` survives whole and `/home/alice/projects` becomes
// `/home`. Doing it again here is the entire job of this file — the helper's
// rule binds the helper, while anything else pushing at this relay is bound by
// nothing but what is written here.
//
// The control-character and bidi cleanup runs FIRST, so a path cannot use an
// override to make its folded form render as something else.
func cleanMountPath(v string) string {
	s := displayText(v, 512)
	if s == "" || s == "/" {
		return ""
	}
	// No leading slash gets exactly one shape: a Windows volume root. Anything
	// else is refused rather than length-capped and passed through — the first
	// version did the latter, which made `alice/private-project` a mount point
	// as far as this relay was concerned.
	if !strings.HasPrefix(s, "/") {
		if volumeRootRe.MatchString(s) {
			return s
		}
		return ""
	}
	segs := make([]string, 0, maxMountDepth)
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." {
			continue
		}
		// A mount table cannot contain "..", and a path that walks upwards is
		// not one this can reason about. Refused, never resolved.
		if seg == ".." {
			return ""
		}
		segs = append(segs, seg)
		if mountAnchors["/"+strings.Join(segs, "/")] || len(segs) >= maxMountDepth {
			break
		}
	}
	if len(segs) == 0 {
		return ""
	}
	return capMountPath("/" + strings.Join(segs, "/"))
}

// Cut to the cap on a RUNE boundary: a byte-level cut can split a multi-byte
// character and put an invalid sequence on the wire.
func capMountPath(p string) string {
	if len([]rune(p)) <= maxMountPath {
		return p
	}
	return string([]rune(p)[:maxMountPath-1]) + "…"
}

// How full a filesystem is, for RANKING only. A row with no percentage sorts
// below every row that has one: it cannot be the answer to "what is about to
// fill up", and calling it 0% would be that same claim in reverse.
func mountFullness(m mount) float64 {
	if m.UsedPct == nil {
		return -1
	}
	return *m.UsedPct
}

// The filesystem list, rebuilt row by row.
//
// Deduplicated on the FOLDED path — folding is lossy on purpose, so two users'
// home directories and four btrfs subvolumes all arrive as rows that mean the
// same thing — and the survivor is the FULLEST of them rather than the first.
// They are not necessarily the same filesystem, and first-one-wins hides a
// 99%-full disk behind a 10%-full one depending on push order.
func cleanMounts(body json.RawMessage) []mount {
	// Decoded here for the reason cleanNICs spells out: a list that is not a
	// list must cost the list and nothing else.
	var raw []json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &raw) != nil {
		return nil
	}
	var out []mount
	at := map[string]int{}
	for i, item := range raw {
		// Bounded before the cap rather than by it: folding every row of an
		// arbitrarily long list is work done on a client's say-so. Generous
		// enough that the cap below is what actually decides the list.
		if i >= 512 {
			break
		}
		var m struct {
			Path    *string         `json:"path"`
			Total   json.RawMessage `json:"total"`
			Used    json.RawMessage `json:"used"`
			UsedPct json.RawMessage `json:"used_percent"`
		}
		if json.Unmarshal(item, &m) != nil || m.Path == nil {
			continue
		}
		path := cleanMountPath(*m.Path)
		if path == "" {
			continue
		}
		row := mount{
			Path:    path,
			Total:   cleanNonNeg(looseNum(m.Total), 1e18),
			Used:    cleanNonNeg(looseNum(m.Used), 1e18),
			UsedPct: cleanPctPtr(looseNum(m.UsedPct)),
		}
		if j, dup := at[path]; dup {
			if mountFullness(row) > mountFullness(out[j]) {
				out[j] = row
			}
			continue
		}
		at[path] = len(out)
		out = append(out, row)
	}
	if len(out) > maxMounts {
		// The cap keeps what needs attention: trimming in arrival order dropped
		// a 100%-full partition because eight healthy volumes were listed ahead
		// of it, and the dashboard's own warning went with it.
		sort.SliceStable(out, func(i, j int) bool {
			fi, fj := mountFullness(out[i]), mountFullness(out[j])
			if fi != fj {
				return fi > fj
			}
			ti, tj := 0.0, 0.0
			if out[i].Total != nil {
				ti = *out[i].Total
			}
			if out[j].Total != nil {
				tj = *out[j].Total
			}
			return ti > tj
		})
		out = out[:maxMounts]
	}
	// Alphabetical for display, like the interface list: rows ordered by
	// fullness would reshuffle whenever two disks crossed over.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
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
// An interface name, which is NOT a kernel-shaped token: Go reports Windows
// interfaces by their friendly name, so "Ethernet 2" and "Wi-Fi" are ordinary
// and a localised install spells them in its own script. A class built around
// eth0/wg0 emptied the panel on every Windows machine. What is dangerous in a
// string about to be rendered is not a space but the characters that CONTROL
// rendering — C0/C1, the bidi overrides and isolates, the zero-width joiners,
// the line separators — and those are the ones refused here. Bounded by RUNES,
// so a name in a non-Latin script gets its full length.
func cleanNicName(v string) string {
	s := strings.TrimSpace(v)
	if s == "" || len([]rune(s)) > maxNICName {
		return ""
	}
	for _, r := range s {
		switch {
		case r < 0x20, r >= 0x7f && r <= 0x9f:
			return ""
		case r == 0x061c, r >= 0x200b && r <= 0x200f:
			return ""
		case r == 0x2028, r == 0x2029, r >= 0x202a && r <= 0x202e:
			return ""
		case r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x2069, r == 0xfeff:
			return ""
		}
	}
	return s
}

func cleanNICs(body json.RawMessage) []nic {
	// Decoded HERE rather than in the caller's struct, and the difference is
	// the same compatibility promise the scalar fields get: a typed
	// `[]json.RawMessage` field turns `"nics":"whatever"` into an Unmarshal
	// error that throws away the WHOLE system block, so one malformed list
	// would cost a machine its CPU and memory readings too.
	var raw []json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &raw) != nil || len(raw) == 0 {
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
		name := cleanNicName(r.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		n := nic{Name: name, Up: r.Up != nil && *r.Up}
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
		n.RxTotal = cleanNonNeg(looseNum(r.RxTotal), counterMax)
		n.TxTotal = cleanNonNeg(looseNum(r.TxTotal), counterMax)
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
		NetRxTotal json.RawMessage `json:"net_rx_total"`
		NetTxTotal json.RawMessage `json:"net_tx_total"`
		NICs       json.RawMessage `json:"nics"`
		Procs      json.RawMessage `json:"procs"`
		TempC      json.RawMessage `json:"temp_c"`
		Series     json.RawMessage `json:"series"`
		Missing    []string        `json:"missing"`
		// The 2026-08-07 round, RawMessage for the same compatibility promise
		// the note above makes: every one of these keys was unknown to every
		// deployed relay before that date, so a rejection has to stay
		// field-local rather than costing the whole system block.
		CPUUser      json.RawMessage `json:"cpu_user"`
		CPUSys       json.RawMessage `json:"cpu_system"`
		CPUIOWait    json.RawMessage `json:"cpu_iowait"`
		CPUSteal     json.RawMessage `json:"cpu_steal"`
		MemAvail     json.RawMessage `json:"mem_available"`
		MemCached    json.RawMessage `json:"mem_cached"`
		SwapInBps    json.RawMessage `json:"swap_in_bps"`
		SwapOutBps   json.RawMessage `json:"swap_out_bps"`
		Mounts       json.RawMessage `json:"mounts"`
		DiskReadBps  json.RawMessage `json:"disk_read_bps"`
		DiskWriteBps json.RawMessage `json:"disk_write_bps"`
		NetRxPackets json.RawMessage `json:"net_rx_packets"`
		NetTxPackets json.RawMessage `json:"net_tx_packets"`
		NetRxErrs    json.RawMessage `json:"net_rx_errs"`
		NetTxErrs    json.RawMessage `json:"net_tx_errs"`
		NetRxDrops   json.RawMessage `json:"net_rx_drops"`
		NetTxDrops   json.RawMessage `json:"net_tx_drops"`
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
	s.CPUUser = cleanPctPtr(looseNum(raw.CPUUser))
	s.CPUSys = cleanPctPtr(looseNum(raw.CPUSys))
	s.CPUIOWait = cleanPctPtr(looseNum(raw.CPUIOWait))
	s.CPUSteal = cleanPctPtr(looseNum(raw.CPUSteal))
	s.Load1 = cleanNonNeg(raw.Load1, 1e18)
	s.Load5 = cleanNonNeg(raw.Load5, 1e18)
	s.Load15 = cleanNonNeg(raw.Load15, 1e18)
	s.MemTotal = cleanNonNeg(raw.MemTotal, 1e18)
	s.MemPct = cleanPctPtr(raw.MemPct)
	s.MemAvail = cleanNonNeg(looseNum(raw.MemAvail), 1e18)
	s.MemCached = cleanNonNeg(looseNum(raw.MemCached), 1e18)
	s.SwapTotal = cleanNonNeg(raw.SwapTotal, 1e18)
	s.SwapPct = cleanPctPtr(raw.SwapPct)
	// Rates, so the tighter of the two ceilings — same bound as the network
	// rates below, for the same reason.
	s.SwapInBps = cleanNonNeg(looseNum(raw.SwapInBps), 1e15)
	s.SwapOutBps = cleanNonNeg(looseNum(raw.SwapOutBps), 1e15)
	s.DiskTotal = cleanNonNeg(raw.DiskTotal, 1e18)
	s.DiskUsed = cleanNonNeg(raw.DiskUsed, 1e18)
	s.DiskPct = cleanPctPtr(raw.DiskPct)
	s.Mounts = cleanMounts(raw.Mounts)
	s.DiskReadBps = cleanNonNeg(looseNum(raw.DiskReadBps), 1e15)
	s.DiskWriteBps = cleanNonNeg(looseNum(raw.DiskWriteBps), 1e15)
	s.UptimeSec = cleanNonNeg(raw.UptimeSec, 3.2e9)
	// Rates in bytes/second: a petabyte a second is already past any link, and
	// the tighter ceiling catches a cumulative counter pushed as a rate.
	s.NetRxBps = cleanNonNeg(looseNum(raw.NetRxBps), 1e15)
	s.NetTxBps = cleanNonNeg(looseNum(raw.NetTxBps), 1e15)
	// Cumulative counters, so neither the rate ceiling nor the capacity one
	// fits: an exabyte is a fine bound for a disk and a real 400 Gb/s link
	// walks past it in eight months. uint64 is what the kernel keeps these
	// in, so that is the bound.
	s.NetRxTotal = cleanNonNeg(looseNum(raw.NetRxTotal), counterMax)
	s.NetTxTotal = cleanNonNeg(looseNum(raw.NetTxTotal), counterMax)
	// Packets, errors and drops: counters like the byte totals, under the same
	// uint64 bound. A packet count on a long-lived router walks past an
	// exabyte-shaped ceiling for the same reason a byte counter does.
	s.NetRxPackets = cleanNonNeg(looseNum(raw.NetRxPackets), counterMax)
	s.NetTxPackets = cleanNonNeg(looseNum(raw.NetTxPackets), counterMax)
	s.NetRxErrs = cleanNonNeg(looseNum(raw.NetRxErrs), counterMax)
	s.NetTxErrs = cleanNonNeg(looseNum(raw.NetTxErrs), counterMax)
	s.NetRxDrops = cleanNonNeg(looseNum(raw.NetRxDrops), counterMax)
	s.NetTxDrops = cleanNonNeg(looseNum(raw.NetTxDrops), counterMax)
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
