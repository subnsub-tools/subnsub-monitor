package main

// The machine's own history — kept HERE, on the box that produced it, and
// served over the tunnel on request. The relay used to be
// the only place a trend survived a page reload; now a machine on the direct
// road carries its own, and better: the helper is always running, so the
// strip fills in whether or not anybody was watching.
//
// The rows are the dashboard's own bucket shape,
// fold for fold — peak of the sampled lanes for
// cpu/mem/rx/tx, latest for disk/swap, the worst used-percent per provider,
// and each service's state — so the page renders them with the code it has.
//
// THE GRAIN: the archive is THIRTY-SECOND buckets — one per
// reading, the sampler's own cadence — for THIRTY DAYS. The relay's copy and
// the page's default view stay at five minutes, so the page asks for
// `step=300` by default and the helper folds the rows on the way out
// (historyServe); a reader who wants more asks for `step=60` or `step=30`.
// Thirty days of readings is 86 400 rows — a dozen megabytes on disk, far
// more than one tunnel frame — so the server pages: newest first, as many
// rows as fit the frame, and `more` says whether older ones exist for the
// next ask (`to=<first start>`).
//
// Bounded three ways: age (thirty days), a row cap under it, and a byte
// ceiling, whichever bites first. The file is JSON Lines, one bucket a line,
// APPENDED on each rollover — rewriting eight megabytes every minute would be
// twelve gigabytes a day on a machine whose disk the helper is meant to watch,
// not wear — and rewritten whole (write-then-rename, 0600) only when it holds
// a day of rows the archive has since let go, or when process names must
// leave it. A history.json from the five-minute era is read once and carried
// over; its rows keep their five-minute starts and fold like any other.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	histBucketSec  = 30              // the archive's grain: one reading (thirty-second buckets, thirty days)
	histMinuteSec  = 60              // the middle step a reader may ask for
	histCoarseSec  = 300             // the page's default step, folded on request
	histKeepSec    = 30 * 24 * 3600  // thirty days
	histKeepRows   = 31 * 24 * 120   // a row cap under the age bound: a clock jump cannot grow the file without limit
	histFileName   = "history.jsonl" // one bucket a line, appended
	histLegacyFile = "history.json"  // the five-minute era's array, read once
	histCompactAt  = 24 * 120        // rewrite the file once it carries a day of rows the archive no longer keeps
	histMountMax   = 32
	histPathMax    = 512
	histCustomMax  = 8 // the relay's HIST_CUSTOM_MAX
	// A step of the clock back within this long of boot is the boot's clock
	// being put right (historyRecord), and the rows it leaves in the future
	// were written by the wrong one.
	histEarlyStepSec = 30 * 60
)

// The recorder's clock: a variable only so the tests can step it.
var histNow = time.Now

// The byte ceiling: 86 400 rows at ~200 bytes is 17 MB, but a machine with
// diagnostics on writes ~315 bytes a row and reaches it before thirty days
// do; names go first, then rows, when it bites. A variable only so the tests
// can shrink it.
var histMaxBytes = 24 << 20

// histLowWater is where a trim past the ceiling stops: a tenth under it.
// Trimming to the ceiling itself left no room for the next row, so once an
// archive reached it EVERY rollover stripped a row's names (or dropped a
// row) and rewrote the whole file — 24 MiB every thirty seconds, some 70 GB
// a day on the disk this helper is meant to watch (audit 2026-10-16). A
// tenth is about three days of rows at the widest; the file is rewritten
// once, then appended to until the ceiling bites again.
func histLowWater() int { return histMaxBytes - histMaxBytes/10 }

// histSlack is how far past the ceiling the FILE may run — the kept rows are
// held under the ceiling by histTrim, the rest is the dead head a daily
// compaction reclaims — before an append becomes a rewrite. Under the
// loader's own allowance (the ceiling and 1 MiB) at every size.
func histSlack() int { return histMaxBytes / 32 }

// histRewrites counts the whole-file rewrites this process has made; the
// tests read it to hold the archive to one rewrite per trim, not per row.
var histRewrites int

// One row: [bucket_start, cpu, mem, disk, swap, rx, tx, {provider: worst%},
// {service: status}, {io_read/io_write/mount:<path>: peak},
// {process name: [avg%, peak%], "": [the rest, avg%]}]. Numbers are one-decimal
// floats or nil. The last two are optional: old rows lack the system map, and
// only a machine with diagnostics on writes the process one (prochist.go).
type histRow = []any

type histBucket struct {
	start                        int64
	cpu, mem, rx, tx, disk, swap *float64 // peaks, all six — the page's rule
	quota                        map[string]float64
	services                     map[string]int
	system                       map[string]float64
	// Which programs had the CPU (prochist.go). Filled only as the row is
	// made; nil on a machine that has not switched diagnostics on.
	procs map[string][]float64
}

// The keys of a row's system map that are SUMS over the bucket rather than
// its peak: the bytes the interfaces moved in it (histBytes), so any step's
// fold adds them up and a month's traffic is the sum of a month's rows. These
// two by name: an operator's tile or a mount may end in "_bytes" too.
func histSumKey(k string) bool { return k == "rx_bytes" || k == "tx_bytes" }

