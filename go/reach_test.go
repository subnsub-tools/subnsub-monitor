package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func resetReach(t *testing.T) {
	t.Helper()
	clear := func() {
		reach.Lock()
		reach.loaded, reach.rows, reach.open, reach.failing = false, nil, false, false
		reach.lastOK, reach.anchor, reach.saved = 0, 0, 0
		reach.Unlock()
		reachPush.Lock()
		reachPush.ack, reachPush.sent = 0, 0
		reachPush.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func reachRows(t *testing.T) [][3]any {
	t.Helper()
	var out [][3]any
	for _, r := range reachPoints() {
		out = append(out, [3]any{int64(r[0].(float64)), int64(r[1].(float64)), r[2]})
	}
	return out
}

func sameRows(t *testing.T, got [][3]any, want ...[3]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
}

// Nothing is recorded while the relay hears the machine: there is no
// silence to explain.
func TestReachOnlyWhileThePushFails(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	base := time.Unix(1_800_000_000, 0)
	reachRelayHeard(base)
	reachNote(base.Add(10*time.Second), reachKindTunnel)
	reachNote(base.Add(20*time.Second), reachKindDERP)
	if got := reachRows(t); len(got) != 0 {
		t.Fatalf("a machine the relay hears filed %v", got)
	}
}

// A stretch starts at the last push the relay took, when its first evidence
// comes within the join, runs on with the evidence, and ends at the push
// that reaches the relay again.
func TestReachStretchRunsFromPushToPush(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	T := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return T.Add(time.Duration(s) * time.Second) }
	reachRelayHeard(T)
	reachRelayLost(at(45))
	reachNote(at(50), reachKindDERP)
	reachNote(at(80), reachKindDERP)
	reachNote(at(110), reachKindDERP)
	reachRelayHeard(at(130))
	sameRows(t, reachRows(t), [3]any{T.Unix(), T.Unix() + 130, reachKindDERP})
	// Written whole, and a fresh process reads it back.
	if _, err := os.Stat(filepath.Join(configDir(), reachFile)); err != nil {
		t.Fatalf("reach file not written: %v", err)
	}
	reach.Lock()
	reach.loaded, reach.rows = false, nil
	reach.Unlock()
	sameRows(t, reachRows(t), [3]any{T.Unix(), T.Unix() + 130, reachKindDERP})
}

// Evidence further apart than the join is two stretches — what lies between
// is not known to have been online — and only the first takes the anchor.
// A stretch too short to explain any silence is dropped when it closes.
func TestReachGapSplitsAndShortStretchesGo(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	T := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return T.Add(time.Duration(s) * time.Second) }
	reachRelayHeard(T)
	reachRelayLost(at(45))
	reachNote(at(50), reachKindDERP)
	reachNote(at(300), reachKindDERP)
	reachRelayHeard(at(320))
	sameRows(t, reachRows(t), [3]any{T.Unix(), T.Unix() + 50, reachKindDERP})
}

// The push that ends a failure only closes a stretch whose evidence ran up
// to it: a machine with no evidence at all files nothing, and one whose
// evidence stopped long before the relay heard it again ends there.
func TestReachHeardOnlyExtendsWhatWasSeen(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	T := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return T.Add(time.Duration(s) * time.Second) }
	reachRelayHeard(T)
	reachRelayLost(at(45))
	reachRelayHeard(at(400))
	if got := reachRows(t); len(got) != 0 {
		t.Fatalf("no evidence filed %v", got)
	}
	reachRelayLost(at(430))
	reachNote(at(440), reachKindDERP)
	reachNote(at(500), reachKindDERP)
	reachRelayHeard(at(900))
	sameRows(t, reachRows(t), [3]any{T.Unix() + 400, T.Unix() + 500, reachKindDERP})
}

// The anchor is the last acknowledged push only when the first evidence is
// within the join of it.
func TestReachAnchorTooOldIsNotTaken(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	T := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return T.Add(time.Duration(s) * time.Second) }
	reachRelayHeard(T)
	reachRelayLost(at(200))
	reachNote(at(250), reachKindDERP)
	reachNote(at(300), reachKindDERP)
	reachRelayHeard(at(320))
	sameRows(t, reachRows(t), [3]any{T.Unix() + 250, T.Unix() + 320, reachKindDERP})
}

