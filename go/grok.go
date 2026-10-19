package main

// Grok: quota costs a credential Grok's own CLI left on disk.
//
// The grok CLI stores an OAuth token at ${GROK_HOME:-~/.grok}/auth.json — a
// map keyed by scope URL, each entry carrying a `key` (the bearer) and a
// `refresh_token`. Grok's own dashboard reads its usage from a Connect/
// grpc-web endpoint, and that is what this asks:
//
//	POST https://grok.com/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig
//	  Content-Type: application/grpc-web+proto
//	  body: 00 00 00 00 02 08 00   (one frame: field 1 = false)
//
// The frame is not empty any more: field 1 is `exclude_legacy_monthly_usage`,
// and the server answers an empty message with grpc-status 13 "Missing
// request message" (CodexBar GrokWebBillingFetcher, steipete/CodexBar@bb62d35532).
// Written as `08 00` and never `08 02` — the field is a bool, not a period
// selector, and a 2 there would drop the legacy monthly usage.
//
// Claude's cost rung and Claude's rules (claude.go / provhttp.go): read the
// one field, never write back, publish no transport text.
//
// The reply is a protobuf message with no schema we own, so — exactly as
// CodexBar's GrokWebBillingFetcher does — the frame is unwrapped and the
// payload SCANNED along the field paths the billing descriptor declares: the
// used-percent is the float32 at [1,1], the reset the varint at [1,5,1], and a
// message that carries a billing period but no percent at all is proto3's
// omitted zero scalar — 0% used, accepted only when the period is current.
// This is a heuristic on an undocumented wire, so it errs toward reporting
// nothing rather than a wrong number.
//
// THIS IS THE FALLBACK ROAD. The table (adapters.json `xai`, read by local.go)
// asks cli-chat-proxy.grok.com first; this runs only when that answered no
// window. grok.com's endpoint has since started demanding a browser-held WKE
// keypair and refusing bearer and cookie clients with grpc-status 16
// `no-credentials` (CodexBar@79077dd72a, issue #2812), so on many accounts
// this road now ends in `token-expired` — reported as such, not as a parse
// failure, so the page says "sign in" rather than "unreadable".
//
// CodexBar also drives `grok agent stdio` for a richer billing object; on the
// CLI versions seen here that RPC answers "Method not found", and it is not
// shipped.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	grokBillingURL = "https://grok.com/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig"
	// Reset coupons: the tokens an account holds to clear its usage window
	// early (CodexBar GrokRemainingResetsFetcher, steipete/CodexBar@0ead0d6787).
	grokResetsURL = "https://grok.com/prod_mc_billing.ConsumerUiSvc/GetRemainingResets"
	grokHomeEnv   = "GROK_HOME"
	grokTokenEnv  = "MON_GROK_TOKEN"
	// The CLI's own override, which CodexBar's GrokSettingsReader honours
	// ahead of auth.json; honoured here after this helper's own.
	grokCLITokenEnv = "GROK_OAUTH_TOKEN"
	// The coupon call is a side question the reading can do without: a
	// deadline of its own (CodexBar's requestTimeoutSeconds = 2) so a stall
	// there never holds the credits reading.
	grokResetsTimeout = 2 * time.Second
)

// Field 1 = false, framed (see the header).
var grokBillingBody = []byte{0, 0, 0, 0, 2, 0x08, 0x00}

// The coupon call still takes the empty frame.
var grokResetsBody = []byte{0, 0, 0, 0, 0}

var grokCache = provCache{pace: paceMark{id: "grok"}}

func collectGrok() Provider {
	return grokCache.collect(fetchGrok)
}

