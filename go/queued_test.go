package main

// The six 2026-08 additions, tested at the parse seam. Kiro and Grok also got
// a live run against the real CLI/endpoint on the machine this was built on;
// these pin the shapes that live run cannot cover (older CLI vintages, the
// protobuf scan, the tRPC fallbacks) and that a refactor could break quietly.

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// ── Kiro ──────────────────────────────────────────────────────────────────

func TestKiroResetRollsMMDDForward(t *testing.T) {
	// A bare MM/DD in the past rolls to next year; yyyy-MM-dd is taken as-is.
	iso := kiroReset("2099-01-15")
	if iso == nil {
		t.Fatal("iso date should parse")
	}
	past := time.Now().AddDate(0, 0, -2)
	mmdd := past.Format("01/02")
	got := kiroReset(mmdd)
	if got == nil {
		t.Fatalf("MM/DD %q should parse", mmdd)
	}
	if *got <= float64(time.Now().Unix()) {
		t.Fatal("a past MM/DD must roll forward to the future")
	}
}

func TestKiroDisplayPlan(t *testing.T) {
	for in, want := range map[string]string{
		"KIRO FREE": "Kiro Free", "KIRO PRO": "Kiro Pro", "": "",
	} {
		if got := kiroDisplayPlan(in); got != want {
			t.Errorf("kiroDisplayPlan(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── Doubao ────────────────────────────────────────────────────────────────

func TestDoubaoWindowVocabulary(t *testing.T) {
	for _, tc := range []struct {
		label string
		key   string
		mins  float64
	}{
		{"5-hour", "session", 300}, {"Weekly", "weekly", 10080}, {"month", "monthly", 43200},
	} {
		k, m := doubaoWindow(tc.label)
		if k != tc.key || m == nil || *m != tc.mins {
			t.Errorf("doubaoWindow(%q) = %q/%v, want %q/%v", tc.label, k, m, tc.key, tc.mins)
		}
	}
	if k, m := doubaoWindow("annual"); k != "" || m != nil {
		t.Errorf("unknown label must yield empty: %q/%v", k, m)
	}
}

func TestDoubaoResetUnits(t *testing.T) {
	if r := doubaoReset("2100-01-01T00:00:00Z"); r == nil {
		t.Fatal("iso reset")
	}
	if r := doubaoReset(4_102_444_800_000.0); r == nil || *r != 4_102_444_800 {
		t.Fatalf("ms epoch: %v", r)
	}
	if r := doubaoReset(-5.0); r != nil {
		t.Fatal("non-positive must be nil")
	}
}

// ── Kilo ──────────────────────────────────────────────────────────────────

func TestKiloContextsAndMoney(t *testing.T) {
	// A nested payload: money is found two levels down, cents beats plain.
	pay := map[string]any{
		"subscription": map[string]any{
			"usedCents": 250.0, "total": 10.0,
		},
	}
	ctxs := kiloContexts(pay)
	var foundUsed bool
	for _, c := range ctxs {
		if v, ok := kiloMoney(c, []string{"usedCents"}, nil, []string{"used"}); ok {
			if v != 2.5 {
				t.Fatalf("cents should divide by 100: got %v", v)
			}
			foundUsed = true
		}
	}
	if !foundUsed {
		t.Fatal("nested usedCents not reached by the BFS")
	}
}

func TestKiloPayloadUnwrap(t *testing.T) {
	entry := map[string]any{
		"result": map[string]any{
			"data": map[string]any{"json": map[string]any{"x": 1.0}},
		},
	}
	pay, ok := kiloPayload(entry).(map[string]any)
	if !ok || pay["x"] != 1.0 {
		t.Fatalf("result.data.json not unwrapped: %v", kiloPayload(entry))
	}
	// A null json unwraps to nil, not to the wrapper.
	entry2 := map[string]any{"result": map[string]any{"data": map[string]any{"json": nil}}}
	if kiloPayload(entry2) != nil {
		t.Fatal("null json should unwrap to nil")
	}
}

func TestKiloTierNames(t *testing.T) {
	if got := kiloPlanName(map[string]any{"tier": "tier_49"}); got != "Pro" {
		t.Errorf("tier_49 = %q, want Pro", got)
	}
	if got := kiloPlanName(map[string]any{"planName": "Custom"}); got != "Custom" {
		t.Errorf("planName fallback = %q", got)
	}
}

// ── Grok ──────────────────────────────────────────────────────────────────

// Build one grpc-web data frame around a protobuf payload.
func grokFrame(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// field 1, wire 5 (fixed32), a float32.
func protoFixed32(field int, f float32) []byte {
	b := []byte{byte(field<<3 | 5), 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(b[1:], math.Float32bits(f))
	return b
}

func TestGrokParsesUsedPercentFromFrame(t *testing.T) {
	// A nested message whose inner field 1 is 37.5% used.
	inner := protoFixed32(1, 37.5)
	outer := append([]byte{byte(1<<3 | 2), byte(len(inner))}, inner...)
	used, _, err := grokParseBilling(grokFrame(outer))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if used == nil || math.Abs(*used-37.5) > 0.01 {
		t.Fatalf("used = %v, want 37.5", used)
	}
}

func TestGrokTrailerStatusFails(t *testing.T) {
	// A trailer frame (flag 0x80) carrying grpc-status 7 must surface as error.
	trailer := []byte("grpc-status:7\r\ngrpc-message:denied\r\n")
	frame := make([]byte, 5+len(trailer))
	frame[0] = 0x80
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(trailer)))
	copy(frame[5:], trailer)
	if _, _, err := grokParseBilling(frame); err == nil {
		t.Fatal("non-zero grpc-status must be an error")
	}
}

func TestGrokResetPicksSoonestFuture(t *testing.T) {
	// Two varints in the epoch band; the one still in the future wins.
	future := uint64(time.Now().Add(48 * time.Hour).Unix())
	past := uint64(1_700_000_100)
	payload := append(protoVarint(6, past), protoVarint(6, future)...)
	// also a valid used% so the scan yields something
	payload = append(payload, protoFixed32(1, 10)...)
	_, resets, err := grokParseBilling(grokFrame(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resets == nil || uint64(*resets) != future {
		t.Fatalf("reset = %v, want %d", resets, future)
	}
}

func protoVarint(field int, v uint64) []byte {
	out := []byte{byte(field<<3 | 0)}
	var buf [10]byte
	n := binary.PutUvarint(buf[:], v)
	return append(out, buf[:n]...)
}

// ── Wayfinder ─────────────────────────────────────────────────────────────

func TestWayfinderBaseRejectsNonLoopback(t *testing.T) {
	t.Setenv(wayfinderURLEnv, "")
	if got := wayfinderBase(); got != wayfinderDefault {
		t.Fatalf("empty env should give default, got %q", got)
	}
	t.Setenv(wayfinderURLEnv, "http://10.0.0.5:8088")
	if got := wayfinderBase(); got != "" {
		t.Fatalf("remote plain HTTP must be refused, got %q", got)
	}
	t.Setenv(wayfinderURLEnv, "http://127.0.0.1:9000")
	if got := wayfinderBase(); got != "http://127.0.0.1:9000" {
		t.Fatalf("loopback HTTP should pass, got %q", got)
	}
	t.Setenv(wayfinderURLEnv, "https://gw.example.com")
	if got := wayfinderBase(); got != "https://gw.example.com" {
		t.Fatalf("HTTPS should pass, got %q", got)
	}
}
