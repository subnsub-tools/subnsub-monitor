//go:build linux

package main

// Linux reads everything out of /proc and one statfs. No cgo, no shelling out,
// no /etc/os-release (that file carries a pretty name and sometimes a build ID,
// and the kernel release alone answers the question this field is for).

import (
	"strconv"
	"strings"
	"syscall"
)

func systemSnapshot() System {
	s := baseSystem()

	if v, ok := readSmall("/proc/sys/kernel/osrelease"); ok {
		s.OSVersion = shortKernel(v)
	}

	linuxCPU(&s)
	linuxMem(&s)
	linuxLoad(&s)
	linuxUptime(&s)
	statfsRoot(&s)
	linuxNet(&s)
	linuxProcs(&s)
	linuxTemp(&s)

	// "ok" means the collector ran and produced at least one measurement — not
	// that everything worked. A machine where /proc is masked (some hardened
	// containers) still reports its platform and core count, and the page shows
	// the gaps rather than an error card.
	s.OK = s.CPUPercent != nil || s.MemUsedPercent != nil || s.Load1 != nil ||
		s.UptimeSec != nil || s.DiskUsedPercent != nil
	return s
}

// First line of /proc/stat:
//
//	cpu  user nice system idle iowait irq softirq steal guest guest_nice
//
// Everything is busy except idle and iowait. guest and guest_nice are already
// counted inside user and nice — adding them again inflates total and quietly
// deflates the percentage, which is the classic way to write this wrong.
func linuxCPU(s *System) {
	raw, ok := readSmall("/proc/stat")
	if !ok {
		s.miss(mCPU)
		return
	}
	line := raw
	if i := strings.IndexByte(raw, '\n'); i >= 0 {
		line = raw[:i]
	}
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		s.miss(mCPU)
		return
	}
	var total, idle float64
	for i, v := range f[1:] {
		if i >= 8 { // stop before guest / guest_nice
			break
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 {
			s.miss(mCPU)
			return
		}
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	if p := cpuDelta(total-idle, total); p != nil {
		s.CPUPercent = p
	} else {
		// No previous sample yet (or the counters moved backwards). Say so:
		// the very first push after install would otherwise show a machine
		// with no CPU line and no explanation.
		s.miss(mCPU)
	}
}

// MemAvailable, not MemFree. MemFree on a healthy Linux box is small by design
// — the page cache is doing its job — and reporting it as "used" makes every
// machine look like it is about to die.
func linuxMem(s *System) {
	raw, ok := readSmall("/proc/meminfo")
	if !ok {
		s.miss(mMemory)
		s.miss(mSwap)
		return
	}
	kb := map[string]float64{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || n < 0 {
			continue
		}
		kb[k] = n * 1024 // every value in meminfo is kB
	}

	// comma-ok on every lookup, because a map's zero value and a real zero are
	// indistinguishable and the failure is silent AND alarming: MemAvailable
	// is absent on kernels before 3.14, and reading that missing key as 0
	// reports the machine at 100% memory — a fake emergency on an idle box.
	// Same for SwapFree if the read was truncated.
	total, okT := kb["MemTotal"]
	avail, okA := kb["MemAvailable"]
	if okT && okA && total > 0 && avail >= 0 && avail <= total {
		s.MemTotal = fp(total)
		s.MemUsedPercent = pct(total-avail, total)
	} else {
		s.miss(mMemory)
	}

	swTotal, okS := kb["SwapTotal"]
	swFree, okF := kb["SwapFree"]
	if okS && swTotal > 0 {
		s.SwapTotal = fp(swTotal)
		if okF && swFree >= 0 && swFree <= swTotal {
			s.SwapUsedPercent = pct(swTotal-swFree, swTotal)
		} else {
			s.miss(mSwap)
		}
	}
	// Swap being absent entirely is a configuration, not a failure — most cloud
	// images ship without it — so THAT case is not reported as missing.
}

// df/statvfs scale block counts by the fundamental fragment size, not by
// f_bsize (which Linux reports as the optimal transfer size). They are equal
// on most filesystems, which is exactly why using the wrong one survives
// casual testing and then reports a wrong total on the one filesystem where
// they differ.
func fsBlockSize(st *syscall.Statfs_t) float64 {
	if st.Frsize > 0 {
		return float64(st.Frsize)
	}
	return float64(st.Bsize)
}

// /proc/net/dev, everything summed except loopback:
//
//	Inter-|   Receive                            ...|  Transmit
//	 face |bytes    packets errs drop fifo frame ...|bytes    packets ...
//	    lo: 1234567     890    0    0    0     0    ...
//	  eth0: 9876543    2109    0    0    0     0    ...
//
// The sum is what travels (see NetRxBps in system.go for why no per-interface
// rows), and lo is excluded because local chatter would drown the number the
// gauge exists to show. Interface names are read to make that one exclusion
// and are never kept.
func linuxNet(s *System) {
	raw, ok := readSmall("/proc/net/dev")
	if !ok {
		s.miss(mNetwork)
		return
	}
	var rx, tx float64
	var sawAny bool
	for _, line := range strings.Split(raw, "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" {
			continue
		}
		f := strings.Fields(rest)
		// bytes is field 0 on the receive side and field 8 on the transmit side.
		if len(f) < 9 {
			continue
		}
		r, err1 := strconv.ParseFloat(f[0], 64)
		t, err2 := strconv.ParseFloat(f[8], 64)
		if err1 != nil || err2 != nil || r < 0 || t < 0 || !finite(r) || !finite(t) {
			continue
		}
		rx += r
		tx += t
		sawAny = true
	}
	if !sawAny {
		s.miss(mNetwork)
		return
	}
	rxBps, txBps := netDelta(rx, tx, now())
	if rxBps == nil {
		// First sample since startup, or the sum went backwards (an interface
		// vanished). Both are "no reading yet", and saying so beats a zero.
		s.miss(mNetwork)
		return
	}
	s.NetRxBps, s.NetTxBps = rxBps, txBps
}

// Fourth field of /proc/loadavg is "runnable/total"; total is the process
// count, already maintained by the kernel — no walk of /proc, which would be
// both the slow way and the way that reads other people's cmdlines.
func linuxProcs(s *System) {
	raw, ok := readSmall("/proc/loadavg")
	if !ok {
		s.miss(mProcs)
		return
	}
	f := strings.Fields(raw)
	if len(f) < 4 {
		s.miss(mProcs)
		return
	}
	_, totalStr, found := strings.Cut(f[3], "/")
	if !found {
		s.miss(mProcs)
		return
	}
	n, err := strconv.ParseFloat(totalStr, 64)
	if err != nil || n <= 0 || !finite(n) {
		s.miss(mProcs)
		return
	}
	s.Procs = fp(n)
}

// Hottest thermal zone. Millidegrees in, °C out; a zone that reads absurd
// (≤0 or >150°C) is a broken or sleeping sensor and contributes nothing.
// No reading at all is silent by design — see TempC in system.go.
func linuxTemp(s *System) {
	// A fixed, shallow scan: zone indices are small integers in practice, and a
	// bounded loop cannot be steered by whatever else lives under /sys.
	best := 0.0
	found := false
	for i := 0; i < 24; i++ {
		raw, ok := readSmall("/sys/class/thermal/thermal_zone" + strconv.Itoa(i) + "/temp")
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || !finite(n) {
			continue
		}
		c := n / 1000
		if c <= 0 || c > 150 {
			continue
		}
		if !found || c > best {
			best, found = c, true
		}
	}
	if found {
		s.TempC = fp(round2(best))
	}
}

func linuxLoad(s *System) {
	raw, ok := readSmall("/proc/loadavg")
	if !ok {
		s.miss(mLoad)
		return
	}
	f := strings.Fields(raw)
	if len(f) < 3 {
		s.miss(mLoad)
		return
	}
	out := make([]*float64, 3)
	for i := 0; i < 3; i++ {
		n, err := strconv.ParseFloat(f[i], 64)
		if err != nil || n < 0 || !finite(n) {
			s.miss(mLoad)
			return
		}
		out[i] = fp(round2(n))
	}
	s.Load1, s.Load5, s.Load15 = out[0], out[1], out[2]
}

func linuxUptime(s *System) {
	raw, ok := readSmall("/proc/uptime")
	if !ok {
		s.miss(mUptime)
		return
	}
	f := strings.Fields(raw)
	if len(f) < 1 {
		s.miss(mUptime)
		return
	}
	n, err := strconv.ParseFloat(f[0], 64)
	if err != nil || n < 0 || !finite(n) {
		s.miss(mUptime)
		return
	}
	s.UptimeSec = fp(round2(n))
}
