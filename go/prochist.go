package main

// Which program had the CPU — kept in the history, beside the line it explains.
//
// The trend says "the CPU was at 95% at 14:05" and nothing in it can say whose
// 95% that was (operator 2026-09-20: there is no way to tell which program's CPU
// was high during a stretch of the history). On-demand diagnostics (inspect.go) answer it for
// NOW; this answers it for then, and has to be running before anyone asks —
// so it records whether or not a dashboard is open, like the rest of history.go.
//
// WHAT IT KEEPS: per five-minute bucket, the five process NAMES that used the
// most CPU, each as [average %, peak %], plus one unnamed remainder under "".
// A name is the kernel's comm, through cleanProcessName — never a command
// line, a path, a pid or a user. Percentages are of the whole machine, the
// same 0–100 the CPU tile speaks, so the rows sum with the remainder to the
// bucket's average busy share.
//
// WHERE IT GOES: row[10] of this machine's own history.json, served over the
// tunnel to keys on the allowlist. It never enters a Snapshot, so it never
// reaches the relay — system.go's bar ("no process list") still describes
// everything the relay holds.
//
// IT IS THE DIAGNOSTICS OPT-IN, not a new one. A machine that lets its
// dashboard ask for process names now has agreed to the same names, in the
// same place, five minutes at a time. With the switch off nothing is swept,
// nothing is served, and the next rollover strips what was stored (history.go).
//
// WHY TEN SECONDS AND NOT THE SAMPLER'S ONE: utime+stime are cumulative, so two
// reads bracket exactly what ran between them and a bucket's average is the
// same whatever the cadence. A faster sweep buys only a finer peak and the
// processes that live and die inside the gap — and costs a read of every
// /proc/<pid>/stat each time, on a helper whose whole job is to not be the
// thing using the CPU. What dies between sweeps is not lost from the total:
// the remainder is "busy minus the named five", not "the processes we saw".

import (
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	procSweepEvery = 10 * time.Second
	procTopN       = 5
	// Distinct names one bucket will track. A build farm spawns thousands of
	// short-lived names; past this the newcomers fall into the remainder.
	procNamesMax = 512
	// Finished buckets held for history.go to collect. It collects on its own
	// clock (a snapshot, every thirty seconds or so), so one is the usual
	// depth; the rest is slack for a helper that was suspended.
	procDoneMax = 4
	// A gap this long between two sweeps is a suspended machine or a stalled
	// helper, not an interval: the delta across it is dropped rather than
	// averaged into one bucket.
	procSweepStale = 3 * procSweepEvery
	procOtherKey   = ""
)

type procAcc struct{ ticks, peak float64 }

type procTick struct{ start, ticks float64 }

var procHist struct {
	sync.Mutex
	on bool

	prev       map[int]procTick
	prevTotal  float64
	prevBusy   float64
	prevUptime float64
	prevAt     time.Time

	bucket int64
	names  map[string]*procAcc
	total  float64 // machine ticks this bucket has covered
	busy   float64 // of which not idle, not iowait — CPUPercent's rule

	done map[int64]map[string][]float64
}

// startProcHistory begins the sweep. Called with the sampler, so exactly the
// processes that record history sweep for it. The switch is read on every
// tick rather than here: `diagnostics on` takes effect without a restart,
// and so does off.
func startProcHistory() {
	if !diagnosticsAvailable() {
		return
	}
	procHist.Lock()
	if procHist.on {
		procHist.Unlock()
		return
	}
	procHist.on = true
	procHist.Unlock()
	go func() {
		procSweep(time.Now())
		t := time.NewTicker(procSweepEvery)
		defer t.Stop()
		for at := range t.C {
			procSweep(at)
		}
	}()
}

// A variable so a test can keep the real /proc out of a staged history.
var procSweepNow = func() { procSweep(time.Now()) }

// One sweep at a time: the ticker's and procHistTake's would otherwise read
// /proc side by side and fold out of order, and the second would look like a
// clock that ran backwards.
var procSweepMu sync.Mutex

func procSweep(at time.Time) {
	procSweepMu.Lock()
	defer procSweepMu.Unlock()
	if !diagnosticsEnabled() {
		procHist.Lock()
		procHist.prev, procHist.names, procHist.done = nil, nil, nil
		procHist.total, procHist.busy = 0, 0
		procHist.Unlock()
		return
	}
	cur, total, busy, uptime, ok := readProcTicks()
	procHist.Lock()
	defer procHist.Unlock()
	if !ok {
		procHist.prev = nil
		return
	}
	procFold(cur, total, busy, uptime, at)
}

