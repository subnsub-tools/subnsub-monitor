package main

// Reach — the stretches this machine was demonstrably online while its push
// to the relay was failing, kept HERE beside the outage record (outages.go)
// and sent to the relay with the push the same way.
//
// The relay files a stop for every gap of more than 100 s between two of its
// stamps for a machine. It cannot tell a machine that stopped from a road —
// or a relay — that failed: on 2026-10-23 a provider outage in front of the
// relay made every push hang or fail for an hour, and the relay's record had
// the whole fleet stopping and starting while every machine was up and its
// tailcat tunnel was answering a browser. The machine's own outage record
// says the helper kept running, but not that the machine could be reached.
// This record says that, from two kinds of evidence:
//
//   tunnel  a browser was receiving this machine's tailcat stream
//           (direct.go events): the machine answered end to end.
//   derp    the machine completed a TLS handshake with the DERP relay its
//           tunnel waits at (transport ProbeRelay), probed every 30 s while
//           the push is failing: it could be reached over tailcat.
//
// Only stretches in which the push was failing are recorded. While the relay
// hears the machine it has no silence to explain, and recording every second
// a browser watches would grow a row the relay rewrites on every push for
// nothing. A stretch begins at the last push the relay acknowledged, when the
// first evidence comes within the join of it (the relay's silence begins at
// that same push), and ends at the push that reaches the relay again, when
// the evidence ran up to it. Evidence further apart than the join is two
// stretches: what lies between is not known to have been online.
//
// The page subtracts these stretches from the relay's silences
// (monitor-worker presence.js mergeOutages): what the relay could not hear
// while the machine was reachable is not a stop.
//
// Bounded like the outage record: a row count and an age, written whole,
// write-then-rename, 0600. A row still growing is written at most every
// 30 s, so a crash or a self-update loses that much of it at the most — and
// a process that starts while the push is still failing carries on from the
// row's last written end (reachRelayLost), so the stretch has no hole where
// the helper was swapped.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	reachKindTunnel = "tunnel"
	reachKindDERP   = "derp"

	// Evidence closer than this is one stretch. The probe runs every 30 s
	// and may take its 10 s timeout; the tunnel's frames come every second.
	// Under the relay's own 100 s presence threshold, so a gap that could
	// hide a stop the relay would file is never bridged.
	reachJoinS = 90
	// A stretch shorter than this, once closed, is dropped: the relay files
	// no silence under 100 s, and a row that cannot explain one is noise.
	reachMinSec = 30
	reachKeep   = 2000
	reachKeepS  = histKeepSec
	// Twice what the cap can hold, as outageReadMax is.
	reachReadMax   = 160 << 10
	reachFile      = "reach.json"
	reachSaveEvery = 30

	reachProbeEvery   = 30 * time.Second
	reachProbeTimeout = 10 * time.Second

	// Evidence this far behind the growing row's end is the wall clock
	// stepped back, not two frames in one second.
	reachClockSlackS = 2

	reachPushMax     = 200
	reachPushWindowS = 7 * 86400
)

func reachKindKnown(k string) bool { return k == reachKindTunnel || k == reachKindDERP }

type reachSpan struct {
	At, End int64
	Kind    string
}

func (r reachSpan) row() outageRow { return outageRow{float64(r.At), float64(r.End), r.Kind} }

var reach struct {
	sync.Mutex
	loaded bool
	rows   []reachSpan
	// The last row is still growing: the push is failing and evidence keeps
	// coming within the join.
	open bool
	// The push is failing now (transport.go connect).
	failing bool
	// When the push last reached the relay, and that moment as it stood when
	// the failing began — the start the first stretch may take.
	lastOK, anchor int64
	// When the file was last written, unix seconds.
	saved int64
}

// reachRelayLost is the push loop saying the relay did not take a push: no
// answer, or an answer that was not an acceptance. A relay pacing the room
// (429) heard the machine moments ago and is not this.
func reachRelayLost(now time.Time) {
	reach.Lock()
	defer reach.Unlock()
	if reach.failing {
		return
	}
	reach.failing = true
	reach.anchor = reach.lastOK
	// A process that has not yet had a push accepted — a self-update or a
	// restart while the relay was away — continues from where the record it
	// found ends: the anchor holds only if the first evidence comes within
	// the join of it, so a machine back from a long stop starts afresh.
	if reach.anchor == 0 {
		reachLoad()
		if n := len(reach.rows); n > 0 {
			reach.anchor = reach.rows[n-1].End
		}
	}
	// Probe now rather than at the next tick: a stretch takes the last
	// acknowledged push as its start only when its first evidence comes
	// within the join of it.
	select {
	case reachKick <- struct{}{}:
	default:
	}
}

