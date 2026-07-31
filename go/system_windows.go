//go:build windows

package main

// Windows has no /proc and no sysctl. Everything here comes from four Win32
// calls, reached through LazyDLL rather than cgo because CGO_ENABLED=0 is what
// makes every build in this project reproducible and static.
//
// kernel32 and ntdll are KnownDLLs — Windows resolves those two from System32
// and never from the directory the program was started in — so loading them by
// bare name is not the DLL-planting hazard the same line would be for an
// ordinary library. That property is the reason this file uses no other DLL.
//
// EVERY CALL BELOW IS SPELLED OUT AT ITS OWN SITE, and the repetition is the
// point rather than an oversight.
//
// The compiler has one rule that makes `uintptr(unsafe.Pointer(&x))` safe: the
// conversion must appear IN THE ARGUMENT LIST of a call to an
// assembly-implemented function, of which syscall.SyscallN is one. It then
// keeps the object alive and unmoved for that call. Hand the same expression to
// an ordinary Go helper — even one that does nothing but forward it to
// SyscallN — and the rule no longer applies: by the time the real call happens
// the pointer is a plain number the collector never saw, and a stack growth in
// between can move what it pointed at. That is what an earlier version of this
// file did while a comment claimed the opposite, which is the worse half of the
// bug: it would have been correct almost always and wrong under a garbage
// collection nobody could reproduce.
//
// runtime.KeepAlive does not rescue it either. KeepAlive stops a collection; it
// does not stop the stack from moving, and the uintptr was already taken.
//
// WHAT WINDOWS CANNOT ANSWER, and is therefore reported as missing rather than
// guessed at:
//
//	load average   There is no such counter. It is a Unix idea — a decaying
//	               average of the run queue — and Windows has never kept one.
//	               "Processor Queue Length" is an instantaneous depth, not an
//	               average, and putting it in a field labelled load1 would put
//	               a number with different units and a different meaning under
//	               a heading people read at a glance.
//	swap           GlobalMemoryStatusEx reports the COMMIT LIMIT and the commit
//	               charge, which is not the page file. The limit is roughly RAM
//	               plus the page file, so the usual trick is to subtract — and
//	               it is a trick: non-paged pools and a page file Windows is
//	               free to grow both move it. A machine that reads 60% swapped
//	               when nothing has been paged out is worse than one that says
//	               it does not know.

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"
)

var (
	kernel32System           = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes       = kernel32System.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32System.NewProc("GlobalMemoryStatusEx")
	procGetDiskFreeSpaceExW  = kernel32System.NewProc("GetDiskFreeSpaceExW")
	procGetTickCount64       = kernel32System.NewProc("GetTickCount64")

	ntdll             = syscall.NewLazyDLL("ntdll.dll")
	procRtlGetVersion = ntdll.NewProc("RtlGetVersion")
)

// Every symbol is resolved before it is used. LazyProc.Addr panics on one that
// is not there, and "not there" is a real state on Wine, on ReactOS, and on
// whatever Microsoft trims out of the next server SKU — a missing counter
// should cost that one reading and nothing else.
//
// Resolution only. It deliberately does NOT make the call, because a helper
// that took the arguments would be the very shape the note above rules out.
func winReady(p *syscall.LazyProc) bool { return p.Find() == nil }

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

type osVersionInfoExW struct {
	OSVersionInfoSize uint32
	MajorVersion      uint32
	MinorVersion      uint32
	BuildNumber       uint32
	PlatformID        uint32
	CSDVersion        [128]uint16
	ServicePackMajor  uint16
	ServicePackMinor  uint16
	SuiteMask         uint16
	ProductType       uint8
	Reserved          uint8
}

func systemSnapshot() System {
	s := baseSystem()

	winVersion(&s)
	winCPU(&s)
	winMem(&s)
	winDisk(&s)
	winUptime(&s)

	// No load average exists to read. Named rather than left out, so the page
	// says "not available here" instead of rendering an idle machine.
	s.miss(mLoad)
	// Nor a page-file figure worth the name — see the note at the top.
	s.miss(mSwap)

	s.OK = s.CPUPercent != nil || s.MemUsedPercent != nil ||
		s.UptimeSec != nil || s.DiskUsedPercent != nil
	return s
}

// RtlGetVersion rather than GetVersionEx, and the difference matters here more
// than usual: GetVersionEx lies to a program whose manifest does not claim
// support for the running Windows, reporting 6.2 on everything since Windows 8.
// A helper cross-compiled on Linux has no manifest at all, so it would report
// every machine in the fleet as Windows 8.
//
// Cut to major.minor like every other platform's, which for the NT kernel means
// "10.0" on both Windows 10 and Windows 11. That is coarse on purpose and it is
// the same bar system.go sets for Linux: the build number, which is the part
// that would tell you 23H2 from 24H2, also tells anyone reading the relay which
// patch level to try things against.
func winVersion(s *System) {
	if !winReady(procRtlGetVersion) {
		return
	}
	var v osVersionInfoExW
	v.OSVersionInfoSize = uint32(unsafe.Sizeof(v))
	// NTSTATUS, so zero is success — the opposite of the BOOL convention every
	// other call in this file follows.
	if st, _, _ := syscall.SyscallN(procRtlGetVersion.Addr(), uintptr(unsafe.Pointer(&v))); st != 0 {
		return
	}
	s.OSVersion = shortKernel(
		strconv.FormatUint(uint64(v.MajorVersion), 10) + "." +
			strconv.FormatUint(uint64(v.MinorVersion), 10))
}

