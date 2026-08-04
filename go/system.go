package main

// Machine health, reported alongside the quota numbers.
//
// THE BAR EVERY FIELD HERE HAS TO CLEAR: useful for deciding whether a machine
// needs attention, useless for working out whose machine it is. No hostname, no
// username, no path, no process list, no serial, no MAC, no public IP. "93% of
// the disk is gone" is actionable; "93% of /home/alice on build-07 is gone" is
// the same fact wearing an identity, and the relay has no business holding the
// second one. On-demand diagnostics are the explicit exception: when separately
// enabled they return a bounded process resource summary to the requesting
// dashboard only, and never enter this snapshot or its history. The kernel
// version is deliberately cut to major.minor for the
// same reason — "6.8" tells you whether a box is ancient, "6.8.0-1050-oracle"
// also tells an attacker who reads the relay which cloud to look at and which
// CVEs to try.
//
// COVERAGE IS HONEST RATHER THAN UNIFORM. Linux gets everything, because /proc
// hands it over for free. macOS gets what the standard library can reach
// without cgo, which is far less than you would hope: syscall.Sysctl returns a
// Go string cut at the first NUL byte, and SysctlUint32 refuses anything that
// is not exactly four bytes wide. Between them that rules out hw.memsize (64
// bits, leading zero bytes), vm.loadavg (a struct) and kern.boottime (a
// timeval) — every one of which would come back either empty or, worse,
// plausibly wrong. CGO_ENABLED=0 is not negotiable here (build.sh needs the
// Linux binaries genuinely static), so the mach APIs that would answer these
// are out of reach.
//
// So a platform reports what it can actually read, names what it cannot in
// `missing`, and the page renders that gap instead of a zero. A fabricated
// number is worse than an absent one: absent looks like a gap, wrong looks
// like a healthy machine.

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// One machine's health. Every measured field is a pointer: absent and zero are
// different claims, and "0% memory used" is not something any running computer
// should ever be able to say.
type System struct {
	OK       bool   `json:"ok"`
	Platform string `json:"platform"` // GOOS — "linux", "darwin"
	Arch     string `json:"arch"`     // GOARCH

	// Kernel release, cut to major.minor. Never the full uname string.
	OSVersion *string `json:"os_version,omitempty"`

	CPUCount   int      `json:"cpu_count,omitempty"`
	CPUPercent *float64 `json:"cpu_percent,omitempty"` // 0–100, differential

	Load1  *float64 `json:"load1,omitempty"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`

	MemTotal       *float64 `json:"mem_total,omitempty"` // bytes
	MemUsedPercent *float64 `json:"mem_used_percent,omitempty"`

	SwapTotal       *float64 `json:"swap_total,omitempty"`
	SwapUsedPercent *float64 `json:"swap_used_percent,omitempty"`

	// The root filesystem only. A list of every mount would be a map of
	// somebody's machine, and the question this answers — "is it about to run
	// out of room" — is nearly always about /.
	//
	// DiskUsed is sent rather than left for the page to derive: the percentage
	// deliberately excludes root-reserved blocks and DiskTotal includes them,
	// so total × percent is not the used figure and overstates it.
	DiskTotal       *float64 `json:"disk_total,omitempty"`
	DiskUsed        *float64 `json:"disk_used,omitempty"`
	DiskUsedPercent *float64 `json:"disk_used_percent,omitempty"`

	UptimeSec *float64 `json:"uptime_sec,omitempty"`

	// Whole-box traffic, bytes per second, summed over every interface except
	// loopback. A rate like CPUPercent, so it needs two samples and the first
	// collection reports nothing. The SUM is the privacy line here: per-
	// interface rows would name interfaces, and interface names on a modern box
	// (wg0, tailscale0, br-<hash>) describe what the machine is connected to.
	NetRxBps *float64 `json:"net_rx_bps,omitempty"`
	NetTxBps *float64 `json:"net_tx_bps,omitempty"`

	// Aggregate TCP health: counts and a retransmission rate, never endpoints,
	// addresses, ports or interface names. Like network throughput, the counter
	// rate needs two samples and is omitted on the first collection.
	TCPEstab     *float64 `json:"tcp_estab,omitempty"`
	TCPTimeWait  *float64 `json:"tcp_time_wait,omitempty"`
	TCPRetransPS *float64 `json:"tcp_retrans_ps,omitempty"`

	// How many processes exist, not what any of them is. The count answers
	// "is something leaking processes"; a list would answer "what does this
	// person run", which is the question this file refuses.
	Procs *float64 `json:"procs,omitempty"`

	// Hottest thermal zone, °C. Absent-and-silent when the platform exposes no
	// sensor — which is every VM — so, unlike the fields above, absence here is
	// a configuration and never reported in `missing`. Same judgement as swap.
	TempC *float64 `json:"temp_c,omitempty"`

	// What this platform could not read, so the page can say "not available
	// here" rather than leaving a reader to guess whether the machine is idle
	// or the collector is broken.
	Missing []string `json:"missing,omitempty"`

	// The fast-moving readings above, sampled every `step` seconds since the
	// last push instead of once when it happened. Absent unless a sampler is
	// running (see sampler.go) — one-shot and serve collections carry the
	// snapshot alone, which is all a single collection can honestly claim.
	Series *SysSeries `json:"series,omitempty"`
}

