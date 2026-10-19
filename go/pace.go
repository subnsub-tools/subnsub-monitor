package main

// Quota pacing: a vendor is asked when a person asks — or, on a machine whose
// owner chose that, every ten minutes while its dashboard is open.
//
// Every remote collector here used to run on its own two-minute floor, and
// the push loop ran them whether or not anyone had a dashboard open — 720
// calls a day per vendor per machine, at the same minute marks, for a number
// that moves by the hour. Enough of that from one account and the vendor
// stops answering (Claude returned 429 more than once; a limit that would
// not clear while the loop kept trying, 2026-09-09). CLI Proxy API's own
// console asks only when a person presses its button, which is the model
// the operator asked for (2026-09-12).
//
// The first build of this file took two departures from that model for
// every machine: an open dashboard counted as a person looking, and a
// watched machine asked on a ten-minute floor. A dashboard left open in a
// tab kept every vendor on that timer for as long as the tab lived, and the
// operator had not asked for it (operator ruling 2026-09-28: even while being watched, no
// automatic checks — manual refresh only). So manual is what a machine does unless told
// otherwise, and the floor is a choice made per machine in its card's
// Settings (operator ruling 2026-09-28: each machine is set on its own —
// refresh every ten minutes while open, or by hand).
//
// Three inputs and one question. "Forced" is a refresh pressed on the page,
// which asks once now but never twice inside quotaForceGap. "Watched" is
// the word that a dashboard is open AND this machine is set to refresh on
// its own, kept per road because each road can take it back: the relay
// says so on a push response (`watch`, see hint.go — it holds the setting
// and knows who is looking), good for quotaWatchWindow and withdrawn by
// the first response that does not say it; a tunnel reader says so by
// opening its stream with ?quota=auto, good for exactly as long as that
// stream is open. "Reader" is a process that is
// itself the person's request — the one-shot CLI, `watch`, `serve`, run by
// hand at the machine. The question every cache asks is due(): forced, else
// watched or a reader, and past the floor. A cache that is not due serves
// its last reading whatever its age — recorded_at says when the numbers
// were fetched, so a reading nobody refreshed visibly ages rather than
// lying about freshness.
//
// SILENCE IS MANUAL. A relay that does not send the field, a response that
// cannot be read, a tunnel stream opened without the parameter: none of
// them is a yes. Until 2026-09-28 it was the other way round — unknown
// counted as watched, to keep an older relay on its old behaviour — and
// that is exactly how a machine nobody had chosen anything for ended up on
// a timer. The word can still only buy the floor, never anything faster —
// and on the floor an ATTEMPT counts like a success: a vendor that fails is
// asked again in ten minutes, not in the two the retry floor allows a
// reader (review 2026-09-28).
//
// What is left that nobody asked for: a cache holding nothing at all still
// asks once (see provCache.collect), so that is one call per provider per
// process start.
//
// Local collectors — Codex's log, Antigravity's language server, CPA's own
// credential listing — are not paced here: they cost nothing anyone meters.

import (
	"sync"
)

const (
	// Seconds between two real vendor calls while watched, or in a reader.
	// The dashboard shows the reading's age; ten minutes of age on a number
	// that moves by the hour is honest.
	quotaFloor = 600.0
	// How long the relay's "watched" is good for with no further word — a
	// push that failed outright, a relay gone quiet. Longer than the slowest
	// push cadence (60s) with room for a lost response. A response that
	// arrives and does not say it ends it at once.
	quotaWatchWindow = 180.0
	// The least time between two FORCED asks of one provider. A button that
	// can be pressed faster than a vendor answers is the loop this file
	// exists to stop, with a person's finger for a timer.
	quotaForceGap = 30.0
)

var pace struct {
	sync.Mutex
	// The newest moment the relay said this machine is watched and set to
	// refresh on its own. Zero until it says so, and again as soon as it
	// says anything else: a process that has heard nothing is on manual.
	watchedAt float64
	// Tunnel streams open right now that were opened with ?quota=auto.
	streams int
	// This process is the reader (CLI modes): it may ask on the floor.
	always bool
	// Bumped by every refresh request. Each cache remembers the generation
	// it last honoured, so one press is one ask per provider and a second
	// press is a second ask — not a flag that the first cache to look at it
	// would clear for the rest.
	forceGen uint64
	// The same count for a press on ONE provider's own button, by the id
	// its tile carries (`claude`, `codex@2`). A cache answers to the sum of
	// the two, so either press is one ask and neither hides the other.
	forceOne map[string]uint64
	// The tiles the last reading filled from CPA (mergeCPA). The vendor's
	// native collector still runs behind such a tile, under the same id, so
	// a press on it is filed for the CPA credential alone (`cpa:` + id) and
	// the account nobody is looking at is not asked along with it.
	viaCPA map[string]bool
}

// noteWatched files what the relay last said: true is a dashboard open on
// this account and this machine set to refresh on its own; anything else
// takes the word back now rather than letting it run out its window.
func noteWatched(yes bool) {
	pace.Lock()
	if yes {
		pace.watchedAt = now()
	} else {
		pace.watchedAt = 0
	}
	pace.Unlock()
}

