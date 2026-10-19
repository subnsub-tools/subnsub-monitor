package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tailscale.com/types/key"
)

func TestFrameRoundTrip(t *testing.T) {
	f, err := New(TypeHello, "abc123", "corr-1", Hello{Caps: []string{CapObserve}, Carrier: "tailcat"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encode(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != Version || got.Type != TypeHello || got.AgentID != "abc123" || got.ID != "corr-1" {
		t.Fatalf("envelope changed in flight: %+v", got)
	}
	var h Hello
	if err := json.Unmarshal(got.Body, &h); err != nil {
		t.Fatal(err)
	}
	if len(h.Caps) != 1 || h.Caps[0] != CapObserve || h.Carrier != "tailcat" {
		t.Fatalf("body changed in flight: %+v", h)
	}
}

// A type this build has not heard of is refused, whichever side it is on.
// This is plan invariant 3 in executable form: an old helper must never take
// a frame it does not understand and do something with it.
func TestUnknownTypeIsRefusedBothWays(t *testing.T) {
	if _, err := New("exec", "a", "b", nil); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("New accepted an unknown type: %v", err)
	}
	if _, err := Encode(Frame{V: Version, Type: "exec"}); !errors.Is(err, ErrUnknownType) {
		t.Fatalf("Encode accepted an unknown type: %v", err)
	}
	_, err := Decode([]byte(`{"v":1,"type":"exec","body":{"cmd":"rm -rf /"}}`))
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("Decode accepted an unknown type: %v", err)
	}
}

func TestUnknownVersionIsRefused(t *testing.T) {
	_, err := Decode([]byte(`{"v":2,"type":"hello","body":{}}`))
	if !errors.Is(err, ErrVersion) {
		t.Fatalf("want ErrVersion, got %v", err)
	}
	_, err = Decode([]byte(`{"type":"hello","body":{}}`))
	if !errors.Is(err, ErrVersion) {
		t.Fatalf("a frame with no version must not pass as v1: %v", err)
	}
}

// The ceiling is per type and applies to the serialised bytes, and it is
// checked before the body is parsed. A hello the size of a snapshot is not a
// hello.
func TestPerTypeLimits(t *testing.T) {
	big := strings.Repeat("x", Limits[TypeHello])
	_, err := Decode([]byte(`{"v":1,"type":"hello","body":{"pad":"` + big + `"}}`))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized hello passed: %v", err)
	}
	// The same bytes under a type with a larger ceiling are fine — the limit
	// is the type's, not a global one.
	if _, err := Decode([]byte(`{"v":1,"type":"snapshot","body":{"pad":"` + big + `"}}`)); err != nil {
		t.Fatalf("snapshot-sized frame refused: %v", err)
	}
	// And Encode enforces the same rule on the way out.
	f, _ := New(TypeHello, "", "", map[string]string{"pad": big})
	if _, err := Encode(f); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Encode let an oversized hello out: %v", err)
	}
	// Above every ceiling: refused before any type is even read.
	huge := strings.Repeat("x", maxEnvelope+1)
	if _, err := Decode([]byte(huge)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over-envelope bytes were parsed: %v", err)
	}
}

func TestMalformedIsMalformed(t *testing.T) {
	for _, in := range []string{``, `{`, `[]`, `"hello"`, `{"v":"1","type":"hello"}`} {
		_, err := Decode([]byte(in))
		if err == nil {
			t.Fatalf("%q decoded", in)
		}
	}
}

