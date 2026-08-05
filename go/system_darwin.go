//go:build darwin

package main

// macOS health, and why it is read the way it is.
//
// IN-PROCESS IS STILL OUT OF REACH, and that part has not changed. The standard
// library offers exactly two ways to read a sysctl:
//
//	syscall.Sysctl(name)        // returns a Go string, CUT AT THE FIRST NUL
//	syscall.SysctlUint32(name)  // errors unless the value is exactly 4 bytes
//
// Between them that rules out hw.memsize (64-bit — a 16 GiB machine's
// little-endian bytes start 00 00 00 00 04, so Sysctl returns "" and
// SysctlUint32 errors on the width), vm.loadavg (a struct of fixed-point
// values), vm.swapusage (a struct) and kern.boottime (a timeval). x/sys/unix
// has SysctlRaw, which would answer all of them, but adding it puts a
// dependency in a helper whose zero-dependency build is what makes "read the
// source, then run it" a reasonable offer. The mach APIs behind CPU and memory
// (host_processor_info, host_statistics64) need cgo, and CGO_ENABLED=0 is what
// makes the Linux binaries genuinely static.
//
// SO THE NUMBERS COME FROM APPLE'S OWN TOOLS INSTEAD. An earlier version of
// this file reported disk and nothing else and named the rest missing, on the
// reasoning that a fabricated number is worse than an absent one. That
// reasoning is still right; the conclusion drawn from it was too broad.
// Running a subprocess is not a new capability here — the Amp, Kiro, Auggie
// and arkcli collectors have always done it — and these five are Apple's, on
// the read-only system volume, addressed by absolute path, handed a
// four-variable environment, bounded, deadlined, and parsed by something that
// refuses everything it does not fully recognise (see macstats.go). The bar
// did not move. What moved is that the bar can now be cleared.
//
// COSTS, STATED. Five short-lived processes per collection, run concurrently,
// so the wall-clock is the slowest one rather than the sum. Four of them are
// milliseconds. `top` is the exception and costs a full second by design: it
// is the only no-cgo way to get a CPU RATE, because macOS exposes no
// cumulative tick counter outside mach, so the sample window has to be waited
// out rather than differenced against a previous push.
//
// WHAT IS STILL MISSING: swap. vm.swapusage is trivially readable and does not
// mean what the field means — macstats.go carries the full reason where anyone
// reaching for the easy sysctl will hit it first. That is not a gap waiting
// for effort: the number could be shipped today and would be wrong, which is
// the one thing this collector will not do.
//
// Network sat beside it in this list for a while, kept out by a FORMAT hazard
// rather than a meaning problem — netstat's table repeats every interface once
// per address family and shifts its columns row by row, which is how a
// positional parse ends up reading somebody else's column. But a parsing
// problem has a parsing answer: parseMacNetstat anchors itself to the header
// and the `<Link#N>` rows and refuses the whole table at the first row it
// cannot fully account for. Swap's problem is what the number MEANS, and no
// parser fixes that.

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Absolute paths, never PATH — the same rule vendorBinary follows in
// clirun.go, and it matters more here, not less: these run on every collection
// and the names are short enough to be shadowed by anything a user has in
// front of /usr/bin.
const (
	macSysctl  = "/usr/sbin/sysctl"
	macVMStat  = "/usr/bin/vm_stat"
	macPS      = "/bin/ps"
	macTop     = "/usr/bin/top"
	macNetstat = "/usr/sbin/netstat"
)

const macMaxOut = 64 << 10 // ~10k PIDs from ps; everything else is a few lines

// Darwin's statfs has no f_frsize; f_bsize IS the fundamental block size here,
// and is what df scales by on this platform.
func fsBlockSize(st *syscall.Statfs_t) float64 { return float64(st.Bsize) }

