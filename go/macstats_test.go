package main

// The macOS parsers, fed the real text the five tools print.
//
// These run on every platform on purpose. The collector they serve only
// compiles on darwin, and this project has no Mac to try it on — so the one
// place the logic can be checked is here, against captured output, with the
// failure modes it is supposed to refuse written down as cases rather than
// left to a reviewer's imagination.

import "testing"

const sampleSysctl = `hw.memsize: 17179869184
vm.loadavg: { 1.83 1.94 2.02 }
kern.boottime: { sec = 1754000000, usec = 123456 } Fri Aug  1 12:00:00 2026
`

// Intel: 4 KiB pages. Apple Silicon prints 16384 — see the 16 KiB case below.
const sampleVMStat = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                4681.
Pages active:                            238442.
Pages inactive:                          235122.
Pages speculative:                         2141.
Pages throttled:                              0.
Pages wired down:                        132908.
Pages purgeable:                           5411.
"Translation faults":                 862341219.
Pages copy-on-write:                   30288131.
Pages zero filled:                    481104213.
Pages reactivated:                     32950411.
Pages purged:                           4472019.
File-backed pages:                       128455.
Anonymous pages:                         347250.
Pages stored in compressor:              432101.
Pages occupied by compressor:            121430.
Decompressions:                        21234567.
Compressions:                          33456789.
Pageins:                                8123456.
Pageouts:                                 45678.
Swapins:                                 123456.
Swapouts:                                234567.
`

const sampleTop = `Processes: 512 total, 2 running, 510 sleeping, 2841 threads
2026/08/01 12:00:00
Load Avg: 1.83, 1.94, 2.02
CPU usage: 40.00% user, 20.00% sys, 40.00% idle
SharedLibs: 512M resident, 78M data, 42M linkedit.
PhysMem: 15G used (2841M wired, 1234M compressor), 512M unused.
Networks: packets: 12345678/9876M in, 8765432/5432M out.
Disks: 1234567/12G read, 654321/8G written.

Processes: 512 total, 3 running, 509 sleeping, 2843 threads
2026/08/01 12:00:01
Load Avg: 1.83, 1.94, 2.02
CPU usage: 3.44% user, 6.89% sys, 89.65% idle
SharedLibs: 512M resident, 78M data, 42M linkedit.
PhysMem: 15G used (2841M wired, 1234M compressor), 512M unused.
Networks: packets: 12345678/9876M in, 8765432/5432M out.
Disks: 1234567/12G read, 654321/8G written.
`

func TestParseSysctlPairs(t *testing.T) {
	vals := parseSysctlPairs(sampleSysctl)
	for k, want := range map[string]string{
		"hw.memsize":    "17179869184",
		"vm.loadavg":    "{ 1.83 1.94 2.02 }",
		"kern.boottime": "{ sec = 1754000000, usec = 123456 } Fri Aug  1 12:00:00 2026",
	} {
		if got := vals[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// The reason the collector does not use `sysctl -n`. With bare values in
// argument order, one unknown MIB shifts every later line up by one and each
// reading after it is confidently wrong. Keyed by name, an absent MIB is an
// absent reading and the others are untouched.
func TestParseSysctlPairsIsNotPositional(t *testing.T) {
	// vm.loadavg did not exist on this kernel: sysctl complained on stderr and
	// printed two lines instead of three.
	vals := parseSysctlPairs(`hw.memsize: 17179869184
