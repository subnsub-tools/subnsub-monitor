package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func resetOutages(t *testing.T) {
	t.Helper()
	outages.Lock()
	outages.loaded, outages.rows, outages.prev, outages.stamped, outages.booted = false, nil, time.Time{}, 0, 0
	outages.bootAt, outages.bootEnd = 0, 0
	outages.Unlock()
	t.Cleanup(func() {
		outages.Lock()
		outages.loaded, outages.rows, outages.prev, outages.stamped, outages.booted = false, nil, time.Time{}, 0, 0
		outages.bootAt, outages.bootEnd = 0, 0
		outages.Unlock()
	})
}

// A ticker that comes back to a wall clock far ahead of its last tick has
// been suspended for that long; an ordinary second, or a few, has not.
func TestOutageSuspendIsAWallClockJump(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	base := time.Unix(1_800_000_000, 0)
	outageBoot(base)
	outageTick(base.Add(time.Second))
	outageTick(base.Add(3 * time.Second)) // a starved tick, not an outage
	if got := outagePoints(); len(got) != 0 {
		t.Fatalf("a two-second late tick filed an outage: %v", got)
	}
	outageTick(base.Add(3*time.Second + 15*time.Minute))
	got := outagePoints()
	if len(got) != 1 {
		t.Fatalf("outages = %v, want the one suspend", got)
	}
	if got[0][2] != outageKindSuspend || got[0][0].(float64) != float64(base.Unix()+3) || got[0][1].(float64) != float64(base.Unix()+3+900) {
		t.Errorf("row = %v, want [%d %d suspend]", got[0], base.Unix()+3, base.Unix()+903)
	}
	// Written whole, and a fresh process reads it back.
	if _, err := os.Stat(filepath.Join(configDir(), outageFile)); err != nil {
		t.Fatalf("outage file not written: %v", err)
	}
	outages.Lock()
	outages.loaded, outages.rows = false, nil
	outages.Unlock()
	if again := outagePoints(); len(again) != 1 || again[0][2] != outageKindSuspend {
		t.Errorf("reloaded = %v, want the same row", again)
	}
}

