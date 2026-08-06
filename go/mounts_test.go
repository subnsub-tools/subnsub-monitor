package main

import "testing"

// The privacy rule, exhaustively. Every case here is a path a real machine
// mounts something on, and the assertion is that what leaves this helper
// describes a DISK and never a person.
func TestFoldMountPath(t *testing.T) {
	cases := []struct{ in, want string }{
		// The ordinary ones, untouched.
		{"/data", "/data"},
		{"/mnt/backup", "/mnt/backup"},
		{"/var/lib/docker", "/var/lib/docker"},
		{"/System/Volumes/Data", "/System/Volumes/Data"},

		// Home directories: the segment after the home is a username by
		// construction, and everything below it is somebody's work.
		{"/home/alice", "/home"},
		{"/home/alice/projects", "/home"},
		{"/home/alice/projects/client-acme/secret", "/home"},
		{"/Users/alice/Library", "/Users"},
		{"/export/home/alice", "/export/home"},

		// Depth: three segments, so a path stops before it describes what
		// somebody is working on.
		{"/var/lib/docker/overlay2/f3a9/merged", "/var/lib/docker"},
		{"/srv/www/customer-acme/releases", "/srv/www/customer-acme"},

		// Shapes that are not a path this can reason about.
		{"/", ""},
		{"", ""},
		{"   ", ""},
		{"/a/../../etc", ""},

		// Anchors beyond the two home directories, all of them places where the
		// NEXT segment is generated from an identity rather than chosen for a
		// disk. Depth alone kept these, which is what a review caught.
		{"/root", "/root"},
		{"/root/projects/acme", "/root"},
		{"/media/alice/usb-stick", "/media"},
		{"/run/media/alice/usb-stick", "/run/media"},
		{"/Volumes/Alice Tax Backup", "/Volumes"},

		// …and the paths that still go out whole, because a mount point somebody
		// chose for a disk is the thing this section exists to report.
		{"/mnt/backup-acme", "/mnt/backup-acme"},
		{"/srv/data", "/srv/data"},

		// Windows volume roots, and ONLY volume roots.
		{`D:\`, `D:\`},
		{"D:", "D:"},
		{`d:/`, `d:/`},
		{`C:\Users\alice`, ""},
		{"relative/path", ""},
		{"alice/private-project", ""},

		// Slash noise the kernel is entitled to produce.
		{"//mnt//backup//", "/mnt/backup"},
		{"/mnt/./backup", "/mnt/backup"},
	}
	for _, c := range cases {
		if got := foldMountPath(c.in); got != c.want {
			t.Errorf("foldMountPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A single absurd segment must not become an absurd wire field, and the cut
// must land on a rune boundary — a byte-level truncation would put an invalid
// UTF-8 sequence on the wire, which is exactly the sort of thing that survives
// every test written in English.
func TestFoldMountPathCaps(t *testing.T) {
	long := "/mnt/" + string(make([]byte, 0))
	for i := 0; i < 60; i++ {
		long += "x"
	}
	got := foldMountPath(long)
	if len([]rune(got)) > maxMountPath {
		t.Fatalf("path not capped: %d runes", len([]rune(got)))
	}

	wide := "/mnt/"
	for i := 0; i < 60; i++ {
		wide += "宽"
	}
	got = foldMountPath(wide)
	if len([]rune(got)) > maxMountPath {
		t.Fatalf("wide path not capped: %d runes", len([]rune(got)))
	}
	for _, r := range got {
		if r == '\uFFFD' {
			t.Fatalf("cut split a rune: %q", got)
		}
	}
}

// Folding is lossy on purpose, so duplicates are the normal case rather than
// an edge one: two users' home directories and four btrfs subvolumes all
// collapse onto one row. The row that survives is the FULLEST — they are not
// necessarily the same filesystem, and first-one-wins hid a 99% disk behind a
// 10% one depending on mount order.
func TestCapMountsFoldsDuplicatesOntoTheWorstReading(t *testing.T) {
	in := []Mount{
		{Path: "/home/alice", Total: fp(100), UsedPercent: fp(10)},
		{Path: "/home/bob", Total: fp(200), UsedPercent: fp(99)},
		{Path: "/mnt/data", Total: fp(300), UsedPercent: fp(50)},
	}
	got := capMounts(in)
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(got), got)
	}
	if got[0].Path != "/home" || got[0].UsedPercent == nil || *got[0].UsedPercent != 99 {
		t.Errorf("want /home at its worst (99%%), got %+v", got[0])
	}
	if got[1].Path != "/mnt/data" {
		t.Errorf("want /mnt/data second, got %+v", got[1])
	}
	// Order must not decide the answer.
	rev := capMounts([]Mount{in[1], in[0], in[2]})
	if rev[0].UsedPercent == nil || *rev[0].UsedPercent != 99 {
		t.Errorf("mount order changed the reading: %+v", rev[0])
	}
}

// The cap keeps what needs attention, not what is largest: a small full
// partition is exactly the row worth sending, and eight healthy volumes used
// to push it off the list — out of the page's hot-flag reach with it.
func TestCapMountsKeepsTheFullestNotTheBiggest(t *testing.T) {
	in := []Mount{{Path: "/mnt/tiny", Total: fp(2e9), UsedPercent: fp(100)}}
	for i := 0; i < maxMounts; i++ {
		in = append(in, Mount{
			Path:        "/mnt/big" + string(rune('a'+i)),
			Total:       fp(4e12),
			UsedPercent: fp(12),
		})
	}
	got := capMounts(in)
	if len(got) != maxMounts {
		t.Fatalf("want %d rows, got %d", maxMounts, len(got))
	}
	for _, m := range got {
		if m.Path == "/mnt/tiny" {
			return
		}
	}
	t.Errorf("the full partition was dropped in favour of larger healthy ones: %+v", got)
}

func TestCapMountsRanksBySizeAmongEquals(t *testing.T) {
	var in []Mount
	// Same fullness across the board, so size is the tie-break that decides.
	for i := 0; i < maxMounts+6; i++ {
		in = append(in, Mount{Path: "/mnt/d" + string(rune('a'+i)),
			Total: fp(float64(i)), UsedPercent: fp(40)})
	}
	got := capMounts(in)
	if len(got) != maxMounts {
		t.Fatalf("want %d rows, got %d", maxMounts, len(got))
	}
	for _, m := range got {
		if m.Total == nil || *m.Total < 6 {
			t.Errorf("kept a small filesystem over a large one: %+v", m)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Path >= got[i].Path {
			t.Fatalf("rows are not in display order: %+v", got)
		}
	}
}

// A row with no measurement still folds and still counts against the cap —
// what it must not do is crash the sort that reads its total.
func TestCapMountsSurvivesMissingTotals(t *testing.T) {
	in := []Mount{{Path: "/mnt/a"}, {Path: "/mnt/b", Total: fp(5)}}
	if got := capMounts(in); len(got) != 2 {
		t.Fatalf("want both rows, got %+v", got)
	}
}

// The kernel escapes space, tab, newline and backslash in /proc/mounts as
// three-digit octal. A parser that misses that reads one path as two fields
// and measures the wrong filesystem.
func TestUnescapeMountField(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`/mnt/my\040disk`, "/mnt/my disk"},
		{`/mnt/a\011b`, "/mnt/a\tb"},
		{`/mnt/a\012b`, "/mnt/a\nb"},
		{`/mnt/a\134b`, `/mnt/a\b`},
		{"/mnt/plain", "/mnt/plain"},
		// Not an escape this understands: left exactly as found rather than
		// given an invented meaning.
		{`/mnt/a\zzb`, `/mnt/a\zzb`},
		{`/mnt/trailing\`, `/mnt/trailing\`},
		{`/mnt/short\04`, `/mnt/short\04`},
	} {
		if got := unescapeMountField(c.in); got != c.want {
			t.Errorf("unescapeMountField(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The sampler calls the collector once a second and a scan costs a statfs per
// filesystem, so the second call inside the window must reuse the first's
// answer rather than repeat the work.
func TestFillMountsThrottlesScans(t *testing.T) {
	resetMountsCache()
	scans := 0
	scan := func() []Mount {
		scans++
		return []Mount{{Path: "/mnt/data", Total: fp(10)}}
	}
	var a, b System
	fillMounts(&a, scan)
	fillMounts(&b, scan)
	if scans != 1 {
		t.Fatalf("want one scan inside the window, got %d", scans)
	}
	if len(a.Mounts) != 1 || len(b.Mounts) != 1 || b.Mounts[0].Path != "/mnt/data" {
		t.Fatalf("both snapshots should carry the row: %+v / %+v", a.Mounts, b.Mounts)
	}
	// Each snapshot gets its own slice: the cache hands the same rows to every
	// frame for thirty seconds, and one caller editing them in place would be
	// editing what all the others report.
	a.Mounts[0].Path = "/edited"
	if b.Mounts[0].Path != "/mnt/data" {
		t.Fatal("snapshots share a backing array")
	}
	mountsCache.Lock()
	mountsCache.at -= mountsRefreshSec + 1
	mountsCache.Unlock()
	var c System
	fillMounts(&c, scan)
	if scans != 2 {
		t.Fatalf("want a rescan once the window passed, got %d scans", scans)
	}
}

// A scan that comes back empty must not leave the previous answer standing in
// the cache — an unmounted disk should disappear from the card.
func TestFillMountsForgetsUnmounted(t *testing.T) {
	resetMountsCache()
	fillMounts(&System{}, func() []Mount { return []Mount{{Path: "/mnt/gone", Total: fp(1)}} })
	mountsCache.Lock()
	mountsCache.at -= mountsRefreshSec + 1
	mountsCache.Unlock()
	var s System
	fillMounts(&s, func() []Mount { return nil })
	if len(s.Mounts) != 0 {
		t.Fatalf("want no rows, got %+v", s.Mounts)
	}
}

func resetMountsCache() {
	mountsCache.Lock()
	mountsCache.at, mountsCache.out, mountsCache.done = 0, nil, false
	mountsCache.Unlock()
}
