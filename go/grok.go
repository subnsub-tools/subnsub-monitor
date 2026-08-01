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
//	  body: 00 00 00 00 00   (one empty grpc-web frame)
//
// Claude's cost rung and Claude's rules (claude.go / provhttp.go): read the
// one field, never write back, publish no transport text.
//
// The reply is a protobuf message with no schema we own, so — exactly as
// CodexBar's GrokWebBillingFetcher does — the frame is unwrapped and the
// payload SCANNED: the used-percent is a float32 field, the reset is a varint
// in the epoch band. This is a heuristic on an undocumented wire, so it errs
// toward reporting nothing rather than a wrong number.
//
// CodexBar also drives `grok agent stdio` for a richer billing object; on the
// CLI versions seen here that RPC answers "Method not found", so the
// web-billing path is the one that actually works and the only one shipped.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	grokBillingURL = "https://grok.com/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig"
	grokHomeEnv    = "GROK_HOME"
	grokTokenEnv   = "MON_GROK_TOKEN"
)

var grokCache provCache

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
	if v := strings.TrimSpace(os.Getenv(grokTokenEnv)); v != "" {
		return v
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

func fetchGrok() Provider {
	p := Provider{ID: "grok", Name: "Grok", Source: "api", CapturedAt: now()}
	fail := func(err, detail string) Provider {
		p.Error, p.Detail = err, detail
		return p
	}

	token := grokToken()
	if token == "" {
		return fail("not-signed-in", "~/.grok/auth.json 里没有可用的凭据；跑一次 grok login。")
	}

	// One empty grpc-web frame: 1 flag byte + 4-byte big-endian length 0.
	reqBody := []byte{0, 0, 0, 0, 0}
	status, body, err := provRequest("POST", grokBillingURL, map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/grpc-web+proto",
		"Accept":        "*/*",
		"Origin":        "https://grok.com",
		"Referer":       "https://grok.com/?_s=usage",
		"X-Grpc-Web":    "1",
		"X-User-Agent":  "connect-es/2.1.1",
	}, reqBody)
	if err != nil {
		return fail("unreachable", "请求失败")
	}
	switch {
	case status == 401 || status == 403:
		return fail("token-expired", "Grok 计费接口返回 "+itoa(status))
	case status != 200:
		return fail("api-error", "Grok 计费接口返回 "+itoa(status))
	}

	used, resets, perr := grokParseBilling(body)
	if perr != nil {
		return fail("api-error", "Grok 计费响应无法解析")
	}
	if used == nil {
		return fail("no-readings", "Grok 计费响应里没有可用的用量")
	}

	l := Limit{Key: "credits", UsedPercent: round2(*used)}
	if resets != nil {
		l.ResetsAt = resets
	}
	p.Limits = append(p.Limits, l)
	p.OK = true
	p.RecordedAt = fp(now())
	return p
}

// ── grpc-web + heuristic protobuf scan ───────────────────────────────────

