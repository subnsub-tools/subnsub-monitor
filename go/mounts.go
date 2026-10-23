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
// WHAT THIS DOES AND DOES NOT PROMISE, stated plainly because a review read
// the first version as claiming more than it delivers. The fold removes the
// segments an operating system GENERATES from an identity — the username under
// a home directory, the account under an automount root. It does NOT redact a
// name somebody chose to type, and a Mac's disk label is one: a
// machine that mounts a disk at /srv/customer-acme sends /srv/customer-acme,
// because that string is the answer to "which disk is full" and inventing a
// number in its place would leave the section unable to do its job.
//
// That is a narrower promise than "no identifying strings", and it is the one
// the code keeps. The escape hatch for anyone who wants the stricter version
// is the same one the interface list has: a self-hosted relay, where none of
// this reaches us at all.
//
//	/data                     → /data
//	/mnt/backup               → /mnt/backup
//	/home/alice               → /home
//	/home/alice/projects/big  → /home
//	/var/lib/docker/overlay2  → /var/lib/docker
//
//	/run/media/alice/stick    → /run/media
//	/Volumes/Alice Backup     → /Volumes/Alice Backup
//
// Home directories and the automount roots collapse to the anchor itself
// because the segment after them is an identity by construction. Everything
// else keeps three segments, which is enough for the paths people actually
// mount disks on and short of the depth where a path starts describing what
// somebody is working on.
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
	// the ones that matter are the fullest — which is what capMounts keeps.
	maxMounts = 8
	// A folded path is at most three short segments. The cap is a backstop
	// against a single absurd segment, not the main limit — and it is also the
	// row's identity, so it has room for a Mac volume name: two disks called
	// "MacBook Pro Time Machine Backup A" and "…B" cut to the same 40
	// characters were one tile reading the fuller (review 2026-10-23). The
	// relay's MAX_MOUNT_PATH is the same number.
	maxMountPath = 80
	// How deep a path may be before it stops describing a disk. Three keeps
	// /var/lib/docker and macOS's /System/Volumes/Data whole.
	maxMountDepth = 3
)

// Directories whose NEXT segment names a person rather than a disk. A path
// that reaches one of these keeps the anchor and drops everything below it.
//
// Depth alone cannot tell the two apart, which is the flaw a review found in
// the first version of this file: three segments of `/home/alice/projects` is
// a person, three segments of `/var/lib/docker` is a disk, and the rule that
// kept the second also kept the first's middle segment. So the fold is anchored
// on the places where the convention is known:
//
//	/home /Users /root /export/home    the login directories
//	/media /run/media                  where desktop Linux automounts a stick,
//	                                   as /media/<user>/<label>
//
// /Volumes is NOT one. macOS mounts every external disk there as
// /Volumes/<label>, with no account in the path, and folding it put every
// disk a Mac had plugged in on one tile reading the fullest of them. Each
// goes out under its own label now, the name Finder shows (operator ruling
// 2026-10-23: one tile per external disk, with its volume name; the Monitor
// keeps no identity-privacy rule since 2026-10-18).
//
// Whole prefixes rather than segment names, because /System/Volumes/Data is
// where macOS keeps its data volume on every machine ever made and carries no
// risk at all.
//
// What this does NOT do is decide that a path is safe because it is short.
// `/mnt/backup-acme` still goes out whole: a mount point somebody chose for a
// disk is the thing this section exists to report, and folding it away would
// leave a card that cannot say which disk is full. The line is drawn where a
// segment is machine-generated from an identity, not where it might have been
// typed carelessly.
var foldAnchors = map[string]bool{
	"/home": true, "/Users": true, "/root": true,
	"/export/home": true,
	"/media":       true, "/run/media": true,
}

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

// <sys/mount.h>'s MNT_RDONLY and MNT_DONTBROWSE, spelled out for the reason
// system_darwin.go spells out MNT_NOWAIT: the syscall package does not export
// them on darwin. Here rather than there so the rule below is tested on every
// platform the suite runs on.
const (
	macMntRdonly     = 0x00000001
	macMntDontBrowse = 0x00100000
)