var hist struct {
	sync.Mutex
	loaded bool
	rows   []histRow
	cur    *histBucket
	// The interfaces' byte counters at the last reading (histBytes): what
	// the next reading's are measured against. Zero until one is seen.
	lastRx, lastTx float64
	haveTot        bool
	// The first bucket this process added to the archive: the rows a step of
	// the clock soon after boot may take back are from here on.
	procFirst int64
	// fileRows is how many lines the file holds, kept or not; the gap
	// between it and len(rows) is what a compaction would reclaim. -1 until
	// the file has been read or written by this process. fileBytes is the
	// file's size as this process knows it (read, written, appended): the
	// second trigger for a rewrite, so the file never outgrows what the
	// loader will read back (a month of wide rows near
	// the ceiling plus a few hundred dead ones would have crossed it, and
	// the next start would have thrown the whole archive away).
	fileRows  int
	fileBytes int
	// dirty: the file says something the archive no longer does — names
	// were stripped — and the next save must rewrite it rather than append.
	dirty bool
	// size is the rows' serialised size, a line a row, kept as rows come
	// and go so the byte ceiling need not re-measure forty thousand rows
	// every minute; sized says it has been counted since the rows last
	// changed under it (a load, a strip).
	size  int
	sized bool
}

// histRowSize: a row's share of the file — its JSON and a newline.
func histRowSize(r histRow) int {
	b, err := json.Marshal(r)
	if err != nil {
		return 0
	}
	return len(b) + 1
}

func histPath() string {
	dir := configDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, histFileName)
}

// histRowOK: a row is trusted only in shape — at least the seven positional
// columns, the first a number — the same test the old reader applied.
func histRowOK(r histRow) bool {
	if len(r) < 7 {
		return false
	}
	_, ok := r[0].(float64)
	return ok
}

func histLoad() {
	if hist.loaded {
		return
	}
	hist.loaded = true
	hist.fileRows = -1
	hist.dirty = false
	hist.sized = false
	p := histPath()
	if p == "" {
		return
	}
	if f, err := os.Open(p); err == nil {
		defer f.Close()
		if st, err := f.Stat(); err != nil || st.Size() > int64(histMaxBytes)+(1<<20) {
			// Past the ceiling plus a day's slack: not ours, or grown out of
			// hand. Start over rather than hold megabytes nobody bounded.
			hist.fileRows = 0
			hist.dirty = true
			return
		}
		if st, err := f.Stat(); err == nil {
			hist.fileBytes = int(st.Size())
			// A file that does not end in a newline — a crash between the
			// row and its line break — would glue the next appended row to
			// this one, and lose both at the next read. Rewritten whole
			// before anything is appended.
			if st.Size() > 0 {
				tail := make([]byte, 1)
				if _, err := f.ReadAt(tail, st.Size()-1); err != nil || tail[0] != '\n' {
					hist.dirty = true
				}
			}
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		n := 0
		var last float64 = -1
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			n++
			var r histRow
			if json.Unmarshal(line, &r) != nil || !histRowOK(r) {
				hist.dirty = true // a torn line (a crash mid-append): rewrite it out
				continue
			}
			if start := r[0].(float64); start <= last {
				hist.dirty = true // out of order or repeated: a compaction restores the invariant
				continue
			} else {
				last = start
			}
			hist.rows = append(hist.rows, r)
		}
		hist.fileRows = n
		return
	}
	// No archive of this shape: the five-minute era's array, if it is there,
	// becomes the first rows of this one and is then put away. Its rows keep
	// their five-minute starts; historyServe folds them like any other.
	lp := filepath.Join(filepath.Dir(p), histLegacyFile)
	b, err := os.ReadFile(lp)
	if err != nil || len(b) > (2<<20) {
		hist.fileRows = 0
		return
	}
	var rows []histRow
	if json.Unmarshal(b, &rows) != nil {
		hist.fileRows = 0
		return
	}
	var last float64 = -1
	for _, r := range rows {
		if histRowOK(r) && r[0].(float64) > last {
			last = r[0].(float64)
			hist.rows = append(hist.rows, r)
		}
	}
	hist.fileRows = 0
	hist.dirty = true // written out in the new shape on the first rollover
	if histCompact() {
		os.Remove(lp)
	}
}

// histCompact writes the archive whole: the kept rows, one a line,
// write-then-rename beside the id. Reports success.
func histCompact() bool {
	p := histPath()
	if p == "" {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return false
	}
	var buf bytes.Buffer
	for _, r := range hist.rows {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	// Written, synced, then renamed over the old file: a crash between the
	// rename and the kernel's own flush must not leave an empty archive in
	// place of a month of rows. The sync is affordable because this runs
	// about once a day (histLowWater), not once a row.
	tmp := p + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	_, werr := f.Write(buf.Bytes())
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		os.Remove(tmp)
		return false
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return false
	}
	histRewrites++
	hist.fileRows = len(hist.rows)
	hist.fileBytes = buf.Len()
	hist.dirty = false
	return true
}