// One frame's worth of samples, oldest first, laid on a fixed grid.
//
// The last slot of every array is the snapshot this rides on — the same
// measurement, rounded for the wire — so the gauges and the end of the line
// can never disagree. Only the readings that visibly move are carried: disk, swap, load,
// process count and temperature change on a scale where one point per push is
// already more resolution than the eye asks for, and thirty copies of an
// unchanged number would be most of the frame.
type SysSeries struct {
	// When the LAST slot was taken, on the machine's clock. The page anchors
	// the series to the relay's clock through this: the frame's captured_at is
	// the same clock, so (captured_at − at) is a lag the page can subtract
	// from the relay's seen_at without ever trusting a machine's absolute time.
	At   float64 `json:"at"`
	Step float64 `json:"step"` // seconds per slot

	// nil where the platform cannot read that metric at all; a nil ELEMENT is
	// a slot with no sample in it.
	CPU []*float64 `json:"cpu,omitempty"`
	Mem []*float64 `json:"mem,omitempty"`
	Rx  []*float64 `json:"rx,omitempty"`
	Tx  []*float64 `json:"tx,omitempty"`
}

// Ordered so the page can render a stable list; also the vocabulary Missing
// draws from, kept in one place so a typo cannot invent a category.
const (
	mCPU     = "cpu"
	mMemory  = "memory"
	mSwap    = "swap"
	mDisk    = "disk"
	mLoad    = "load"
	mUptime  = "uptime"
	mNetwork = "network"
	mProcs   = "procs"
	// No mTemp: a sensorless machine is a configuration, not a gap — see TempC.
)

func (s *System) miss(what string) { s.Missing = append(s.Missing, what) }

// CPU usage is a rate, and a rate needs two samples. The first collection
// after startup therefore has no percentage to report — which is correct and
// stated, not filled in with the since-boot average. That average is a
// different quantity that happens to have the same units, and on a box that
// was hammered last week and is idle now it reads as permanently busy.
var cpuPrev struct {
	sync.Mutex
	valid       bool
	busy, total float64
}

// Feed one /proc/stat-style reading in, get the percentage since the previous
// one out. Split from the Linux collector so the arithmetic — which is where
// the counter-wrap and divide-by-zero traps live — can be tested directly.
func cpuDelta(busy, total float64) *float64 {
	cpuPrev.Lock()
	defer cpuPrev.Unlock()

	prevValid, prevBusy, prevTotal := cpuPrev.valid, cpuPrev.busy, cpuPrev.total
	cpuPrev.valid, cpuPrev.busy, cpuPrev.total = true, busy, total

	if !prevValid {
		return nil
	}
	dt, db := total-prevTotal, busy-prevBusy
	// Two pushes inside the same jiffy give dt == 0; a counter that went
	// backwards means a reset (or a machine that slept through it). Neither is
	// a reading — report nothing rather than 0%, NaN or a spike.
	if dt <= 0 || db < 0 {
		return nil
	}
	pct := db / dt * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return fp(round2(pct))
}

