//go:build linux || darwin

package main

import "syscall"

// Root filesystem usage, the way df reports it: the denominator excludes the
// blocks reserved for root, so a disk df already calls full does not show up
// here as 95% with room to spare.
//
// Shared between Linux and macOS because the field WIDTHS differ (Bsize is
// int64 on Linux and uint32 on Darwin) but the names and meanings do not, and
// float64() converts both without the file having to care which it got. The
// block size itself IS platform-specific — see fsBlockSize in each.
//
// Both the percentage and the absolute used figure are sent. They cannot be
// derived from each other: the percentage's denominator excludes reserved
// blocks and DiskTotal includes them, so total × percent overstates usage by
// however much the filesystem is holding back for root (5% by default on
// ext4 — enough to be visibly wrong on a large disk).
func statfsRoot(s *System) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		s.miss(mDisk)
		return
	}
	bs := fsBlockSize(&st)
	total := float64(st.Blocks) * bs
	free := float64(st.Bfree) * bs
	avail := float64(st.Bavail) * bs
	used := total - free
	if bs <= 0 || total <= 0 || used < 0 || used+avail <= 0 {
		s.miss(mDisk)
		return
	}
	s.DiskTotal = fp(total)
	s.DiskUsed = fp(used)
	s.DiskUsedPercent = pct(used, used+avail)
}