kern.boottime: { sec = 1754000000, usec = 0 } Fri Aug  1 12:00:00 2026
`)
	if _, ok := vals["vm.loadavg"]; ok {
		t.Fatal("a name that was never printed must not be present")
	}
	if got := vals["hw.memsize"]; got != "17179869184" {
		t.Fatalf("hw.memsize = %q, want the value it was actually printed with", got)
	}
	if _, ok := parseMacBoottime(vals["kern.boottime"]); !ok {
		t.Fatal("kern.boottime must still parse after an earlier name went missing")
	}
}

func TestParseMacLoadavg(t *testing.T) {
	l1, l5, l15, ok := parseMacLoadavg("{ 1.83 1.94 2.02 }")
	if !ok || l1 == nil || l5 == nil || l15 == nil {
		t.Fatal("want three load figures")
	}
	if *l1 != 1.83 || *l5 != 1.94 || *l15 != 2.02 {
		t.Fatalf("got %v %v %v", *l1, *l5, *l15)
	}
	for _, in := range []string{
		"",
		"{ }",
		"{ 1.83 1.94 }",
		"{ 1.83 nan 2.02 }",
		"{ -1 1 1 }",
		"1.83 1.94 2.02",        // no braces at all
		"{ 1.83 1.94 2.02",      // truncated
		"{ 12 1.83 1.94 2.02 }", // a leading field: 12 would read as a load
		"{ 1.83 1.94 2.02 } trailing",
	} {
		if _, _, _, ok := parseMacLoadavg(in); ok {
			t.Errorf("parseMacLoadavg(%q) should refuse", in)
		}
	}
}

// round2 multiplies by 100 before rounding, so a finite but absurd load would
// overflow to +Inf — and Go's JSON encoder refuses Inf, which would take the
// whole snapshot down to "bad-values" rather than just the load figure.
func TestParseMacLoadavgRefusesValuesThatWouldOverflowRounding(t *testing.T) {
	if _, _, _, ok := parseMacLoadavg("{ 1e308 1 1 }"); ok {
		t.Fatal("a load that cannot survive round2 must be refused")
	}
	l1, _, _, ok := parseMacLoadavg("{ 99999 1 1 }")
	if !ok || l1 == nil || !finite(*l1) {
		t.Fatal("a high but possible load must still be read, and stay finite")
	}
}

func TestParseMacBoottime(t *testing.T) {
	sec, ok := parseMacBoottime("{ sec = 1754000000, usec = 123456 } Fri Aug  1 12:00:00 2026")
	if !ok || sec != 1754000000 {
		t.Fatalf("got %v/%v", sec, ok)
	}
	for _, in := range []string{"", "{ usec = 5 }", "{ sec = zero }", "{ sec = 0 }", "{ sec = -1 }"} {
		if _, ok := parseMacBoottime(in); ok {
			t.Errorf("parseMacBoottime(%q) should refuse", in)
		}
	}
}

func TestParseVMStat(t *testing.T) {
	pageSize, pages, ok := parseVMStat(sampleVMStat)
	if !ok {
		t.Fatal("want a parse")
	}
	if pageSize != 16384 {
		t.Fatalf("page size %v, want 16384 — it is READ, never assumed", pageSize)
	}
	for k, want := range map[string]float64{
		"Pages free": 4681, "Pages inactive": 235122, "Pages speculative": 2141,
		"Pages active": 238442, "Pages wired down": 132908,
	} {
		if got, present := pages[k]; !present || got != want {
			t.Errorf("%q = %v/%v, want %v", k, got, present, want)
		}
	}
	// The header carries a colon of its own and must not land in the counters.
	if _, present := pages["Mach Virtual Memory Statistics"]; present {
		t.Error("the header line was read as a page counter")
	}
	// Intel reports 4 KiB. Reading the size rather than assuming one is the
	// difference between a right answer and one four times off.
	if ps, _, ok := parseVMStat("Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: 100.\n"); !ok || ps != 4096 {
		t.Fatalf("intel page size = %v/%v, want 4096", ps, ok)
	}
	if _, _, ok := parseVMStat("vm_stat: command not found\n"); ok {
		t.Error("output with no page size must refuse")
	}
	// The unit has to be read, not skipped past. Taken as bytes, each of these
	// would scale every counter under it into a figure that is wrong by orders
	// of magnitude while looking entirely reasonable on the card.
	for _, header := range []string{
		"Mach Virtual Memory Statistics: (page size of 4096 kilobytes)",
		"Mach Virtual Memory Statistics: (page size of 4 KiB)",
		"Mach Virtual Memory Statistics: (page size of 4096)",
		"Mach Virtual Memory Statistics: (page size of 4096.5 bytes)",
	} {
		if _, _, ok := parseVMStat(header + "\nPages free: 100.\n"); ok {
			t.Errorf("%q must refuse: the unit is part of the grammar", header)
		}
	}
}

func TestMacMemAvailable(t *testing.T) {
	_, pages, _ := parseVMStat(sampleVMStat)
	avail, ok := macMemAvailable(pages, 16384)
	if !ok {
		t.Fatal("want an availability figure")
	}
	// free + speculative + min(inactive, file-backed MINUS speculative): the
	// sample's file cache (128455) is smaller than its inactive queue
	// (235122), so the queue is counted only as far as there are file pages to
	// account for it — and the 2141 speculative pages, already added by name,
	// are taken out of that cache first so they are not counted twice.
	if want := float64(4681+2141+(128455-2141)) * 16384; avail != want {
		t.Fatalf("avail = %v, want %v", avail, want)
	}
	// Comma-ok on every key, no defaulting: a counter that vanished in a macOS
	// update and got read as zero would silently move the number, and a memory
	// figure that is quietly wrong is worse than a card that has none.
	for _, drop := range []string{"Pages free", "Pages inactive", "Pages speculative", "File-backed pages"} {
		partial := map[string]float64{}
		for k, v := range pages {
			partial[k] = v
		}
		delete(partial, drop)
		if _, ok := macMemAvailable(partial, 16384); ok {
			t.Errorf("missing %q must refuse, not default to zero", drop)
		}
	}
}

// The reason the inactive queue is bounded by the file cache rather than
// trusted whole: a machine whose cache has been squeezed out by anonymous
// memory is the one in trouble, and counting its whole inactive queue as
// available reports it as comfortable.
func TestMacMemAvailableTightensUnderPressure(t *testing.T) {
	relaxed := map[string]float64{
		"Pages free": 1000, "Pages speculative": 500,
		"Pages inactive": 200000, "File-backed pages": 300000,
	}
	// Same queue, but almost none of it is file-backed any more.
	squeezed := map[string]float64{
		"Pages free": 1000, "Pages speculative": 500,
		"Pages inactive": 200000, "File-backed pages": 2000,
	}
	before, ok1 := macMemAvailable(relaxed, 4096)
	after, ok2 := macMemAvailable(squeezed, 4096)
	if !ok1 || !ok2 {
		t.Fatal("both want a figure")
	}
	if after >= before {
		t.Fatalf("availability must fall as the file cache is squeezed: %v -> %v", before, after)
	}
	if want := float64(1000+500+(2000-500)) * 4096; after != want {
		t.Fatalf("squeezed avail = %v, want %v", after, want)
	}
}

// Speculative pages are file-backed, and vm_stat counts them in BOTH lines. If
// the file total went into the bound whole, they would be added once by name
// and again through the bound — and only on the machine whose cache is the
// smaller of the two, i.e. the one already short of memory.
func TestMacMemAvailableDoesNotDoubleCountSpeculative(t *testing.T) {
	// File cache is the binding side here (2000 < 50000), and every page of it
	// is speculative. Availability is then free + the speculative pages and
	// nothing else — the inactive queue has no file pages left to vouch for it.
	pages := map[string]float64{
		"Pages free": 1000, "Pages speculative": 2000,
		"Pages inactive": 50000, "File-backed pages": 2000,
	}
	avail, ok := macMemAvailable(pages, 4096)
	if !ok {
		t.Fatal("want a figure")
	}
	if want := float64(1000+2000) * 4096; avail != want {
		t.Fatalf("avail = %v, want %v — the speculative pages were counted twice", avail, want)
	}
	// A file total smaller than the speculative count is not arithmetic this
	// understands, but it must not become a negative bound either.
	odd := map[string]float64{
		"Pages free": 1000, "Pages speculative": 2000,
		"Pages inactive": 50000, "File-backed pages": 500,
	}
	if got, ok := macMemAvailable(odd, 4096); !ok || got != float64(1000+2000)*4096 {
		t.Fatalf("avail = %v (ok=%v), want the floor at zero reclaimable", got, ok)
	}
}

// The percentage the page actually renders, end to end.
func TestMacMemoryPercentage(t *testing.T) {
	pageSize, pages, _ := parseVMStat(sampleVMStat)
	avail, _ := macMemAvailable(pages, pageSize)
	const total = 17179869184 // hw.memsize, 16 GiB
	got := pct(total-avail, total)
	if got == nil {
		t.Fatal("want a percentage")
	}
	if *got != 87.3 {
		t.Fatalf("memory used = %v%%, want 87.3%%", *got)
	}
}

func TestParseMacTopBusy(t *testing.T) {
	busy, ok := parseMacTopBusy(sampleTop)
	if !ok || busy == nil {
		t.Fatal("want a CPU percentage")
	}
	// The SECOND frame: 100 - 89.65. The first frame says 60% busy and is the
	// average since boot — a different quantity in the same units.
	if *busy != 10.35 {
		t.Fatalf("cpu = %v, want 10.35 (the second frame, not the first)", *busy)
	}
}

func TestParseMacTopBusyNeedsTwoFrames(t *testing.T) {
	one := `Processes: 512 total