// histSave puts the archive on disk after a rollover: the new last row is
// appended, unless the file needs rewriting — names stripped, a torn line
// seen, a day of rows let go since the last rewrite, or no file yet.
func histSave() {
	p := histPath()
	if p == "" {
		return
	}
	n := len(hist.rows)
	if n == 0 {
		// Nothing kept (a clock that stepped a month back trims everything):
		// the file is written empty rather than indexed past its end.
		histCompact()
		return
	}
	// fileRows counts every line since the last rewrite — the kept rows but
	// the newest, plus the ones trimmed since — so the live rows are the
	// file's suffix and fileRows-(n-1) is the dead head.
	dead := hist.fileRows - (n - 1)
	last := hist.rows[n-1]
	b, err := json.Marshal(last)
	if err != nil {
		return
	}
	// A rewrite when the file says something the rows no longer do, when a
	// day of rows has left it, or when one more line would carry it past
	// what the loader reads back (histLoad allows the ceiling and 1 MiB;
	// the rows themselves are held under the ceiling by histTrim, so only
	// dead head rows can carry the file into the slack).
	if hist.dirty || hist.fileRows < 0 || n == 0 || dead < 0 || dead >= histCompactAt || hist.fileBytes+len(b)+1 > histMaxBytes+histSlack() {
		histCompact()
		return
	}
	// The file holds every row but the newest: append that one.
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		histCompact()
		return
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		hist.dirty = true
		return
	}
	hist.fileRows++
	hist.fileBytes += len(b) + 1
}

// serviceRank mirrors monitor.js: up 1 → 0, maintenance 3 → 1, pending 2 → 2,
// down 0 → 3; anything else is unknown and never wins.
func serviceRank(status int) int {
	switch status {
	case 1:
		return 0
	case 3:
		return 1
	case 2:
		return 2
	case 0:
		return 3
	}
	return -1
}

func round1(v *float64) any {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	return math.Round(*v*10) / 10
}

func (b *histBucket) row() histRow {
	q := map[string]float64{}
	for k, v := range b.quota {
		q[k] = math.Round(v*10) / 10
	}
	s := map[string]int{}
	for k, v := range b.services {
		s[k] = v
	}
	sys := map[string]float64{}
	for k, v := range b.system {
		switch {
		case strings.HasPrefix(k, "cmax:") || strings.HasPrefix(k, "cmin:"):
			// Four places, as the relay and the page keep them: a balance of
			// 0.0423 or a ratio of 0.734 is not 0 and 0.7 (audit 2026-10-16).
			sys[k] = math.Round(v*10000) / 10000
		case histSumKey(k) || k == "boot":
			sys[k] = math.Round(v)
		default:
			sys[k] = math.Round(v*10) / 10
		}
	}
	row := histRow{float64(b.start), round1(b.cpu), round1(b.mem), round1(b.disk), round1(b.swap),
		round1(b.rx), round1(b.tx), q, s, sys}
	if len(b.procs) > 0 {
		row = append(row, b.procs)
	}
	return row
}

// histProcCol is where a row keeps its process names, when it has any.
const histProcCol = 10

// histStripProcs takes the process names back out of every stored row: the
// switch that allowed them is off, and "off" has to mean the names are gone
// from this file, not merely unserved. Reports whether anything changed.
func histStripProcs() bool {
	changed := false
	for i, r := range hist.rows {
		if len(r) > histProcCol {
			hist.rows[i] = r[:histProcCol:histProcCol]
			changed = true
		}
	}
	if changed {
		hist.dirty = true // the file still has them; the next save rewrites it
		hist.sized = false
	}
	return changed
}

// histTrim holds the archive to its three bounds: thirty days, the row cap
// under it, and the byte ceiling. Over the ceiling the process names go
// first, oldest row first, and only then whole rows: the trend line is what
// every other part of the page is drawn from, and a month of it is worth
// more than a month of who-was-busy. Dropping old rows leaves the file as
// it was (they are its head, and histSave counts them); stripping names
// changes rows the file holds, so that marks it for a rewrite.
func histTrim() {
	// The byte ceiling is measured in the file's own shape — a line a row —
	// over the rows the age and row bounds keep. The sum is carried between
	// calls (hist.size) and re-counted only after the rows changed under
	// it; rows leave from the front, so each departure is subtracted.
	if !hist.sized {
		hist.size = 0
		for _, r := range hist.rows {
			hist.size += histRowSize(r)
		}
		hist.sized = true
	}
	floor := float64(time.Now().Unix() - histKeepSec)
	cut := 0
	for cut < len(hist.rows) && (hist.rows[cut][0].(float64) < floor || len(hist.rows)-cut > histKeepRows) {
		hist.size -= histRowSize(hist.rows[cut])
		cut++
	}
	if cut > 0 {
		hist.rows = hist.rows[cut:]
	}
	// Past the ceiling, down to the low-water mark rather than to the
	// ceiling: one rewrite now, then days of appends (histLowWater).
	if hist.size > histMaxBytes {
		target := histLowWater()
		for i := 0; hist.size > target && i < len(hist.rows); i++ {
			if r := hist.rows[i]; len(r) > histProcCol {
				hist.size -= histRowSize(r)
				hist.rows[i] = r[:histProcCol:histProcCol]
				hist.size += histRowSize(hist.rows[i])
				hist.dirty = true
			}
		}
		for hist.size > target && len(hist.rows) > 1 {
			hist.size -= histRowSize(hist.rows[0])
			hist.rows = hist.rows[1:]
		}
	}
	if hist.size < 0 {
		hist.sized = false // drifted; counted afresh next time
	}
}