// Unknown FIELDS are tolerated — an envelope that refused them could never
// grow — while unknown TYPES are not. Both halves in one test so nobody
// "fixes" one by breaking the other.
func TestUnknownFieldsTolerated(t *testing.T) {
	f, err := Decode([]byte(`{"v":1,"type":"system","agent_id":"m1","future":true,"body":{"cpu_percent":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if f.AgentID != "m1" {
		t.Fatalf("agent id lost: %+v", f)
	}
}

func TestFakePairCarriesFramesInOrderAndStampsIdentity(t *testing.T) {
	a, b := NewFakePair(4)
	ctx := context.Background()
	if err := a.Send(ctx, Frame{V: Version, Type: TypeHello}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Send before Start: %v", err)
	}
	if err := a.Start(ctx, Identity{AgentID: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(ctx, Identity{AgentID: "beta"}); err != nil {
		t.Fatal(err)
	}
	for i, typ := range []string{TypeHello, TypeSystem, TypeSnapshot} {
		f, _ := New(typ, "forged", "id-"+string(rune('a'+i)), nil)
		if err := a.Send(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	rx, err := b.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{TypeHello, TypeSystem, TypeSnapshot} {
		select {
		case f := <-rx:
			if f.Type != want {
				t.Fatalf("out of order: got %s want %s", f.Type, want)
			}
			if f.AgentID != "alpha" {
				t.Fatalf("carrier let a frame claim to be from %q", f.AgentID)
			}
		case <-time.After(time.Second):
			t.Fatal("frame never arrived")
		}
	}
	if n := len(a.Sent()); n != 3 {
		t.Fatalf("recorder holds %d frames, want 3", n)
	}
	a.Close()
	a.Close() // idempotent
	if _, ok := <-rx; ok {
		t.Fatal("peer did not see end-of-stream after Close")
	}
	if err := a.Send(ctx, Frame{V: Version, Type: TypeHello}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close: %v", err)
	}
}

// Backpressure is a blocking Send, not a growing buffer: with the queue full
// and nobody reading, Send waits for the context and returns its error.
func TestFakeSendBlocksWhenPeerStopsReading(t *testing.T) {
	a, b := NewFakePair(1)
	ctx := context.Background()
	a.Start(ctx, Identity{AgentID: "a"})
	b.Start(ctx, Identity{AgentID: "b"})
	f, _ := New(TypeSystem, "", "", nil)
	if err := a.Send(ctx, f); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := a.Send(short, f); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Send into a full queue: %v (want deadline)", err)
	}
}

// The fake refuses what the wire would refuse, so a test that passes here
// is not passing on a frame the real carrier would drop.
func TestFakeRefusesOversized(t *testing.T) {
	a, b := NewFakePair(1)
	ctx := context.Background()
	a.Start(ctx, Identity{AgentID: "a"})
	b.Start(ctx, Identity{AgentID: "b"})
	f, _ := New(TypeHello, "", "", map[string]string{"pad": strings.Repeat("x", Limits[TypeHello])})
	if err := a.Send(ctx, f); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("fake carried an oversized frame: %v", err)
	}
}

// ── tunnel identity ─────────────────────────────────────────────────

func TestKeyPersistsAndRefusesLooseModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	k1, err := loadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := loadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !k1.Equal(k2) {
		t.Fatal("key changed between two loads — that is an identity rotation")
	}
	fi, _ := os.Stat(filepath.Join(dir, nodeKeyFile))
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("key file is %v, want owner-only", fi.Mode().Perm())
	}
	// A key file other users can read is refused, not used, and NOT replaced.
	os.Chmod(filepath.Join(dir, nodeKeyFile), 0o644)
	if _, err := loadOrCreateKey(dir); err == nil {
		t.Fatal("world-readable key was accepted")
	}
	os.Chmod(filepath.Join(dir, nodeKeyFile), 0o600)
	k3, err := loadOrCreateKey(dir)
	if err != nil || !k3.Equal(k1) {
		t.Fatalf("refusal must not have rotated the key: %v", err)
	}
	// A corrupt file is corrupt, not "no key": creation must not run.
	os.WriteFile(filepath.Join(dir, nodeKeyFile), []byte("privkey:zz\n"), 0o600)
	if _, err := loadOrCreateKey(dir); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt key: %v", err)
	}
	// A directory wider than 0700 — every pre-existing install's config dir,
	// made by the installer under a 022 umask — is tightened, not refused.
	wide := filepath.Join(t.TempDir(), "wide")
	os.MkdirAll(wide, 0o755)
	if _, err := loadOrCreateKey(wide); err != nil {
		t.Fatalf("0755 state dir was not tightened: %v", err)
	}
	if fi, _ := os.Stat(wide); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state dir left at %v", fi.Mode().Perm())
	}
}

func TestEncodeRefusesWrongVersion(t *testing.T) {
	for _, v := range []int{0, 2} {
		if _, err := Encode(Frame{V: v, Type: TypeHello}); !errors.Is(err, ErrVersion) {
			t.Fatalf("Encode accepted v=%d: %v", v, err)
		}
	}
}

// Close under a Send blocked on backpressure must end that Send with
// ErrClosed, never panic on a channel closed beneath it.
func TestFakeCloseDuringBlockedSend(t *testing.T) {
	a, b := NewFakePair(1)
	ctx := context.Background()
	a.Start(ctx, Identity{AgentID: "a"})
	b.Start(ctx, Identity{AgentID: "b"})
	f, _ := New(TypeSystem, "", "", nil)
	if err := a.Send(ctx, f); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- a.Send(ctx, f) }() // blocks: queue full, nobody reading
	time.Sleep(30 * time.Millisecond)
	a.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("blocked Send ended with %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Send never returned after Close")
	}
}

func TestRegionPinRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if r := readRegion(dir); r != 0 {
		t.Fatalf("fresh dir pinned %d", r)
	}
	if err := writeRegion(dir, 302); err != nil {
		t.Fatal(err)
	}
	if r := readRegion(dir); r != 302 {
		t.Fatalf("pin read back as %d", r)
	}
	os.WriteFile(filepath.Join(dir, regionFile), []byte("garbage\n"), 0o600)
	if r := readRegion(dir); r != 0 {
		t.Fatalf("garbage pin read as %d", r)
	}
}

func TestParseNodeKey(t *testing.T) {
	priv := key.NewNode()
	text, _ := priv.Public().MarshalText()
	pub := string(text)
	for _, ok := range []string{pub, "  " + pub + "  ", pub + " # laptop", pub + "\tcomment"} {
		k, good := ParseNodeKey(ok)
		if !good || k != priv.Public() {
			t.Fatalf("%q did not parse", ok)
		}
	}
	for _, bad := range []string{"", "# " + pub, "nodekey:", pub[:len(pub)-2], "privkey:" + pub[len("nodekey:"):],
		"nodekey:" + strings.Repeat("0", 64)} {
		if _, good := ParseNodeKey(bad); good {
			t.Fatalf("%q parsed", bad)
		}
	}
}

// The gate attributes a request to a key by the address the tunnel gives
// that key. The derivation is pinned here against the documented layout so
// an upstream change is caught by a failing test, not by every watcher being
// refused at the door.
func TestAddrForKeyLayout(t *testing.T) {
	priv := key.NewNode()
	pub := priv.Public()
	a := addrForKey(pub)
	if !a.Is6() || !strings.HasPrefix(a.String(), "fd7a:115c:a1e0:") {
		t.Fatalf("unexpected address %s", a)
	}
	raw := pub.Raw32()
	b := a.As16()
	for i := 0; i < 10; i++ {
		if b[6+i] != raw[i] {
			t.Fatalf("byte %d of the address is not the key's", i)
		}
	}
	// Two keys, two addresses.
	if addrForKey(key.NewNode().Public()) == a {
		t.Fatal("distinct keys share an address")
	}
}

func TestPeerKeyConsultsCurrentAllowlist(t *testing.T) {
	k1, k2 := key.NewNode().Public(), key.NewNode().Public()
	current := []key.NodePublic{k1}
	tc := &Tailcat{cfg: TailcatConfig{Allowed: func() []key.NodePublic { return current }}}
	remote := netip.AddrPortFrom(addrForKey(k1), 41234).String()
	if got, ok := tc.PeerKey(remote); !ok || got != k1 {
		t.Fatal("allowed peer not recognised")
	}
	if _, ok := tc.PeerKey(netip.AddrPortFrom(addrForKey(k2), 1).String()); ok {
		t.Fatal("unlisted peer recognised")
	}
	// Struck from the list → refused on the next look, no restart involved.
	current = nil
	if _, ok := tc.PeerKey(remote); ok {
		t.Fatal("revoked peer still recognised")
	}
	if _, ok := tc.PeerKey("not-an-address"); ok {
		t.Fatal("garbage address recognised")
	}
}
