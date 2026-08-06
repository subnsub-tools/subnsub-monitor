package main

// Parsers for the five Apple command-line tools the macOS health collector
// reads. They live here, without a build constraint, so they compile and are
// TESTED everywhere — a parser that can only be exercised on the one platform
// nobody in this project owns hardware for is a parser nobody checks.
//
// Every one of them refuses rather than guesses. That rule is not decoration:
// these read text formats owned by somebody else, on machines this author
// cannot try anything on, and system.go's bar is that an absent number beats a
// wrong one. So a suffix that is not recognised, a column that is not there, a
// value that is not a number, or a frame count that means the reading would be
// a since-boot average instead of a rate all produce "no", never a best guess.

import (
	"strconv"
	"strings"
)

// `sysctl a b c` — no -n — prints one "name: value" per line.
//
// The names are echoed ON PURPOSE. With -n the output is bare values in
// argument order, and sysctl skips (to stderr) any name it does not know, so a
// single unknown or renamed MIB shifts every later line up by one and every
// reading after it is confidently wrong. Keying by name makes that failure
// mode structurally impossible: an absent name is an absent reading.
//
// Cut on the FIRST ": " because values contain colons of their own —
// kern.boottime ends with a ctime() string, and "12:00:00" would split a
// value-first parser down the middle.
func parseSysctlPairs(out string) map[string]string {
	vals := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, value, found := strings.Cut(line, ": ")
		if !found || name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		vals[name] = strings.TrimSpace(value)
	}
	return vals
}

// vm.loadavg, printed by sysctl as "{ 1.83 1.94 2.02 }".
//
// The braces are REQUIRED and the field count is EXACT. An earlier version
// stripped braces wherever they fell and took the first three numbers it
// found, which reads "{ 12 1.83 1.94 2.02 }" — a format that grew a leading
// field — as a load of 12, and 12 is a number somebody would act on. Three
// fields between one brace and the other, or nothing.
func parseMacLoadavg(v string) (l1, l5, l15 *float64, ok bool) {
	body := strings.TrimSpace(v)
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return nil, nil, nil, false
	}
	f := strings.Fields(body[1 : len(body)-1])
	if len(f) != 3 {
		return nil, nil, nil, false
	}
	out := make([]*float64, 3)
	for i := 0; i < 3; i++ {
		n, err := strconv.ParseFloat(f[i], 64)
		// The ceiling is not tidiness. round2 multiplies by 100 before
		// rounding, so a finite but absurd value overflows to +Inf, and Go's
		// JSON encoder refuses Inf — one impossible load figure would take the
		// WHOLE snapshot down to "bad-values", quota readings included.
		if err != nil || n < 0 || n > macLoadCeiling || !finite(n) {
			return nil, nil, nil, false
		}
		out[i] = fp(round2(n))
	}
	return out[0], out[1], out[2], true
}

// No machine has a load average of a hundred thousand. Anything past this is a
// misread, not a busy box.
const macLoadCeiling = 1e5

// THERE IS NO SWAP PARSER HERE, AND THAT IS THE POINT.
//
// vm.swapusage prints "total = 2048.00M  used = 1024.25M  free = 1023.75M",
// which parses easily and means something this protocol cannot say. macOS
// allocates swap files on demand: `total` is how much backing store the
// dynamic pager has created SO FAR, not a capacity. Under real paging it
// climbs toward full, a new swap file appears, and it drops again — so
// used/total, rendered into a field the page reads with Linux's meaning and
// colours at 75% and 90%, would put a Mac in the red on a schedule that has
// nothing to do with running out of anything.
//
// So system_darwin.go names swap in `missing` instead. Reporting the number
// would not be a smaller lie for being easy to compute. Expressing it properly
// needs a "used bytes against a moving ceiling" shape the snapshot does not
// have — which is a protocol change, not a parser.