func validPeak(v *float64, max float64) (float64, bool) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > max {
		return 0, false
	}
	return *v, true
}

func systemPeak(dst map[string]float64, key string, v *float64, max float64) {
	if x, ok := validPeak(v, max); ok {
		if old, exists := dst[key]; !exists || x > old {
			dst[key] = x
		}
	}
}

// validCustom: a custom value the history may keep. Unlike the system peaks
// these may be negative (a temperature, a balance), so only the shape of the
// number is checked.
func validCustom(v float64) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) >= 1e15 {
		return 0, false
	}
	return v, true
}

// statusSeverity: a status tile's state as the number its history keeps —
// 0 ok, 1 warn, 2 bad — so the bucket's peak is its worst state, under the
// same cmax:<id> key a number uses. pending and unknown are not states the
// thing was in, and a stale or failed tile is not a claim about this bucket:
// neither writes anything. The relay and the page use the same three numbers.
func statusSeverity(c Tile) (float64, bool) {
	if c.Stale || c.Error != "" {
		return 0, false
	}
	switch c.State {
	case "ok":
		return 0, true
	case "warn":
		return 1, true
	case "bad":
		return 2, true
	}
	return 0, false
}

// histCustomFold: the operator's own numbers that asked for a trend, under
// cmax:<id> (the bucket's peak) or cmin:<id> (its trough — an inverted gauge,
// a battery, wants the dip kept). Same keys the relay and the page write, so
// a tunnel's history and a relay's render alike. Bounded to histCustomMax
// keys already in the bucket plus new ones up to that count.
func histCustomFold(dst map[string]float64, tiles []Tile, okSrc map[string]bool) {
	n := 0
	for k := range dst {
		if strings.HasPrefix(k, "cmax:") || strings.HasPrefix(k, "cmin:") {
			n++
		}
	}
	for _, c := range tiles {
		if !c.History || c.ID == "" {
			continue
		}
		// A tile whose source failed keeps its last good value on the card;
		// written into the record it would be a flat line where the record
		// should show a gap (audit 2026-10-16) — the relay skips it too.
		if okSrc != nil && !okSrc[c.Source] {
			continue
		}
		var x float64
		switch c.Type {
		case "gauge", "number":
			// A stale or failed value is not a measurement of this bucket.
			if c.Value == nil || c.Stale || c.Error != "" {
				continue
			}
			var ok bool
			if x, ok = validCustom(*c.Value); !ok {
				continue
			}
		case "status":
			var ok bool
			if x, ok = statusSeverity(c); !ok {
				continue
			}
		default:
			continue
		}
		// A status has no trough: its worst state is always the peak.
		lower := c.Fold == "min" && c.Type != "status"
		key := "cmax:" + c.ID
		if lower {
			key = "cmin:" + c.ID
		}
		old, have := dst[key]
		if !have {
			if n >= histCustomMax {
				continue
			}
			n++
			dst[key] = x
			continue
		}
		if (lower && x < old) || (!lower && x > old) {
			dst[key] = x
		}
	}
}

// histBytes adds to the bucket what the interfaces moved since the last
// reading, from their byte counters (rx_bytes, tx_bytes): a sum, so a month
// of rows says a month's traffic exactly, whatever the step it is read at.
// The counters are the interfaces' total, so one that fell says an interface
// went (or one was reset), not how much the others moved: that reading adds
// nothing and the next is measured from it. A delta past what the fastest
// link could move in the gap is not traffic either.
func histBytes(b *histBucket, sys *System) {
	if sys.NetRxTotal == nil || sys.NetTxTotal == nil {
		return
	}
	rx, tx := *sys.NetRxTotal, *sys.NetTxTotal
	if math.IsNaN(rx) || math.IsNaN(tx) || rx < 0 || tx < 0 {
		return
	}
	if hist.haveTot {
		d := func(cur, last float64) float64 {
			if cur >= last {
				return cur - last
			}
			return 0
		}
		dr, dt := d(rx, hist.lastRx), d(tx, hist.lastTx)
		const histBytesMax = 400e9 // 100 Gb/s for half a minute and a little
		if dr <= histBytesMax {
			b.system["rx_bytes"] += dr
		}
		if dt <= histBytesMax {
			b.system["tx_bytes"] += dt
		}
	}
	hist.lastRx, hist.lastTx, hist.haveTot = rx, tx, true
}

