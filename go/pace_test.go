package main

// Quota pacing: a machine is on manual unless it is told, recently and in so
// many words, that it is watched and set to refresh on its own. Manual asks
// when a refresh is pressed and never on a timer; the other asks on the long
// floor. A press never asks twice inside the gap.

import (
	"net/http/httptest"
	"testing"
)

// Each test puts the pacing state where it wants it and restores it after,
// because the state is package-level and the other cache tests run as a
// reader (see paceReader).
func paceReset(t *testing.T) {
	t.Helper()
	pace.Lock()
	saved := struct {
		at      float64
		streams int
		always  bool
		gen     uint64
		one     map[string]uint64
	}{pace.watchedAt, pace.streams, pace.always, pace.forceGen, pace.forceOne}
	pace.Unlock()
	t.Cleanup(func() {
		pace.Lock()
		pace.watchedAt, pace.streams, pace.always, pace.forceGen = saved.at, saved.streams, saved.always, saved.gen
		pace.forceOne = saved.one
		pace.Unlock()
	})
}

// paceDaemon is the state `connect` starts in: not a reader, told nothing.
func paceDaemon(t *testing.T) {
	t.Helper()
	paceReset(t)
	pace.Lock()
	pace.always = false
	pace.watchedAt = 0
	pace.streams = 0
	pace.Unlock()
}

// paceReader is the state the CLI modes run in, for the cache tests that
// exercise the floors themselves.
func paceReader(t *testing.T) {
	t.Helper()
	paceReset(t)
	paceAlways()
}

func setWatchedAgo(secs float64) {
	pace.Lock()
	pace.watchedAt = now() - secs
	pace.always = false
	pace.Unlock()
}

// A process that has heard nothing is on manual: the default, not a mode
// somebody has to switch on.
func TestPaceManualIsTheDefault(t *testing.T) {
	paceDaemon(t)
	if watched() {
		t.Fatal("a daemon told nothing must not count as watched")
	}
	c := &provCache{}
	calls := 0
	ok := func() Provider { calls++; return Provider{ID: "x", OK: true} }
	// Nothing cached: asked once, so the tile is never blank for want of it.
	c.collect(ok)
	if calls != 1 {
		t.Fatalf("empty cache must ask once: %d", calls)
	}
	// A reading a day old and no press: served as-is, no call — past the
	// floor, past the retry floor, past the stale window.
	c.fetchedAt -= 86400
	c.attemptedAt -= 86400
	for i := 0; i < 3; i++ {
		if p := c.collect(ok); calls != 1 || !p.OK {
			t.Fatalf("manual must serve the stale reading: calls=%d ok=%v", calls, p.OK)
		}
	}
}

func TestPaceWatchedAsksOnTheFloor(t *testing.T) {
	paceDaemon(t)
	c := &provCache{}
	calls := 0
	ok := func() Provider { calls++; return Provider{ID: "x", OK: true} }
	c.collect(ok)
	c.fetchedAt -= 86400
	c.attemptedAt -= 86400
	// Told it is watched and set to refresh on its own: past the floor, so
	// the next collection asks.
	noteWatched(true)
	c.collect(ok)
	if calls != 2 {
		t.Fatalf("watched and past the floor must ask: %d", calls)
	}
	// Inside the floor while watched: served.
	c.collect(ok)
	if calls != 2 {
		t.Errorf("inside the floor must serve: %d", calls)
	}
	// The retry floor still guards a failing vendor while watched.
	c.fetchedAt -= quotaFloor + 1
	c.attemptedAt = now()
	c.collect(ok)
	if calls != 2 {
		t.Errorf("inside the retry floor must serve: %d", calls)
	}
	// The word wears off when the relay goes quiet...
	c.attemptedAt -= quotaFloor + 1
	setWatchedAgo(quotaWatchWindow + 1)
	c.collect(ok)
	if calls != 2 {
		t.Errorf("a word older than the window must not ask: %d", calls)
	}
	// ...and is taken back at once when the relay says otherwise: switched
	// to manual ten seconds before the floor, nothing is asked after it.
	noteWatched(true)
	noteWatched(false)
	c.collect(ok)
	if calls != 2 || watched() {
		t.Errorf("a withdrawn word must not ask: calls=%d watched=%v", calls, watched())
	}
}

// On its own the daemon tries a vendor once per floor, failed or not; the
// two-minute retry floor is a reader's.
func TestPaceWatchedRetriesOnTheFloor(t *testing.T) {
	paceDaemon(t)
	noteWatched(true)
	c := &provCache{}
	calls := 0
	bad := func() Provider { calls++; p := Provider{ID: "x"}; p.failWith("api-error", "http-500"); return p }
	c.collect(bad)
	c.attemptedAt -= provMinInterval + 1
	c.collect(bad)
	if calls != 1 {
		t.Fatalf("a failure must not be retried inside the floor while watched: %d", calls)
	}
	c.attemptedAt -= quotaFloor
	c.collect(bad)
	if calls != 2 {
		t.Errorf("past the floor the failure is tried again: %d", calls)
	}
	// A reader keeps the short retry floor.
	paceAlways()
	c.attemptedAt -= provMinInterval + 1
	c.collect(bad)
	if calls != 3 {
		t.Errorf("a reader retries on the retry floor: %d", calls)
	}
}