// Whether a Mac mount is one of the system's own volumes rather than a disk
// somebody uses. APFS gives the one internal disk a mount point per volume —
// Data, VM, Preboot and Update in the main container, xarts, iSCPreboot and
// Hardware in the small iBoot one — and each arrived as a tile of its own:
// seven tiles for one disk, six of them reading a percent or two. Any one of
// three marks is enough:
//
//	/System/            Apple's own hierarchy, where nothing else can mount
//	CoreSimulator       the read-only image Xcode mounts per simulator runtime
//	read-only AND       a Time Machine snapshot, an image the system mounted
//	nobrowse            out of Finder's sight: it cannot fill, and nobody
//	                    put it there to be looked at
//
// Never either flag on its own. nobrowse is a mount option anyone can give a
// backup disk to keep it out of Finder, and an NTFS or write-protected disk
// mounts read-only; both are real disks whose fullness belongs on the card
// (review 2026-10-23).
//
// The internal disk's fullness goes nowhere with them: macRoot reports it,
// for the whole container, in the root figures.
func macMountHidden(path string, flags uint32) bool {
	if strings.HasPrefix(path, "/System/") || path == "/Library/Developer/CoreSimulator" ||
		strings.HasPrefix(path, "/Library/Developer/CoreSimulator/") {
		return true
	}
	return flags&macMntRdonly != 0 && flags&macMntDontBrowse != 0
}