func mountPeak(dst map[string]float64, key string, value float64) {
	if old, exists := dst[key]; exists {
		if value > old {
			dst[key] = value
		}
		return
	}
	n, largest := 0, ""
	for k := range dst {
		if len(k) > 6 && k[:6] == "mount:" {
			n++
			if k > largest {
				largest = k
			}
		}
	}
	if n >= histMountMax {
		if key >= largest {
			return
		}
		delete(dst, largest)
	}
	dst[key] = value
}

func peakOf(top *float64, scalar *float64, lane []*float64) *float64 {
	if scalar != nil && (top == nil || *scalar > *top) {
		v := *scalar
		top = &v
	}
	for _, x := range lane {
		if x != nil && (top == nil || *x > *top) {
			v := *x
			top = &v
		}
	}
	return top
}

// historyRecord folds one reading into the current bucket, rolling the
// finished one into the file when the clock has crossed a boundary. Only a
// long-running helper records: a one-shot collection has no trend to keep,
// and must not write into the config directory just for having been run.
func historyRecord(snap *Snapshot) {
	sampler.Lock()
	on := sampler.on
	sampler.Unlock()
	if !on || snap == nil {
		return
	}
	now := histNow().Unix()
	start := now - now%histBucketSec

	hist.Lock()
	defer hist.Unlock()
	histLoad()
	// The clock stepped back past the bucket being filled: that bucket is
	// from a future that did not happen, and without this every reading of
	// the next hours folded into it and the page drew a point hours ahead
	// (audit 2026-10-16). The rows past now go too — but only when the step
	// came soon after boot (a dual-boot box's RTC put right by NTP), when they
	// can only be this process's own from the wrong clock. Later in a
	// machine's life a step back is as likely the clock going wrong as right,
	// and the rows stay: the archive takes new rows again once the clock has
	// passed them.
	if hist.cur != nil && start < hist.cur.start-60 {
		hist.cur = nil
		early := snap.System != nil && snap.System.UptimeSec != nil && *snap.System.UptimeSec < histEarlyStepSec
		n := len(hist.rows)
		for early && n > 0 && hist.rows[n-1][0].(float64) > float64(now) && hist.rows[n-1][0].(float64) >= float64(hist.procFirst) && hist.procFirst > 0 {
			n--
		}
		if n < len(hist.rows) {
			hist.rows = hist.rows[:n]
			hist.dirty = true
			hist.sized = false
		}
	}
	if hist.cur != nil && hist.cur.start < start {
		hist.cur.procs = procHistTake(hist.cur.start)
		if !diagnosticsEnabled() {
			hist.cur.procs = nil
			histStripProcs()
		}
		done := hist.cur.row()
		added := false
		if n := len(hist.rows); n == 0 || hist.rows[n-1][0].(float64) < done[0].(float64) {
			hist.rows = append(hist.rows, done)
			added = true
			if hist.procFirst == 0 {
				hist.procFirst = hist.cur.start
			}
			if hist.sized {
				hist.size += histRowSize(done)
			}
		}
		histTrim()
		// Only a row that joined the archive goes to the file: histSave
		// appends the newest row, and after a clock step backwards the
		// newest is one the file already holds — a repeated line the loader
		// then has to rewrite away.
		if added || hist.dirty {
			histSave()
		}
		hist.cur = nil
	}
	if hist.cur == nil {
		hist.cur = &histBucket{start: start, quota: map[string]float64{}, services: map[string]int{}, system: map[string]float64{}}
	}
	b := hist.cur
	if sys := snap.System; sys != nil {
		var cpuL, memL, rxL, txL []*float64
		if se := sys.Series; se != nil {
			cpuL, memL, rxL, txL = se.CPU, se.Mem, se.Rx, se.Tx
		}
		b.cpu = peakOf(b.cpu, sys.CPUPercent, cpuL)
		b.mem = peakOf(b.mem, sys.MemUsedPercent, memL)
		b.rx = peakOf(b.rx, sys.NetRxBps, rxL)
		b.tx = peakOf(b.tx, sys.NetTxBps, txL)
		b.disk = peakOf(b.disk, sys.DiskUsedPercent, nil)
		b.swap = peakOf(b.swap, sys.SwapUsedPercent, nil)
		// The disk's rates are the second's peak now, like the network's:
		// the scalar alone was one second in thirty, so a five-second burst
		// was caught one time in six (audit 2026-10-16).
		var ioR, ioW []*float64
		if se := sys.Series; se != nil {
			ioR, ioW = se.IORead, se.IOWrite
		}
		systemPeak(b.system, "io_read", peakOf(nil, sys.DiskReadBps, ioR), math.MaxFloat64)
		systemPeak(b.system, "io_write", peakOf(nil, sys.DiskWriteBps, ioW), math.MaxFloat64)
		// When the system booted, to the minute: a change between rows is a
		// reboot the page can mark on the line.
		if sys.UptimeSec != nil && *sys.UptimeSec > 0 && snap.CapturedAt > 0 {
			boot := math.Round((snap.CapturedAt-*sys.UptimeSec)/60) * 60
			if old, have := b.system["boot"]; !have || boot > old {
				b.system["boot"] = boot
			}
		}
		histBytes(b, sys)
		mounts := append([]Mount(nil), sys.Mounts...)
		sort.Slice(mounts, func(i, j int) bool { return mounts[i].Path < mounts[j].Path })
		mountN := 0
		last := ""
		for _, m := range mounts {
			path := m.Path
			x, ok := validPeak(m.UsedPercent, 100)
			if path == "" || len([]rune(path)) > histPathMax || !ok {
				continue
			}
			key := "mount:" + path
			if path != last {
				if mountN >= histMountMax {
					break
				}
				mountN++
				last = path
			}
			mountPeak(b.system, key, x)
		}
	}
	for _, p := range snap.Providers {
		if !p.OK || p.ID == "" {
			continue
		}
		worst, have := 0.0, false
		for _, l := range p.Limits {
			if !have || l.UsedPercent > worst {
				worst, have = l.UsedPercent, true
			}
		}
		if have {
			if cur, ok := b.quota[p.ID]; !ok || worst > cur {
				b.quota[p.ID] = worst
			}
		}
	}
	okCustom := map[string]bool{}
	for _, s := range snap.CustomSources {
		if s.OK {
			okCustom[s.ID] = true
		}
	}
	histCustomFold(b.system, snap.Custom, okCustom)
	okSrc := map[string]bool{}
	for _, s := range snap.ServiceSources {
		if s.OK {
			okSrc[s.ID] = true
		}
	}
	// The same 32 the relay and the page keep: the lowest ids, not the first
	// reported (audit 2026-10-16).
	svcs := append([]Service(nil), snap.Services...)
	sort.Slice(svcs, func(i, j int) bool { return svcs[i].ID < svcs[j].ID })
	n := 0
	for _, s := range svcs {
		if !okSrc[s.Source] || s.ID == "" {
			continue
		}
		if n++; n > 32 {
			break
		}
		// The bucket keeps the WORST state it saw, as the page's fold does
		// (serviceRank: down > pending > maintenance > up), so a service that
		// blipped inside five minutes shows the blip.
		if cur, ok := b.services[s.ID]; !ok || serviceRank(s.Status) > serviceRank(cur) {
			b.services[s.ID] = s.Status
		}
	}
}