// The bearer out of auth.json. Entries are keyed by scope URL; the OIDC scope
// (auth.x.ai) is preferred over the legacy sign-in scope, matching GrokAuth.
// Only `key` and — for a freshness check we do not currently enforce —
// expiry are read; refresh is deliberately not implemented (the web endpoint
// tolerates a slightly stale bearer, and holding a refresh flow for a
// heuristic reading is not worth the credential handling).
func grokToken() string {
	for _, env := range []string{grokTokenEnv, grokCLITokenEnv} {
		if v := grokNormalizeToken(os.Getenv(env)); v != "" {
			return v
		}
	}
	dir := strings.TrimSpace(os.Getenv(grokHomeEnv))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".grok")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil {
		return ""
	}
	var doc map[string]struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	var oidc, legacy string
	for scope, entry := range doc {
		if entry.Key == "" {
			continue
		}
		if strings.HasPrefix(scope, "https://auth.x.ai::") {
			oidc = entry.Key
		} else if strings.Contains(scope, "/sign-in") {
			legacy = entry.Key
		}
	}
	if oidc != "" {
		return oidc
	}
	return legacy
}

// A pasted token, as CodexBar's GrokCredentialRouting reads one: a "Bearer "
// prefix is shed; a cookie header ("cookie:…", or anything with an `=` in
// it), and an `xai-` management API key — which this endpoint does not take
// and which must not be sent to it — are no token at all.
func grokNormalizeToken(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 7 && strings.EqualFold(s[:7], "bearer ") {
		s = strings.TrimSpace(s[7:])
	}
	lower := strings.ToLower(s)
	if s == "" || strings.HasPrefix(lower, "cookie:") || strings.HasPrefix(lower, "xai-") || strings.Contains(s, "=") {
		return ""
	}
	return s
}

func grokHeaders(token string) map[string]string {
	return map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/grpc-web+proto",
		"Accept":        "*/*",
		"Origin":        "https://grok.com",
		"Referer":       "https://grok.com/?_s=usage",
		"X-Grpc-Web":    "1",
		"X-User-Agent":  "connect-es/2.1.1",
	}
}

func fetchGrok() Provider {
	p := Provider{ID: "grok", Name: "Grok", Source: "api", CapturedAt: now()}
	fail := func(err, code string, arg ...string) Provider {
		p.failWith(err, code, arg...)
		return p
	}

	token := grokToken()
	if token == "" {
		return fail("not-signed-in", "creds-missing", "~/.grok/auth.json")
	}

	status, body, err := provRequest("POST", grokBillingURL, grokHeaders(token), grokBillingBody)
	if err != nil {
		return fail("unreachable", "req-failed")
	}
	switch {
	case status == 401 || status == 403:
		return fail("token-expired", "http-status", itoa(status))
	case status != 200:
		return fail("api-error", "http-status", itoa(status))
	}

	t := now()
	bill, perr := grokParseBilling(body, t)
	if perr != nil {
		var rpc *grokRPCError
		if errors.As(perr, &rpc) {
			switch {
			case rpc.authFailure():
				// Status 16 is the WKE rejection (see the header) or any other
				// unauthenticated answer; 7 only with a credential message.
				return fail("token-expired", "http-status", "grpc-status "+itoa(rpc.status))
			case rpc.teamUnsupported():
				// A team principal: the consumer billing surface has nothing
				// for it, and that is a missing reading, not a broken one.
				return fail("no-readings", "no-usage")
			}
			return fail("api-error", "http-status", "grpc-status "+itoa(rpc.status))
		}
		return fail("api-error", "http-parse")
	}
	if bill.used == nil {
		return fail("no-readings", "no-usage")
	}

	l := Limit{Key: "credits", UsedPercent: round2(*bill.used)}
	if bill.resets != nil {
		l.ResetsAt = bill.resets
	}
	p.Limits = append(p.Limits, l)
	// The coupons ride on the same credential; a failure there costs the
	// coupons and nothing else.
	p.Resets = grokFetchResets(token, t)
	p.OK = true
	p.RecordedAt = fp(t)
	return p
}

// ── grpc-web + heuristic protobuf scan ───────────────────────────────────

