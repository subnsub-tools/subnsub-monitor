package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
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
