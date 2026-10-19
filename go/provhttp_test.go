package main

// The shared provider cache: the floor, the stale window, and the backoff a
// 429 earns.

import "testing"

func TestProvCacheBacksOffAfterRateLimit(t *testing.T) {
	// The floors are a reader's: the daemon asks only when pressed (pace.go).
	paceReader(t)
	c := &provCache{}
	calls := 0
	limited := func() Provider {
		calls++
		p := Provider{ID: "x"}
		p.failWith("rate-limited", "http-429")
		return p
	}
	// The floor is on attempts; the backoff is on top of it.
	c.collect(limited)
	if calls != 1 {
		t.Fatalf("first call: %d", calls)
	}
	// Inside the first pause (60s) nothing is asked again.
	c.attemptedAt -= provMinInterval + 1
	c.collect(limited)
	if calls != 1 {
		t.Errorf("asked again inside the backoff: %d calls", calls)
	}
	// Past the pause: one more ask, and the pause doubles.
	c.coolUntil = 0
	c.collect(limited)
	if calls != 2 || c.strikes != 2 {
		t.Errorf("after the pause: calls=%d strikes=%v", calls, c.strikes)
	}
	if want := provBackoffBase * 2; c.coolUntil-now() < want-5 || c.coolUntil-now() > want+5 {
		t.Errorf("second pause should be ~%vs, got %vs", want, c.coolUntil-now())
	}
	// A success clears it all.
	c.coolUntil, c.attemptedAt = 0, 0
	c.collect(func() Provider { calls++; return Provider{ID: "x", OK: true} })
	if c.strikes != 0 || c.coolUntil != 0 || c.fail != nil {
		t.Errorf("success must clear the backoff: %+v", c)
	}
}
