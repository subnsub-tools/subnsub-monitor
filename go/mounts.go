package main

// The filesystems other than root, and the line this file holds while it
// reports them.
//
// A mount point is not the same kind of fact as a disk's size. "/" says
// nothing about anyone; "/mnt/backup" describes a disk; "/home/alice/projects"
// describes a person, and system.go's opening rule refuses exactly that. The
// answer is not to drop the section — a server whose data disk is full has a
// real problem that a root-only card cannot show — but to FOLD the path until
// only the disk is left in it:
//
//	/data                     → /data
//	/mnt/backup               → /mnt/backup
//	/home/alice               → /home
//	/home/alice/projects/big  → /home
//	/var/lib/docker/overlay2  → /var/lib/docker
//
// Home directories collapse to the home itself because the segment after them
// is a username by construction. Everything else keeps three segments, which
// is enough for the paths people actually mount disks on and short of the
// depth where a path starts describing what somebody is working on.
//
// The relay applies this same fold again on arrival. Not redundancy: the
// helper's rule binds this helper, and the relay's rule binds every OTHER
// agent that ever pushes to it.

import (
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// How many non-root filesystems one machine may report. A build host with
	// forty container overlays has a list nobody reads long before this, and
	// the ones that matter are the biggest — which is what capMounts keeps.
	maxMounts = 8
	// A folded path is at most three short segments. The cap is a backstop
	// against a single absurd segment, not the main limit.
	maxMountPath = 40
	// How deep a path may be before it stops describing a disk. Three keeps
	// /var/lib/docker and macOS's /System/Volumes/Data whole.
	maxMountDepth = 3
)

// Where a user's own directory lives on each platform. A path under one of
// these keeps the home and drops everything below it — the next segment is a
// username, and the ones after that are somebody's work.
var homeRoots = map[string]bool{"home": true, "Users": true}

// Filesystems that are a DISK. An allowlist rather than a list of things to
// skip, because the skip list is unbounded and gets longer with every kernel
// release: tmpfs, devtmpfs, cgroup2, overlay, squashfs (every snap package is
// one), efivarfs, tracefs, autofs, nsfs, and whatever ships next. Missing one
// of those puts a fake "disk" on the card; missing a real filesystem here
// leaves a real disk off it, which is the failure that gets noticed and fixed.
var realFsTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "btrfs": true, "zfs": true, "f2fs": true, "jfs": true,
	"reiserfs": true, "nilfs2": true, "bcachefs": true,
	"vfat": true, "exfat": true, "msdos": true,
	"ntfs": true, "ntfs3": true, "fuseblk": true,
	"apfs": true, "hfs": true, "hfsplus": true,
	"ufs": true, "ffs": true,
}

// Fold one mount point down to the disk it describes. Returns "" for anything
// that should not be sent at all — the root (which has its own fields), a
// relative path, an empty one.
//
// A path that does not start with "/" is not folded by segment: on Windows a
// mount point is a volume root like `D:\`, which has no segments to walk and
// no user directory to hide. It is length-capped and passed through.
func foldMountPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		return capMountPath(p)
	}
	segs := make([]string, 0, maxMountDepth)
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." {
			continue
		}
		// A mount table should never contain "..", and a path that walks
		// upwards is not one this can reason about. Refuse rather than
		// resolve: resolving is the kernel's job and guessing is nobody's.
		if seg == ".." {
			return ""
		}
		segs = append(segs, seg)
		// The home rule wins wherever it applies, at whatever depth: /home is
		// the usual one, but a machine mounting /export/home/alice folds at the
		// same segment for the same reason.
		if homeRoots[seg] {
			return capMountPath("/" + strings.Join(segs, "/"))
		}
		if len(segs) >= maxMountDepth {
			break
		}
	}
	if len(segs) == 0 {
		return ""
	}
	return capMountPath("/" + strings.Join(segs, "/"))
}

// Cut to the cap on a RUNE boundary. A byte-level cut can split a multi-byte
// character and put an invalid sequence on the wire, which is the sort of thing
// that survives every test written in English.
func capMountPath(p string) string {
	if utf8.RuneCountInString(p) <= maxMountPath {
		return p
	}
	n := 0
	for i := range p {
		if n == maxMountPath-1 {
			return p[:i] + "…"
		}
		n++
	}
	return p
}

// How long a scan of the mount table stays good for. Everything else in
// collectSystem is a few /proc reads; this one is a statfs PER FILESYSTEM, and
// the sampler calls the collector once a second. Thirty seconds is the push
// interval, so the number on the card is never older than the frame it rides
// in — and a disk that fills fast enough for thirty seconds to matter was
// already past saving when the last frame said 99%.
//
// It also bounds a hazard the rest of the collector does not have: statfs can
// BLOCK. The filesystem allowlist keeps network mounts out, but a failing local
// disk can still take its time, and a scan per second would put that in the way
// of every reading rather than one in thirty.
const mountsRefreshSec = 30

var mountsCache struct {
	sync.Mutex
	at   float64
	out  []Mount
	done bool
}

// Run the platform's scan, or hand back the last one if it is still fresh.
// Refusing to hold a lock across the scan is deliberate: two collections
// racing is a wasted scan, while a scan holding the lock would make one slow
// filesystem block every other reading in the process.
func fillMounts(s *System, scan func() []Mount) {
	now := now()
	mountsCache.Lock()
	fresh := mountsCache.done && now-mountsCache.at < mountsRefreshSec && now >= mountsCache.at
	out := mountsCache.out
	mountsCache.Unlock()

	if !fresh {
		out = capMounts(scan())
		mountsCache.Lock()
		mountsCache.at, mountsCache.out, mountsCache.done = now, out, true
		mountsCache.Unlock()
	}
	if len(out) > 0 {
		// A copy: the cache hands the same backing array to every snapshot, and
		// a caller that sorted or trimmed it in place would be editing what the
		// next twenty-nine seconds of frames report.
		s.Mounts = append([]Mount(nil), out...)
	}
}

// Fold, drop what folding refused, drop duplicates, and keep the biggest.
//
// Duplicates are the normal case rather than an edge one: folding is lossy on
// purpose, so /home/alice and /home/bob arrive here as two rows called /home,
// and a btrfs volume with four subvolumes mounted is four rows with identical
// numbers. The first one wins — they are the same filesystem, and the card has
// nothing to gain from saying so four times.
//
// Biggest-first for the cap, then alphabetical for display: the same two-step
// capNICs uses, and for the same reason. A list sorted by size would reshuffle
// itself whenever two disks crossed over, and rows that move under the cursor
// are rows nobody can click.
func capMounts(in []Mount) []Mount {
	seen := make(map[string]bool, len(in))
	out := make([]Mount, 0, len(in))
	for _, m := range in {
		path := foldMountPath(m.Path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		m.Path = path
		out = append(out, m)
	}
	if len(out) > maxMounts {
		sort.SliceStable(out, func(i, j int) bool {
			ti, tj := 0.0, 0.0
			if out[i].Total != nil {
				ti = *out[i].Total
			}
			if out[j].Total != nil {
				tj = *out[j].Total
			}
			return ti > tj
		})
		out = out[:maxMounts]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
