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
	total, used, usedPct, ok := statfsUsage("/")
	if !ok {
		s.miss(mDisk)
		return
	}
	s.DiskTotal, s.DiskUsed, s.DiskUsedPercent = fp(total), fp(used), usedPct
}

// One filesystem's numbers, by the rule above. Split out so the non-root
// mounts get measured exactly the way root is — a second implementation would
// be a second chance to use f_bfree where f_bavail belongs, and the two differ
// by the reserve that makes a "95% full" disk actually full.
//
// A filesystem that answers with nothing (an autofs point nobody has touched,
// a network mount whose server is away) is reported as unmeasurable rather
// than as an empty disk.
func statfsUsage(path string) (total, used float64, usedPct *float64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, nil, false
	}
	return statfsCalc(&st)
}

// The arithmetic on its own, for the caller that already HAS the struct: macOS
// enumerates its filesystems with getfsstat, which hands back a full statfs per
// mount, and calling statfs again on each path would be a second syscall that
// can block on a network volume whose server is away.
func statfsCalc(st *syscall.Statfs_t) (total, used float64, usedPct *float64, ok bool) {
	bs := fsBlockSize(st)
	total = float64(st.Blocks) * bs
	free := float64(st.Bfree) * bs
	avail := float64(st.Bavail) * bs
	used = total - free
	if bs <= 0 || total <= 0 || used < 0 || used+avail <= 0 {
		return 0, 0, nil, false
	}
	return total, used, pct(used, used+avail), true
}
