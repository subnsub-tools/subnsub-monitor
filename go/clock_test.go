package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Clock skew after boot, interface traffic selection, boot outage timestamps,
// failed custom tiles, and history span bounds.

// A box whose clock ran eight hours ahead at boot and was put right a minute
// later: the row filed at boot ends when the right clock first ran, and no
// other row is touched.
func TestOutageBootRowFollowsTheClockBack(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	real := time.Unix(1_800_000_000, 0)
	ahead := real.Add(8 * time.Hour)
	stamp := filepath.Join(configDir(), outageStamp)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(real.Unix()-3600, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outages.Lock()
	outages.loaded = true
	outages.rows = []outage{{At: real.Unix() - 7200, End: real.Unix() - 7000, Kind: outageKindSuspend}}
	outages.Unlock()
	outageBoot(ahead)
	got := outagePoints()
	if len(got) != 2 || got[1][1].(float64) != float64(ahead.Unix()) {
		t.Fatalf("at boot = %v, want the down ending at the boot's clock", got)
	}
	/* The sampler's ordinary seconds on the wrong clock come first. */
	outageTick(ahead.Add(time.Second))
	outageTick(ahead.Add(2 * time.Second))
	outageTick(real.Add(time.Minute))
	got = outagePoints()
	if len(got) != 2 || got[1][0].(float64) != float64(real.Unix()-3600) || got[1][1].(float64) != float64(real.Add(time.Minute).Unix()) {
		t.Fatalf("after the clock came back = %v, want the down ending now", got)
	}
	if got[0][1].(float64) != float64(real.Unix()-7000) {
		t.Errorf("the other row moved: %v", got[0])
	}
	if b, _ := os.ReadFile(stamp); string(b) != strconv.FormatInt(real.Add(time.Minute).Unix(), 10)+"\n" {
		t.Errorf("the stamp did not follow the clock back: %q", b)
	}
}

// A clock that is behind for a while — a board with no RTC started on its
// last saved time — costs the record nothing: rows "in the future" stay.
func TestOutageClockBehindKeepsTheRows(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	now := time.Unix(1_800_000_000, 0)
	outages.Lock()
	outages.loaded = true
	outages.rows = []outage{{At: now.Unix() + 1000, End: now.Unix() + 2000, Kind: outageKindSuspend},
		{At: now.Unix() - 100, End: now.Unix() + 900, Kind: outageKindDown}}
	outages.prev = now
	outages.Unlock()
	outageTick(now.Add(time.Second))
	got := outagePoints()
	if len(got) != 2 || got[0][1].(float64) != float64(now.Unix()+2000) || got[1][1].(float64) != float64(now.Unix()+900) {
		t.Fatalf("rows = %v, want both as they were", got)
	}
}

func stepSnap(cpu, uptime float64) *Snapshot {
	f := func(v float64) *float64 { return &v }
	return &Snapshot{System: &System{OK: true, CPUPercent: f(cpu), UptimeSec: f(uptime)}}
}

// The clock stepped back soon after boot: the bucket from the wrong clock
// and the rows it wrote go, and the archive takes rows again at once. The
// same step hours into the machine's life leaves the rows alone.
func TestHistoryClockStepBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		uptime float64
		kept   int
	}{{"soon after boot", 120, 1}, {"hours in", 5 * 3600, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			scratchHome(t)
			resetHist(t)
			sampler.Lock()
			sampler.on = true
			sampler.Unlock()
			t.Cleanup(func() {
				sampler.Lock()
				sampler.on = false
				sampler.Unlock()
				histNow = time.Now
				hist.Lock()
				hist.procFirst = 0
				hist.Unlock()
			})
			real := time.Unix(1_800_000_000, 0)
			/* A row the archive held before this process, from the right clock. */
			hist.Lock()
			hist.loaded = true
			hist.rows = []histRow{minuteRow(real.Unix()-600, 5, nil)}
			hist.procFirst = 0
			hist.Unlock()
			at := real.Add(8 * time.Hour)
			for i := 0; i < 3; i++ {
				histNow = func() time.Time { return at }
				historyRecord(stepSnap(10, tc.uptime))
				at = at.Add(30 * time.Second)
			}
			/* Put right. */
			histNow = func() time.Time { return real.Add(time.Minute) }
			historyRecord(stepSnap(20, tc.uptime+90))
			hist.Lock()
			n := len(hist.rows)
			first := hist.rows[0][0].(float64)
			hist.Unlock()
			if n != tc.kept || first != float64(real.Unix()-600) {
				t.Fatalf("archive holds %d rows from %v, want %d from the old row", n, first, tc.kept)
			}
		})
	}
}