// procFold is the sweep with its reads taken out, so a test can drive it.
//
// An interval belongs to the time it covers, not to the moment it was read:
// a sweep at 14:04:55 and the next at 14:05:05 measured five seconds of each
// bucket, and filing all ten under 14:05 would show the new bucket a program
// that had already stopped. Counters cannot say when inside the interval the
// ticks were spent, so a crossing interval is split by time. Its peak goes to
// the side that holds more of it.
func procFold(cur map[int]procRead, total, busy, uptime float64, at time.Time) {
	p := &procHist
	bucketOf := func(t time.Time) int64 { return t.Unix() - t.Unix()%histBucketSec }
	prev := p.prev
	dTotal, dBusy := total-p.prevTotal, busy-p.prevBusy
	span := at.Sub(p.prevAt)
	fresh := prev != nil && dTotal > 0 && dBusy >= 0 && span > 0 && span <= procSweepStale

	next := make(map[int]procTick, len(cur))
	var sweep map[string]float64
	if fresh {
		sweep = map[string]float64{}
	}
	for pid, c := range cur {
		next[pid] = procTick{start: c.start, ticks: c.ticks}
		if !fresh {
			continue
		}
		var d float64
		if was, found := prev[pid]; found && was.start == c.start {
			d = c.ticks - was.ticks
		} else if c.start/procUserHZ >= p.prevUptime {
			// Born since the last sweep: everything it has used, it used in
			// this interval. (A recycled pid lands here too, by its start.)
			d = c.ticks
		}
		if d > 0 {
			sweep[c.name] += d
		}
	}
	credit := func(share float64, peak bool) {
		p.total += dTotal * share
		p.busy += dBusy * share
		for name, d := range sweep {
			acc := p.names[name]
			if acc == nil {
				if len(p.names) >= procNamesMax {
					continue
				}
				acc = &procAcc{}
				p.names[name] = acc
			}
			acc.ticks += d * share
			if pct := d / dTotal * 100; peak && pct > acc.peak {
				acc.peak = pct
			}
		}
	}
	open := func(bucket int64) {
		procFinish()
		p.bucket, p.names, p.total, p.busy = bucket, map[string]*procAcc{}, 0, 0
	}
	now := bucketOf(at)
	if fresh {
		if was := bucketOf(p.prevAt); was != now && p.names != nil && p.bucket == was {
			// The old bucket is still open: give it its part first. (When
			// history.go has already collected it, that part has no row left
			// to go to and only the new bucket's share is kept.)
			old := time.Unix(now, 0).Sub(p.prevAt).Seconds() / span.Seconds()
			credit(old, old >= 0.5)
			open(now)
			credit(1-old, old < 0.5)
		} else {
			share := 1.0
			if was != now {
				share = at.Sub(time.Unix(now, 0)).Seconds() / span.Seconds()
			}
			if p.names == nil || p.bucket != now {
				open(now)
			}
			credit(share, true)
		}
	} else if p.names == nil || p.bucket != now {
		open(now)
	}
	p.prev, p.prevTotal, p.prevBusy, p.prevUptime, p.prevAt = next, total, busy, uptime, at
}

// /proc's tick fields are USER_HZ, which the kernel ABI fixes at 100.
const procUserHZ = 100.0

type procRead struct {
	start, ticks float64
	name         string
}

func procPct(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 100 {
		v = 100
	}
	return math.Round(v*10) / 10
}

// procRows is the bucket as it stands: the top names as [avg, peak] and the
// remainder as [avg]. nil when nothing was measured — a row then carries no
// eleventh element at all, which is what an old row looks like too.
func procRows() map[string][]float64 {
	p := &procHist
	if p.total <= 0 || len(p.names) == 0 {
		return nil
	}
	type kv struct {
		name string
		acc  *procAcc
	}
	all := make([]kv, 0, len(p.names))
	for name, acc := range p.names {
		all = append(all, kv{name, acc})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].acc.ticks != all[j].acc.ticks {
			return all[i].acc.ticks > all[j].acc.ticks
		}
		return all[i].name < all[j].name
	})
	out := map[string][]float64{}
	named := 0.0
	for _, e := range all {
		if len(out) >= procTopN {
			break
		}
		avg := procPct(e.acc.ticks / p.total * 100)
		if avg <= 0 && procPct(e.acc.peak) <= 0 {
			continue // too small to say; a later name may still have spiked
		}
		out[e.name] = []float64{avg, procPct(e.acc.peak)}
		named += e.acc.ticks
	}
	if len(out) == 0 {
		return nil
	}
	if rest := procPct((p.busy - named) / p.total * 100); rest > 0 {
		out[procOtherKey] = []float64{rest}
	}
	return out
}