// streamWatched counts one tunnel stream opened with ?quota=auto for as long
// as it is open; the returned func is its close, safe to call once. Each
// stream holds its own count, so one reader leaving — or reopening its
// stream on manual — takes back only its own word.
func streamWatched() func() {
	pace.Lock()
	pace.streams++
	pace.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			pace.Lock()
			if pace.streams > 0 {
				pace.streams--
			}
			pace.Unlock()
		})
	}
}

// paceAlways marks this process as its own reader: the CLI modes, where the
// person who ran it is looking at the output.
func paceAlways() {
	pace.Lock()
	pace.always = true
	pace.Unlock()
}

// quotaForce asks every provider to fetch once at its next collection,
// floor or no floor (the 429 backoff still holds; see each cache).
func quotaForce() {
	pace.Lock()
	pace.forceGen++
	pace.Unlock()
}

// watched reports whether a vendor may be asked on the floor at all: a CLI
// mode somebody ran by hand, or a machine recently told it is watched.
func watched() bool {
	pace.Lock()
	defer pace.Unlock()
	return pace.always || pace.streams > 0 ||
		(pace.watchedAt > 0 && now()-pace.watchedAt < quotaWatchWindow)
}

// reader reports whether this process is a CLI mode somebody ran by hand.
func reader() bool {
	pace.Lock()
	defer pace.Unlock()
	return pace.always
}

// quotaForceOne is quotaForce for one provider, by the id on its tile: that
// provider asks once at its next collection and the others serve what they
// hold. An id no cache answers to forces nothing.
func quotaForceOne(id string) {
	pace.Lock()
	if pace.forceOne == nil {
		pace.forceOne = map[string]uint64{}
	}
	if pace.viaCPA[id] {
		id = cpaMark(id)
	}
	pace.forceOne[id]++
	pace.Unlock()
}

// cpaMark is the name a CPA credential's cache answers to, for the tile id.
func cpaMark(id string) string { return "cpa:" + id }

// noteCPATiles files which tiles the reading just built took from CPA.
func noteCPATiles(ids []string) {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	pace.Lock()
	pace.viaCPA = set
	pace.Unlock()
}

// quotaForceFor is the press as it arrives: everything, or the one provider
// named.
func quotaForceFor(id string) {
	if id == "" {
		quotaForce()
		return
	}
	quotaForceOne(id)
}

// forceTarget reports whether s can name a provider's tile: the vendor's
// slug, and `@n` on the second and later accounts of one vendor (cpa.go).
func forceTarget(s string) bool {
	if len(s) == 0 || len(s) > 40 {
		return false
	}
	at := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case at:
			return false
		case c >= 'a' && c <= 'z', c == '-', c == '_', c == '.':
		case c == '@' && i > 0 && i < len(s)-1:
			at = true
		default:
			return false
		}
	}
	return true
}

// forceGen is the press count a cache named id answers to: every press on
// the machine's button, plus every press on its own.
func forceGen(id string) uint64 {
	pace.Lock()
	defer pace.Unlock()
	return pace.forceGen + pace.forceOne[id]
}

// paceMark is what one cache keeps to answer due(): which force it has
// already honoured, and when it last honoured one.
type paceMark struct {
	// The id on this cache's tile, for a press on that tile alone. Empty
	// answers only to the machine's button.
	id        string
	forceSeen uint64
	forcedAt  float64
}

// consume drops any press this cache has not honoured yet, without asking.
// For a cache that is refusing for its own reason — a vendor's 429 backoff —
// so a press made during the backoff does not fire on its own when the
// backoff ends, with nobody waiting for it by then.
func (m *paceMark) consume() { m.forceSeen = forceGen(m.id) }

// due decides whether a cache may make a real call now. t is the caller's
// clock; fetchedAt is when its last SUCCESS was fetched (hasLast says
// whether there is one); attemptedAt is when it last tried at all, and
// retryFloor the least gap between two attempts — the old two-minute floor,
// kept so a failing vendor is not retried on every push while watched.
func (m *paceMark) due(t, fetchedAt, attemptedAt, retryFloor float64, hasLast bool) bool {
	if gen := forceGen(m.id); gen != m.forceSeen {
		// Honoured or dropped, the press is consumed either way: a press
		// inside the gap must not be banked and fire on its own later, when
		// nobody is waiting for it. Dropped, it falls through to the
		// ordinary question rather than answering it.
		m.forceSeen = gen
		if t-m.forcedAt >= quotaForceGap {
			m.forcedAt = t
			return true
		}
	}
	if !watched() {
		return false
	}
	if hasLast && t-fetchedAt < quotaFloor {
		return false
	}
	// On its own, the daemon tries a vendor once per floor whether the try
	// succeeded or not. The shorter retry floor is a reader's, where a
	// person is waiting on the output.
	if !reader() && retryFloor < quotaFloor {
		retryFloor = quotaFloor
	}
	if attemptedAt > 0 && t-attemptedAt < retryFloor {
		return false
	}
	return true
}
