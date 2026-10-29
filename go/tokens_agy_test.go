package main

// Antigravity's token counts off its conversation databases (tokens_agy.go,
// sqliteread.go), on the synthetic databases testdata/gen-agy-fixture.py
// writes: interior and overflow pages, a WAL-mode database whose calls are
// only in its -wal file, the five kinds as Antigravity states them (input
// without the cache, output with the thinking), a failed call and a repeated
// response id counted for nothing, a call that names only its enum, each call
// at its own time, the step's or the conversation's, a file read on from where
// the last pass stopped, and a pass that runs out of budget finishing later.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const agyT0 = 1793200000

func agyFixture(t *testing.T, home, name, as string) string {
	t.Helper()
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"", "-wal"} {
		b, err := os.ReadFile(filepath.Join("testdata", name+ext))
		if err != nil {
			if ext != "" && os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		dst := filepath.Join(dir, as+ext)
		// Written over in place, as a writer would, so the file stays the same.
		fh, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fh.Write(b); err != nil {
			t.Fatal(err)
		}
		fh.Close()
	}
	return dir
}

func agyReset(t *testing.T) {
	t.Helper()
	agyNames = map[uint64]string{}
	t.Cleanup(func() { agyNames = map[uint64]string{} })
}

func TestSQLiteReadsTablesThroughOverflowAndWAL(t *testing.T) {
	db, err := sqlOpen(filepath.Join("testdata", "agy-plain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root, cols, err := db.table("gen_metadata")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"idx", "data", "size"}; len(cols) != 3 || cols[0] != want[0] || cols[1] != want[1] || cols[2] != want[2] {
		t.Fatalf("columns = %v", cols)
	}
	n, last, sizes := 0, int64(-1), true
	if err := db.scan(root, func(rowid int64, row []any) bool {
		if rowid != last+1 || row[0] != nil {
			t.Fatalf("row %d after %d: %v", rowid, last, row[0])
		}
		last = rowid
		data, _ := row[1].([]byte)
		size, _ := row[2].(int64)
		if int64(len(data)) != size {
			sizes = false
		}
		n++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if n != 305 || !sizes {
		t.Fatalf("rows = %d, sizes whole = %v", n, sizes)
	}
	// The overflowing row, by its rowid, and a range from the middle.
	row, ok, err := db.row(root, 4)
	if err != nil || !ok {
		t.Fatalf("row 4: %v %v", ok, err)
	}
	if b, _ := row[1].([]byte); len(b) < 20000 {
		t.Fatalf("row 4 holds %d bytes", len(b))
	}
	from := 0
	db.scanFrom(root, 300, func(rowid int64, _ []any) bool { from++; return true })
	if from != 5 {
		t.Fatalf("rows from 300 = %d", from)
	}
	if !db.steady() {
		t.Fatal("not steady")
	}

	w, err := sqlOpen(filepath.Join("testdata", "agy-wal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if len(w.walPage) == 0 {
		t.Fatal("the -wal file's pages were not read")
	}
	root, _, err = w.table("gen_metadata")
	if err != nil {
		t.Fatal(err)
	}
	n = 0
	w.scan(root, func(int64, []any) bool { n++; return true })
	if n != 3 {
		t.Fatalf("WAL rows = %d, want 3", n)
	}
	// Opened again from where it was read, the log is not read again.
	again, err := sqlFrom(mustOpen(t, filepath.Join("testdata", "agy-wal.db")), mustOpen(t, filepath.Join("testdata", "agy-wal.db-wal")), w.walRead)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.read != 0 || len(again.walPage) != len(w.walPage) {
		t.Fatalf("read again %d bytes, %d pages", again.read, len(again.walPage))
	}
}

func mustOpen(t *testing.T, p string) *os.File {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSQLColumns(t *testing.T) {
	got := sqlColumns("CREATE TABLE `steps` (`idx` integer,`step_type` integer NOT NULL DEFAULT 0,\"metadata\" blob, [w] text, x numeric(10,2), PRIMARY KEY (`idx`), UNIQUE(x))")
	want := []string{"idx", "step_type", "metadata", "w", "x"}
	if len(got) != len(want) {
		t.Fatalf("columns = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("columns = %v", got)
		}
	}
}

func TestTokensAntigravity(t *testing.T) {
	home := testHome(t)
	tkuReset(t)
	agyReset(t)
	agyFixture(t, home, "agy-plain.db", "a.db")
	agyFixture(t, home, "agy-wal.db", "w.db")
	now := time.Unix(agyT0+3600, 0)

	u := tokPass(now)
	if u == nil || u.Partial {
		t.Fatalf("answer = %+v", u)
	}
	// 0 and 2 (2 named by 0's enum): input without the cache, output less
	// the thinking it holds; 1 failed, 3 repeats 0.
	if got, want := tkuSum(u, "antigravity", "gemini-3.8-flash", true), (tokCell{in: 13896 + 2810, out: 41 + 75, cr: 20353, rs: 117 + 111, n: 2}); got != want {
		t.Fatalf("3.8 flash = %+v, want %+v", got, want)
	}
	if got, want := tkuSum(u, "antigravity", "gemini-4-argon-c", true), (tokCell{in: 5, out: 6, cw: 7, rs: 4, n: 1}); got != want {
		t.Fatalf("argon = %+v, want %+v", got, want)
	}
	if got, want := tkuSum(u, "antigravity", "gemini-3.7-flash", true), (tokCell{in: 300, out: 300, rs: 300, n: 300}); got != want {
		t.Fatalf("3.7 flash = %+v, want %+v", got, want)
	}
	// Only in the -wal file.
	if got, want := tkuSum(u, "antigravity", "gemini-3.8-flash-n", true), (tokCell{in: 303, out: 45, rs: 15, n: 3}); got != want {
		t.Fatalf("WAL = %+v, want %+v", got, want)
	}
	for _, k := range u.Keys {
		if k[0] == "antigravity" && (k[1] == "unknown" || k[1] == "other") {
			t.Fatalf("unexpected key %v", k)
		}
	}
	// Each at its time: 0 by its step, 2 by its own start, the rest by the
	// conversation's start.
	hours := map[int64]int64{}
	k38 := int64(-1)
	for i, k := range u.Keys {
		if k == [2]string{"antigravity", "gemini-3.8-flash"} {
			k38 = int64(i)
		}
	}
	for _, r := range u.Hours {
		if r[1] == k38 {
			hours[r[0]] += r[8]
		}
	}
	if hours[agyT0/3600] != 2 {
		t.Fatalf("3.8 flash hours = %v", hours)
	}

	// Two more calls: the file is read on from the row after the last, the
	// repeated response id counting for nothing.
	agyFixture(t, home, "agy-more.db", "a.db")
	u = tokPass(now)
	if got, want := tkuSum(u, "antigravity", "gemini-3.8-flash", true), (tokCell{in: 13896 + 2810 + 7, out: 41 + 75 + 6, cr: 20353, rs: 117 + 111 + 3, n: 3}); got != want {
		t.Fatalf("after more = %+v, want %+v", got, want)
	}
	for id, f := range tokState.files {
		if f.tool == "antigravity" && filepath.Base(id) == "a.db" && f.next != 307 {
			t.Fatalf("next = %d", f.next)
		}
	}
}

func TestTokensAntigravityBudget(t *testing.T) {
	home := testHome(t)
	tkuReset(t)
	agyReset(t)
	agyFixture(t, home, "agy-plain.db", "a.db")
	tokFileBudget = 20 << 10
	now := time.Unix(agyT0+3600, 0)
	var u *TokenUsage
	passes := 0
	for passes = 1; passes < 200; passes++ {
		u = tokPass(now)
		if u != nil && !u.Partial {
			break
		}
	}
	if passes < 2 || u == nil || u.Partial {
		t.Fatalf("after %d passes: %+v", passes, u)
	}
	if got := tkuSum(u, "antigravity", "gemini-3.7-flash", true); got.n != 300 {
		t.Fatalf("3.7 flash = %+v", got)
	}
	if got := tkuSum(u, "antigravity", "gemini-3.8-flash", true); got.n != 2 {
		t.Fatalf("3.8 flash = %+v", got)
	}
}

// Another program's files, read while it writes them: whatever their bytes, a
// read is an error or an answer, never a panic.
func TestSQLiteAndProtobufSurviveGarbage(t *testing.T) {
	orig, err := os.ReadFile(filepath.Join("testdata", "agy-plain.db"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	seed := uint64(0x9e3779b97f4a7c15)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	for round := 0; round < 300; round++ {
		b := append([]byte(nil), orig...)
		for i := 0; i < 1+round%40; i++ {
			b[next()%uint64(len(b))] = byte(next())
		}
		if round%7 == 0 {
			b = b[:next()%uint64(len(b))]
		}
		p := filepath.Join(dir, "g.db")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("round %d: panic %v", round, r)
				}
			}()
			db, err := sqlOpen(p)
			if err != nil {
				return
			}
			defer db.Close()
			root, _, err := db.table("gen_metadata")
			if err != nil {
				return
			}
			db.scan(root, func(_ int64, cols []any) bool {
				if len(cols) > 1 {
					if data, ok := cols[1].([]byte); ok {
						agyParse(data)
					}
				}
				return true
			})
			db.row(root, int64(next()%400))
		}()
	}
	for round := 0; round < 2000; round++ {
		b := make([]byte, next()%64)
		for i := range b {
			b[i] = byte(next())
		}
		agyParse(b)
		sqlRecord(b)
		pbEach(b, func(int, int, uint64, []byte) bool { return true })
	}
}

// A -wal file read to its last commit only: frames after it (a transaction
// that may roll back and be written over) are not where a later read goes on
// from; and a commit landing in the log while a read was under way makes the
// read not steady.
func TestSQLiteWALResumesAtCommitsAndSeesNewOnes(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"agy-wal.db", "agy-wal.db-wal"} {
		b, err := os.ReadFile(filepath.Join("testdata", n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(dir, "agy-wal.db")
	first, err := sqlOpen(db)
	if err != nil {
		t.Fatal(err)
	}
	w := first.walRead
	end := w.off
	frame := int64(24 + first.pageSize)
	if end != 32+3*frame {
		t.Fatalf("read to %d, want the third commit's end %d", end, 32+3*frame)
	}
	// A frame of a transaction not yet committed: valid, but no commit.
	walPath := db + "-wal"
	raw, _ := os.ReadFile(walPath)
	page := raw[32+24 : 32+24+first.pageSize]
	mk := func(pg, size uint32, s0, s1 uint32) ([]byte, uint32, uint32) {
		f := make([]byte, 24, 24+len(page))
		binary.BigEndian.PutUint32(f[0:], pg)
		binary.BigEndian.PutUint32(f[4:], size)
		binary.BigEndian.PutUint32(f[8:], w.salt1)
		binary.BigEndian.PutUint32(f[12:], w.salt2)
		a, b := walSum(f[:8], s0, s1, w.big)
		a, b = walSum(page, a, b, w.big)
		binary.BigEndian.PutUint32(f[16:], a)
		binary.BigEndian.PutUint32(f[20:], b)
		return append(f, page...), a, b
	}
	open := func() *os.File {
		f, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	pending, a, b := mk(2, 0, w.s0, w.s1)
	f := open()
	f.Write(pending)
	f.Close()
	second, err := sqlFrom(mustOpen(t, db), mustOpen(t, walPath), w)
	if err != nil {
		t.Fatal(err)
	}
	if second.walRead.off != end {
		t.Fatalf("went on from %d, past the last commit at %d", second.walRead.off, end)
	}
	if !second.steady() {
		t.Fatal("an uncommitted frame is not a new state")
	}
	// The same transaction committed while the read was under way.
	commit, _, _ := mk(3, first.pages, a, b)
	f = open()
	f.Write(commit)
	f.Close()
	if second.steady() {
		t.Fatal("a commit landed under the read and it still says steady")
	}
	second.Close()
	third, err := sqlFrom(mustOpen(t, db), mustOpen(t, walPath), w)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if third.walRead.off != end+2*frame || !third.steady() {
		t.Fatalf("after the commit: read to %d, steady %v", third.walRead.off, third.steady())
	}
	first.Close()
}