// procFinish files the bucket in progress under its start. Caller holds the lock.
func procFinish() {
	p := &procHist
	if p.names == nil {
		return
	}
	if rows := procRows(); rows != nil {
		if p.done == nil {
			p.done = map[int64]map[string][]float64{}
		}
		p.done[p.bucket] = rows
		for len(p.done) > procDoneMax {
			oldest := int64(math.MaxInt64)
			for k := range p.done {
				if k < oldest {
					oldest = k
				}
			}
			delete(p.done, oldest)
		}
	}
	p.names, p.total, p.busy = nil, 0, 0
}

// procHistTake hands history.go the finished bucket, once.
func procHistTake(bucket int64) map[string][]float64 {
	procHist.Lock()
	still := procHist.on && procHist.names != nil && procHist.bucket == bucket
	procHist.Unlock()
	if still {
		// The history's clock crossed the boundary before the sweep's did.
		// Sweep now, so the bucket gets its last seconds (procFold splits the
		// interval) instead of being closed short of them.
		procSweepNow()
	}
	procHist.Lock()
	defer procHist.Unlock()
	if procHist.names != nil && procHist.bucket == bucket {
		procFinish()
	}
	rows := procHist.done[bucket]
	delete(procHist.done, bucket)
	for k := range procHist.done {
		if k < bucket {
			delete(procHist.done, k)
		}
	}
	return rows
}

// procHistPeek is the same for the bucket in progress, without closing it.
func procHistPeek(bucket int64) map[string][]float64 {
	procHist.Lock()
	defer procHist.Unlock()
	if procHist.names != nil && procHist.bucket == bucket {
		return procRows()
	}
	return procHist.done[bucket]
}

// readProcTicks is inspect.go's readProcessSample on a diet. That one is
// called when a person presses a button; this one runs for the life of the
// helper, and readSmall's 64 KiB buffer per file — two files per process —
// would be tens of megabytes of garbage every sweep. One small buffer, reused:
// a stat line is a few hundred bytes and comm is sixteen.
func readProcTicks() (procs map[int]procRead, total, busy, uptime float64, ok bool) {
	buf := make([]byte, 4096)
	read := func(path string) (string, bool) {
		f, err := os.Open(path)
		if err != nil {
			return "", false
		}
		n, _ := f.Read(buf)
		f.Close()
		if n <= 0 {
			return "", false
		}
		return string(buf[:n]), true
	}
	stat, ok := read("/proc/stat")
	if !ok {
		return nil, 0, 0, 0, false
	}
	if i := strings.IndexByte(stat, '\n'); i >= 0 {
		stat = stat[:i]
	}
	total, busy, ok = parseBusyCPUTicks(stat)
	if !ok {
		return nil, 0, 0, 0, false
	}
	up, ok := read("/proc/uptime")
	if !ok {
		return nil, 0, 0, 0, false
	}
	if f := strings.Fields(up); len(f) > 0 {
		uptime, _ = strconv.ParseFloat(f[0], 64)
	}
	if !(uptime > 0) || !finite(uptime) {
		return nil, 0, 0, 0, false
	}
	dir, err := os.Open("/proc")
	if err != nil {
		return nil, 0, 0, 0, false
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return nil, 0, 0, 0, false
	}
	procs = make(map[int]procRead, len(names))
	for _, n := range names {
		if n == "" || n[0] < '1' || n[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		raw, ok := read("/proc/" + n + "/stat")
		if !ok {
			continue // exited between the listing and the read
		}
		start, ticks, _, ok := parseProcessStat(raw)
		if !ok {
			continue
		}
		// comm from its own file rather than from between stat's brackets:
		// one rule for what a name is, shared with the live inspector.
		nameRaw, ok := read("/proc/" + n + "/comm")
		if !ok {
			continue
		}
		name := cleanProcessName(nameRaw)
		if name == "" {
			continue
		}
		procs[pid] = procRead{start: start, ticks: ticks, name: name}
	}
	return procs, total, busy, uptime, true
}

// parseBusyCPUTicks: the aggregate cpu line as (all ticks, busy ticks), busy
// by linuxCPU's rule — idle and iowait are not work, steal is time this
// machine did not get and is counted as gone. guest and guest_nice are left
// off for the reason parseTotalCPUTicks gives.
func parseBusyCPUTicks(line string) (total, busy float64, ok bool) {
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	values := f[1:]
	if len(values) > 8 {
		values = values[:8]
	}
	var idle float64
	for i, raw := range values {
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || n < 0 || !finite(n) {
			return 0, 0, false
		}
		total += n
		if i == 3 || i == 4 {
			idle += n
		}
	}
	return total, total - idle, total > 0
}