// What a GetGrokCreditsConfig reply said.
type grokBilling struct {
	used   *float64
	resets *float64
	// used is the proto3 omitted zero rather than a float on the wire —
	// accepted only when the message's current period spans now, so an
	// unrelated message that merely mentions a date cannot read as 0% used.
	implicitZero bool
}

// A grpc-status other than 0 in a trailer, with its message. The message is
// matched against, never published.
type grokRPCError struct {
	status  int
	message string
}

func (e *grokRPCError) Error() string { return "grpc-status " + itoa(e.status) }

// CodexBar GrokWebBillingError.isAuthenticationFailure: 16 always; 7 only
// when the message says the credential is what failed.
func (e *grokRPCError) authFailure() bool {
	if e.status == 16 {
		return true
	}
	if e.status != 7 {
		return false
	}
	m := strings.ToLower(e.message)
	return strings.Contains(m, "bad-credentials") || strings.Contains(m, "unauthenticated") ||
		(strings.Contains(m, "oauth2") && strings.Contains(m, "could not be validated")) ||
		(strings.Contains(m, "access token") &&
			(strings.Contains(m, "invalid") || strings.Contains(m, "expired") || strings.Contains(m, "could not be validated")))
}

// CodexBar GrokWebBillingFetcher.isTeamBillingUnavailable.
func (e *grokRPCError) teamUnsupported() bool {
	if e.status != 9 {
		return false
	}
	m := strings.ToLower(strings.TrimSpace(e.message))
	return m == "no personal team" || m == "no personal team."
}

// The fields a scan found, each with the path of field numbers that led to
// it. `order` is the fixed32's position in the walk — the tie-break when two
// candidates sit at the same depth.
type grokScan struct {
	fixed32  []grokFixed32
	varints  []grokVarint
	complete bool
	order    int
}

type grokFixed32 struct {
	path  []int
	value float32
	order int
}

type grokVarint struct {
	path  []int
	value uint64
}

// Pull the used-percent and a reset epoch out of a GetGrokCreditsConfig
// reply. Returns nil used when nothing plausible is found; error when the
// framing itself is broken or a trailer carries a failure status.
func grokParseBilling(body []byte, t float64) (grokBilling, error) {
	var out grokBilling
	payloads, perr := grokDataFrames(body)
	if perr != nil {
		return out, perr
	}
	// A message with no data frame but a payload that opens like protobuf is
	// itself the payload (some deployments skip framing on the happy path).
	if len(payloads) == 0 && grokLooksProto(body) {
		payloads = [][]byte{body}
	}
	if len(payloads) == 0 {
		return out, errors.New("no payload")
	}

	scan := grokScan{complete: true}
	for _, pl := range payloads {
		grokScanProto(pl, 0, nil, &scan)
	}

	// The percent: a float32 at a field 1, finite and in [0,100] — the
	// shallowest, then the first. On the real wire that is [1,1]
	// (credit_usage_percent). A non-zero subnormal is an integer field misread
	// as float and never a percentage (review finding, kept from the first
	// version of this scan); an exact zero is a genuine 0% (0x00000000).
	var pct *grokFixed32
	for i := range scan.fixed32 {
		f := &scan.fixed32[i]
		v := float64(f.value)
		if f.path[len(f.path)-1] != 1 || !finite(v) || v < 0 || v > 100 {
			continue
		}
		if v != 0 && math.Abs(v) < 1e-6 {
			continue
		}
		if pct == nil || len(f.path) < len(pct.path) || (len(f.path) == len(pct.path) && f.order < pct.order) {
			pct = f
		}
	}
	if pct != nil {
		out.used = fp(round2(float64(pct.value)))
	}

	// Dates: every varint in the epoch band. The reset is the one at
	// [1,5,1] when it is still ahead, else the soonest ahead anywhere.
	var dated []grokVarint
	for _, v := range scan.varints {
		if v.value >= 1_700_000_000 && v.value <= 2_100_000_000 {
			dated = append(dated, v)
		}
	}
	var reset *float64
	for _, declaredOnly := range []bool{true, false} {
		for _, v := range dated {
			f := float64(v.value)
			if f <= t || (declaredOnly && !grokPathIs(v.path, 1, 5, 1)) {
				continue
			}
			if reset == nil || f < *reset {
				reset = fp(f)
			}
		}
		if reset != nil {
			break
		}
	}
	out.resets = reset

	// No percent, no float of any kind, but a reset and a usage period: the
	// proto3 omitted zero scalar. Published as 0% only when the current
	// period ([1,8]: type 1 or 2 — weekly or monthly — with start at [1,8,2,1]
	// and end at [1,8,3,1]) spans now, from one complete payload.
	if out.used == nil && len(scan.fixed32) == 0 && reset != nil {
		hasPeriod, periodKind := false, false
		var start, end *float64
		for _, v := range scan.varints {
			switch {
			case len(v.path) >= 2 && v.path[0] == 1 && v.path[1] == 6:
				hasPeriod = true
			case grokPathIs(v.path, 1, 8, 1) && (v.value == 1 || v.value == 2):
				hasPeriod, periodKind = true, true
			}
		}
		for _, v := range dated {
			if start == nil && grokPathIs(v.path, 1, 8, 2, 1) {
				start = fp(float64(v.value))
			}
			if end == nil && grokPathIs(v.path, 1, 8, 3, 1) {
				end = fp(float64(v.value))
			}
		}
		if hasPeriod {
			active := periodKind && start != nil && end != nil && *start <= t && *end > t
			if len(payloads) == 1 && scan.complete && active {
				out.used = fp(0)
				out.implicitZero = true
			}
		}
	}
	return out, nil
}