// historyPoints is the archive at its own grain: every kept row plus the
// bucket in progress, so the line reaches now rather than stopping a minute
// short — the same two halves the relay's hello sends. The tunnel serves it
// through historyServe, which folds and pages; this is the whole.
func historyPoints() (bucketSec int, points []histRow) {
	hist.Lock()
	defer hist.Unlock()
	histLoad()
	// The names are served under the switch that recorded them, read now: a
	// machine that turned diagnostics off a minute ago answers without them
	// even though the file is not rewritten until the next rollover.
	names := diagnosticsEnabled()
	for _, r := range hist.rows {
		if !names && len(r) > histProcCol {
			r = r[:histProcCol:histProcCol]
		}
		points = append(points, r)
	}
	if hist.cur != nil {
		hist.cur.procs = nil
		if names {
			hist.cur.procs = procHistPeek(hist.cur.start)
		}
		points = append(points, hist.cur.row())
		hist.cur.procs = nil
	}
	return histBucketSec, points
}

// historyCoarseClosed is what a beacon carries of the archive (transport.go
// pushBody): the newest `n` five-minute buckets whose every thirty-second
// row is in, folded the way the page's default view and the relay's own
// copy fold them, WITHOUT process names — those never leave the tunnel. The
// relay files them as it files a reading's buckets, so a machine on the
// direct road keeps a trend in the one place a browser that cannot reach
// its tunnel can still read (audit 2026-10-16: the relay held nothing for
// it). Two a beacon: each bucket rides five pushes, so a lost push or a
// relay restart costs nothing, and the relay keeps only what is newer than
// its own tail.
func historyCoarseClosed(n int) []histRow {
	if n <= 0 {
		return nil
	}
	hist.Lock()
	defer hist.Unlock()
	histLoad()
	// The bucket in progress decides what is closed: a coarse bucket is done
	// once the thirty-second row being filled now lies past its end.
	cutoff := time.Now().Unix()
	if hist.cur != nil {
		cutoff = hist.cur.start
	}
	cutoff -= cutoff % histCoarseSec
	from := cutoff - int64(n)*histCoarseSec
	lo := sort.Search(len(hist.rows), func(i int) bool { return hist.rows[i][0].(float64) >= float64(from) })
	var src []histRow
	for _, r := range hist.rows[lo:] {
		if r[0].(float64) >= float64(cutoff) {
			break
		}
		if len(r) > histProcCol {
			r = r[:histProcCol:histProcCol]
		}
		src = append(src, r)
	}
	out := histFold(src, histCoarseSec)
	for i, r := range out {
		if len(r) > histProcCol {
			out[i] = r[:histProcCol:histProcCol]
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// historyPointsRange is historyPoints for one span: only the rows whose
// buckets at `step` can fall in [from, to) — found by their starts, which the
// archive keeps in order — so a page folds a day, not the month (audit
// 2026-10-16: every page re-folded every row). `oldest` is the archive's
// oldest bucket at that step, whatever was asked.
func historyPointsRange(from, to int64, step int) (points []histRow, oldest int64) {
	hist.Lock()
	defer hist.Unlock()
	histLoad()
	w := int64(step)
	if w <= 0 {
		w = histCoarseSec
	}
	if len(hist.rows) > 0 {
		oldest = int64(hist.rows[0][0].(float64))
		oldest -= oldest % w
	} else if hist.cur != nil {
		oldest = hist.cur.start - hist.cur.start%w
	}
	lo, hi := 0, len(hist.rows)
	if from > 0 {
		f := float64(from - from%w)
		lo = sort.Search(len(hist.rows), func(i int) bool { return hist.rows[i][0].(float64) >= f })
	}
	if to > 0 {
		t := float64(to + w)
		hi = sort.Search(len(hist.rows), func(i int) bool { return hist.rows[i][0].(float64) >= t })
	}
	names := diagnosticsEnabled()
	if lo < hi {
		points = make([]histRow, 0, hi-lo+1)
		for _, r := range hist.rows[lo:hi] {
			if !names && len(r) > histProcCol {
				r = r[:histProcCol:histProcCol]
			}
			points = append(points, r)
		}
	}
	if hist.cur != nil && (from <= 0 || hist.cur.start >= from-from%w) && (to <= 0 || hist.cur.start < to+w) {
		hist.cur.procs = nil
		if names {
			hist.cur.procs = procHistPeek(hist.cur.start)
		}
		points = append(points, hist.cur.row())
		hist.cur.procs = nil
	}
	return points, oldest
}

// histServed is one answer to GET /v1/history: a page of buckets at the
// asked step, newest first in the choosing and oldest first in the array.
type histServed struct {
	Step   int       // the buckets' width, seconds
	Points []histRow // oldest first
	From   int64     // the first point's start; 0 when there are none
	Newest int64     // the last point's start; 0 when there are none
	Oldest int64     // the archive's oldest bucket start at this step, whatever was asked; 0 when empty
	More   bool      // buckets older than From exist inside the asked span — ask again with to=From
}

// histStepOK: the steps a reader may ask for. The archive's own reading,
// the minute and the page's five, nothing else: a fold at an arbitrary
// width would let a client make the helper do unbounded work per request
// for no view anyone draws.
func histStepOK(step int) bool {
	return step == histBucketSec || step == histMinuteSec || step == histCoarseSec
}

// historyServe answers one page. step is 60 or 300 (anything else is read as
// 300); from/to bound bucket starts, from inclusive and to exclusive, 0 for
// no bound; budget is the most bytes the points may take serialised — the
// frame's limit less what else rides in it — or 0 for no limit. Rows are
// chosen newest first, so a page always reaches the asked end and `More`
// tells the reader to come back for the rest.
func historyServe(step int, from, to int64, budget int) histServed {
	if !histStepOK(step) {
		step = histCoarseSec
	}
	rows, oldest := historyPointsRange(from, to, step)
	all := histFold(rows, step)
	out := histServed{Step: step, Oldest: oldest}
	if len(all) == 0 {
		return out
	}
	// The span, as indexes into all: [lo, hi).
	lo, hi := 0, len(all)
	for lo < hi && (from > 0 && int64(all[lo][0].(float64)) < from) {
		lo++
	}
	for hi > lo && (to > 0 && int64(all[hi-1][0].(float64)) >= to) {
		hi--
	}
	if lo >= hi {
		return out
	}
	// Newest first, until the budget is spent.
	start := lo
	if budget > 0 {
		used := 2 // the brackets
		start = hi
		for start > lo {
			b, err := json.Marshal(all[start-1])
			if err != nil {
				break
			}
			if used+len(b)+1 > budget && start < hi {
				break
			}
			used += len(b) + 1
			start--
		}
		if start == hi {
			// Not even one row fits — a row cannot be split; send it anyway,
			// the frame check in the handler is the last word.
			start = hi - 1
		}
	}
	out.Points = all[start:hi]
	out.From = int64(out.Points[0][0].(float64))
	out.Newest = int64(out.Points[len(out.Points)-1][0].(float64))
	out.More = start > lo
	return out
}

// histFold: the rows re-bucketed at step. At the archive's own step a row is
// itself. At a wider one the rows whose starts fall in the same window fold
// the way a bucket folds its readings and the page folds its rows
// (foldIntoPersist): the peak of each number, the worst of each service, the
// most of each quota, the extreme each system key asked for (cmin: the
// trough), and the process names merged — each name's average over the
// rows that saw it, its peak the highest of them — so a five-minute row
// built from readings says what a five-minute bucket would have said.
func histFold(rows []histRow, step int) []histRow {
	if step <= histBucketSec || len(rows) == 0 {
		return rows
	}
	var out []histRow
	var cur *histBucket
	var curProcs map[string]*procMerge
	flush := func() {
		if cur == nil {
			return
		}
		if len(curProcs) > 0 {
			cur.procs = procMerged(curProcs)
		}
		out = append(out, cur.row())
		cur, curProcs = nil, nil
	}
	w := int64(step)
	for _, r := range rows {
		if !histRowOK(r) {
			continue
		}
		start := int64(r[0].(float64))
		start -= start % w
		if cur == nil || cur.start != start {
			flush()
			cur = &histBucket{start: start, quota: map[string]float64{}, services: map[string]int{}, system: map[string]float64{}}
		}
		cols := []**float64{&cur.cpu, &cur.mem, &cur.disk, &cur.swap, &cur.rx, &cur.tx}
		for i, dst := range cols {
			if v, ok := histNum(r[1+i]); ok {
				*dst = peakOf(*dst, &v, nil)
			}
		}
		if len(r) > 7 {
			for k, v := range histNumMap(r[7]) {
				if old, have := cur.quota[k]; !have || v > old {
					cur.quota[k] = v
				}
			}
		}
		if len(r) > 8 {
			for k, v := range histNumMap(r[8]) {
				st := int(v)
				if old, have := cur.services[k]; !have || serviceRank(st) > serviceRank(old) {
					cur.services[k] = st
				}
			}
		}
		if len(r) > 9 {
			for k, v := range histNumMap(r[9]) {
				old, have := cur.system[k]
				if histSumKey(k) {
					cur.system[k] = old + v
					continue
				}
				if !have || (strings.HasPrefix(k, "cmin:") && v < old) || (!strings.HasPrefix(k, "cmin:") && v > old) {
					cur.system[k] = v
				}
			}
		}
		if len(r) > histProcCol {
			if pm := histProcMap(r[histProcCol]); len(pm) > 0 {
				if curProcs == nil {
					curProcs = map[string]*procMerge{}
				}
				for name, av := range pm {
					m := curProcs[name]
					if m == nil {
						m = &procMerge{}
						curProcs[name] = m
					}
					m.n++
					m.sum += av[0]
					if len(av) > 1 && av[1] > m.peak {
						m.peak = av[1]
					}
				}
				// A minute that kept names but not this one: the name was
				// below the fold, not idle — but counted as 0 it would drag
				// the average under what the bucket saw. Minutes are
				// counted per name (m.n), so the average is over the minutes
				// that named it. Said here because it is the one place the
				// fold departs from "divide by five".
			}
		}
	}
	flush()
	return out
}

type procMerge struct {
	n    int
	sum  float64
	peak float64
}

// procMerged: the merged names as a row keeps them — the top five by
// average as [avg, peak], the remainder under procOtherKey as [avg]. Five
// minutes' top fives can be more than five names; the ones below the fold
// here are not dropped but added to the remainder, as they would have
// been had the five-minute sweep seen them (their averages are each over
// the minutes that named them, so the sum is an estimate, and said so).
func procMerged(m map[string]*procMerge) map[string][]float64 {
	type kv struct {
		name string
		avg  float64
		peak float64
	}
	var all []kv
	restAvg := 0.0
	for name, p := range m {
		if p.n == 0 {
			continue
		}
		if name == procOtherKey {
			restAvg += p.sum / float64(p.n)
			continue
		}
		all = append(all, kv{name, p.sum / float64(p.n), p.peak})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].avg != all[j].avg {
			return all[i].avg > all[j].avg
		}
		return all[i].name < all[j].name
	})
	out := map[string][]float64{}
	for i, e := range all {
		if i >= procTopN {
			restAvg += e.avg
			continue
		}
		out[e.name] = []float64{procPct(e.avg), procPct(e.peak)}
	}
	if v := procPct(restAvg); v > 0 {
		out[procOtherKey] = []float64{v}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// The rows come in two dresses: typed, as row() made them, and as
// encoding/json read them back (float64 and map[string]any). The fold reads
// both.
func histNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return x, true
	case int:
		return float64(x), true
	case *float64:
		if x == nil {
			return 0, false
		}
		return histNum(*x)
	}
	return 0, false
}

func histNumMap(v any) map[string]float64 {
	switch x := v.(type) {
	case map[string]float64:
		return x
	case map[string]int:
		out := make(map[string]float64, len(x))
		for k, n := range x {
			out[k] = float64(n)
		}
		return out
	case map[string]any:
		out := make(map[string]float64, len(x))
		for k, raw := range x {
			if f, ok := histNum(raw); ok {
				out[k] = f
			}
		}
		return out
	}
	return nil
}

func histProcMap(v any) map[string][]float64 {
	switch x := v.(type) {
	case map[string][]float64:
		return x
	case map[string]any:
		out := make(map[string][]float64, len(x))
		for k, raw := range x {
			list, ok := raw.([]any)
			if !ok || len(list) == 0 {
				continue
			}
			var fs []float64
			for _, e := range list {
				if f, ok := histNum(e); ok {
					fs = append(fs, f)
				}
			}
			if len(fs) > 0 {
				out[k] = fs
			}
		}
		return out
	}
	return nil
}