CPU usage: 40.00% user, 20.00% sys, 40.00% idle
`
	if _, ok := parseMacTopBusy(one); ok {
		t.Fatal("one frame is the since-boot average and must be refused")
	}
	if _, ok := parseMacTopBusy(""); ok {
		t.Fatal("no output must be refused")
	}
	// A frame whose CPU line cannot be read means the format moved. Half-
	// understood frames are not samples, so the whole reading goes.
	drifted := `CPU usage: 40.00% user, 20.00% sys, 40.00% idle
CPU usage: unavailable
`
	if _, ok := parseMacTopBusy(drifted); ok {
		t.Fatal("an unreadable CPU line must refuse the whole reading")
	}
	// Ordinary lines that merely contain a colon must not be mistaken for one.
	if _, ok := parseMacTopBusy("Networks: packets: 1/2M in, 3/4M out\n"); ok {
		t.Fatal("a line that is not a CPU frame must not count as one")
	}
}

// Interface table as netstat -ibn actually prints it: en0 appears three times
// (link row plus one row per address family, all repeating the same counters),
// lo0 and utun0 have no link-layer address so their counters sit one column
// further left, and gif0's trailing * marks it down.
const sampleNetstat = `Name       Mtu   Network       Address            Ipkts Ierrs     Ibytes    Opkts Oerrs     Obytes  Coll
lo0        16384 <Link#1>                          845657     0  381415076   845657     0  381415076     0
lo0        16384 127           127.0.0.1           845657     -  381415076   845657     -  381415076     -
lo0        16384 ::1/128       ::1                 845657     -  381415076   845657     -  381415076     -
lo1        16384 <Link#9>                             999     0      99999      999     0      99999     0
gif0*      1280  <Link#2>                              12     0       1000        7     0       2000     0
en0        1500  <Link#12>     aa:bb:cc:dd:ee:ff 96979796     0 7940292039 44387614     0 6935162045     0
en0        1500  192.168.1     192.168.1.10      96979796     - 7940292039 44387614     - 6935162045     -
utun0      1380  <Link#17>                              0     0          0      123     0      13487     0
`

func TestParseMacNetstat(t *testing.T) {
	rx, tx, ok := parseMacNetstat(sampleNetstat)
	if !ok {
		t.Fatal("want a reading")
	}
	// gif0 (down but still counted) + en0 + utun0, each from its <Link#N> row
	// only. Summing every row instead would count en0 three times, and both
	// loopbacks are out — local chatter is not traffic.
	if want := float64(1000 + 7940292039 + 0); rx != want {
		t.Fatalf("rx = %v, want %v", rx, want)
	}
	if want := float64(2000 + 6935162045 + 13487); tx != want {
		t.Fatalf("tx = %v, want %v", tx, want)
	}
}

// The parser reads the header, not positions memorised from one machine. A
// counter block that grew a trailing Drop column — as this table has done
// across macOS versions — or lost a column entirely moves nothing it reads.
func TestParseMacNetstatFollowsTheHeader(t *testing.T) {
	grown := `Name  Mtu  Network   Address            Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll Drop