// A stamp older than the threshold at start is the stretch the process was
// not running; the stamp is then brought to now so a restart on its heels
// does not file it twice. A young stamp files nothing.
func TestOutageDownFromStaleStamp(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	now := time.Unix(1_800_000_000, 0)
	stamp := filepath.Join(configDir(), outageStamp)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(now.Unix()-600, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBoot(now)
	got := outagePoints()
	if len(got) != 1 || got[0][2] != outageKindDown || got[0][0].(float64) != float64(now.Unix()-600) || got[0][1].(float64) != float64(now.Unix()) {
		t.Fatalf("outages = %v, want one down of 600 s ending now", got)
	}
	b, err := os.ReadFile(stamp)
	if err != nil || string(b) != strconv.FormatInt(now.Unix(), 10)+"\n" {
		t.Errorf("stamp after boot = %q (%v), want now", b, err)
	}
	// The restart a self-update makes, seconds later: same stamp, nothing new.
	resetOutages(t)
	outageBoot(now.Add(5 * time.Second))
	if got := outagePoints(); len(got) != 1 {
		t.Errorf("after a quick restart: %v, want still one", got)
	}
	// A restart late in the stamp's minute: the process ran on for most of
	// it, so the gap is the stamp's slack, not a stop, and files nothing.
	resetOutages(t)
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(now.Unix()-80, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBoot(now)
	if got := outagePoints(); len(got) != 1 {
		t.Errorf("an 80 s old stamp filed a down: %v", got)
	}
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(now.Unix()-95, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBoot(now)
	if got := outagePoints(); len(got) != 2 || got[1][0].(float64) != float64(now.Unix()-95) {
		t.Errorf("a 95 s old stamp did not file a down: %v", got)
	}
	// The stamp keeps up while the process runs, once a minute.
	outageTick(now.Add(30 * time.Second))
	if b, _ := os.ReadFile(stamp); string(b) != strconv.FormatInt(now.Unix(), 10)+"\n" {
		t.Errorf("stamp rewritten inside a minute: %q", b)
	}
	outageTick(now.Add(70 * time.Second))
	if b, _ := os.ReadFile(stamp); string(b) != strconv.FormatInt(now.Unix()+70, 10)+"\n" {
		t.Errorf("stamp not rewritten after a minute: %q", b)
	}
}

// Bounded in count and age, and a file with rows this build cannot vouch for
// is read for the rows it can.
func TestOutageBoundsAndShape(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	p := filepath.Join(configDir(), outageFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`[[100,200,"suspend"],[300,250,"suspend"],[400,500,"reboot"],[600,700],[800,900,"down"]]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := outagePoints(); len(got) != 2 || got[0][0].(float64) != 100 || got[1][2] != outageKindDown {
		t.Fatalf("loaded = %v, want the two well-formed rows", got)
	}
	now := time.Unix(1_800_000_000, 0)
	outages.Lock()
	for i := 0; i < outageKeep+5; i++ {
		at := now.Unix() - int64(outageKeep+5-i)*100
		outageAdd(outage{At: at, End: at + 60, Kind: outageKindSuspend})
	}
	outages.Unlock()
	got := outagePoints()
	if len(got) != outageKeep {
		t.Errorf("kept %d rows, want %d", len(got), outageKeep)
	}
	for _, r := range got {
		if now.Unix()-int64(r[1].(float64)) > outageKeepS {
			t.Errorf("a row older than the reach survived: %v", r)
		}
	}
	// And read back from the disk whole: a full record of suspends is the
	// largest the file gets, and the loader must take all of it (until
	// 2026-10-16 it refused anything past 64 KiB, so a full record read back
	// as nothing and the next row filed overwrote the month).
	if st, err := os.Stat(filepath.Join(configDir(), outageFile)); err != nil || st.Size() <= 64<<10 {
		t.Fatalf("a full record should be past the old 64 KiB limit: %v %v", st, err)
	}
	outages.Lock()
	outages.loaded, outages.rows = false, nil
	outages.Unlock()
	if again := outagePoints(); len(again) != outageKeep {
		t.Fatalf("read back %d rows of a full record, want %d", len(again), outageKeep)
	}
}

// The same stale stamp read by a process whose state came from another
// installation is a wipe, not a down: the machine was rebuilt around the
// restored state. The identity is then brought to this machine, so the
// self-update's restart minutes later reads as an ordinary down again.
func TestOutageWipeFromHostChange(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	now := time.Unix(1_800_000_000, 0)
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, outageStamp), []byte(strconv.FormatInt(now.Unix()-900, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hostIDFile), []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBootOn(now, "fedcba9876543210fedcba9876543210")
	got := outagePoints()
	if len(got) != 1 || got[0][2] != outageKindWipe || got[0][0].(float64) != float64(now.Unix()-900) || got[0][1].(float64) != float64(now.Unix()) {
		t.Fatalf("outages = %v, want one wipe of 900 s ending now", got)
	}
	if w := outageLastWipe(); len(w) != 2 || w[0] != now.Unix()-900 || w[1] != now.Unix() {
		t.Errorf("outageLastWipe = %v, want [%d %d]", w, now.Unix()-900, now.Unix())
	}
	if got := hostIDStored(); got != "fedcba9876543210fedcba9876543210" {
		t.Errorf("host-id after boot = %q, want the running machine's", got)
	}
	// A restart on the same machine with an old stamp is a plain down.
	resetOutages(t)
	if err := os.WriteFile(filepath.Join(dir, outageStamp), []byte(strconv.FormatInt(now.Unix()+3000, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBootOn(now.Add(4000*time.Second), "fedcba9876543210fedcba9876543210")
	got = outagePoints()
	if len(got) != 2 || got[1][2] != outageKindDown {
		t.Fatalf("outages after same-host restart = %v, want a down appended", got)
	}
	// The file is reloaded with the wipe intact — the kind is one this
	// build keeps, not dropped as unknown.
	outages.Lock()
	outages.loaded, outages.rows = false, nil
	outages.Unlock()
	if again := outagePoints(); len(again) != 2 || again[0][2] != outageKindWipe {
		t.Errorf("reloaded = %v, want the wipe kept", again)
	}
}

// No stored identity (a state directory from before host-id existed, or a
// first run) or none on the platform: the comparison is not made and the
// stamp files as before; the identity is recorded for next time.
func TestOutageHostIDFirstSeenIsNotAWipe(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	now := time.Unix(1_800_000_000, 0)
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, outageStamp), []byte(strconv.FormatInt(now.Unix()-900, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBootOn(now, "fedcba9876543210fedcba9876543210")
	if got := outagePoints(); len(got) != 1 || got[0][2] != outageKindDown {
		t.Fatalf("outages = %v, want one down", got)
	}
	if got := hostIDStored(); got != "fedcba9876543210fedcba9876543210" {
		t.Errorf("host-id recorded = %q", got)
	}
	if outageLastWipe() != nil {
		t.Error("a down is not a wipe")
	}
	// A platform with no identity never changes the stored one.
	resetOutages(t)
	outageBootOn(now.Add(time.Hour), "")
	if got := hostIDStored(); got != "fedcba9876543210fedcba9876543210" {
		t.Errorf("host-id after a blank = %q, want unchanged", got)
	}
	for _, c := range []struct{ in, want string }{
		{"0123456789abcdef0123456789abcdef\n", "0123456789abcdef0123456789abcdef"},
		{"  ABC-def_12 \r\n", "ABC-def_12"},
		{"short\n", ""},
		{"", ""},
		{"0123456789abcdef; rm -rf /\n", ""},
	} {
		if got := hostIDClean(c.in); got != c.want {
			t.Errorf("hostIDClean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if hostChanged("", "x") || hostChanged("x", "") || hostChanged("x", "x") || !hostChanged("x", "y") {
		t.Error("hostChanged: blank never counts, equal never counts, different does")
	}
}

func TestHostIDMacFieldParse(t *testing.T) {
	listing := "   Device Identifier:         disk3s1s1\n   Volume Name:               Macintosh HD\n   Volume UUID:               0F3B2C1D-8E7A-4B6C-9D5E-1A2B3C4D5E6F\n   Disk / Partition UUID:     11111111-2222-3333-4444-555555555555\n"
	if got := macFieldUUID(listing, "Volume UUID:"); got != "0F3B2C1D-8E7A-4B6C-9D5E-1A2B3C4D5E6F" {
		t.Errorf("macFieldUUID = %q", got)
	}
	if got := macFieldUUID("nothing here\n", "Volume UUID:"); got != "" {
		t.Errorf("macFieldUUID on a listing without the field = %q, want empty", got)
	}
	if got := hostIDClean(macFieldUUID(listing, "Volume UUID:")); got != "0F3B2C1D-8E7A-4B6C-9D5E-1A2B3C4D5E6F" {
		t.Errorf("a UUID survives cleaning: %q", got)
	}
}

// A declaration left in the state directory by whoever restored it names
// the kind outright: the stamp gives the start, the boot the end, and the
// file is consumed so the next start does not read it again. Neither the
// host id nor the stamp's own guess gets a say.
func TestOutageDeclaredAtBoot(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	now := time.Unix(1_800_000_000, 0)
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := func(v int64) {
		if err := os.WriteFile(filepath.Join(dir, outageStamp), []byte(strconv.FormatInt(v, 10)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stamp(now.Unix() - 900)
	// Same installation as recorded: without the declaration this is a down.
	if err := os.WriteFile(filepath.Join(dir, hostIDFile), []byte("fedcba9876543210fedcba9876543210\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	decl := filepath.Join(dir, outageDeclareFile)
	if err := os.WriteFile(decl, []byte(`{"kind":"wipe","by":"grok-enroll/provision-remote","confirmed_at":1799999800,"note":"restored onto a fresh disk"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBootOn(now, "fedcba9876543210fedcba9876543210")
	got := outagePoints()
	if len(got) != 1 || got[0][2] != outageKindWipe || got[0][0].(float64) != float64(now.Unix()-900) || got[0][1].(float64) != float64(now.Unix()) {
		t.Fatalf("outages = %v, want one declared wipe from the stamp to now", got)
	}
	if _, err := os.Stat(decl); !os.IsNotExist(err) {
		t.Errorf("declaration not consumed: %v", err)
	}
	if w := outageLastWipe(); len(w) != 2 || w[0] != now.Unix()-900 {
		t.Errorf("outageLastWipe = %v", w)
	}
	// A second start minutes later — the self-update's restart — has no
	// declaration left to read and a fresh stamp: nothing more is filed.
	resetOutages(t)
	outageBootOn(now.Add(40*time.Second), "fedcba9876543210fedcba9876543210")
	if again := outagePoints(); len(again) != 1 {
		t.Fatalf("after the restart = %v, want the one wipe", again)
	}
	// A declaration that is not one is reported, removed, and the stamp
	// logic runs as if it had not been there.
	resetOutages(t)
	stamp(now.Unix() + 3000)
	if err := os.WriteFile(decl, []byte(`{"kind":"vanished"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBootOn(now.Add(4000*time.Second), "fedcba9876543210fedcba9876543210")
	got = outagePoints()
	if len(got) != 2 || got[1][2] != outageKindDown {
		t.Fatalf("after a bad declaration = %v, want the stamp's down appended", got)
	}
	if _, err := os.Stat(decl); !os.IsNotExist(err) {
		t.Errorf("bad declaration not consumed: %v", err)
	}
	// With its own `at`/`end` the declaration needs no stamp at all.
	resetOutages(t)
	os.Remove(filepath.Join(dir, outageStamp))
	a, b := now.Unix()+5000, now.Unix()+5400
	if err := os.WriteFile(decl, []byte(fmt.Sprintf(`{"kind":"suspend","at":%d,"end":%d}`, a, b)), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBootOn(time.Unix(b+10, 0), "fedcba9876543210fedcba9876543210")
	got = outagePoints()
	if len(got) != 3 || got[2][2] != outageKindSuspend || got[2][0].(float64) != float64(a) || got[2][1].(float64) != float64(b) {
		t.Fatalf("dated declaration = %v, want [%d %d suspend] last", got, a, b)
	}
}

// Over the socket a declaration corrects the record as often as it adds to
// it: a stretch already filed as a down is relabelled, once, and widened to
// what the caller says; nonsense is refused with a reason and changes
// nothing.
func TestOutageDeclareOverSocket(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	// The socket path dates a declaration against the real clock, so this
	// test lives an hour in the real past rather than at a fixed instant.
	now := time.Now().Add(-time.Hour).Truncate(time.Second)
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, outageStamp), []byte(strconv.FormatInt(now.Unix()-900, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outageBootOn(now, "fedcba9876543210fedcba9876543210")
	if got := outagePoints(); len(got) != 1 || got[0][2] != outageKindDown {
		t.Fatalf("setup: %v", got)
	}
	at, end := now.Unix()-900, now.Unix()
	line := fmt.Sprintf(`{"declare":{"kind":"wipe","at":%d,"end":%d,"by":"grok-enroll:backfill"}}`, at, end)
	if reply := pushApply([]byte(line)); reply != fmt.Sprintf("ok declared wipe %d-%d", at, end) {
		t.Fatalf("reply = %q", reply)
	}
	got := outagePoints()
	if len(got) != 1 || got[0][2] != outageKindWipe {
		t.Fatalf("after declare = %v, want the one row relabelled", got)
	}
	// Overlapping, wider: the row grows to the union, still one row.
	line = fmt.Sprintf(`{"declare":{"kind":"wipe","at":%d,"end":%d}}`, at-120, end)
	if reply := pushApply([]byte(line)); reply != fmt.Sprintf("ok declared wipe %d-%d", at-120, end) {
		t.Fatalf("reply = %q", reply)
	}
	if got = outagePoints(); len(got) != 1 || got[0][0].(float64) != float64(at-120) {
		t.Fatalf("after widening = %v", got)
	}
	// Disjoint and OLDER: a second row, filed before the first — the record
	// stays in time order, so the newest wipe is still the newest.
	line = fmt.Sprintf(`{"declare":{"kind":"suspend","at":%d,"end":%d}}`, at-10000, at-9000)
	if reply := pushApply([]byte(line)); !strings.HasPrefix(reply, "ok declared suspend") {
		t.Fatalf("reply = %q", reply)
	}
	if got = outagePoints(); len(got) != 2 || got[0][2] != outageKindSuspend || got[1][2] != outageKindWipe {
		t.Fatalf("after an older declaration = %v, want it first", got)
	}
	if w := outageLastWipe(); len(w) != 2 || w[0] != at-120 {
		t.Errorf("outageLastWipe = %v, want the wipe, not the older suspend", w)
	}
	// A wipe filed for yesterday does not become "the last wipe" either.
	line = fmt.Sprintf(`{"declare":{"kind":"wipe","at":%d,"end":%d}}`, at-20000, at-19000)
	pushApply([]byte(line))
	if w := outageLastWipe(); len(w) != 2 || w[0] != at-120 {
		t.Errorf("outageLastWipe after an older wipe = %v, want today's", w)
	}
	// One declaration spanning several rows replaces them all with one.
	line = fmt.Sprintf(`{"declare":{"kind":"down","at":%d,"end":%d}}`, at-19500, at-9500)
	if reply := pushApply([]byte(line)); reply != fmt.Sprintf("ok declared down %d-%d", at-20000, at-9000) {
		t.Fatalf("spanning reply = %q", reply)
	}
	if got = outagePoints(); len(got) != 2 || got[0][2] != outageKindDown || got[0][0].(float64) != float64(at-20000) || got[0][1].(float64) != float64(at-9000) {
		t.Fatalf("after a spanning declaration = %v, want the two old rows folded into one down", got)
	}
	// Older than the record keeps: accepted, and gone at once.
	stale := int64(outageKeepS + 2*86400)
	line = fmt.Sprintf(`{"declare":{"kind":"down","at":%d,"end":%d}}`, at-stale, at-stale+600)
	pushApply([]byte(line))
	if got = outagePoints(); len(got) != 2 {
		t.Fatalf("a declaration older than the record keeps stayed: %v", got)
	}
	future := time.Now().Unix()
	for _, bad := range []string{
		`{"declare":{"kind":"reboot","at":1790000000,"end":1790000100}}`,
		`{"declare":{"kind":"wipe","at":100,"end":200}}`,
		`{"declare":{"kind":"wipe","at":1790000000,"end":1790000010}}`,
		`{"declare":{"kind":"wipe","at":1790000000,"end":4000000000}}`,
		fmt.Sprintf(`{"declare":{"kind":"wipe","at":%d,"end":%d}}`, future+10, future+50),
		`{"declare":{"kind":"wipe"}} {"declare":{"kind":"wipe"}}`,
	} {
		reply := pushApply([]byte(bad))
		if !strings.HasPrefix(reply, "error: declare:") {
			t.Errorf("pushApply(%s) = %q, want a declare error", bad, reply)
		}
	}
	if got = outagePoints(); len(got) != 2 {
		t.Errorf("a refused declaration changed the record: %v", got)
	}
	// A tile that merely mentions the word is still a tile (and a bad one).
	if reply := pushApply([]byte(`{"id":"declare","value":1,"declare":true}`)); !strings.HasPrefix(reply, "error: not a tile") {
		t.Errorf("tile with a declare field = %q", reply)
	}
	// With no stamp and no `at`, there is nothing to anchor the start to;
	// a stamp from nowhere near now is no anchor either.
	resetOutages(t)
	os.Remove(filepath.Join(dir, outageStamp))
	if reply := pushApply([]byte(`{"declare":{"kind":"wipe"}}`)); !strings.Contains(reply, "no start") {
		t.Errorf("unanchored declaration = %q", reply)
	}
	if err := os.WriteFile(filepath.Join(dir, outageStamp), []byte("100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if reply := pushApply([]byte(`{"declare":{"kind":"wipe"}}`)); !strings.Contains(reply, "no start") {
		t.Errorf("declaration on a 1970 stamp = %q", reply)
	}
}

// What an outside party says about itself is logged, in one line, cut short.
func TestOutageDeclareProvenanceIsOneLogLine(t *testing.T) {
	c := 1790371765.0
	d := outageDeclaration{By: "grok-enroll/provision-remote\nFAKE: other line", ConfirmedAt: &c, Note: "restored\ronto a fresh disk\t" + strings.Repeat("x", 500)}
	got := outageDeclareBy(d)
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("control characters survived: %q", got)
	}
	if !strings.HasPrefix(got, "grok-enroll/provision-remote FAKE: other line (confirmed_at 1790371765) — restored onto a fresh disk") {
		t.Errorf("provenance = %q", got)
	}
	if len(got) > 400 {
		t.Errorf("provenance is %d bytes; the note should be cut", len(got))
	}
	if outageDeclareBy(outageDeclaration{}) != "an unnamed caller" {
		t.Errorf("empty provenance = %q", outageDeclareBy(outageDeclaration{}))
	}
}

// The relay's copy: only what it has not acknowledged travels, its answer
// sets the mark, and a relay without the field gets the rows once.
func TestOutagePendingFollowsTheRelaysAck(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	resetOutagePush(t)
	base := time.Unix(1_800_000_000, 0)
	outageBoot(base)
	outageTick(base.Add(time.Second))
	outageTick(base.Add(time.Second + 5*time.Minute)) // suspend 1 → 301
	outageTick(base.Add(time.Second + 5*time.Minute + time.Second))
	outageTick(base.Add(time.Second + 15*time.Minute)) // suspend 302 → 901
	now := base.Add(16 * time.Minute)
	if got := outagePending(now); len(got) != 2 {
		t.Fatalf("pending = %v, want both rows before any ack", got)
	}
	if since := outageSince(); since != base.Unix() {
		t.Fatalf("since = %d, want the boot %d", since, base.Unix())
	}
	// The relay says it holds everything up to the first row's end.
	first := float64(base.Unix() + 301)
	outageSent(outagePending(now))
	outageAcked(true, &first)
	if got := outagePending(now); len(got) != 1 || got[0][0].(float64) != float64(base.Unix()+302) {
		t.Fatalf("pending after ack = %v, want the second row alone", got)
	}
	// A lower figure (its write failed) brings a row back.
	zero := 0.0
	outageAcked(true, &zero)
	if got := outagePending(now); len(got) != 2 {
		t.Fatalf("pending after a lower ack = %v, want both rows again", got)
	}
	// A relay that answers without the field: what was sent is delivered.
	outageSent(outagePending(now))
	outageAcked(true, nil)
	if got := outagePending(now); len(got) != 0 {
		t.Fatalf("pending after a legacy answer = %v, want nothing", got)
	}
	// A response that was not the protocol says nothing.
	outageResend()
	outageAcked(false, nil)
	if got := outagePending(now); len(got) != 2 {
		t.Fatalf("pending after an unparsed answer = %v, want both rows", got)
	}
	// The window: a row older than a week stays home, a wipe does not.
	outages.Lock()
	outages.rows = append([]outage{{At: base.Unix() - 30*86400, End: base.Unix() - 30*86400 + 60, Kind: outageKindWipe},
		{At: base.Unix() - 9*86400, End: base.Unix() - 9*86400 + 60, Kind: outageKindDown}}, outages.rows...)
	outages.Unlock()
	got := outagePending(now)
	if len(got) != 3 || got[0][2] != outageKindWipe {
		t.Fatalf("pending with old rows = %v, want the wipe and the two suspends", got)
	}
	if since := outageSince(); since != base.Unix()-30*86400 {
		t.Fatalf("since = %d, want the oldest row", since)
	}
}

func resetOutagePush(t *testing.T) {
	t.Helper()
	reset := func() {
		outagePush.Lock()
		outagePush.ack, outagePush.sent, outagePush.gen, outagePush.sentGen, outagePush.sentFrom = 0, 0, 0, 0, 0
		outagePush.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// An answer to a push assembled before a declaration must not stand as the
// acknowledgement of the rows the declaration rewrote (review 2026-10-12).
func TestOutageAckIsForTheEditionSent(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	resetOutagePush(t)
	base := time.Unix(1_800_000_000, 0)
	outageBoot(base)
	outageTick(base.Add(time.Second))
	outageTick(base.Add(time.Second + 5*time.Minute)) // suspend 1 → 301
	now := base.Add(6 * time.Minute)
	outageSent(outagePending(now)) // the push on the wire
	// A declaration lands while that push is in flight: the window again.
	kind := outageKindWipe
	at, end := float64(base.Unix()+1), float64(base.Unix()+301)
	if _, err := outageDeclare(outageDeclaration{Kind: kind, At: &at, End: &end}, now); err != nil {
		t.Fatal(err)
	}
	stale := float64(base.Unix() + 301)
	outageAcked(true, &stale) // the in-flight push's answer
	if got := outagePending(now); len(got) != 1 || got[0][2] != outageKindWipe {
		t.Fatalf("pending after a stale answer = %v, want the rewritten row still to go", got)
	}
	outageSent(outagePending(now))
	outageAcked(true, &stale) // the answer to the push that carried it
	if got := outagePending(now); len(got) != 0 {
		t.Fatalf("pending after the right answer = %v, want nothing", got)
	}
}

// Rows the cap keeps home are not the relay's: the reach the push claims
// starts at the oldest row that went (review 2026-10-12).
func TestOutageSinceStopsAtTheCut(t *testing.T) {
	scratchHome(t)
	resetOutages(t)
	resetOutagePush(t)
	base := time.Unix(1_800_000_000, 0)
	outageBoot(base)
	outages.Lock()
	for i := 0; i < outagePushMax+50; i++ {
		at := base.Unix() + int64(i)*100
		outages.rows = append(outages.rows, outage{At: at, End: at + 40, Kind: outageKindSuspend})
	}
	outages.Unlock()
	now := base.Add(30000 * time.Second)
	got := outagePending(now)
	if len(got) != outagePushMax {
		t.Fatalf("pending = %d rows, want the cap", len(got))
	}
	oldest, _ := got[0][0].(float64)
	if since := outageSince(); since != int64(oldest) {
		t.Fatalf("since = %d, want the oldest row sent %d, not the boot %d", since, int64(oldest), base.Unix())
	}
	// Acknowledged: the next push fits under the cap, and the reach does
	// not move back past what was cut.
	last, _ := got[len(got)-1][1].(float64)
	outageSent(got)
	outageAcked(true, &last)
	if n := len(outagePending(now)); n != 0 {
		t.Fatalf("pending after ack = %d, want none", n)
	}
	if since := outageSince(); since != int64(oldest) {
		t.Fatalf("since after ack = %d, want it held at the cut %d", since, int64(oldest))
	}
}
