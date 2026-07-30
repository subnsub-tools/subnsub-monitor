package main

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"
)

// A token of the real shape, expiring when asked. Only the expiry field is ever
// read by this package, so the rest is filler — but the LENGTH is not filler:
// tokenExpiry refuses anything that is not exactly 44 bytes, which is what
// keeps a 43-byte self-minted token from being read as a signed one whose
// expiry happens to land somewhere plausible.
func fakeToken(exp time.Time) string {
	raw := make([]byte, tokenBytes)
	for i := range raw {
		raw[i] = byte(i)
	}
	binary.BigEndian.PutUint32(raw[tokenExpOff:tokenExpOff+4], uint32(exp.Unix()))
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestTokenExpiryReadsTheSignedLayout(t *testing.T) {
	want := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	got, ok := tokenExpiry(fakeToken(want))
	if !ok || !got.Equal(want) {
		t.Fatalf("tokenExpiry = %v, %v; want %v, true", got, ok, want)
	}
}

func TestTokenExpiryRefusesUnsignedAndJunk(t *testing.T) {
	// The pre-signing token: 32 random bytes, 43 base64url characters. It has
	// no expiry, and reading one out of its bytes would schedule a renewal that
	// can never succeed and, worse, could be read as "already expired".
	legacy := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if len(legacy) != legacyTokLen {
		t.Fatalf("legacy token is %d chars, expected %d", len(legacy), legacyTokLen)
	}
	for _, tok := range []string{legacy, "", "not base64!!", base64.RawURLEncoding.EncodeToString(make([]byte, 45))} {
		if _, ok := tokenExpiry(tok); ok {
			t.Errorf("tokenExpiry(%q) reported an expiry it should not have", tok)
		}
	}
	// All-zero expiry is a token that never carried one, not one that expired
	// in 1970 — the difference decides between "park it" and "renew forever".
	if _, ok := tokenExpiry(fakeToken(time.Unix(0, 0))); ok {
		t.Error("a zero expiry should not read as an expiry")
	}
}

func TestCleanTokenHoldsTheRelayAlphabet(t *testing.T) {
	good := fakeToken(time.Now())
	if cleanToken("  "+good+"\n") != good {
		t.Error("surrounding whitespace should be trimmed, not fatal — the file is written with a newline")
	}
	for _, bad := range []string{"short", "has space in it aaaaaaaaaaaaaaaaaaaaa", "line\nbreak_aaaaaaaaaaaaaaaaaaaa", "plus+slash/aaaaaaaaaaaaaaaaaaaa"} {
		if cleanToken(bad) != "" {
			t.Errorf("cleanToken(%q) accepted a value the relay would refuse", bad)
		}
	}
}

func TestPickTokenPrefersTheLongerLife(t *testing.T) {
	now := time.Now()
	old := fakeToken(now.Add(2 * 24 * time.Hour))
	fresh := fakeToken(now.Add(29 * 24 * time.Hour))

	if got := pickToken(old, fresh); got != fresh {
		t.Error("a renewed token on disk should supersede the installed one")
	}
	// The reinstall case, and the reason this is not simply "the file wins":
	// pasting a new token from the panel writes it to the environment, and if
	// the stale file outranked it, reinstalling would silently do nothing.
	if got := pickToken(fresh, old); got != fresh {
		t.Error("a freshly installed token should override an older renewed one")
	}
	if got := pickToken(old, ""); got != old {
		t.Error("no file means the installed token")
	}
	if got := pickToken("", fresh); got != fresh {
		t.Error("no environment token means the file")
	}
	if got := pickToken(old, "garbage\x00"); got != old {
		t.Error("an unusable file must not displace a working token")
	}
	// An unsigned environment token works through the relay's allow-list, which
	// a renewed token would not be on — a dated file must not push it aside.
	legacy := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if got := pickToken(legacy, fresh); got != legacy {
		t.Error("a signed file token must not displace an unsigned allow-listed one")
	}
}

func TestRenewerWaitsUntilTheLeadWindow(t *testing.T) {
	now := time.Now()
	r := newRenewer()

	far := fakeToken(now.Add(20 * 24 * time.Hour))
	if r.due(far, now, false) {
		t.Error("a token with 20 days left should not be renewed yet")
	}
	near := fakeToken(now.Add(renewLead - time.Hour))
	if !r.due(near, now, false) {
		t.Error("a token inside the lead window should be renewed")
	}
}

func TestRenewerBackoffAlsoBindsForcedAttempts(t *testing.T) {
	now := time.Now()
	r := newRenewer()
	tok := fakeToken(now.Add(20 * 24 * time.Hour))

	// A relay 403 forces an attempt even though the token looks healthy — that
	// is the point of `forced`.
	if !r.due(tok, now, true) {
		t.Fatal("the first forced attempt should go through")
	}
	// …but once an attempt has failed and set a backoff, forcing must NOT get
	// past it. Without this a relay answering 403 for a reason renewal cannot
	// fix (a room that was never enabled, a genuinely lapsed subscription)
	// means one renewal request per push, forever.
	r.next = now.Add(renewRetryMin)
	if r.due(tok, now, true) {
		t.Error("a forced attempt must still respect the backoff")
	}
	if r.due(tok, now.Add(renewRetryMin+time.Second), true) {
		return // backoff elapsed, forcing works again
	}
	t.Error("after the backoff elapses, forcing should work again")
}

func TestRenewerParksTokensItCannotRenew(t *testing.T) {
	now := time.Now()
	r := newRenewer()
	legacy := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	if r.due(legacy, now, false) {
		t.Error("a token with no expiry has nothing to renew from")
	}
	// And it must not be re-asked on the next push: with no expiry there is no
	// window to compute, so the answer would be the same 2,880 times a day.
	if r.next.IsZero() || !r.next.After(now) {
		t.Error("an unrenewable token should be parked, not re-evaluated every push")
	}
}