// kern.boottime:
//
//	{ sec = 1754000000, usec = 123456 } Fri Aug  1 12:00:00 2026
//
// Only the seconds are taken; usec would add microseconds to a number the page
// renders in days. Scanning for the "sec =" pair rather than slicing braces
// keeps the trailing ctime() text — whose length and content vary with the
// locale and the date — out of the parse entirely.
func parseMacBoottime(v string) (float64, bool) {
	f := strings.Fields(v)
	for i := 0; i+2 < len(f); i++ {
		if f[i] != "sec" || f[i+1] != "=" {
			continue
		}
		n, err := strconv.ParseFloat(strings.Trim(f[i+2], "{},"), 64)
		if err != nil || n <= 0 || !finite(n) {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// vm_stat, whose first line carries the page size the rest of the output is
// counted in:
//
//	Mach Virtual Memory Statistics: (page size of 16384 bytes)
//	Pages free:                              123456.
//	Pages active:                            234567.
//	...
//
// The page size is READ, never assumed: it is 4 KiB on Intel and 16 KiB on
// Apple Silicon, and hardcoding either turns every reading on the other into a
// figure four times off in the direction that matters.
func parseVMStat(out string) (pageSize float64, pages map[string]float64, ok bool) {
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		_, after, found := strings.Cut(line, "page size of ")
		if !found {
			continue
		}
		tok, rest, found := strings.Cut(after, " ")
		// The unit is part of the grammar, not decoration. Without this clause
		// a header reading "page size of 4096 kilobytes)" would be taken as
		// 4096 BYTES and every figure derived from it would come out a
		// thousandfold wrong — confidently, and with no sign anything was
		// misread. Apple has printed "bytes)" here for as long as vm_stat has
		// existed; anything else means the format moved and the whole parse
		// should fall through to absent.
		if !found || strings.TrimSpace(rest) != "bytes)" {
			continue
		}
		// Integer, not float: a page size is a whole number of bytes, and
		// accepting "16384.5" would mean accepting a line this does not
		// actually understand.
		n, err := strconv.ParseInt(tok, 10, 64)
		// An upper bound as well as a lower one: a page size read as something
		// enormous would scale every count into a total larger than the machine.
		if err == nil && n > 0 && n <= 1<<20 {
			pageSize = float64(n)
			break
		}
	}
	if pageSize <= 0 {
		return 0, nil, false
	}
	pages = map[string]float64{}
	for _, line := range lines {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSuffix(strings.TrimSpace(v), ".")
		if k == "" || v == "" {
			continue
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n < 0 || !finite(n) {
			continue // the header line and the quoted counters land here
		}
		pages[k] = n
	}
	return pageSize, pages, len(pages) > 0
}

// Bytes a program could get hold of without the machine having to page
// anything out, from a parsed vm_stat.
//
// free + speculative + the part of the inactive queue that is FILE-BACKED.
// That last clause is the whole difficulty. Linux hands over MemAvailable
// ready-made; macOS does not, and the obvious substitute — treat the entire
// inactive queue as reclaimable — is wrong in the direction that matters. The
// inactive queue holds dirty anonymous pages too, and those cannot be dropped,
// only compressed or written to swap. On a machine under real pressure they
// are most of it, so counting them as available reports a comfortable
// percentage for the exact machine that is in trouble.
//
// vm_stat will not say how much of the queue is file-backed, but it does say
// how many file pages exist in total, and the answer cannot exceed either that
// or the queue itself. So the reclaimable part is bounded by the smaller of
// the two. It is still an upper bound on availability rather than a
// measurement — but it MOVES: as anonymous memory grows and the file cache is
// squeezed out, the bound tightens and the percentage climbs, which is the
// signal the card exists to show. Counting the whole queue does not move at
// all.
//
// The speculative pages come out of that file total before it is used as a
// bound. They are file-backed as well — read-ahead pages, which XNU counts in
// BOTH vm_page_speculative_count and vm_page_pageable_external_count, and it
// is the latter that vm_stat prints as "File-backed pages" — so leaving them
// in would add them once by name and a second time through the bound. It only
// bites when the file total is the smaller of the two, which is exactly the
// squeezed machine this bound exists for: the double count inflates
// availability, and inflated availability reads as a comfortable percentage on
// the machine that is in trouble.
//
// It is not Activity Monitor's "Memory Used" either, and no arithmetic over
// these counters is: that number is built from a compressor accounting this
// cannot see without mach.
//
// Comma-ok on every key, no defaulting. A counter that vanished in a macOS
// update and got read as zero would silently move the number, and a memory
// figure that is quietly wrong is worse than a card that says it has none.
func macMemAvailable(pages map[string]float64, pageSize float64) (float64, bool) {
	free, okFree := pages["Pages free"]
	inactive, okInactive := pages["Pages inactive"]
	spec, okSpec := pages["Pages speculative"]
	file, okFile := pages["File-backed pages"]
	if !okFree || !okInactive || !okSpec || !okFile {
		return 0, false
	}
	fileOther := file - spec
	if fileOther < 0 {
		fileOther = 0
	}
	reclaimable := inactive
	if fileOther < reclaimable {
		reclaimable = fileOther
	}
	avail := (free + spec + reclaimable) * pageSize
	if avail < 0 || !finite(avail) {
		return 0, false
	}
	return avail, true
}

// `top -l 2 -n 0 -s 1` prints one header block per sample, each with a line
// like:
//
//	CPU usage: 3.44% user, 6.89% sys, 89.65% idle
//
// TWO FRAMES ARE REQUIRED, and the LAST one is the reading. The first frame's
// percentages are the average since boot — a different quantity wearing the
// same units, which on a box that was hammered last week and is idle now reads
// as permanently busy. That is the same trap cpuDelta's comment in system.go
// exists to avoid, and a run that produced only one frame (top rejected a
// flag, the deadline cut it short) must therefore report nothing rather than
// hand that average over as if it were current.
//
// Busy is derived from idle rather than summed from user and sys: if a future
// version grows a fourth component, 100-idle stays right and user+sys quietly
// starts undercounting.
func parseMacTopBusy(out string) (*float64, bool) {
	var idle []float64
	for _, line := range strings.Split(out, "\n") {
		_, rest, found := strings.Cut(line, "CPU usage:")
		if !found {
			continue
		}
		n, ok := macTopIdle(rest)
		if !ok {
			// A "CPU usage:" line this cannot read means the format moved.
			// Refuse the whole thing; half-understood frames are not samples.
			return nil, false
		}
		idle = append(idle, n)
	}
	if len(idle) < 2 {
		return nil, false
	}
	busy := 100 - idle[len(idle)-1]
	if busy < 0 {
		busy = 0
	}
	if busy > 100 {
		busy = 100
	}
	return fp(round2(busy)), true
}

func macTopIdle(rest string) (float64, bool) { return macTopShare(rest, "idle") }

// One named share off a `CPU usage: 3.70% user, 8.64% sys, 87.65% idle` line.
//
// Where Linux differences cumulative jiffies, macOS states the split outright
// on the line the busy figure already comes from — so the two platforms reach
// the same four fields by different routes, and this one costs nothing beyond
// reading a line that was already read. There is no iowait or steal here and
// none is invented: the fields stay absent, which is what every consumer on
// this wire treats as "this platform cannot say".
func macTopShare(rest, label string) (float64, bool) {
	for _, part := range strings.Split(rest, ",") {
		f := strings.Fields(part)
		if len(f) != 2 || f[1] != label || !strings.HasSuffix(f[0], "%") {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(f[0], "%"), 64)
		if err != nil || n < 0 || n > 100 || !finite(n) {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// user and sys off the LAST `CPU usage:` line — the same frame parseMacTopBusy
// takes its idle figure from. `top -l 2` prints two: the first covers the whole
// time since boot and is the average that makes a machine hammered last week
// look permanently busy, which is exactly the reading system.go refuses.
//
// Anything unreadable comes back nil rather than zero, and the two travel
// together: a split where one half parsed and the other did not is not a split.
func parseMacTopSplit(out string) (user, sys *float64) {
	var last string
	for _, line := range strings.Split(out, "\n") {
		_, rest, found := strings.Cut(line, "CPU usage:")
		if found {
			last = rest
		}
	}
	if last == "" {
		return nil, nil
	}
	u, okU := macTopShare(last, "user")
	s, okS := macTopShare(last, "sys")
	if !okU || !okS {
		return nil, nil
	}
	return fp(round2(u)), fp(round2(s))
}

// `netstat -ibn` — the interface table with byte counters, nothing resolved.
// Two things about this table make a casual parse read somebody else's column,
// and both are structural, not cosmetic:
//
//  1. One interface prints SEVERAL rows — a link-layer row and one more per
//     address family, each repeating the same counters. Summing rows counts
//     en0 once per address it has; only the `<Link#N>` rows are the table.
//  2. The column COUNT varies row by row. A link row carries a link-layer
//     address field when the interface has one (en0's MAC) and simply omits it
//     when it does not (lo0, gif0, utun) — nothing is left blank, the later
//     fields just shift left. The counter block has also grown before across
//     macOS versions (a trailing Drop column) and can again.
//
// So nothing here is read by assumed position. The header names the columns:
// the counter block is taken to start at "Ipkts" and run to the end of the
// header, and Ibytes/Obytes are read at their offsets WITHIN that block. A
// link row's fields after `<Link#N>` must then number exactly the block — or
// the block plus one leading link address — and every retained field has to be
// a bare decimal count, because the two columns this returns are not the only
// ones a shifted row would move.
//
// Anything else refuses the WHOLE reading, not just the row. So does a link
// marker on a row that did not read as a link row, a second link row for one
// name, and an interface that appears in the table without a link row of its
// own. The counters leave here as one sum, and a sum quietly missing an
// interface is a wrong number wearing a right one's units; a row this cannot
// fully account for means the format moved, and the next row is not more
// trustworthy for having parsed.
func parseMacNetstat(out string) (rx, tx float64, ok bool) {
	lines := strings.Split(out, "\n")
	first := 0
	for first < len(lines) && strings.TrimSpace(lines[first]) == "" {
		first++
	}
	if first == len(lines) {
		return 0, 0, false
	}
	header := strings.Fields(lines[first])
	iIpkts, iIbytes, iObytes := -1, -1, -1
	for i, name := range header {
		switch name {
		case "Ipkts":
			iIpkts = i
		case "Ibytes":
			iIbytes = i
		case "Obytes":
			iObytes = i
		}
	}
	// The order check is part of recognising the format, not pedantry: offsets
	// are taken relative to Ipkts, so a header where the names moved around
	// each other is a header this would misread rather than read differently.
	if len(header) == 0 || header[0] != "Name" ||
		iIpkts < 0 || iIbytes < 0 || iObytes < 0 ||
		iIpkts >= iIbytes || iIbytes >= iObytes {
		return 0, 0, false
	}
	nCounters := len(header) - iIpkts
	offIn, offOut := iIbytes-iIpkts, iObytes-iIpkts

	// Two maps, one grammar rule: every interface the table names anywhere
	// must contribute exactly one fully-read link row. An interface whose link
	// row was mangled would otherwise fall out of the sum on the strength of
	// its OWN malformation — its per-family rows still skipped, its bytes
	// silently gone — and the remainder would be called a reading.
	allNames := map[string]bool{}
	linkDone := map[string]bool{}
	for _, line := range lines[first+1:] {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		name := strings.TrimSuffix(f[0], "*") // trailing * marks a down interface
		isLink := len(f) >= 3 && isMacLinkMarker(f[2])
		if !isLink {
			// A link marker on a row that did not read as a link row is table
			// content this parser failed to recognise, not a row it is free
			// to skip — `<Link#20` with its close cut off would otherwise
			// drop that interface and keep summing.
			if strings.Contains(line, "<Link#") {
				return 0, 0, false
			}
			allNames[name] = true
			continue // the per-address-family repeats
		}
		allNames[name] = true
		if linkDone[name] {
			return 0, 0, false // twice in one table is not the table this knows
		}
		linkDone[name] = true
		tail := f[3:]
		// A link address is never a bare number, and a bare number here is not
		// a link address: it would mean an extra COUNTER column this header did
		// not announce, and dropping it would shift every later read one column
		// left — the exact misread this parser exists to make impossible.
		if len(tail) == nCounters+1 && !allDigits(tail[0]) {
			tail = tail[1:]
		}
		if len(tail) != nCounters {
			return 0, 0, false
		}
		// Every retained field must be a bare count — the whole row grammar,
		// not just the two columns this returns. The addressed row that lost
		// one counter is the row this catches: it has exactly nCounters fields
		// left with the address still at the front, and checking only the two
		// read offsets would hand back two of its OTHER counters as the byte
		// figures.
		for _, fld := range tail {
			if !allDigits(fld) {
				return 0, 0, false
			}
		}
		r, errR := strconv.ParseFloat(tail[offIn], 64)
		t, errT := strconv.ParseFloat(tail[offOut], 64)
		if errR != nil || errT != nil || r < 0 || t < 0 || !finite(r) || !finite(t) {
			return 0, 0, false
		}
		// Validated first, THEN excluded: a loopback row this could not read
		// still means the format moved. Excluded for the same reason Linux
		// leaves out lo — local chatter would drown the number the gauge
		// exists to show.
		if isMacLoopback(name) {
			continue
		}
		rx += r
		tx += t
	}
	if len(linkDone) == 0 {
		return 0, 0, false
	}
	for name := range allNames {
		if !linkDone[name] {
			return 0, 0, false
		}
	}
	return rx, tx, true
}

// The marker is `<Link#N>` with N nothing but digits — not merely a token that
// starts and ends the right way. `<Link#>`, `<Link#x>` and `<Link#4>junk>` are
// not markers netstat prints, and a row wearing one is a row from a format
// this does not know: it falls through to the unrecognised-marker guard above
// and refuses the whole reading, rather than contributing whatever its tail
// happens to hold.
func isMacLinkMarker(tok string) bool {
	body, ok := strings.CutPrefix(tok, "<Link#")
	if !ok {
		return false
	}
	body, ok = strings.CutSuffix(body, ">")
	if !ok {
		return false
	}
	return allDigits(body)
}

// The loopback class: "lo" followed by nothing but digits — lo0 on every Mac,
// plus any further loopback someone has created — and bare "lo" itself, on
// purpose: no macOS prints that name, but if it ever appeared it would be a
// loopback, and the exclusion is about what the interface carries, not how it
// is numbered. Matched as a class rather than hardcoding lo0, because a second
// loopback carries the same local chatter as the first.
func isMacLoopback(name string) bool {
	if len(name) < 2 || name[0] != 'l' || name[1] != 'o' {
		return false
	}
	for i := 2; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// `ps -A -o pid=` — every process, one PID per line, no header and no command
// names. The count is the whole point and the whole payload: see the header of
// system.go for why the names are not read, and linuxProcs for why this counts
// processes rather than the scheduling entities loadavg reports.
//
// Any line that is not a bare number fails the entire count. ps printing
// something else means the flags were not understood, and a "process count"
// scraped off a usage message is not a measurement.
func countMacPIDs(out string) (int, bool) {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		for i := 0; i < len(t); i++ {
			if t[i] < '0' || t[i] > '9' {
				return 0, false
			}
		}
		n++
	}
	return n, n > 0
}