en0   1500 <Link#4>  aa:bb:cc:dd:ee:ff     10     0    111    20     0    222    0    5
utun0 1380 <Link#7>                        30     0    333    40     0    444    0    0
`
	rx, tx, ok := parseMacNetstat(grown)
	if !ok || rx != 111+333 || tx != 222+444 {
		t.Fatalf("grown table: rx=%v tx=%v ok=%v, want 444/666/true", rx, tx, ok)
	}
	shrunk := `Name  Mtu  Network   Address            Ipkts Ibytes Opkts Obytes
en1   1500 <Link#5>  aa:bb:cc:dd:ee:01      7     70     9     90
`
	rx, tx, ok = parseMacNetstat(shrunk)
	if !ok || rx != 70 || tx != 90 {
		t.Fatalf("shrunk table: rx=%v tx=%v ok=%v, want 70/90/true", rx, tx, ok)
	}
}

func TestParseMacNetstatRefusals(t *testing.T) {
	const plainHeader = "Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll\n"
	for name, in := range map[string]string{
		"empty output": "",
		"usage text":   "netstat: option requires an argument -- p\nUsage: netstat [-AabdgiLlmnrRsSv]\n",
		"no Ibytes column": "Name Mtu Network Address Ipkts Ierrs Opkts Oerrs Coll\n" +
			"en0 1500 <Link#4> aa:bb:cc:dd:ee:ff 1 0 2 0 0\n",
		// Offsets are relative to Ipkts; names that moved around each other are
		// a format this would misread, so it must not read it at all.
		"columns out of order": "Name Mtu Network Address Ibytes Ipkts Obytes Opkts\n" +
			"en0 1500 <Link#4> aa:bb:cc:dd:ee:ff 1 2 3 4\n",
		// A table with no link rows is a table whose row grammar moved — there
		// is nothing here this parser recognises as an interface.
		"no link rows": plainHeader + "en0 1500 192.168.1 192.168.1.10 1 - 2 3 - 4 -\n",
		// One row this cannot account for refuses the WHOLE reading, even when
		// every other row parsed: the output is one sum, and a sum quietly
		// missing an interface is a wrong number wearing a right one's units.
		"truncated link row":  sampleNetstat + "en1 1500 <Link#20> aa:bb:cc:dd:ee:01 5 0 6\n",
		"non-numeric counter": plainHeader + "en0 1500 <Link#4> aa:bb:cc:dd:ee:ff 1 0 x 2 0 3 0\n",
		// An extra all-numeric field where the link address would sit is an
		// unannounced counter column, and dropping it as if it were an address
		// would shift every later read one column left.
		"numeric where the link address goes": plainHeader + "en0 1500 <Link#4> 7 1 0 2 3 0 4 0\n",
		// An addressed row that lost ONE counter has exactly as many fields as
		// an addressless row, with the address still at the front. Reading it
		// as addressless would return two of its other counters as the byte
		// figures — every retained field has to be a bare count.
		"addressed row short one counter": plainHeader + "en0 1500 <Link#4> aa:bb:cc:dd:ee:ff 10 0 111 20 0 222\n",
		// A link marker on a row that did not read as a link row is content
		// the parser failed to recognise, not a row it may skip: dropping en1
		// here would produce a sum that is quietly missing an interface.
		"link token missing its close": sampleNetstat + "en1 1500 <Link#20 aa:bb:cc:dd:ee:01 5 0 6 7 0 8 0\n",
		// en1 appears in the table but never with a link row of its own.
		"interface with only family rows": plainHeader +
			"en0 1500 <Link#4> aa:bb:cc:dd:ee:ff 1 0 2 3 0 4 0\n" +
			"en1 1500 192.168.5 192.168.5.7 1 - 2 3 - 4 -\n",
		"duplicate link rows for one interface": plainHeader +
			"en0 1500 <Link#4> aa:bb:cc:dd:ee:ff 1 0 2 3 0 4 0\n" +
			"en0 1500 <Link#4> aa:bb:cc:dd:ee:ff 1 0 2 3 0 4 0\n",
		// The marker interior must be digits. None of these are markers
		// netstat prints, and each row's tail is otherwise perfectly shaped —
		// accepting the marker would sum whatever that tail happens to hold.
		"link marker with no number": plainHeader +
			"en0 1500 <Link#> aa:bb:cc:dd:ee:ff 1 0 2 3 0 4 0\n",
		"link marker with junk interior": plainHeader +
			"en0 1500 <Link#x> aa:bb:cc:dd:ee:ff 1 0 2 3 0 4 0\n",
		"link marker with trailing junk": plainHeader +
			"en0 1500 <Link#4>junk> aa:bb:cc:dd:ee:ff 1 0 2 3 0 4 0\n",
	} {
		if _, _, ok := parseMacNetstat(in); ok {
			t.Errorf("%s: should refuse", name)
		}
	}
}

// Exclusion is a class, not a name: any "lo<digits>" — and bare "lo", which no
// macOS prints but would be a loopback if it ever appeared. Interfaces that
// merely START with those letters stay in the sum.
func TestIsMacLoopback(t *testing.T) {
	for name, want := range map[string]bool{
		"lo0": true, "lo1": true, "lo12": true, "lo": true,
		"en0": false, "utun0": false, "load0": false, "l": false, "": false,
	} {
		if got := isMacLoopback(name); got != want {
			t.Errorf("isMacLoopback(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCountMacPIDs(t *testing.T) {
	n, ok := countMacPIDs("    1\n  412\n 1837\n61245\n")
	if !ok || n != 4 {
		t.Fatalf("got %v/%v, want 4", n, ok)
	}
	if n, ok := countMacPIDs("\n\n  \n"); ok {
		t.Fatalf("empty output must refuse, got %v", n)
	}
	// ps printing anything but PIDs means the flags were not understood, and a
	// count scraped off a usage message is not a measurement.
	for _, in := range []string{
		"usage: ps [-AaCcEefhjlMmrSTvwXx]\n",
		"  1\n  412 login\n",
		"  PID\n  412\n",
	} {
		if _, ok := countMacPIDs(in); ok {
			t.Errorf("countMacPIDs(%q) should refuse", in)
		}
	}
}

// The split off the same line the busy figure comes from. `top -l 2` prints
// two frames and the first is the since-boot average — the reading system.go
// refuses — so the LAST one is the one that counts.
func TestParseMacTopSplit(t *testing.T) {
	out := `Processes: 512 total, 2 running, 510 sleeping