func grokPathIs(path []int, want ...int) bool {
	if len(path) != len(want) {
		return false
	}
	for i := range want {
		if path[i] != want[i] {
			return false
		}
	}
	return true
}

// The messages the billing descriptor declares, by path. Any other
// length-delimited field is opaque bytes — a string, say — and is not
// walked into, so a byte that happens to look like a tag inside one cannot
// turn into a percentage (CodexBar@c3a25fd85f).
var grokKnownMessages = map[string]bool{
	"1":   true,
	"1.2": true, "1.3": true, "1.4": true, "1.5": true, "1.6": true, "1.7": true, "1.8": true, "1.12": true,
	"1.6.1": true, "1.6.2": true, "1.6.3": true, "1.8.2": true, "1.8.3": true,
	"1.6.3.2": true, "1.6.3.3": true,
}

func grokPathKey(path []int) string {
	parts := make([]string, len(path))
	for i, n := range path {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}

// Walk a protobuf payload, bounded in depth and to the declared messages,
// recording every varint and every float32-shaped field with its path. A
// malformed field stops the walk and marks the scan incomplete — what was
// read before it stands, what sits after it is not guessed at.
func grokScanProto(b []byte, depth int, path []int, scan *grokScan) {
	i := 0
	for i < len(b) {
		f, ok := grokReadField(b, &i)
		if !ok {
			scan.complete = false
			return
		}
		fieldPath := append(append([]int{}, path...), f.number)
		switch {
		case f.wire == 0:
			scan.varints = append(scan.varints, grokVarint{path: fieldPath, value: f.varint})
		case f.wire == 2:
			if depth < 4 && grokKnownMessages[grokPathKey(fieldPath)] {
				grokScanProto(f.message, depth+1, fieldPath, scan)
			}
		case f.wire == 5:
			scan.fixed32 = append(scan.fixed32, grokFixed32{path: fieldPath, value: f.fixed32, order: scan.order})
			scan.order++
		}
	}
}

// One wire field. Bounded as CodexBar's GrokProtobufField is: a field number
// in 1…2^29-1, a varint of at most ten bytes whose tenth carries no more
// than one bit, a length that fits the enclosing message, and only the four
// wire types this wire uses — anything else is corruption, not a shrug.
type grokField struct {
	number  int
	wire    int
	varint  uint64
	fixed32 float32
	message []byte
}

func grokReadField(b []byte, i *int) (grokField, bool) {
	var f grokField
	key, ok := grokReadVarint(b, i)
	if !ok {
		return f, false
	}
	num := key >> 3
	if num == 0 || num > 536_870_911 {
		return f, false
	}
	f.number = int(num)
	f.wire = int(key & 7)
	switch f.wire {
	case 0:
		v, ok := grokReadVarint(b, i)
		if !ok {
			return f, false
		}
		f.varint = v
	case 1:
		if len(b)-*i < 8 {
			return f, false
		}
		*i += 8
	case 2:
		ln, ok := grokReadVarint(b, i)
		// Compared as uint64 before any int conversion: a huge varint cast
		// to int wraps negative and slips past a signed bound check.
		if !ok || ln > uint64(len(b)-*i) {
			return f, false
		}
		f.message = b[*i : *i+int(ln)]
		*i += int(ln)
	case 5:
		if len(b)-*i < 4 {
			return f, false
		}
		f.fixed32 = math.Float32frombits(binary.LittleEndian.Uint32(b[*i : *i+4]))
		*i += 4
	default:
		return f, false
	}
	return f, true
}

func grokReadVarint(b []byte, i *int) (uint64, bool) {
	var v uint64
	var shift uint
	for *i < len(b) && shift < 64 {
		c := b[*i]
		*i++
		if shift == 63 && c > 1 {
			return 0, false
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, true
		}
		shift += 7
	}
	return 0, false
}

// grpc-web data frames: [1B flags][4B BE len][payload]. Frames with the
// 0x80 bit set are trailers (a text block of key: value); a non-zero
// grpc-status in one is a failure, returned with its status and message.
//
// A MALFORMED stream is an error, not a shrug: a frame whose declared length
// runs past the buffer, trailing bytes that are not a whole frame, or a flag
// byte that is neither data (0x00) nor trailer (0x80) — the compressed and
// reserved bits Grok does not use — all mean this is not the reply we think
// it is, and "return what parsed so far" could publish a real reading sitting
// in front of a truncated failure trailer.
func grokDataFrames(body []byte) ([][]byte, error) {
	var out [][]byte
	i := 0
	for i < len(body) {
		if i+5 > len(body) {
			return nil, errors.New("trailing bytes are not a whole frame")
		}
		flags := body[i]
		// Length compared BEFORE any int conversion: a uint64→int cast of a
		// huge value wraps negative and slips past a signed bound check.
		n := uint64(binary.BigEndian.Uint32(body[i+1 : i+5]))
		i += 5
		if n > uint64(len(body)-i) {
			return nil, errors.New("frame length overruns body")
		}
		frame := body[i : i+int(n)]
		i += int(n)
		switch flags {
		case 0x00:
			out = append(out, frame)
		case 0x80:
			// Trailer. A grpc-status other than 0 means the call failed.
			if st, msg := grokTrailerStatus(frame); st != "" && st != "0" {
				code, err := strconv.Atoi(st)
				if err != nil {
					return nil, errors.New("grpc-status " + cleanText(st, 8))
				}
				return nil, &grokRPCError{status: code, message: msg}
			}
		default:
			// Compressed (0x01) or any reserved bit: we cannot read it, so we
			// must not pretend the numbers behind it are current.
			return nil, errors.New("unsupported grpc-web frame flags")
		}
	}
	return out, nil
}

// grpc-status and grpc-message out of a trailer block. The message is
// percent-encoded on the wire (grpc-web does that to keep it to one line).
func grokTrailerStatus(frame []byte) (status, message string) {
	for _, line := range strings.Split(string(frame), "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "grpc-status":
			status = strings.TrimSpace(v)
		case "grpc-message":
			message = strings.TrimSpace(v)
			if un, err := url.PathUnescape(message); err == nil {
				message = un
			}
		}
	}
	return status, message
}

