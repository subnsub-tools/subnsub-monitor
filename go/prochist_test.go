package main

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func resetProcHist(t *testing.T) {
	t.Helper()
	clear := func() {
		procHist.Lock()
		procHist.prev, procHist.names, procHist.done = nil, nil, nil
		procHist.total, procHist.busy, procHist.bucket = 0, 0, 0
		procHist.prevTotal, procHist.prevBusy, procHist.prevUptime = 0, 0, 0
		procHist.prevAt = time.Time{}
		procHist.Unlock()
	}
	clear()
	// Staged counters and the real /proc do not mix: no live sweep in a test.
	sweep := procSweepNow
	procSweepNow = func() {}
	t.Cleanup(func() { clear(); procSweepNow = sweep })
}

func foldAt(cur map[int]procRead, total, busy, uptime float64, at time.Time) {
	procHist.Lock()
	procFold(cur, total, busy, uptime, at)
	procHist.Unlock()
}

// Same-named processes are one row, the average is ticks over the bucket's
// ticks, the peak is the worst single sweep, and what the named rows do not
// explain is the remainder — including a process that died between sweeps.
func TestProcFoldAveragesPeaksAndRemainder(t *testing.T) {
	resetProcHist(t)
	at := time.Unix(1_800_000_000, 0) // on a bucket boundary
	p := func(start, ticks float64, name string) procRead { return procRead{start, ticks, name} }

	// Baseline: nothing is attributed to a first sweep.
	foldAt(map[int]procRead{1: p(10, 500, "chrome"), 2: p(20, 100, "chrome"), 3: p(30, 50, "node")},
		10_000, 4_000, 100, at)
	if got := procHistPeek(at.Unix()); got != nil {
		t.Fatalf("a first sweep has no interval to report: %v", got)
	}
	// +1000 machine ticks, 400 of them busy: chrome 100+50, node 30, and a
	// newborn (start 101s ≥ the last sweep's uptime 100s) owns all of its 20.
	foldAt(map[int]procRead{1: p(10, 600, "chrome"), 2: p(20, 150, "chrome"), 3: p(30, 80, "node"),
		4: p(10_100, 20, "cc1")}, 11_000, 4_400, 110, at.Add(10*time.Second))
	// +1000 more, 100 busy: chrome quiet, node 40; pid 4 is gone and pid 3's
	// number is reused by something that started long ago — not a newborn, no
	// previous reading of THAT process, so nothing is claimed for it.
	foldAt(map[int]procRead{1: p(10, 610, "chrome"), 2: p(20, 150, "chrome"), 3: p(30, 120, "node"),
		9: p(5, 9_999, "old")}, 12_000, 4_500, 120, at.Add(20*time.Second))

	got := procHistPeek(at.Unix())
	want := map[string][]float64{
		"chrome": {8, 15}, // 160/2000, peak 150/1000
		"node":   {3.5, 4},
		"cc1":    {1, 2},
		"":       {12.5}, // busy 500 − named 250, over 2000
	}
	if a, b := mustJSON(got), mustJSON(want); a != b {
		t.Fatalf("rows:\n got %s\nwant %s", a, b)
	}
	if _, found := got["old"]; found {
		t.Error("a process first seen mid-life was billed for its whole past")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Only five names, a recycled pid does not inherit its predecessor's counter,
// and a gap too long to be an interval is dropped instead of averaged.
func TestProcFoldBounds(t *testing.T) {
	resetProcHist(t)
	at := time.Unix(1_800_000_000, 0)
	names := []string{"a", "b", "c", "d", "e", "f", "g"}
	first, second := map[int]procRead{}, map[int]procRead{}
	for i, n := range names {
		first[i+1] = procRead{1, 0, n}
		second[i+1] = procRead{1, float64(10 * (i + 1)), n}
	}
	foldAt(first, 1_000, 0, 50, at)
	foldAt(second, 2_000, 900, 60, at.Add(10*time.Second))
	got := procHistPeek(at.Unix())
	if len(got) != procTopN+1 {
		t.Fatalf("want %d names and the remainder, got %v", procTopN, got)
	}
	if _, found := got["a"]; found {
		t.Errorf("the smallest made the cut: %v", got)
	}
	if got["g"][0] != 7 {
		t.Errorf("g: %v", got["g"])
	}

	// pid 7 comes back as a different process (new start) with a LOWER
	// counter: a newborn, billed its own 5 ticks, not −65.
	third := map[int]procRead{7: {6_500, 5, "g"}}
	foldAt(third, 3_000, 1_000, 70, at.Add(20*time.Second))
	if got := procHistPeek(at.Unix()); got["g"][0] != 3.8 { // (70+5)/2000
		t.Errorf("recycled pid: %v", got["g"])
	}

	// A suspended helper: the next sweep is a baseline again.
	before := mustJSON(procHistPeek(at.Unix()))
	foldAt(map[int]procRead{7: {6_500, 5_000, "g"}}, 90_000, 50_000, 900, at.Add(200*time.Second))
	if after := mustJSON(procHistPeek(at.Unix())); after != before {
		t.Errorf("a stale interval was folded in:\n%s\n%s", before, after)
	}
}

// An interval that straddles a bucket boundary is split by the time it spent
// on each side; one that ends exactly on the boundary is all the old bucket's.
func TestProcFoldSplitsAcrossTheBoundary(t *testing.T) {
	resetProcHist(t)
	edge := time.Unix(1_800_000_000, 0)
	old, cur := edge.Unix()-histBucketSec, edge.Unix()
	r := func(ticks float64) map[int]procRead { return map[int]procRead{1: {1, ticks, "ffmpeg"}} }

	// Ends exactly ON the edge: every second of it was the old bucket's.
	foldAt(r(0), 1, 0, 50, edge.Add(-10*time.Second))
	foldAt(r(500), 1_001, 600, 60, edge)
	if got := procHistPeek(old); mustJSON(got) != `{"":[10],"ffmpeg":[50,50]}` {
		t.Fatalf("an interval ending on the edge: %s", mustJSON(got))
	}
	if got := procHistPeek(cur); got != nil {
		t.Fatalf("the new bucket was credited with none of its own time: %v", got)
	}

	// 4 s before the edge, 6 s after it: 800 ticks, 40% of them the old bucket's.
	resetProcHist(t)
	foldAt(r(0), 1, 0, 50, edge.Add(-24*time.Second))
	foldAt(r(500), 1_001, 600, 60, edge.Add(-14*time.Second))
	foldAt(r(1_000), 2_001, 1_200, 70, edge.Add(-4*time.Second))
	foldAt(r(1_800), 3_001, 2_100, 80, edge.Add(6*time.Second))
	// old: (1000+320) of (2000+400) ticks = 55%; busy (1200+360)/2400 = 65%.
	if got := procHistPeek(old); mustJSON(got) != `{"":[10],"ffmpeg":[55,50]}` {
		t.Fatalf("old bucket after the crossing: %s", mustJSON(got))
	}
	// new: 480 of 600 = 80%, and the 80% sweep's peak lands here — the side
	// holding more of the interval.
	if got := procHistPeek(cur); mustJSON(got) != `{"":[10],"ffmpeg":[80,80]}` {
		t.Fatalf("new bucket: %s", mustJSON(got))
	}

	// history.go collected the old bucket before the crossing sweep arrived:
	// only the new side's share is kept, and nothing is filed twice.
	resetProcHist(t)
	foldAt(r(0), 1, 0, 50, edge.Add(-14*time.Second))
	foldAt(r(500), 1_001, 600, 60, edge.Add(-4*time.Second))
	if got := procHistTake(old); mustJSON(got) != `{"":[10],"ffmpeg":[50,50]}` {
		t.Fatalf("collected: %s", mustJSON(got))
	}
	foldAt(r(1_300), 2_001, 1_500, 70, edge.Add(6*time.Second))
	if got := procHistPeek(old); got != nil {
		t.Fatalf("the collected bucket came back: %v", got)
	}
	if got := procHistPeek(cur); mustJSON(got) != `{"":[10],"ffmpeg":[80,80]}` {
		t.Fatalf("new bucket after a collected old one: %s", mustJSON(got))
	}
}

// The names ride the row the history writes and are served with it;
// nothing switches them off any more (2026-10-13), so the row keeps them.
func TestHistoryCarriesProcs(t *testing.T) {
	if !diagnosticsAvailable() {
		t.Skip("process history is Linux only")
	}
	scratchHome(t)
	resetProcHist(t)
	sampler.Lock()
	sampler.on = true
	sampler.Unlock()
	reset := func() {
		hist.Lock()
		hist.loaded, hist.rows, hist.cur = false, nil, nil
		hist.Unlock()
	}
	reset()
	t.Cleanup(func() {
		sampler.Lock()
		sampler.on = false
		sampler.Unlock()
		reset()
	})
	f := func(v float64) *float64 { return &v }
	now := time.Now()
	bucket := now.Unix() - now.Unix()%histBucketSec
	at := time.Unix(bucket, 0)
	foldAt(map[int]procRead{1: {1, 0, "ffmpeg"}}, 1_000, 0, 50, at)
	foldAt(map[int]procRead{1: {1, 300, "ffmpeg"}}, 2_000, 400, 60, at.Add(10*time.Second))

	historyRecord(&Snapshot{System: &System{CPUPercent: f(40)}})
	_, pts := historyPoints()
	if len(pts) != 1 || len(pts[0]) != histProcCol+1 {
		t.Fatalf("the bucket in progress should carry its names: %v", pts)
	}
	if got := mustJSON(pts[0][histProcCol]); got != `{"":[10],"ffmpeg":[30,30]}` {
		t.Fatalf("procs: %s", got)
	}

	// Roll it into the file.
	hist.Lock()
	hist.cur.start -= 2 * histBucketSec
	hist.Unlock()
	procHist.Lock()
	procHist.bucket -= 2 * histBucketSec
	procHist.Unlock()
	historyRecord(&Snapshot{System: &System{CPUPercent: f(1)}})
	rows, b := readHistFile(t)
	if len(rows) != 1 || len(rows[0]) != histProcCol+1 {
		t.Fatalf("stored rows: %s", b)
	}

	// Reloaded from the file, where the column is a decoded map rather than
	// ours, the names are still served.
	reset()
	_, pts = historyPoints()
	if len(pts) != 1 || len(pts[0]) != histProcCol+1 {
		t.Fatalf("names dropped on reload: %v", pts)
	}
}

// Over the byte ceiling the names go before the trend does. The rows sit
// inside the thirty days (the age bound would otherwise take them first) and
// are made wide — long mount paths, long names — so a thousand of them cross
// the ceiling and the names alone are enough to bring them back under it.
func TestHistTrimDropsNamesBeforeRows(t *testing.T) {
	hist.Lock()
	defer hist.Unlock()
	saved, savedDirty := hist.rows, hist.dirty
	defer func() { hist.rows, hist.dirty = saved, savedDirty }()
	hist.rows = nil
	pad := map[string][]float64{}
	for i := 0; i < 5; i++ {
		pad["process-name-"+string(rune('a'+i))+strings.Repeat("x", 1400)] = []float64{12.3, 45.6}
	}
	sys := map[string]float64{"io_read": 123456.7, "io_write": 7654321.0}
	for i := 0; i < 16; i++ {
		sys["mount:/srv/"+strings.Repeat("v", 400)+string(rune('a'+i))] = 55.5
	}
	const n = 2000
	now := time.Now().Unix()
	for i := 0; i < n; i++ {
		hist.rows = append(hist.rows, histRow{float64(now - int64(n-i)*histBucketSec), 1.0, 2.0, 3.0, 4.0, 5.0, 6.0,
			map[string]float64{"claude": 50, "codex": 60}, map[string]int{"web": 1},
			sys, pad})
	}
	if b, _ := json.Marshal(hist.rows); len(b) <= histMaxBytes {
		t.Fatalf("the fixture is %d bytes, under the ceiling: the test proves nothing", len(b))
	}
	hist.dirty, hist.sized = false, false
	histTrim()
	if len(hist.rows) != n {
		t.Fatalf("rows were dropped while names remained to drop: %d", len(hist.rows))
	}
	total := 0
	for _, r := range hist.rows {
		b, _ := json.Marshal(r)
		total += len(b) + 1
	}
	if total > histMaxBytes {
		t.Fatalf("still %d bytes", total)
	}
	if len(hist.rows[0]) != histProcCol || len(hist.rows[n-1]) != histProcCol+1 {
		t.Errorf("the oldest names should go first: first %d, last %d", len(hist.rows[0]), len(hist.rows[n-1]))
	}
	if !hist.dirty {
		t.Errorf("rows the file holds were changed; the next save must rewrite it")
	}
}

// Rows older than thirty days leave at the next rollover, and leaving does
// not dirty the file: they are its head, which histSave accounts for.
func TestHistTrimAge(t *testing.T) {
	hist.Lock()
	defer hist.Unlock()
	saved, savedDirty := hist.rows, hist.dirty
	defer func() { hist.rows, hist.dirty = saved, savedDirty }()
	now := time.Now().Unix()
	hist.rows = []histRow{
		{float64(now - histKeepSec - 120), 1.0, 2.0, 3.0, 4.0, 5.0, 6.0},
		{float64(now - histKeepSec + 120), 1.0, 2.0, 3.0, 4.0, 5.0, 6.0},
		{float64(now - 60), 1.0, 2.0, 3.0, 4.0, 5.0, 6.0},
	}
	hist.dirty, hist.sized = false, false
	histTrim()
	if len(hist.rows) != 2 || hist.rows[0][0].(float64) != float64(now-histKeepSec+120) {
		t.Fatalf("rows after the age trim: %v", hist.rows)
	}
	if hist.dirty {
		t.Errorf("dropping the file's head should not force a rewrite")
	}
}

// readHistFile: the archive as the file holds it — one row a line.
func readHistFile(t *testing.T) ([][]any, []byte) {
	t.Helper()
	b, err := os.ReadFile(histPath())
	if err != nil {
		t.Fatal(err)
	}
	var rows [][]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r []any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		rows = append(rows, r)
	}
	return rows, b
}

// The real /proc: this process is in it, under its own name.
func TestReadProcTicksLive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	procs, total, busy, uptime, ok := readProcTicks()
	if !ok || total <= 0 || busy <= 0 || busy > total || uptime <= 0 {
		t.Fatalf("ok=%v total=%v busy=%v uptime=%v", ok, total, busy, uptime)
	}
	me, found := procs[os.Getpid()]
	if !found || me.name == "" || me.start <= 0 {
		t.Fatalf("this process: %+v (found %v) of %d", me, found, len(procs))
	}
}

func BenchmarkReadProcTicks(b *testing.B) {
	if runtime.GOOS != "linux" {
		b.Skip("reads /proc")
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		readProcTicks()
	}
}