// Fold one mount point down to the disk it describes. Returns "" for anything
// that should not be sent at all — the root (which has its own fields), a
// relative path, an empty one.
//
// A path that does not start with "/" gets exactly one shape: a Windows volume
// root, `D:` or `D:\`. Anything else is refused rather than passed through.
// The first version of this capped the length and sent it, which made
// `alice/private-project` a perfectly acceptable mount point as far as this
// helper was concerned — a relative path is not a mount point on any platform
// this builds for, so there is nothing to lose by saying no.
func foldMountPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" || pathHasBadRune(p) {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		if isVolumeRoot(p) {
			return p
		}
		return ""
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
		// An anchor is a whole PREFIX, not a segment name: /home/alice folds,
		// /srv/home/data does not. Matching the bare segment folded paths away
		// for a privacy risk they do not carry.
		if foldAnchors["/"+strings.Join(segs, "/")] {
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

// Characters a mount path may not contain: the C0 and C1 controls, and every
// invisible or direction-changing mark that can make one string render as
// another. Exactly the set the interface-name gate already refuses, and the
// same set on all three sides of this protocol.
//
// REFUSED, not stripped, and that distinction is the bug this closes. The
// relays used to remove these characters and then fold what was left, while
// the helper folded the raw string — so `/ho<ZWSP>me/alice` folded to /home on
// one side and kept `alice` on the other. Worse, stripping is a rewrite: it
// invents a path the machine never reported and then reasons about it. A path
// with a zero-width space in it is not a path this can vouch for.
func pathHasBadRune(p string) bool {
	for _, r := range p {
		switch {
		case r < 0x20, r >= 0x7f && r <= 0x9f:
			return true
		case r == 0x061c, r >= 0x200b && r <= 0x200f:
			return true
		case r == 0x2028, r == 0x2029, r >= 0x202a && r <= 0x202e:
			return true
		case r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x2069, r == 0xfeff:
			return true
		case r == 0xfffd:
			// A replacement character means the path already lost information
			// somewhere upstream; what it looked like originally is not
			// recoverable and not something to guess at.
			return true
		}
	}
	return false
}

// `D:` or `D:\` and nothing else. Deliberately not "starts with a letter and a
// colon": `C:\Users\alice` satisfies that and is the exact shape this refuses.
func isVolumeRoot(p string) bool {
	if len(p) == 2 {
		return isASCIILetter(p[0]) && p[1] == ':'
	}
	return len(p) == 3 && isASCIILetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
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
	at      float64 // when the cached scan FINISHED
	started float64 // when it began — see fillMounts
	out     []Mount
	done    bool
}

// Run the platform's scan, or hand back the last one if it is still fresh.
// Refusing to hold a lock across the scan is deliberate: two collections
// racing is a wasted scan, while a scan holding the lock would make one slow
// filesystem block every other reading in the process.
//
// That freedom needs two rules, and neither was here at first:
//
//	A LATER SCAN WINS, decided by when each one STARTED. Two collections can
//	overlap, and without this the one that began first and finished last
//	overwrites the fresher answer with its own older view of the mount table.
//
//	A FAILED SCAN IS NOT AN ANSWER. `scan` says whether it could read the
//	mount table at all, because "this machine has no filesystems besides root"
//	and "/proc/mounts could not be opened" are different facts that produce the
//	same empty slice — and caching the second as the first would hide a masked
//	/proc for thirty seconds at a time instead of retrying.
func fillMounts(s *System, scan func() ([]Mount, bool)) {
	started := now()
	mountsCache.Lock()
	fresh := mountsCache.done && started-mountsCache.at < mountsRefreshSec && started >= mountsCache.at
	out := mountsCache.out
	mountsCache.Unlock()

	if !fresh {
		list, ok := scan()
		mountsCache.Lock()
		if ok && (!mountsCache.done || started >= mountsCache.started) {
			mountsCache.started, mountsCache.at = started, now()
			mountsCache.out, mountsCache.done = capMounts(list), true
		}
		// Whatever the cache holds now: this scan's result if it won, the
		// fresher concurrent one if it did not, the previous answer if this
		// scan failed.
		out = mountsCache.out
		mountsCache.Unlock()
	}
	if len(out) > 0 {
		// A copy: the cache hands the same backing array to every snapshot, and
		// a caller that sorted or trimmed it in place would be editing what the
		// next twenty-nine seconds of frames report.
		s.Mounts = append([]Mount(nil), out...)
	}
}

// Fold, drop what folding refused, collapse duplicates onto their WORST
// reading, and keep the fullest.
//
// Both of those rules used to be the obvious wrong one, and a review caught
// them together because they are the same mistake twice:
//
//	DUPLICATES took the first row. Folding is lossy on purpose, so /home/alice
//	and /home/bob arrive as two rows called /home — and they are not
//	necessarily the same filesystem. First-one-wins on a 10%-full disk hides a
//	99%-full one behind it. The fullest reading wins instead: this section
//	exists to answer "is anything about to run out of room", and the answer
//	must not depend on mount order.
//
//	THE CAP took the biggest disks. A 100%-full 2 GB partition is exactly the
//	row worth sending, and eight healthy 4 TB volumes would push it off the
//	list — taking it out of hotSys's reach on the page as well. Fullest first,
//	size as the tie-break for the ones with nothing to report.
//
// Then alphabetical for display: the same two-step capNICs uses, and for the
// same reason. A list ordered by fullness would reshuffle itself whenever two
// disks crossed over, and rows that move under the cursor cannot be clicked.
func capMounts(in []Mount) []Mount {
	at := make(map[string]int, len(in))
	out := make([]Mount, 0, len(in))
	for _, m := range in {
		path := foldMountPath(m.Path)
		if path == "" {
			continue
		}
		m.Path = path
		i, dup := at[path]
		if !dup {
			at[path] = len(out)
			out = append(out, m)
			continue
		}
		if mountFullness(m) > mountFullness(out[i]) {
			out[i] = m
		}
	}
	if len(out) > maxMounts {
		sort.SliceStable(out, func(i, j int) bool {
			fi, fj := mountFullness(out[i]), mountFullness(out[j])
			if fi != fj {
				return fi > fj
			}
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

// How full a filesystem is, for ranking only. A row with no percentage sorts
// below every row that has one — it cannot be the answer to "what is about to
// fill up", and treating an unmeasurable mount as 0% would be the same claim.
func mountFullness(m Mount) float64 {
	if m.UsedPercent == nil {
		return -1
	}
	return *m.UsedPercent
}