func systemSnapshot() System {
	s := baseSystem()

	// kern.osrelease is a genuine C string — the one sysctl shape Sysctl
	// handles without qualification. It is the Darwin kernel version (24.x for
	// macOS 15), not the marketing version, and gets cut to major.minor like
	// every other platform's.
	if v, err := syscall.Sysctl("kern.osrelease"); err == nil {
		s.OSVersion = shortKernel(strings.TrimSpace(v))
	}

	// hw.ncpu fits in 32 bits and is the one count this can read directly.
	// runtime.NumCPU() already filled CPUCount in and respects CPU affinity,
	// which is the more useful answer; this only corrects it if the runtime
	// somehow reported nothing.
	if s.CPUCount <= 0 {
		if n, err := syscall.SysctlUint32("hw.ncpu"); err == nil && n > 0 {
			s.CPUCount = int(n)
		}
	}

	ctl, vmst, pids, cpu, net, netAt := macReadAll()

	// Same order as the Linux collector, so `missing` reads the same on both
	// platforms and the page can render one stable list.
	macCPU(&s, cpu)
	macMem(&s, ctl, vmst)
	// Swap is named missing rather than reported. vm.swapusage is readable and
	// does not mean what this field means — macstats.go says why at length, and
	// says it there so that anyone who notices the easy sysctl and reaches for
	// it reads the reason first.
	s.miss(mSwap)
	macLoad(&s, ctl)
	macUptime(&s, ctl)
	statfsRoot(&s)
	macNet(&s, net, netAt)
	macProcs(&s, pids)

	// "ok" means the collector ran and produced at least one measurement — not
	// that everything worked. Matches Linux: a machine where one tool is
	// missing still reports its platform and its other readings, and the page
	// shows the gaps rather than an error card.
	s.OK = s.CPUPercent != nil || s.MemUsedPercent != nil || s.Load1 != nil ||
		s.UptimeSec != nil || s.DiskUsedPercent != nil
	return s
}

// The five reads, concurrently. Independent on purpose: a tool that is absent,
// slow or unrecognisable costs its own reading and nothing else. Serialising
// them would put `top`'s deliberate one-second window in front of four reads
// that take milliseconds, and collectSystem runs AFTER every provider has
// finished — the budget it is spending is the one between a push and the
// relay's 90-second offline threshold.
func macReadAll() (ctl map[string]string, vmst, pids, cpu, net string, netAt float64) {
	var wg sync.WaitGroup
	var ctlOut string
	read := func(dst *string, bin string, args []string, deadline time.Duration, tolerateExit bool) {
		defer wg.Done()
		*dst = runMacTool(bin, args, deadline, tolerateExit)
	}
	wg.Add(5)
	// sysctl alone is allowed to fail: it exits non-zero when any ONE of the
	// names it was given is unknown, having printed all the others correctly.
	go read(&ctlOut, macSysctl, []string{"hw.memsize", "vm.loadavg", "kern.boottime"}, 5*time.Second, true)
	go read(&vmst, macVMStat, nil, 5*time.Second, false)
	go read(&pids, macPS, []string{"-A", "-o", "pid="}, 8*time.Second, false)
	// -n matters for the deadline as much as the parse: without it netstat
	// resolves names, and a machine with a slow resolver would lose its network
	// reading to DNS rather than to anything about the network.
	//
	// The clock is read the moment netstat returns, not after the Wait below.
	// These counters feed a rate whose denominator is the time between two
	// CAPTURES, and the wait lasts as long as `top`'s sampling window — a
	// duration that varies push to push and says nothing about when the
	// counters were read. Pairing them with the later clock would stretch or
	// shrink every rate by exactly that jitter.
	go func() {
		defer wg.Done()
		net = runMacTool(macNetstat, []string{"-i", "-b", "-n"}, 5*time.Second, false)
		netAt = now()
	}()
	// One second of sampling plus room for a loaded machine to get around to
	// printing it. A deadline that only just covers the happy path turns a busy
	// box into a box with no CPU reading, which is the one time the reading is
	// worth having.
	go read(&cpu, macTop, []string{"-l", "2", "-n", "0", "-s", "1"}, 10*time.Second, false)
	wg.Wait()

	return parseSysctlPairs(ctlOut), vmst, pids, cpu, net, netAt
}

// Run one Apple tool and return its stdout, or "" if there is nothing worth
// parsing.
//
// The environment is four fixed entries rather than a filtered copy of this
// process's. The allowlist in clirun.go exists to keep this helper's relay
// token out of a third party's address space; here there is no third party and
// no reason to hand over anything at all, and pinning the locale keeps a
// machine configured for another language from formatting the numbers
// differently than the parsers expect.
//
// A non-zero exit is tolerated ONLY where the caller says so, and only sysctl
// does: it exits non-zero when any one of the names it was given is unknown,
// having correctly printed all the others, and dropping those would trade good
// readings for one absent MIB. For vm_stat, ps and top a failed run means the
// text is a prefix of something, and a process count or a memory figure
// scraped off a prefix is exactly the plausible-but-wrong number this file
// refuses to produce.
//
// And only a clean non-zero EXIT counts as that: errors.As pins the tolerance
// to *exec.ExitError, so a run cut short by WaitDelay — which also returns a
// non-nil error, with whatever output had been copied so far — is refused
// along with everything else.
func runMacTool(bin string, args []string, deadline time.Duration, tolerateExit bool) string {
	if !usableBinary(bin) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	cmd := toolCommand(ctx, bin, args...)
	cmd.Stdin = nil  // the null device: nothing here should ever wait on input
	cmd.Stderr = nil // ditto — sysctl's "unknown oid" belongs nowhere
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "TERM=dumb", "NO_COLOR=1"}
	// Without this the deadline is a suggestion — see runAmpUsage for the full
	// story about a grandchild holding the pipe open past a context cancel.
	cmd.WaitDelay = 2 * time.Second

	var stdout bytes.Buffer
	lo := &limitedWriter{w: &stdout, n: macMaxOut}
	cmd.Stdout = lo

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded || lo.over {
		return ""
	}
	if err != nil {
		var exit *exec.ExitError
		if !tolerateExit || !errors.As(err, &exit) {
			return ""
		}
	}
	return stdout.String()
}