// Pull the used-percent (a float32 in [0,100]) and a reset epoch out of a
// GetGrokCreditsConfig reply. Returns nil used when nothing plausible is
// found; error only when the framing itself is broken.
func grokParseBilling(body []byte) (used *float64, resets *float64, err error) {
	payloads, perr := grokDataFrames(body)
	if perr != nil {
		return nil, nil, perr
	}
	// A message with no data frame but a payload that opens like protobuf is
	// itself the payload (some deployments skip framing on the happy path).
	if len(payloads) == 0 && grokLooksProto(body) {
		payloads = [][]byte{body}
	}

	// Collect EVERY plausible percent candidate rather than greedily taking
	// the shallowest. The heuristic's failure mode (review finding): an
	// integer field carrying 1 is, read as float32, the subnormal 1.4e-45 —
	// which is "in [0,100]", is shallower than the real 37.5% nested deeper,
	// and rounds to 0. Two defences: reject values that are clearly not a
	// percentage a server would send (non-zero subnormals, i.e. a raw integer
	// misread as float), and require the surviving candidates to AGREE — if
	// two distinct percentages remain, the wire is ambiguous and we publish
	// nothing rather than guess.
	var pctCands []float64
	var resetCands []float64
	for _, pl := range payloads {
		grokScan(pl, 0, func(depth int, lastField int, f float32) {
			if lastField != 1 || f < 0 || f > 100 {
				return
			}
			// A non-zero subnormal is an integer field misread as float, never
			// a percentage. Exact zero is allowed (a genuine 0% is 0x00000000).
			if f != 0 && math.Abs(float64(f)) < 1e-6 {
				return
			}
			pctCands = append(pctCands, round2(float64(f)))
		}, func(v uint64) {
			f := float64(v)
			if f >= 1_700_000_000 && f <= 2_100_000_000 {
				resetCands = append(resetCands, f)
			}
		})
	}
	if u := grokUnique(pctCands); u != nil {
		used = u
	}
	if len(resetCands) > 0 {
		t := now()
		min := 0.0
		found := false
		for _, c := range resetCands {
			if c > t && (!found || c < min) {
				min, found = c, true
			}
		}
		if found {
			resets = &min
		}
	}
	return used, resets, nil
}

// The one value all candidates agree on (within rounding), or nil when they
// disagree or there are none. "One clear answer or none" is the right bias
// for a heuristic scan of an unlabelled wire.
func grokUnique(cands []float64) *float64 {
	if len(cands) == 0 {
		return nil
	}
	first := cands[0]
	for _, c := range cands[1:] {
		if math.Abs(c-first) > 0.01 {
			return nil
		}
	}
	return &first
}

// grpc-web data frames: [1B flags][4B BE len][payload]. Frames with the
// 0x80 bit set are trailers (a text block of key: value); a non-zero
// grpc-status in one is a failure.
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
			if st := grokTrailerStatus(frame); st != "" && st != "0" {
				return nil, errors.New("grpc-status " + st)
			}
		default:
			// Compressed (0x01) or any reserved bit: we cannot read it, so we
			// must not pretend the numbers behind it are current.
			return nil, errors.New("unsupported grpc-web frame flags")
		}
	}
	return out, nil
}

func grokTrailerStatus(frame []byte) string {
	for _, line := range strings.Split(string(frame), "\r\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "grpc-status") {
			return strings.TrimSpace(v)
		}
	}
	return ""
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

// Walk a protobuf payload, bounded in depth, calling onFixed32 for every
// float32-shaped field (with the field number and nesting depth) and onVarint
// for every varint. Malformed input stops the walk rather than erroring — the
// scan is a best effort over an unknown schema.
func grokScan(b []byte, depth int, onFixed32 func(depth, field int, f float32), onVarint func(v uint64)) {
	if depth > 4 {
		return
	}
	i := 0
	for i < len(b) {
		tag, n := binary.Uvarint(b[i:])
		if n <= 0 {
			return
		}
		i += n
		field := int(tag >> 3)
		wire := int(tag & 0x7)
		switch wire {
		case 0: // varint
			v, m := binary.Uvarint(b[i:])
			if m <= 0 {
				return
			}
			i += m
			onVarint(v)
		case 5: // fixed32
			if i+4 > len(b) {
				return
			}
			bits := binary.LittleEndian.Uint32(b[i : i+4])
			onFixed32(depth+1, field, math.Float32frombits(bits))
			i += 4
		case 1: // fixed64
			if i+8 > len(b) {
				return
			}
			i += 8
		case 2: // length-delimited — recurse, it may hold nested messages
			ln, m := binary.Uvarint(b[i:])
			if m <= 0 {
				return
			}
			i += m
			// Compare as uint64 before converting: int(ln) on a huge varint
			// wraps negative and slips past a signed bound check, the same
			// trap grokDataFrames just closed.
			if ln > uint64(len(b)-i) {
				return
			}
			grokScan(b[i:i+int(ln)], depth+1, onFixed32, onVarint)
			i += int(ln)
		default:
			return // groups etc. — not something this wire uses
		}
	}
}