// Does this open like a protobuf message? First byte is a tag whose field
// number is > 0 and wire type is one of the four Grok uses.
func grokLooksProto(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	field := b[0] >> 3
	wire := b[0] & 0x7
	return field > 0 && (wire == 0 || wire == 1 || wire == 2 || wire == 5)
}

// ── reset coupons ─────────────────────────────────────────────────────────

// How many reset coupons the account holds and when each lapses, or nil:
// the call is a courtesy, and any failure — transport, status, a record
// that will not parse — costs the coupons alone. The reply is protobuf:
// repeated field 10, each a record whose field 10 is the token id (UTF-8),
// 20 the grant (a Timestamp: field 1 = seconds) and 30 the expiry. Only the
// count and the expiries leave this function; the id is never published and
// never kept (CodexBar GrokRemainingResetsFetcher, steipete/CodexBar@0ead0d6787).
func grokFetchResets(token string, t float64) *ResetCredits {
	status, body, err := provRequestTimeout("POST", grokResetsURL, grokHeaders(token), grokResetsBody, grokResetsTimeout)
	if err != nil || status != 200 {
		return nil
	}
	return grokParseResets(body, t)
}

func grokParseResets(body []byte, t float64) *ResetCredits {
	payloads, err := grokDataFrames(body)
	if err != nil {
		return nil
	}
	if len(payloads) == 0 && grokLooksProto(body) {
		payloads = [][]byte{body}
	}
	var expires []float64
	for _, pl := range payloads {
		fields, ok := grokFields(pl)
		if !ok {
			return nil
		}
		for _, f := range fields {
			if f.number != 10 || f.wire != 2 {
				continue
			}
			exp, ok := grokResetRecord(f.message, t)
			if !ok {
				// A record that will not parse is a wire we do not know;
				// a count taken over the rest would be a count of nothing.
				return nil
			}
			if exp != nil {
				expires = append(expires, *exp)
			}
		}
	}
	if len(expires) == 0 {
		return nil
	}
	sort.Float64s(expires)
	return &ResetCredits{Left: float64(len(expires)), Expires: expires}
}