// A browser served is the stronger word: the stretch says tunnel.
func TestReachTunnelOutranksTheProbe(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	T := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return T.Add(time.Duration(s) * time.Second) }
	reachRelayHeard(T)
	reachRelayLost(at(45))
	reachNote(at(50), reachKindDERP)
	reachNote(at(51), reachKindTunnel)
	reachNote(at(80), reachKindDERP)
	reachRelayHeard(at(130))
	sameRows(t, reachRows(t), [3]any{T.Unix(), T.Unix() + 130, reachKindTunnel})
	reachNote(at(140), "bogus")
	if got := reachRows(t); len(got) != 1 {
		t.Fatalf("an unknown kind filed something: %v", got)
	}
}

// What travels is what the relay has not acknowledged; a stretch still
// growing goes again once it has grown, and a relay that answers without the
// field is taken to have it.
func TestReachPendingAndAck(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	T := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return T.Add(time.Duration(s) * time.Second) }
	reachRelayHeard(T)
	reachRelayLost(at(45))
	reachNote(at(50), reachKindDERP)
	p := reachPending(at(60))
	if len(p) != 1 || p[0][1].(float64) != float64(T.Unix()+50) {
		t.Fatalf("pending = %v, want the growing stretch", p)
	}
	reachSent(p)
	until := float64(T.Unix() + 50)
	reachAcked(true, &until)
	if p := reachPending(at(61)); len(p) != 0 {
		t.Fatalf("pending after the ack = %v, want none", p)
	}
	reachNote(at(80), reachKindDERP)
	if p := reachPending(at(81)); len(p) != 1 || p[0][1].(float64) != float64(T.Unix()+80) {
		t.Fatalf("pending after it grew = %v, want it again with its new end", p)
	}
	reachSent(reachPending(at(81)))
	reachAcked(true, nil)
	if p := reachPending(at(82)); len(p) != 0 {
		t.Fatalf("a relay without the field still gets %v", p)
	}
	reachAcked(false, nil) // not the protocol: says nothing
	reachPush.Lock()
	ack := reachPush.ack
	reachPush.Unlock()
	if ack != T.Unix()+80 {
		t.Fatalf("ack = %d, want %d", ack, T.Unix()+80)
	}
	// And a push that carries the rows: the field rides it. A fresh home,
	// so the record above is not read back from its file.
	scratchHome(t)
	resetReach(t)
	reachRelayHeard(T)
	reachRelayLost(at(45))
	reachNote(at(50), reachKindTunnel)
	b := dump(Snapshot{Reach: reachPending(at(60))})
	if want := `"reach":[[` + strconv.FormatInt(T.Unix(), 10) + `,` + strconv.FormatInt(T.Unix()+50, 10) + `,"tunnel"]]`; !strings.Contains(string(b), want) {
		t.Fatalf("snapshot %s lacks %s", b, want)
	}
}

// A process that starts while the relay is still away — a self-update in
// the middle of a failure — carries the stretch on from where the record it
// found ends, so there is no hole where the helper was swapped; one that
// comes back after a long stop does not.
func TestReachRestartCarriesOn(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	T := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return T.Add(time.Duration(s) * time.Second) }
	reachRelayHeard(T)
	reachRelayLost(at(45))
	reachNote(at(50), reachKindDERP)
	reachNote(at(100), reachKindDERP) // written: 50 s after the last write
	reachNote(at(120), reachKindDERP) // not yet written
	// The process is replaced; the new one finds the file.
	reach.Lock()
	reach.loaded, reach.rows, reach.open, reach.failing, reach.lastOK, reach.anchor, reach.saved = false, nil, false, false, 0, 0, 0
	reach.Unlock()
	reachRelayLost(at(160))
	reachNote(at(161), reachKindDERP)
	reachRelayHeard(at(200))
	sameRows(t, reachRows(t), [3]any{T.Unix(), T.Unix() + 100, reachKindDERP}, [3]any{T.Unix() + 100, T.Unix() + 200, reachKindDERP})
	// And after a long stop the old end is no anchor.
	reach.Lock()
	reach.loaded, reach.rows, reach.open, reach.failing, reach.lastOK, reach.anchor, reach.saved = false, nil, false, false, 0, 0, 0
	reach.Unlock()
	reachRelayLost(at(5000))
	reachNote(at(5010), reachKindDERP)
	reachRelayHeard(at(5100))
	got := reachRows(t)
	sameRows(t, got[len(got)-1:], [3]any{T.Unix() + 5010, T.Unix() + 5100, reachKindDERP})
}

