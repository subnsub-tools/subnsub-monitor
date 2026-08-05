//go:build linux

package main

// Linux reads everything out of /proc and one statfs. No cgo, no shelling out,
// no /etc/os-release (that file carries a pretty name and sometimes a build ID,
// and the kernel release alone answers the question this field is for).

import (
	"os"
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
	linuxTCP(&s)
	linuxProcs(&s)
	linuxTemp(&s)

	// "ok" means the collector ran and produced at least one measurement — not
	// that everything worked. A machine where /proc is masked (some hardened
	// containers) still reports its platform and core count, and the page shows
	// the gaps rather than an error card.
	s.OK = s.CPUPercent != nil || s.MemUsedPercent != nil || s.Load1 != nil ||
		s.UptimeSec != nil || s.DiskUsedPercent != nil || s.TCPEstab != nil || s.TCPTimeWait != nil
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
	// The same walk now feeds the per-interface rows too — one read of the
	// table, two uses of it (see nicCounters).
	perRx := make(map[string]float64, 8)
	perTx := make(map[string]float64, 8)
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
		perRx[name], perTx[name] = r, t
		sawAny = true
	}
	if !sawAny {
		s.miss(mNetwork)
		return
	}
	// The totals stand on their own: unlike the rate below they need no second
	// sample, so the first push after a restart already carries them.
	s.NetRxTotal, s.NetTxTotal = fp(rx), fp(tx)
	nicCounters(s, perRx, perTx)
	rxBps, txBps := netDelta(rx, tx, now())
	if rxBps == nil {
		// First sample since startup, or the sum went backwards (an interface
		// vanished). Both are "no reading yet", and saying so beats a zero.
		s.miss(mNetwork)
		return
	}
	s.NetRxBps, s.NetTxBps = rxBps, txBps
}

// TCP MIB values are named by the preceding header line; their positions have
// changed across kernels, so a positional parser can quietly turn MaxConn or
// InSegs into a plausible-looking established count. MaxConn is legitimately
// -1 and is ignored rather than making the whole line invalid.
func parseLinuxTCPSNMP(raw string) (estab, retrans float64, ok bool) {
	lines := strings.Split(raw, "\n")
	for i := 0; i+1 < len(lines); i++ {
		h := strings.Fields(lines[i])
		v := strings.Fields(lines[i+1])
		if len(h) < 2 || len(v) != len(h) || h[0] != "Tcp:" || v[0] != "Tcp:" {
			continue
		}
		cols := make(map[string]string, len(h)-1)
		for j := 1; j < len(h); j++ {
			cols[h[j]] = v[j]
		}
		e, e1 := strconv.ParseFloat(cols["CurrEstab"], 64)
		r, e2 := strconv.ParseFloat(cols["RetransSegs"], 64)
		if e1 != nil || e2 != nil || e < 0 || r < 0 || !finite(e) || !finite(r) {
			return 0, 0, false
		}
		return e, r, true
	}
	return 0, 0, false
}

func parseLinuxTCPTimeWait(raw string) (float64, bool) {
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "TCP:" {
			continue
		}
		for i := 1; i+1 < len(f); i += 2 {
			if f[i] != "tw" {
				continue
			}
			n, err := strconv.ParseFloat(f[i+1], 64)
			return n, err == nil && n >= 0 && finite(n)
		}
	}
	return 0, false
}

func linuxTCP(s *System) {
	if raw, read := readSmall("/proc/net/snmp"); read {
		if estab, retrans, ok := parseLinuxTCPSNMP(raw); ok {
			s.TCPEstab = fp(estab)
			s.TCPRetransPS = tcpDelta(retrans, now())
		}
	}
	if raw, read := readSmall("/proc/net/sockstat"); read {
		if tw, ok := parseLinuxTCPTimeWait(raw); ok {
			s.TCPTimeWait = fp(tw)
		}
	}
}

// PROCESSES, counted as the numeric directories under /proc. An earlier
// version read /proc/loadavg's "runnable/total" — cheaper, but total there is
// scheduling ENTITIES, i.e. threads, and one busy JVM inflates it by
// thousands. The card says "procs", so it counts processes: names only, no
// entry is opened, and directory names carry none of the identity this file
// refuses (see the header — cmdlines are exactly what this walk does NOT
// read).
func linuxProcs(s *System) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		s.miss(mProcs)
		return
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "" || name[0] < '1' || name[0] > '9' {
			continue // PIDs never start with 0
		}
		digits := true
		for i := 1; i < len(name); i++ {
			if name[i] < '0' || name[i] > '9' {
				digits = false
				break
			}
		}
		if digits {
			n++
		}
	}
	if n == 0 {
		s.miss(mProcs)
		return
	}
	s.Procs = fp(float64(n))
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