// GetSystemTimes, whose three FILETIMEs are cumulative since boot in 100ns
// units. The kernel figure INCLUDES idle — that is the trap in this API, and
// treating them as separate buckets inflates the total and quietly deflates
// every percentage, the same mistake /proc/stat invites with guest time.
func winCPU(s *System) {
	if !winReady(procGetSystemTimes) {
		s.miss(mCPU)
		return
	}
	var idle, kernel, user syscall.Filetime
	r, _, _ := syscall.SyscallN(procGetSystemTimes.Addr(),
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		s.miss(mCPU)
		return
	}
	total := filetimeF(kernel) + filetimeF(user)
	busy := total - filetimeF(idle)
	if p := cpuDelta(busy, total); p != nil {
		s.CPUPercent = p
	} else {
		// No previous sample yet. Said rather than shown as 0%, exactly as on
		// Linux: the first push after install would otherwise show a machine
		// with no CPU line and no explanation.
		s.miss(mCPU)
	}
}

func filetimeF(t syscall.Filetime) float64 {
	return float64(uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime))
}

// AvailPhys is what Windows will hand out without paging anything, which is the
// same quantity MemAvailable names on Linux and the same reason it is used
// here: a healthy machine keeps its cache full, and reporting free-and-nothing-
// else makes every one of them look about to die.
func winMem(s *System) {
	if !winReady(procGlobalMemoryStatusEx) {
		s.miss(mMemory)
		return
	}
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := syscall.SyscallN(procGlobalMemoryStatusEx.Addr(), uintptr(unsafe.Pointer(&m)))
	if r == 0 || m.TotalPhys == 0 || m.AvailPhys > m.TotalPhys {
		s.miss(mMemory)
		return
	}
	s.MemTotal = fp(float64(m.TotalPhys))
	s.MemUsedPercent = pct(float64(m.TotalPhys-m.AvailPhys), float64(m.TotalPhys))
}

// The system volume, which is what "/" means on the platforms with one.
//
// GetDiskFreeSpaceExW answers two different questions and both are wanted: the
// free space on the VOLUME, and the free space available TO THIS USER, which is
// smaller wherever a disk quota applies. Used comes from the first and the
// percentage's denominator from the second, exactly as statfsRoot uses f_bfree
// and f_bavail — so a quota that has been reached reads as full, and the
// absolute figures still describe the disk.
func winDisk(s *System) {
	p, err := syscall.UTF16PtrFromString(winSystemRoot())
	if err != nil || !winReady(procGetDiskFreeSpaceExW) {
		s.miss(mDisk)
		return
	}
	var availToCaller, total, totalFree uint64
	r, _, _ := syscall.SyscallN(procGetDiskFreeSpaceExW.Addr(),
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&availToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 || total == 0 || totalFree > total {
		s.miss(mDisk)
		return
	}
	used := float64(total) - float64(totalFree)
	if used+float64(availToCaller) <= 0 {
		s.miss(mDisk)
		return
	}
	s.DiskTotal = fp(float64(total))
	s.DiskUsed = fp(used)
	s.DiskUsedPercent = pct(used, used+float64(availToCaller))
}

// Where Windows itself is installed, reduced to its volume root. Read from the
// environment rather than assumed to be C:, because it is not always C: — and
// falling back to a literal only after both variables have failed keeps a
// machine with an unusual layout reporting its own disk rather than a wrong one.
func winSystemRoot() string {
	for _, name := range []string{"SystemRoot", "windir"} {
		if v := os.Getenv(name); v != "" {
			if vol := filepath.VolumeName(v); vol != "" {
				return vol + `\`
			}
		}
	}
	if v := filepath.VolumeName(os.Getenv("SystemDrive")); v != "" {
		return v + `\`
	}
	return `C:\`
}

// GetTickCount64: milliseconds since boot, and it does NOT count time the
// machine spent asleep. On a server — which is every machine this helper is
// built for — that is the same number as wall-clock uptime. On a laptop that
// hibernates nightly it reads lower, which is the safer direction for the one
// question this field answers: a small uptime means "recently up", and a
// machine that was asleep genuinely was not up.
func winUptime(s *System) {
	// A 64-bit return does not fit a 32-bit register pair the same way on every
	// architecture, and reassembling it wrong would report a plausible but
	// arbitrary uptime. This build targets 64-bit Windows; on anything else the
	// reading is refused rather than guessed.
	if ^uintptr(0)>>32 == 0 || !winReady(procGetTickCount64) {
		s.miss(mUptime)
		return
	}
	ms, _, _ := syscall.SyscallN(procGetTickCount64.Addr())
	if ms == 0 {
		s.miss(mUptime)
		return
	}
	s.UptimeSec = fp(round2(float64(ms) / 1000))
}