CPU usage: 40.00% user, 20.00% sys, 40.00% idle
Processes: 512 total, 2 running, 510 sleeping
CPU usage: 3.44% user, 6.89% sys, 89.65% idle
`
	user, sys := parseMacTopSplit(out)
	if user == nil || sys == nil {
		t.Fatal("want both shares")
	}
	if *user != 3.44 || *sys != 6.89 {
		t.Fatalf("got %v/%v, want 3.44/6.89 from the LAST frame", *user, *sys)
	}
}

// Half a split is not a split: a line this cannot fully account for is a line
// whose format moved, and reporting the half that parsed would put a number
// under a heading that no longer means what it says.
func TestParseMacTopSplitRefusesPartialAndAbsent(t *testing.T) {
	for _, in := range []string{
		// The one this exists for: a COMPLETE frame, but only one of them.
		// `top -l 2` prints the since-boot average first, so output cut short
		// after a single frame would hand that average over as the current
		// split — a plausible number for a machine hammered last week and idle
		// now, and entirely wrong.
		"Processes: 512 total\nCPU usage: 40.00% user, 20.00% sys, 40.00% idle\n",
		"CPU usage: 40.00% user, 40.00% idle\n",
		"CPU usage: unavailable\n",
		"CPU usage: 40.00% user, 900.00% sys, 40.00% idle\n",
		"Processes: 512 total\n",
		"",
	} {
		if user, sys := parseMacTopSplit(in); user != nil || sys != nil {
			t.Errorf("parseMacTopSplit(%q) should refuse, got %v/%v", in, user, sys)
		}
	}
}