// One coupon record: its expiry when it is still ahead and the record names
// a token, nil for a lapsed one, !ok for a shape that is not a record.
func grokResetRecord(b []byte, t float64) (*float64, bool) {
	fields, ok := grokFields(b)
	if !ok {
		return nil, false
	}
	var hasID bool
	var expires *float64
	for _, f := range fields {
		if f.wire != 2 {
			continue
		}
		switch f.number {
		case 10:
			hasID = len(f.message) > 0 && utf8.Valid(f.message)
		case 30:
			expires = grokTimestamp(f.message)
		}
	}
	if !hasID || expires == nil {
		return nil, false
	}
	if *expires <= t {
		return nil, true
	}
	return expires, true
}

// A Timestamp message's seconds (field 1), in the epoch band or nothing.
func grokTimestamp(b []byte) *float64 {
	fields, ok := grokFields(b)
	if !ok {
		return nil
	}
	for _, f := range fields {
		if f.number == 1 && f.wire == 0 && f.value >= 1_700_000_000 && f.value <= 2_100_000_000 {
			return fp(float64(f.value))
		}
	}
	return nil
}

type grokFlatField struct {
	number  int
	wire    int
	value   uint64
	message []byte
}

// A message's top-level fields, or !ok when any of them is malformed.
func grokFields(b []byte) ([]grokFlatField, bool) {
	var out []grokFlatField
	i := 0
	for i < len(b) {
		f, ok := grokReadField(b, &i)
		if !ok {
			return nil, false
		}
		out = append(out, grokFlatField{number: f.number, wire: f.wire, value: f.varint, message: f.message})
	}
	return out, true
}