// reachKick starts a probe at once (reachLoop). Holds one token.
var reachKick = make(chan struct{}, 1)

// reachRelayHeard is the push loop saying the relay took a push. A stretch
// still growing runs to here — the relay hears the machine again from this
// moment — when its evidence came within the join; it is then closed.
func reachRelayHeard(now time.Time) {
	reach.Lock()
	defer reach.Unlock()
	s := now.Unix()
	if reach.failing {
		if n := len(reach.rows); reach.open && n > 0 {
			if last := &reach.rows[n-1]; s > last.End && s-last.End <= reachJoinS {
				last.End = s
			} else if s < last.End-reachClockSlackS {
				reachClockBackLocked(s)
			}
		}
		reachCloseLocked()
		reach.failing = false
		reach.anchor = 0
	}
	reach.lastOK = s
}

// reachNote files one piece of evidence that the machine was online at
// `now`. Nothing while the push is succeeding.
func reachNote(now time.Time, kind string) {
	if !reachKindKnown(kind) {
		return
	}
	reach.Lock()
	defer reach.Unlock()
	if !reach.failing {
		return
	}
	reachLoad()
	s := now.Unix()
	if n := len(reach.rows); reach.open && n > 0 && s < reach.rows[n-1].End-reachClockSlackS {
		// The wall clock stepped back: what the row says after `s` was said
		// on a clock that ran ahead. It is cut back to now and goes on from
		// here; the relay's copy is put right by sending it again.
		reachClockBackLocked(s)
	}
	if n := len(reach.rows); reach.open && n > 0 {
		last := &reach.rows[n-1]
		switch {
		case s <= last.End:
			// The same second as the last word: only the kind can move.
		case s-last.End <= reachJoinS:
			last.End = s
			if s-reach.saved >= reachSaveEvery || s < reach.saved {
				reachSave(s)
			}
		default:
			reachCloseLocked()
		}
		if reach.open {
			// The stronger word wins: a browser that was served is the
			// machine answering end to end, not only reaching its relay.
			if kind == reachKindTunnel {
				last.Kind = reachKindTunnel
			}
			return
		}
	}
	if reach.anchor > s {
		reach.anchor = 0
	}
	at := s
	if a := reach.anchor; a > 0 && s-a <= reachJoinS {
		at = a
	}
	reach.anchor = 0
	reach.rows = append(reach.rows, reachSpan{At: at, End: s, Kind: kind})
	reach.open = true
	reachTrim(s)
	reachSave(s)
}

// reachCloseLocked ends the growing row, dropping it when it is too short to
// explain any silence the relay files. Caller holds the lock.
func reachCloseLocked() {
	if !reach.open {
		return
	}
	reach.open = false
	if n := len(reach.rows); n > 0 && reach.rows[n-1].End-reach.rows[n-1].At < reachMinSec {
		reach.rows = reach.rows[:n-1]
	}
	reachSave(time.Now().Unix())
}

// reachClockBackLocked puts the growing row right after the wall clock
// stepped back to `s`: its end is cut to `s`, or — begun after `s` on the
// clock that ran ahead — it goes. The relay's copy may hold the old end, so
// the whole window is offered again (the relay keeps the latest word for a
// start). A row the cut removes cannot be taken back from the relay: a step
// back past a whole stretch is rare enough to leave at that. Caller holds the
// lock.
func reachClockBackLocked(s int64) {
	n := len(reach.rows)
	if !reach.open || n == 0 {
		return
	}
	if last := &reach.rows[n-1]; s > last.At {
		last.End = s
	} else {
		reach.rows = reach.rows[:n-1]
		reach.open = false
	}
	if reach.anchor > s {
		reach.anchor = 0
	}
	reachSave(s)
	reachPush.Lock()
	reachPush.ack = 0
	reachPush.Unlock()
}

func reachPath() string {
	dir := configDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, reachFile)
}

func reachLoad() {
	if reach.loaded {
		return
	}
	reach.loaded = true
	p := reachPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) > reachReadMax {
		return
	}
	var rows []outageRow
	if json.Unmarshal(b, &rows) != nil {
		return
	}
	// Trusted only in shape, as the outage record is.
	for _, r := range rows {
		if len(r) != 3 {
			continue
		}
		at, ok1 := r[0].(float64)
		end, ok2 := r[1].(float64)
		kind, ok3 := r[2].(string)
		if !ok1 || !ok2 || !ok3 || end <= at || !reachKindKnown(kind) {
			continue
		}
		reach.rows = append(reach.rows, reachSpan{At: int64(at), End: int64(end), Kind: kind})
	}
}