func macCPU(s *System, out string) {
	if p, ok := parseMacTopBusy(out); ok {
		s.CPUPercent = p
		return
	}
	s.miss(mCPU)
}

// Total from hw.memsize (exact bytes, straight from the kernel), the available
// figure from vm_stat's page counts. Two sources for one percentage, which is
// worth one guard: page counts that add up to more than the machine has means
// the two disagree about what they are counting, and a percentage computed
// across that disagreement would be arbitrary.
func macMem(s *System, ctl map[string]string, vmst string) {
	total, err := strconv.ParseFloat(ctl["hw.memsize"], 64)
	pageSize, pages, okStat := parseVMStat(vmst)
	if err != nil || total <= 0 || !finite(total) || !okStat {
		s.miss(mMemory)
		return
	}
	avail, okAvail := macMemAvailable(pages, pageSize)
	if !okAvail || avail > total {
		s.miss(mMemory)
		return
	}
	s.MemTotal = fp(total)
	s.MemUsedPercent = pct(total-avail, total)
}

func macLoad(s *System, ctl map[string]string) {
	l1, l5, l15, ok := parseMacLoadavg(ctl["vm.loadavg"])
	if !ok {
		s.miss(mLoad)
		return
	}
	s.Load1, s.Load5, s.Load15 = l1, l5, l15
}

// Uptime is derived, not read: macOS reports when the machine booted and the
// difference is taken here.
//
// That makes it the one reading whose correctness depends on the CLOCK as well
// as the source, so both ends are bounded. A boot time in the future is a
// machine whose clock has just been stepped (or a VM restored from a snapshot),
// and a negative uptime rendered as a duration is worse than no uptime at all.
func macUptime(s *System, ctl map[string]string) {
	boot, ok := parseMacBoottime(ctl["kern.boottime"])
	if !ok {
		s.miss(mUptime)
		return
	}
	up := now() - boot
	if up <= 0 || up > 100*365*24*3600 || !finite(up) {
		s.miss(mUptime)
		return
	}
	s.UptimeSec = fp(round2(up))
}

// Whole-box traffic through the same differencing as Linux: netstat's
// counters are cumulative bytes, netDelta turns two samples into a rate, and
// the first collection after startup reports nothing — that gap is stated in
// `missing` rather than papered over with a zero, exactly as linuxNet does.
// `at` is the clock read the moment netstat returned — see macReadAll for why
// it travels with the counters instead of being read here.
//
// Differencing a SUM has one known blind spot, shared with Linux on purpose:
// an interface that vanishes between samples only surfaces when its loss
// drives the whole sum backwards (netDelta then reports nothing). Masked by
// enough growth elsewhere, it is absorbed as one understated sample and
// corrects on the next push. Differencing per interface would fix that
// one-sample dip — at the cost of the two platforms disagreeing about what
// this field means, which is worse than the dip.
func macNet(s *System, out string, at float64) {
	rx, tx, ok := parseMacNetstat(out)
	if !ok {
		s.miss(mNetwork)
		return
	}
	// Totals need no second sample, so they land on the first push — the rate
	// below still does not. Per-interface counters are not split out here:
	// netstat -ib repeats an interface once per address family and the sum
	// already folds those, which is exactly the fold the parser exists for.
	s.NetRxTotal, s.NetTxTotal = fp(rx), fp(tx)
	rxBps, txBps := netDelta(rx, tx, at)
	if rxBps == nil {
		// First sample since startup, or the sum went backwards (an interface
		// vanished — a VPN's utun being torn down is routine on a Mac). Both
		// are "no reading yet", and saying so beats a zero.
		s.miss(mNetwork)
		return
	}
	s.NetRxBps, s.NetTxBps = rxBps, txBps
}

func macProcs(s *System, out string) {
	n, ok := countMacPIDs(out)
	if !ok {
		s.miss(mProcs)
		return
	}
	s.Procs = fp(float64(n))
}
