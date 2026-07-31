//go:build !linux && !darwin && !windows

package main

// Everything with no collector of its own — which is now the BSDs, illumos and
// anything else someone points a Go toolchain at. Nothing here is unreachable
// code: the helper should compile and report honestly rather than fail to build
// or claim a healthy machine it never measured.
//
// A FreeBSD build lands here and is a real, published target: it reports its
// platform, its architecture and its core count, and says plainly that it
// measured nothing else. The reason it gets no more than that is the same one
// system.go gives for macOS — syscall.Sysctl returns a Go string cut at the
// first NUL and SysctlUint32 refuses anything not exactly four bytes wide, so
// hw.physmem, vm.loadavg and kern.cp_time all come back empty or, worse,
// plausibly wrong. The read that WOULD work here, statfs, is written against
// the Linux and Darwin field names; the BSDs spell them F_bsize and F_blocks,
// and a version of it typed against a header this author cannot run is exactly
// the kind of guess the rest of this file exists to refuse.

func systemSnapshot() System {
	s := baseSystem()
	s.miss(mCPU)
	s.miss(mMemory)
	s.miss(mSwap)
	s.miss(mDisk)
	s.miss(mLoad)
	s.miss(mUptime)
	return s
}