// A wall clock that steps back cuts the growing stretch to the corrected now
// and offers it to the relay again; one that steps back past the stretch's
// start takes it away, and the evidence that follows starts afresh.
func TestReachClockStepBack(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	T := time.Unix(1_800_000_000, 0)
	at := func(s int) time.Time { return T.Add(time.Duration(s) * time.Second) }
	reachRelayHeard(T)
	reachRelayLost(at(45))
	reachNote(at(50), reachKindDERP)
	reachNote(at(120), reachKindDERP)
	reachNote(at(190), reachKindDERP)
	reachSent(reachPending(at(191)))
	until := float64(T.Unix() + 190)
	reachAcked(true, &until)
	reachNote(at(100), reachKindDERP) // the clock was ninety seconds fast
	reachNote(at(130), reachKindDERP)
	reachRelayHeard(at(150))
	sameRows(t, reachRows(t), [3]any{T.Unix(), T.Unix() + 150, reachKindDERP})
	if p := reachPending(at(151)); len(p) != 1 || p[0][1].(float64) != float64(T.Unix()+150) {
		t.Fatalf("pending after the step = %v, want the cut stretch again", p)
	}
	// Back past the start: the stretch goes, and what follows is new.
	resetReach(t)
	scratchHome(t)
	reachRelayHeard(at(1000))
	reachRelayLost(at(1045))
	reachNote(at(1050), reachKindDERP)
	reachNote(at(1100), reachKindDERP)
	reachNote(at(500), reachKindDERP)
	reachNote(at(560), reachKindDERP)
	reachRelayHeard(at(600))
	sameRows(t, reachRows(t), [3]any{T.Unix() + 500, T.Unix() + 600, reachKindDERP})
}

// More than the cap pending: the oldest go first, and the rest follow on
// the next pushes rather than being acknowledged away. A stretch still too
// short to keep is not sent at all.
func TestReachOldestFirstAndNoShortRows(t *testing.T) {
	scratchHome(t)
	resetReach(t)
	T := time.Unix(1_800_000_000, 0)
	reach.Lock()
	reach.loaded = true
	for i := 0; i < reachPushMax+5; i++ {
		a := T.Unix() + int64(i)*200
		reach.rows = append(reach.rows, reachSpan{At: a, End: a + 100, Kind: reachKindDERP})
	}
	reach.Unlock()
	now := T.Add(time.Duration(reachPushMax+10) * 200 * time.Second)
	p := reachPending(now)
	if len(p) != reachPushMax || p[0][0].(float64) != float64(T.Unix()) {
		t.Fatalf("first batch = %d rows from %v, want %d from the oldest", len(p), p[0][0], reachPushMax)
	}
	reachSent(p)
	until := p[len(p)-1][1].(float64)
	reachAcked(true, &until)
	if p := reachPending(now); len(p) != 5 {
		t.Fatalf("second batch = %d rows, want the 5 left", len(p))
	}
	// A short growing stretch stays home until it is long enough.
	resetReach(t)
	scratchHome(t)
	reachRelayHeard(T)
	reachRelayLost(T.Add(200 * time.Second))
	reachNote(T.Add(300*time.Second), reachKindTunnel)
	reachNote(T.Add(310*time.Second), reachKindTunnel)
	if p := reachPending(T.Add(311 * time.Second)); len(p) != 0 {
		t.Fatalf("a ten-second stretch was offered: %v", p)
	}
	reachNote(T.Add(340*time.Second), reachKindTunnel)
	if p := reachPending(T.Add(341 * time.Second)); len(p) != 1 {
		t.Fatalf("a forty-second stretch was not offered: %v", p)
	}
}
