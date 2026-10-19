package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"subnsubmonitor/transport"
)

// A minute row with a few of everything, as row() would make it.
func minuteRow(start int64, cpu float64, procs map[string][]float64) histRow {
	r := histRow{float64(start), cpu, 50.0, 70.0, nil, 1000.0, 2000.0,
		map[string]float64{"claude": cpu}, map[string]int{"web": 1},
		map[string]float64{"io_read": cpu * 10, "mount:/": 40, "cmin:bat": cpu, "cmax:temp": cpu}}
	if procs != nil {
		r = append(r, procs)
	}
	return r
}

func resetHist(t *testing.T) {
	t.Helper()
	clear := func() {
		hist.Lock()
		hist.loaded, hist.rows, hist.cur, hist.fileRows, hist.dirty, hist.sized = false, nil, nil, -1, false, false
		hist.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// Five minutes fold into one the way a bucket folds its readings: peaks,
// the worst service, the most of a quota, the trough where asked, and the
// names merged — an average over the minutes that named it, the peak the
// highest of them.
func TestHistFoldFiveMinutes(t *testing.T) {
	base := int64(1_800_000_000) // on a five-minute boundary
	rows := []histRow{
		minuteRow(base, 10, map[string][]float64{"ffmpeg": {10, 20}, "": {5}}),
		minuteRow(base+60, 30, map[string][]float64{"ffmpeg": {20, 60}, "node": {5, 9}}),
		minuteRow(base+120, 20, nil),
		minuteRow(base+300, 90, nil), // the next five
	}
	// The worst service state in the second minute.
	rows[1][8] = map[string]int{"web": 0}
	out := histFold(rows, histCoarseSec)
	if len(out) != 2 || out[0][0].(float64) != float64(base) || out[1][0].(float64) != float64(base+300) {
		t.Fatalf("folded into %d rows: %v", len(out), out)
	}
	five := out[0]
	if five[1].(float64) != 30 {
		t.Errorf("cpu = %v, want the peak 30", five[1])
	}
	if five[4] != nil {
		t.Errorf("swap = %v, want nil when no minute had one", five[4])
	}
	if q := five[7].(map[string]float64); q["claude"] != 30 {
		t.Errorf("quota = %v, want the most used, 30", q)
	}
	if s := five[8].(map[string]int); s["web"] != 0 {
		t.Errorf("services = %v, want the worst state seen, 0", s)
	}
	sys := five[9].(map[string]float64)
	if sys["io_read"] != 300 || sys["cmax:temp"] != 30 || sys["cmin:bat"] != 10 || sys["mount:/"] != 40 {
		t.Errorf("system = %v: want io_read 300, cmax 30, cmin 10 (the trough), mount 40", sys)
	}
	if len(five) != histProcCol+1 {
		t.Fatalf("the five-minute row should carry the merged names: %v", five)
	}
	procs := five[histProcCol].(map[string][]float64)
	if got := mustJSON(procs); got != `{"":[5],"ffmpeg":[15,60],"node":[5,9]}` {
		t.Errorf("merged names: %s", got)
	}
	// Six names across the minutes: the sixth is not lost but goes to the
	// remainder.
	six := []histRow{
		minuteRow(base, 10, map[string][]float64{"a": {10, 10}, "b": {9, 9}, "c": {8, 8}, "d": {7, 7}, "e": {6, 6}}),
		minuteRow(base+60, 10, map[string][]float64{"f": {1, 2}, "": {3}}),
	}
	sp := histFold(six, histCoarseSec)[0][histProcCol].(map[string][]float64)
	if _, has := sp["f"]; has || len(sp) != 6 || sp[procOtherKey][0] != 4 {
		t.Errorf("sixth name should fold into the remainder (1 + 3 = 4): %v", sp)
	}
	// Two readings thirty seconds apart make one minute; a minute apart, two.
	two := []histRow{six[0], minuteRow(base+histBucketSec, 10, map[string][]float64{"f": {1, 2}, "": {3}})}
	if m := histFold(two, histMinuteSec); len(m) != 1 || m[0][histProcCol].(map[string][]float64)[procOtherKey][0] != 4 {
		t.Errorf("two readings should fold into one minute with the remainder 4: %v", m)
	}
	if m := histFold(six, histMinuteSec); len(m) != 2 {
		t.Errorf("readings a minute apart should stay two minutes: %v", m)
	}
	// Rows already decoded from the file — maps of any — fold the same.
	b, _ := json.Marshal(rows)
	var decoded []histRow
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	again := histFold(decoded, histCoarseSec)
	if mustJSON(again) != mustJSON(out) {
		t.Errorf("decoded rows fold differently:\n%s\n%s", mustJSON(again), mustJSON(out))
	}
	// At the archive's own step a row is itself.
	if same := histFold(rows, histBucketSec); len(same) != len(rows) {
		t.Errorf("step 60 should not fold: %d rows", len(same))
	}
}

// A page is the newest rows that fit; `more` says there are older ones;
// from/to bound the span; the archive's oldest is named whatever was asked;
// an unknown step reads as five minutes.
func TestHistoryServePages(t *testing.T) {
	scratchHome(t)
	resetHist(t)
	now := time.Now().Unix()
	now -= now % histCoarseSec
	const n = 600 // five hours of readings
	hist.Lock()
	for i := n; i > 0; i-- {
		hist.rows = append(hist.rows, minuteRow(now-int64(i)*histBucketSec, float64(i%100), nil))
	}
	hist.loaded = true
	hist.Unlock()
	oldest := now - n*histBucketSec

	whole := historyServe(histBucketSec, 0, 0, 0)
	if len(whole.Points) != n || whole.From != oldest || whole.More || whole.Oldest != oldest || whole.Newest != now-histBucketSec {
		t.Fatalf("unbounded page: %d rows from %d (oldest %d) more=%v newest=%d", len(whole.Points), whole.From, whole.Oldest, whole.More, whole.Newest)
	}
	rowBytes := len(mustJSON(whole.Points[0])) + 1
	// A budget for about fifty rows: the page is the NEWEST fifty, and more.
	page := historyServe(histBucketSec, 0, 0, rowBytes*50+2)
	if len(page.Points) < 45 || len(page.Points) > 50 || !page.More || page.Newest != now-histBucketSec || page.Oldest != oldest {
		t.Fatalf("budgeted page: %d rows more=%v newest=%d oldest=%d", len(page.Points), page.More, page.Newest, page.Oldest)
	}
	if page.From != int64(page.Points[0][0].(float64)) {
		t.Errorf("from %d is not the first row's start %v", page.From, page.Points[0][0])
	}
	// The next page: everything before this one's from, within the same budget.
	next := historyServe(histBucketSec, 0, page.From, rowBytes*50+2)
	if next.Newest != page.From-histBucketSec || !next.More {
		t.Errorf("next page should end just before the last one: newest %d, want %d (more %v)", next.Newest, page.From-histBucketSec, next.More)
	}
	// A span inside the archive, and one that reaches its start: no more.
	span := historyServe(histBucketSec, oldest+600, oldest+1200, 0)
	if len(span.Points) != 600/histBucketSec || span.From != oldest+600 || span.More {
		t.Errorf("span: %d rows from %d more=%v", len(span.Points), span.From, span.More)
	}
	head := historyServe(histBucketSec, 0, oldest+600, 0)
	if len(head.Points) != 600/histBucketSec || head.More {
		t.Errorf("head: %d rows more=%v", len(head.Points), head.More)
	}
	// A span past the newest row: nothing, but the archive's oldest still named.
	none := historyServe(histBucketSec, now+3600, 0, 0)
	if len(none.Points) != 0 || none.More || none.Oldest != oldest || none.From != 0 {
		t.Errorf("empty span: %+v", none)
	}
	// Five minutes is the default and the fold.
	coarse := historyServe(0, 0, 0, 0)
	if want := n * histBucketSec / histCoarseSec; coarse.Step != histCoarseSec || len(coarse.Points) != want {
		t.Errorf("default step: %d, %d rows (want %d)", coarse.Step, len(coarse.Points), want)
	}
	if mid := historyServe(histMinuteSec, 0, 0, 0); mid.Step != histMinuteSec || len(mid.Points) != n*histBucketSec/histMinuteSec {
		t.Errorf("minute step: %d, %d rows", mid.Step, len(mid.Points))
	}
	if odd := historyServe(120, 0, 0, 0); odd.Step != histCoarseSec {
		t.Errorf("step 120 should read as %d, got %d", histCoarseSec, odd.Step)
	}
}

// The file: a line appended per rollover, a rewrite once a day of rows has
// left, a torn last line survived, the five-minute file carried over.
func TestHistoryFileAppendsAndCompacts(t *testing.T) {
	scratchHome(t)
	resetHist(t)
	sampler.Lock()
	sampler.on = true
	sampler.Unlock()
	t.Cleanup(func() {
		sampler.Lock()
		sampler.on = false
		sampler.Unlock()
	})
	f := func(v float64) *float64 { return &v }
	// roll closes one bucket: the one in progress is moved k minutes back
	// and the next reading opens a new one. Successive calls use a smaller k
	// so the closed starts climb (a row is appended only when newer).
	roll := func(k int64) {
		snap := &Snapshot{System: &System{CPUPercent: f(1)}}
		historyRecord(snap)
		hist.Lock()
		hist.cur.start -= k * histBucketSec
		hist.Unlock()
		historyRecord(snap)
	}
	roll(6)
	roll(5)
	rows, _ := readHistFile(t)
	if len(rows) != 2 {
		t.Fatalf("two rollovers, %d lines", len(rows))
	}
	hist.Lock()
	fileRows := hist.fileRows
	hist.Unlock()
	if fileRows != 2 {
		t.Fatalf("fileRows = %d, want 2", fileRows)
	}
	// A torn tail (a crash mid-append) does not take the archive with it, and
	// is written out at the next save.
	fh, _ := os.OpenFile(histPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	fh.WriteString(`[1800000000,1`)
	fh.Close()
	resetHist(t)
	hist.Lock()
	histLoad()
	n, dirty := len(hist.rows), hist.dirty
	hist.Unlock()
	if n != 2 || !dirty {
		t.Fatalf("after a torn line: %d rows, dirty=%v", n, dirty)
	}
	roll(4)
	rows, b := readHistFile(t)
	if len(rows) != 3 || strings.Contains(string(b), `[1800000000,1`) {
		t.Fatalf("the rewrite should have dropped the torn line: %d rows\n%s", len(rows), b)
	}
	// A file whose last line lost its newline (a crash between the row and
	// the break) is rewritten before anything is appended to it, so the
	// next row cannot be glued onto this one.
	raw, _ := os.ReadFile(histPath())
	os.WriteFile(histPath(), raw[:len(raw)-1], 0o600)
	resetHist(t)
	hist.Lock()
	histLoad()
	n, dirty = len(hist.rows), hist.dirty
	hist.Unlock()
	if n != 3 || !dirty {
		t.Fatalf("a file without its last newline: %d rows, dirty=%v", n, dirty)
	}
	roll(3)
	if rows, _ = readHistFile(t); len(rows) != 4 {
		t.Fatalf("after the rewrite and one append: %d rows", len(rows))
	}
	// The file is never let past what the loader reads: one more line past
	// the ceiling and its slack means a rewrite rather than an append.
	hist.Lock()
	hist.fileBytes = histMaxBytes + histSlack() - 10
	hist.fileRows = len(hist.rows)
	hist.Unlock()
	roll(2)
	hist.Lock()
	fileBytes, n := hist.fileBytes, len(hist.rows)
	st, _ := os.Stat(histPath())
	hist.Unlock()
	if fileBytes > histMaxBytes+histSlack() || st == nil || int(st.Size()) != fileBytes || n != 5 {
		t.Fatalf("over the ceiling: fileBytes %d, file %v, rows %d", fileBytes, st, n)
	}
	// Rows the archive lets go stay in the file until a day of them has
	// gone; then the file is rewritten to the kept rows.
	hist.Lock()
	hist.fileRows = len(hist.rows) + histCompactAt
	hist.Unlock()
	roll(1)
	hist.Lock()
	fileRows, n = hist.fileRows, len(hist.rows)
	hist.Unlock()
	if fileRows != n {
		t.Fatalf("a day of dead rows should trigger a rewrite: fileRows %d, rows %d", fileRows, n)
	}
	// Names stripped from rows the file holds: the next save rewrites.
	hist.Lock()
	hist.rows[0] = append(hist.rows[0][:histProcCol:histProcCol], map[string][]float64{"x": {1, 2}})
	histStripProcs()
	dirty = hist.dirty
	hist.Unlock()
	if !dirty {
		t.Fatalf("stripping names should mark the file for a rewrite")
	}
}

// An archive at the byte ceiling is trimmed to the low-water mark and then
// appended to, not rewritten on every rollover (audit 2026-10-16: trimming
// to the ceiling itself left no room for the next row, so a full archive
// rewrote 24 MiB every thirty seconds).
func TestHistoryCeilingRewritesOnce(t *testing.T) {
	scratchHome(t)
	resetHist(t)
	was := histMaxBytes
	histMaxBytes = 64 << 10
	t.Cleanup(func() { histMaxBytes = was })
	sampler.Lock()
	sampler.on = true
	sampler.Unlock()
	t.Cleanup(func() {
		sampler.Lock()
		sampler.on = false
		sampler.Unlock()
	})
	// A month's archive in miniature: wide rows, newest a minute ago, just
	// past the ceiling.
	now := time.Now().Unix()
	now -= now % histBucketSec
	wide := func(start int64) histRow {
		r := minuteRow(start, 12.5, nil)
		sys := map[string]float64{}
		for i := 0; i < 12; i++ {
			sys["mount:/srv/volume-"+strconv.Itoa(i)] = float64(i)
		}
		r[9] = sys
		return r
	}
	hist.Lock()
	histLoad()
	size := 0
	var rows []histRow
	for i := 0; size <= histMaxBytes+2000; i++ {
		r := wide(0)
		rows = append(rows, r)
		size += histRowSize(r)
	}
	// Ending well before the rollovers below, whose starts climb to now.
	for i := range rows {
		rows[i][0] = float64(now - int64(len(rows)-i+50)*histBucketSec)
	}
	hist.rows = rows
	hist.sized = false
	histCompact()
	hist.Unlock()
	f := func(v float64) *float64 { return &v }
	// Each call closes one bucket, k buckets back, so the closed starts
	// climb as k falls.
	roll := func(k int64) {
		snap := &Snapshot{System: &System{CPUPercent: f(1)}}
		historyRecord(snap)
		hist.Lock()
		hist.cur.start -= k * histBucketSec
		hist.Unlock()
		historyRecord(snap)
	}
	before := histRewrites
	for k := int64(40); k >= 1; k-- {
		roll(k)
	}
	hist.Lock()
	size, n := hist.size, len(hist.rows)
	hist.Unlock()
	if got := histRewrites - before; got > 1 {
		t.Fatalf("40 rollovers at the ceiling rewrote the file %d times, want at most 1", got)
	}
	if size > histMaxBytes || size > histLowWater()+40*histRowSize(wide(0)) {
		t.Fatalf("archive size %d after the trim, low water %d, ceiling %d", size, histLowWater(), histMaxBytes)
	}
	if rows, _ := readHistFile(t); len(rows) != n {
		t.Fatalf("the file holds %d rows, the archive %d", len(rows), n)
	}
}

// A beacon carries the newest closed five-minute buckets, folded and
// without process names, never the bucket still filling.
func TestHistoryCoarseClosed(t *testing.T) {
	scratchHome(t)
	resetHist(t)
	now := time.Now().Unix()
	cur := now - now%histBucketSec
	five := cur - cur%histCoarseSec
	hist.Lock()
	histLoad()
	for start := five - 3*histCoarseSec; start < cur; start += histBucketSec {
		hist.rows = append(hist.rows, minuteRow(start, float64((start/histBucketSec)%7), map[string][]float64{"secret": {50, 90}}))
	}
	hist.cur = &histBucket{start: cur, quota: map[string]float64{}, services: map[string]int{}, system: map[string]float64{}}
	hist.Unlock()
	got := historyCoarseClosed(2)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %v", len(got), got)
	}
	if got[0][0].(float64) != float64(five-2*histCoarseSec) || got[1][0].(float64) != float64(five-histCoarseSec) {
		t.Fatalf("starts %v %v, want the two buckets before %d", got[0][0], got[1][0], five)
	}
	for _, r := range got {
		if len(r) > histProcCol {
			t.Fatalf("process names left the archive: %v", r)
		}
		if b, _ := json.Marshal(r); strings.Contains(string(b), "secret") {
			t.Fatalf("a program name rode the beacon: %s", b)
		}
	}
	if none := historyCoarseClosed(0); none != nil {
		t.Fatalf("n=0 gave %v", none)
	}
}

func TestHistoryLegacyFileCarriedOver(t *testing.T) {
	scratchHome(t)
	resetHist(t)
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	start := now - now%histCoarseSec - 3*histCoarseSec
	legacy := []histRow{
		{float64(start), 10.0, 20.0, 30.0, nil, 1.0, 2.0, map[string]float64{}, map[string]int{}, map[string]float64{}},
		{float64(start + histCoarseSec), 11.0, 20.0, 30.0, nil, 1.0, 2.0, map[string]float64{}, map[string]int{}, map[string]float64{}},
		{"bad"},
	}
	b, _ := json.Marshal(legacy)
	lp := filepath.Join(dir, histLegacyFile)
	if err := os.WriteFile(lp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	hist.Lock()
	histLoad()
	n := len(hist.rows)
	hist.Unlock()
	if n != 2 {
		t.Fatalf("legacy rows carried over: %d, want 2", n)
	}
	if _, err := os.Stat(lp); !os.IsNotExist(err) {
		t.Errorf("the legacy file should be put away once carried over: %v", err)
	}
	rows, _ := readHistFile(t)
	if len(rows) != 2 {
		t.Fatalf("the new file should hold the carried rows: %d", len(rows))
	}
	// They keep their five-minute starts and fold like any other row.
	s := historyServe(histCoarseSec, 0, 0, 0)
	if len(s.Points) != 2 || s.Points[1][1].(float64) != 11 {
		t.Errorf("served legacy rows: %v", s.Points)
	}
}

// GET /v1/history: the parameters reach historyServe, the answer names the
// page, and it fits the frame whatever the budget estimate missed.
func TestHistoryHandlerPages(t *testing.T) {
	scratchHome(t)
	resetHist(t)
	now := time.Now().Unix()
	now -= now % histCoarseSec
	hist.Lock()
	// Rows fat enough that thirty days of them would never fit one frame:
	// four hundred readings at ~12 KB each is about five megabytes.
	fat := map[string]float64{}
	for i := 0; i < 24; i++ {
		fat["mount:/"+strings.Repeat("p", 450)+string(rune('a'+i))] = 50
	}
	for i := 400; i > 0; i-- {
		r := minuteRow(now-int64(i)*histBucketSec, float64(i%100), nil)
		r[9] = fat
		hist.rows = append(hist.rows, r)
	}
	hist.loaded = true
	hist.Unlock()
	h := newDirectHandler()
	get := func(q string) map[string]any {
		rec := httptest.NewRecorder()
		h.history(rec, httptest.NewRequest(http.MethodGet, "/v1/history"+q, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		if rec.Body.Len() > transport.Limits[transport.TypeHistory] {
			t.Fatalf("%s: %d bytes, over the frame limit", q, rec.Body.Len())
		}
		var f struct {
			Type string         `json:"type"`
			Body map[string]any `json:"body"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil || f.Type != "history" {
			t.Fatalf("%s: %v %s", q, err, rec.Body.String())
		}
		return f.Body
	}
	plain := get("")
	if plain["bucket_sec"].(float64) != histCoarseSec || plain["more"] != false || len(plain["points"].([]any)) != 400*histBucketSec/histCoarseSec {
		t.Errorf("no parameters: step %v more %v rows %d", plain["bucket_sec"], plain["more"], len(plain["points"].([]any)))
	}
	fine := get("?step=30")
	pts := fine["points"].([]any)
	if fine["bucket_sec"].(float64) != histBucketSec || fine["more"] != true || len(pts) >= 400 || len(pts) < 40 {
		t.Fatalf("step=60: more %v rows %d", fine["more"], len(pts))
	}
	from := int64(fine["from"].(float64))
	if from != int64(pts[0].([]any)[0].(float64)) || fine["oldest"].(float64) != float64(now-400*histBucketSec) {
		t.Errorf("from %d oldest %v", from, fine["oldest"])
	}
	next := get("?step=30&to=" + strconv.FormatInt(from, 10))
	if int64(next["newest"].(float64)) != from-histBucketSec {
		t.Errorf("the next page should end at %d, got %v", from-histBucketSec, next["newest"])
	}
	if _, ok := next["outages"].([]any); !ok {
		t.Errorf("outages should ride on every page")
	}
	if bad := get("?step=abc&from=-5&to=zzz"); bad["bucket_sec"].(float64) != histCoarseSec {
		t.Errorf("unparseable parameters should read as the defaults: %v", bad["bucket_sec"])
	}
}