// Only a readable `watch: true` counts. Everything else — the field absent,
// false, or a body that cannot be read — leaves the machine on manual.
func TestPaceHintWatchBit(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"ok":true,"poll":false,"watch":true}`, true},
		{`{"ok":true,"watch":true}`, true},
		{`{"ok":true,"poll":false,"watch":false}`, false},
		{`{"ok":true,"poll":true}`, false},
		{`{"ok":true}`, false},
		{`{"ok":true,"watch":"true"}`, false},
		{`{"ok":true,"watch":1}`, false},
		{`not json`, false},
		{``, false},
	} {
		// On a machine that takes no instructions (nil box) and on one that
		// does: the bit is read the same way on both.
		for _, b := range []*hintBox{nil, newHintBox()} {
			paceDaemon(t)
			b.absorb([]byte(tc.body))
			if got := watched(); got != tc.want {
				t.Errorf("absorb(%q) box=%v: watched=%v, want %v", tc.body, b != nil, got, tc.want)
			}
			// And after a yes: only another yes keeps it.
			noteWatched(true)
			b.absorb([]byte(tc.body))
			if got := watched(); got != tc.want {
				t.Errorf("absorb(%q) after a yes, box=%v: watched=%v, want %v", tc.body, b != nil, got, tc.want)
			}
		}
	}
	// The poll bit is still filed beside it.
	paceDaemon(t)
	b := newHintBox()
	b.absorb([]byte(`{"ok":true,"poll":false,"watch":false}`))
	if watched() || !b.signalMode() {
		t.Error("watch:false with a poll bit: not watched, and the poll bit still filed")
	}
	b.absorb([]byte(`not json`))
	if watched() || b.signalMode() {
		t.Error("an unreadable body must read as manual and legacy polling")
	}
}

// The tunnel road: a reader counts as a watcher only when it says the
// machine is set to refresh on its own, in exactly those words.
func TestPaceTunnelReaderMustSayAuto(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   bool
	}{
		{"/v1/events?quota=auto", true},
		{"/v1/events?quota=auto&x=1", true},
		{"/v1/events", false},
		{"/v1/snapshot?fresh=1", false},
		{"/v1/events?quota=manual", false},
		{"/v1/events?quota=1", false},
		{"/v1/events?quota=AUTO", false},
		{"/v1/events?quota=", false},
	} {
		if got := quotaAutoAsked(httptest.NewRequest("GET", tc.target, nil)); got != tc.want {
			t.Errorf("quotaAutoAsked(%s) = %v, want %v", tc.target, got, tc.want)
		}
	}
	// A stream that said so counts for as long as it is open, and each
	// takes back only its own word.
	paceDaemon(t)
	closeA := streamWatched()
	closeB := streamWatched()
	if !watched() {
		t.Fatal("an open auto stream must count as watched")
	}
	closeA()
	closeA() // a second close of the same stream takes nothing more
	if !watched() {
		t.Error("the other stream is still open")
	}
	closeB()
	if watched() {
		t.Error("with every auto stream closed the machine is on manual")
	}
	// The relay taking its word back does not close a tunnel reader's.
	closeC := streamWatched()
	noteWatched(false)
	if !watched() {
		t.Error("the relay's no must not end a tunnel stream's yes")
	}
	closeC()
}

func TestPaceManualFailureHeld(t *testing.T) {
	paceDaemon(t)
	c := &provCache{}
	calls := 0
	bad := func() Provider { calls++; p := Provider{ID: "x"}; p.failWith("api-error", "http-500"); return p }
	c.collect(bad)
	c.attemptedAt -= 86400
	p := c.collect(bad)
	if calls != 1 || p.OK || p.Error != "api-error" {
		t.Errorf("manual must hold the last refusal: calls=%d %+v", calls, p)
	}
}

func TestPaceForceAsksOncePerPress(t *testing.T) {
	paceDaemon(t)
	c := &provCache{}
	calls := 0
	ok := func() Provider { calls++; return Provider{ID: "x", OK: true} }
	c.collect(ok) // the first, empty-cache ask
	quotaForce()
	c.collect(ok)
	if calls != 2 {
		t.Fatalf("a press must ask, on manual and inside the floor: %d", calls)
	}
	// The same press again: consumed, not repeated.
	c.collect(ok)
	if calls != 2 {
		t.Errorf("one press is one ask: %d", calls)
	}
	// A second press inside the gap is dropped, not banked.
	quotaForce()
	c.collect(ok)
	if calls != 2 {
		t.Errorf("a press inside the gap must be dropped: %d", calls)
	}
	c.pace.forcedAt -= quotaForceGap + 1
	c.collect(ok)
	if calls != 2 {
		t.Errorf("a dropped press must not fire later on its own: %d", calls)
	}
	// Past the gap, a new press asks again.
	quotaForce()
	c.collect(ok)
	if calls != 3 {
		t.Errorf("a press past the gap must ask: %d", calls)
	}
}

// A press on one provider's own tile asks that provider and no other; the
// machine's button still asks them all, and neither press hides the other.
func TestPaceForceOneAsksOnlyThatProvider(t *testing.T) {
	paceDaemon(t)
	pace.Lock()
	pace.forceOne, pace.viaCPA = nil, nil
	pace.Unlock()
	a := &provCache{pace: paceMark{id: "claude"}}
	b := &provCache{pace: paceMark{id: "claude@2"}}
	var na, nb int
	fa := func() Provider { na++; return Provider{ID: "claude", OK: true} }
	fb := func() Provider { nb++; return Provider{ID: "claude", OK: true} }
	a.collect(fa) // the first, empty-cache asks
	b.collect(fb)
	quotaForceFor("claude@2")
	a.collect(fa)
	b.collect(fb)
	if na != 1 || nb != 2 {
		t.Fatalf("a press on one tile must ask that provider alone: a=%d b=%d", na, nb)
	}
	// Consumed, not repeated.
	b.collect(fb)
	if nb != 2 {
		t.Errorf("one press is one ask: %d", nb)
	}
	// The machine's button reaches both, the one just pressed past its gap.
	b.pace.forcedAt -= quotaForceGap + 1
	quotaForceFor("")
	a.collect(fa)
	b.collect(fb)
	if na != 2 || nb != 3 {
		t.Errorf("the machine's press must ask every provider: a=%d b=%d", na, nb)
	}
	// Inside its own gap a provider drops its press; the other is untouched.
	quotaForceFor("claude@2")
	a.collect(fa)
	b.collect(fb)
	if na != 2 || nb != 3 {
		t.Errorf("a press inside the gap must be dropped: a=%d b=%d", na, nb)
	}
}

// A tile CPA fills has the vendor's native collector behind it under the same
// id: a press on the tile asks the CPA credential and not the hidden account.
// And a credential renumbered to a new id does not take the old count for a
// press.
func TestPaceForceOneBehindCPA(t *testing.T) {
	paceDaemon(t)
	pace.Lock()
	pace.forceOne, pace.viaCPA = nil, nil
	pace.Unlock()
	t.Cleanup(func() { noteCPATiles(nil) })
	native := &provCache{pace: paceMark{id: "claude"}}
	first, second := &provCache{}, &provCache{}
	var nn, n1, n2 int
	fn := func() Provider { nn++; return Provider{ID: "claude", OK: true} }
	f1 := func() Provider { n1++; return Provider{ID: "claude", OK: true} }
	f2 := func() Provider { n2++; return Provider{ID: "claude", OK: true} }
	native.collect(fn)
	first.collectAs(cpaMark("claude"), f1)
	second.collectAs(cpaMark("claude@2"), f2)
	noteCPATiles([]string{"claude", "claude@2"})
	quotaForceFor("claude")
	native.collect(fn)
	first.collectAs(cpaMark("claude"), f1)
	second.collectAs(cpaMark("claude@2"), f2)
	if nn != 1 || n1 != 2 || n2 != 1 {
		t.Fatalf("a press on a CPA tile must ask that credential alone: native=%d first=%d second=%d", nn, n1, n2)
	}
	// The first account goes; the second becomes `claude`, whose count is 1
	// where this cache has honoured 0 of them.
	second.collectAs(cpaMark("claude"), f2)
	if n2 != 1 {
		t.Errorf("a renumbered credential must not ask on its own: %d", n2)
	}
	// CPA gone from the reading: the press is the native collector's again.
	noteCPATiles(nil)
	quotaForceFor("claude")
	native.collect(fn)
	if nn != 2 {
		t.Errorf("with no CPA tile the press must reach the native collector: %d", nn)
	}
}

func TestForceTarget(t *testing.T) {
	for _, ok := range []string{"claude", "codex@2", "kilo-code", "a_b.c@12"} {
		if !forceTarget(ok) {
			t.Errorf("%q names a tile", ok)
		}
	}
	for _, bad := range []string{"", "Claude", "claude@", "@2", "claude@x", "claude@2@3", "a b", "a/b", "claude@2x",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if forceTarget(bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestPaceForceRespectsBackoff(t *testing.T) {
	paceDaemon(t)
	c := &provCache{}
	calls := 0
	limited := func() Provider {
		calls++
		p := Provider{ID: "x"}
		p.failWith("rate-limited", "http-429")
		return p
	}
	c.collect(limited)
	quotaForce()
	c.collect(limited)
	if calls != 1 {
		t.Errorf("a press must not poke a vendor inside its 429 backoff: %d", calls)
	}
	// ...and is not banked: when the backoff ends, the press made during it
	// must not fire on its own.
	c.coolUntil = 0
	c.attemptedAt -= provMinInterval + 1
	c.collect(limited)
	if calls != 1 {
		t.Errorf("a press consumed by the backoff must not fire later: %d", calls)
	}
}

func TestPaceAlways(t *testing.T) {
	paceDaemon(t)
	if watched() {
		t.Fatal("precondition")
	}
	paceAlways()
	if !watched() {
		t.Error("a CLI reader is always watched")
	}
}
