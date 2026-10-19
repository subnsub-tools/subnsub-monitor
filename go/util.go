package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func itoa(n int) string { return strconv.Itoa(n) }

// Rollout timestamps are ISO-8601 Zulu; the usage API returns ISO-8601 with an
// offset. Both normalise to unix seconds here so nothing downstream has to
// wonder which unit it holds. Returns 0 on anything unparseable, so a malformed
// record sorts last instead of taking the scan down.
func isoToEpoch(ts string) float64 {
	if ts == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return float64(t.UnixNano()) / 1e9
		}
	}
	return 0
}

// Codex reports credit balances as either a string or a number depending on
// version. Normalise without letting a surprise type through as "%!s(...)".
func anyToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}

// Serialise, refusing to emit anything JSON cannot represent.
//
// Go's encoder errors on NaN/Inf rather than writing bare tokens the browser
// would reject, so a failure here means the snapshot held a value that should
// have been filtered earlier — report that instead of shipping half a document.
func dump(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		fallback, _ := json.Marshal(map[string]any{
			"ok":          false,
			"error":       "bad-values",
			"detail":      "snapshot contained values that cannot be serialised",
			"captured_at": now(),
		})
		return fallback
	}
	return b
}

func dumpIndent(v any) []byte {
	var out []byte
	b := dump(v)
	var tmp any
	if json.Unmarshal(b, &tmp) == nil {
		if pretty, err := json.MarshalIndent(tmp, "", "  "); err == nil {
			return pretty
		}
	}
	out = b
	return out
}

func warnf(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }

// A setting the installer may have written to the private token file, with
// the process environment taking precedence — the same lookup the CLI Proxy
// API and the machine label use.
func envSetting(key string, file map[string]string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return file[key]
}

// cleanText is the rule for any free text that will be shown on the
// dashboard: no control characters, no bidi overrides, cut at `max` BYTES on
// a rune boundary. Mirrors clean.js's displayText; the relay applies it again.
func cleanText(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			continue
		}
		if b.Len()+len(string(r)) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// A dollar amount for a balance string: two decimals when they carry
// information, none when they are zeros — "$8.71" and "$5", never "$5.00".
func trimAmount(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}