func reachSave(now int64) {
	reach.saved = now
	p := reachPath()
	if p == "" {
		return
	}
	rows := make([]outageRow, 0, len(reach.rows))
	for _, r := range reach.rows {
		rows = append(rows, r.row())
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return
	}
	for len(b) > reachReadMax && len(rows) > 1 {
		rows = rows[len(rows)/8+1:]
		if b, err = json.Marshal(rows); err != nil {
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".new"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
	}
}

// reachTrim holds the record to its two bounds, as of `now`. The growing
// row is the newest and is never the one cut.
func reachTrim(now int64) {
	keep := reach.rows[:0]
	for _, r := range reach.rows {
		if now-r.End <= reachKeepS {
			keep = append(keep, r)
		}
	}
	reach.rows = keep
	if len(reach.rows) > reachKeep {
		reach.rows = reach.rows[len(reach.rows)-reachKeep:]
	}
}

// reachPoints is the record as rows, oldest first, never nil.
func reachPoints() []outageRow {
	reach.Lock()
	defer reach.Unlock()
	reachLoad()
	out := make([]outageRow, 0, len(reach.rows))
	for _, r := range reach.rows {
		out = append(out, r.row())
	}
	return out
}

// ── the relay's copy ─────────────────────────────────────────────────────
//
// As for the outage record (outages.go): only what the relay has not
// acknowledged travels, the acknowledgement being how far its copy reaches
// (`reach_until`, hint.go). A row still growing ends later than that on
// every push until the relay has its final end, and so goes again.

var reachPush struct {
	sync.Mutex
	ack, sent int64
}

// reachPending is what the next push carries: rows ending after the relay's
// acknowledgement, inside the window, oldest first — and the OLDEST batch
// when there are more than the cap, since the relay's answer acknowledges up
// to the newest end it was sent and a newest-first batch would leave the rest
// behind it for good (review 2026-10-24). A row still too short to be kept is
// not sent: it may yet be dropped, and the relay cannot be told to forget
// one. Nil when there is nothing, which is the ordinary case.
func reachPending(now time.Time) []outageRow {
	reachPush.Lock()
	ack := reachPush.ack
	reachPush.Unlock()
	floor := now.Unix() - reachPushWindowS
	reach.Lock()
	defer reach.Unlock()
	reachLoad()
	var out []outageRow
	for _, r := range reach.rows {
		if r.End > ack && r.End >= floor && r.End-r.At >= reachMinSec {
			out = append(out, r.row())
			if len(out) == reachPushMax {
				break
			}
		}
	}
	return out
}

// reachSent notes what a push carried (transport.go pushBody).
func reachSent(rows []outageRow) {
	var top int64
	for _, r := range rows {
		if len(r) == 3 {
			if end, ok := r[1].(float64); ok && int64(end) > top {
				top = int64(end)
			}
		}
	}
	reachPush.Lock()
	reachPush.sent = top
	reachPush.Unlock()
}

// reachAcked files what the push response said (hint.go absorb), by the
// rule outageAcked follows: no field from a relay that speaks the protocol
// is a relay that does not keep the record, and what was sent counts as
// delivered; a figure is taken as it is, lower included.
func reachAcked(parsed bool, until *float64) {
	if !parsed {
		return
	}
	reachPush.Lock()
	defer reachPush.Unlock()
	if until == nil {
		if reachPush.sent > reachPush.ack {
			reachPush.ack = reachPush.sent
		}
		return
	}
	if v := *until; v == v && v >= 0 && v < 1e12 {
		reachPush.ack = int64(v)
	}
}

// ── the probe ───────────────────────────────────────────────────────────

// reachLoop probes the tailcat rendezvous every reachProbeEvery while the
// push is failing and the tunnel is up, and files each answer as evidence.
// Started by connect, beside the push loop it watches.
func reachLoop() {
	t := time.NewTicker(reachProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-reachKick:
		}
		reach.Lock()
		failing := reach.failing
		reach.Unlock()
		if !failing {
			continue
		}
		probe := directProbe()
		if probe == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), reachProbeTimeout)
		err := probe(ctx)
		cancel()
		if err == nil {
			reachNote(time.Now(), reachKindDERP)
		}
	}
}
