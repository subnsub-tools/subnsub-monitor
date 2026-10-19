//go:build linux

package main

// The system tests that are about LINUX rather than about the collector: the
// parsers they exercise read /proc and live in system_linux.go, behind that
// file's own build tag.
//
// Split out rather than left where they were written, because the file they
// came from has no tag and `GOOS=windows go vet ./...` is what build.sh leans
// on to keep the Windows port from rotting while it is switched off. One
// untagged reference to a Linux-only symbol turns that guard into a failure
// nobody reads.

import "testing"

func TestLinuxTCPParsersUseNamesNotPositions(t *testing.T) {
	snmp := "Tcp: RtoAlgorithm CurrEstab MaxConn RetransSegs InSegs\n" +
		"Tcp: 1 17 -1 42 999\n"
	estab, retrans, ok := parseLinuxTCPSNMP(snmp)
	if !ok || estab != 17 || retrans != 42 {
		t.Fatalf("SNMP parsed %v/%v ok=%v", estab, retrans, ok)
	}
	tw, ok := parseLinuxTCPTimeWait("sockets: used 20\nTCP: inuse 5 orphan 0 tw 13 alloc 8 mem 2\n")
	if !ok || tw != 13 {
		t.Fatalf("sockstat tw=%v ok=%v", tw, ok)
	}
}

// All or nothing, because a partial answer would put one column's number under
// another column's name.
func TestParseNonNeg(t *testing.T) {
	if v, ok := parseNonNeg("1", "2", "3"); !ok || v[0] != 1 || v[2] != 3 {
		t.Fatalf("got %v/%v", v, ok)
	}
	for _, in := range [][]string{
		{"1", "-2"},
		{"1", "two"},
		{"1", ""},
		{"1", "NaN"},
		{"1", "Inf"},
	} {
		if _, ok := parseNonNeg(in...); ok {
			t.Errorf("parseNonNeg(%q) should refuse", in)
		}
	}
}

// The balloon figure is inflate minus deflate in pages, scaled by the page
// size, and absent whenever that would say nothing true: counters missing
// (a kernel without the driver), nothing held (bare metal, or a host that
// gave everything back), or a deflate count ahead of inflate.
func TestBalloonBytes(t *testing.T) {
	const held = "nr_free_pages 1\nballoon_inflate 2621440\nballoon_deflate 0\nballoon_migrate 3479\n"
	if v, ok := balloonBytes(held, 4096); !ok || v != 2621440*4096 {
		t.Fatalf("held: got %v %v, want %v true", v, ok, 2621440*4096)
	}
	// Inflated and fully deflated again: nothing to report, not a zero.
	if _, ok := balloonBytes("balloon_inflate 1572864\nballoon_deflate 1572864\n", 4096); ok {
		t.Error("deflated balloon reported")
	}
	if _, ok := balloonBytes("balloon_inflate 0\nballoon_deflate 0\n", 4096); ok {
		t.Error("idle counters reported")
	}
	if _, ok := balloonBytes("nr_free_pages 1\npswpin 0\n", 4096); ok {
		t.Error("missing counters reported")
	}
	if _, ok := balloonBytes("balloon_inflate 10\nballoon_deflate 11\n", 4096); ok {
		t.Error("deflate ahead of inflate reported")
	}
	if _, ok := balloonBytes(held, 0); ok {
		t.Error("zero page size reported")
	}
	// Page size comes from the runtime, not an assumed 4 KiB: a 64 KiB arm64
	// kernel holds sixteen times as much per counted page.
	if v, ok := balloonBytes("balloon_inflate 100\nballoon_deflate 25\n", 65536); !ok || v != 75*65536 {
		t.Errorf("64k pages: got %v %v", v, ok)
	}
}