// Network counters are cumulative bytes, so a rate needs two samples AND the
// wall-clock span between them — unlike cpuDelta, whose jiffies carry their own
// time base. Same first-sample rule: the first collection reports nothing.
var netPrev struct {
	sync.Mutex
	valid      bool
	rx, tx, at float64
}

var tcpPrev struct {
	sync.Mutex
	valid     bool
	retransAt float64
	at        float64
}

func tcpDelta(retrans, at float64) *float64 {
	tcpPrev.Lock()
	defer tcpPrev.Unlock()
	valid, old, oldAt := tcpPrev.valid, tcpPrev.retransAt, tcpPrev.at
	tcpPrev.valid, tcpPrev.retransAt, tcpPrev.at = true, retrans, at
	if !valid || at <= oldAt || retrans < old {
		return nil
	}
	return fp(round2((retrans - old) / (at - oldAt)))
}

// Feed one cumulative (rx, tx) reading in, get bytes-per-second since the
// previous one out. Nil on the first sample, on a non-advancing clock, and on
// counters that went backwards — which happens legitimately here: the sum
// shrinks whenever an interface disappears (a container's veth being torn down
// is routine), and that must read as "no reading", never as a negative rate.
func netDelta(rx, tx, at float64) (rxBps, txBps *float64) {
	netPrev.Lock()
	defer netPrev.Unlock()

	prevValid, prevRx, prevTx, prevAt := netPrev.valid, netPrev.rx, netPrev.tx, netPrev.at
	netPrev.valid, netPrev.rx, netPrev.tx, netPrev.at = true, rx, tx, at

	if !prevValid {
		return nil, nil
	}
	dt := at - prevAt
	if dt <= 0 {
		return nil, nil
	}
	dRx, dTx := rx-prevRx, tx-prevTx
	if dRx < 0 || dTx < 0 {
		return nil, nil
	}
	return fp(round2(dRx / dt)), fp(round2(dTx / dt))
}

// Kernel release trimmed to major.minor: "6.8.0-1050-oracle" -> "6.8",
// "24.5.0" -> "24.5". Anything that does not look like a version at all is
// dropped entirely rather than forwarded — this field exists to be coarse.
func shortKernel(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return nil
	}
	major, err1 := strconv.Atoi(parts[0])
	minorField := parts[1]
	// "8-rc1" and friends: keep the leading digits, drop the rest.
	end := 0
	for end < len(minorField) && minorField[end] >= '0' && minorField[end] <= '9' {
		end++
	}
	minor, err2 := strconv.Atoi(minorField[:end])
	if err1 != nil || err2 != nil || major < 0 || minor < 0 {
		return nil
	}
	return sp(strconv.Itoa(major) + "." + strconv.Itoa(minor))
}

// Read a small pseudo-file. /proc entries are generated on read and are tiny,
// but this runs on other people's machines against paths this program does not
// own, so the read is bounded anyway.
func readSmall(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	n, err := f.Read(buf)
	if n <= 0 || (err != nil && n == 0) {
		return "", false
	}
	return string(buf[:n]), true
}

func pct(used, total float64) *float64 {
	if total <= 0 || used < 0 {
		return nil
	}
	p := used / total * 100
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return fp(round2(p))
}

// Collect, contained. Same reasoning as safeCollect for providers: this reads
// files and syscalls on machines the author has never seen, and a panic here
// must not take the quota numbers down with it.
func collectSystem() (s System) {
	defer func() {
		if r := recover(); r != nil {
			s = System{OK: false, Platform: runtime.GOOS, Arch: runtime.GOARCH,
				Missing: []string{mCPU, mMemory, mSwap, mDisk, mLoad, mUptime, mNetwork, mProcs}}
		}
	}()
	return systemSnapshot()
}

// The shell every platform collector starts from, so none of them can forget
// the two fields that are free everywhere.
func baseSystem() System {
	return System{Platform: runtime.GOOS, Arch: runtime.GOARCH, CPUCount: runtime.NumCPU()}
}
