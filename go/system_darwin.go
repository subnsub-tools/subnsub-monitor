//go:build darwin

package main

// macOS reports far less than Linux, and the reason is worth stating plainly
// so nobody "fixes" it by inventing numbers.
//
// The standard library offers exactly two ways to read a sysctl:
//
//	syscall.Sysctl(name)        // returns a Go string, CUT AT THE FIRST NUL
//	syscall.SysctlUint32(name)  // errors unless the value is exactly 4 bytes
//
// x/sys/unix has SysctlRaw, which would answer all of this — but adding it
// means adding a dependency to a helper whose zero-dependency build is the
// thing that makes "read the source, then run it" a reasonable offer. And the
// mach APIs (host_statistics64 for memory, host_processor_info for CPU) need
// cgo, which build.sh cannot turn on: CGO_ENABLED=0 is what makes the Linux
// binaries genuinely static.
//
// So, concretely:
//
//	hw.memsize     64-bit, and a 16 GiB machine's little-endian bytes start
//	               00 00 00 00 04 — Sysctl returns "" and SysctlUint32 errors.
//	vm.loadavg     a struct of three fixed-point values; same truncation.
//	kern.boottime  a timeval. Its first four bytes are usually non-zero, so
//	               there IS a trick available here — and it is not taken. It
//	               depends on a byte layout this author cannot test on real
//	               hardware, and a silently wrong boot time is worse than an
//	               absent one: it would render as a machine that rebooted
//	               minutes ago and send someone looking for a crash.
//
// What is left reads cleanly and is reported. The rest is named in `missing`,
// and the page says "not available on this platform" rather than showing zero.

import (
	"strings"
	"syscall"
)

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

	statfsRoot(&s)

	s.miss(mCPU)
	s.miss(mMemory)
	s.miss(mSwap)
	s.miss(mLoad)
	s.miss(mUptime)

	s.OK = s.DiskUsedPercent != nil
	return s
}
