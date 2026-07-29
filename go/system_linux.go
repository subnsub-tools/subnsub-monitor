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