// The interfaces' counters: what each reading adds since the one before; a
// total that fell (an interface gone) adds nothing, and a leap past any
// link's rate is left out — and a sum at every step it is folded to.
func TestHistBytesSum(t *testing.T) {
	hist.Lock()
	hist.haveTot, hist.lastRx, hist.lastTx = false, 0, 0
	hist.Unlock()
	t.Cleanup(func() {
		hist.Lock()
		hist.haveTot, hist.lastRx, hist.lastTx = false, 0, 0
		hist.Unlock()
	})
	f := func(v float64) *float64 { return &v }
	b := &histBucket{system: map[string]float64{}}
	for _, c := range [][2]float64{{1000, 100}, {3000, 400}, {500, 450}, {9e12, 450}} {
		hist.Lock()
		histBytes(b, &System{NetRxTotal: f(c[0]), NetTxTotal: f(c[1])})
		hist.Unlock()
	}
	/* 2000, then a fall (nothing), then a leap no link makes. */
	if b.system["rx_bytes"] != 2000 || b.system["tx_bytes"] != 350 {
		t.Fatalf("sums = %v / %v, want 2000 / 350", b.system["rx_bytes"], b.system["tx_bytes"])
	}
	base := int64(1_800_000_000)
	rows := []histRow{minuteRow(base, 1, nil), minuteRow(base+60, 1, nil)}
	rows[0][9].(map[string]float64)["rx_bytes"] = 100
	rows[1][9].(map[string]float64)["rx_bytes"] = 250
	rows[0][9].(map[string]float64)["boot"] = float64(base - 3600)
	rows[1][9].(map[string]float64)["boot"] = float64(base - 3600)
	out := histFold(rows, histCoarseSec)
	sys := histNumMap(out[0][9])
	if sys["rx_bytes"] != 350 {
		t.Errorf("folded traffic = %v, want the sum 350", sys["rx_bytes"])
	}
	if sys["boot"] != float64(base-3600) {
		t.Errorf("folded boot = %v", sys["boot"])
	}
}

// A tile whose source failed, or whose value is stale or an error, writes
// nothing into the bucket: a gap, not a flat line.
func TestHistCustomFoldSkipsWhatWasNotMeasured(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tiles := []Tile{
		{ID: "good", Type: "gauge", Value: f(1.23456), History: true, Source: "a"},
		{ID: "gone", Type: "gauge", Value: f(9), History: true, Source: "b"},
		{ID: "old", Type: "number", Value: f(7), History: true, Source: "a", Stale: true},
		{ID: "bad", Type: "number", Value: f(7), History: true, Source: "a", Error: "exec-failed"},
	}
	dst := map[string]float64{}
	histCustomFold(dst, tiles, map[string]bool{"a": true})
	if len(dst) != 1 || dst["cmax:good"] != 1.23456 {
		t.Fatalf("folded %v, want the one measured tile", dst)
	}
	b := &histBucket{start: 1_800_000_000, quota: map[string]float64{}, services: map[string]int{}, system: dst}
	if v := b.row()[9].(map[string]float64)["cmax:good"]; v != 1.2346 {
		t.Errorf("a tile's value kept to %v, want four places", v)
	}
}

// A page of the archive is folded from the rows its span can reach, and
// says the same as the whole archive folded and cut.
func TestHistoryPointsRangeIsTheSpan(t *testing.T) {
	scratchHome(t)
	resetHist(t)
	base := int64(1_800_000_000)
	rows := make([]histRow, 0, 600)
	for i := int64(0); i < 600; i++ {
		rows = append(rows, minuteRow(base+i*30, float64(i%97), nil))
	}
	hist.Lock()
	hist.loaded, hist.rows = true, rows
	hist.Unlock()
	from, to := base+3000, base+9000
	part, oldest := historyPointsRange(from, to, histCoarseSec)
	if oldest != base {
		t.Errorf("oldest = %d, want the archive's first", oldest)
	}
	got := histFold(part, histCoarseSec)
	all := histFold(rows, histCoarseSec)
	var want []histRow
	for _, r := range all {
		if s := int64(r[0].(float64)); s >= from-from%histCoarseSec && s < to+histCoarseSec {
			want = append(want, r)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%d buckets, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i][0] != want[i][0] || got[i][1] != want[i][1] {
			t.Fatalf("bucket %d = %v, want %v", i, got[i][:2], want[i][:2])
		}
	}
}
