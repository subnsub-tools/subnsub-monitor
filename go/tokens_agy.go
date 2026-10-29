package main

// Antigravity's token counts (operator request 2026-10-29: add Antigravity and
// Amp the way Tokscale does), read the way Tokscale (github.com/junhoyeo/tokscale,
// MIT) reads them, off the conversation databases Antigravity keeps — the
// CLI's under ~/.gemini/antigravity-cli/conversations/<uuid>.db, the editor
// extension's under ~/.gemini/antigravity/conversations/<uuid>.db — through
// sqliteread.go, since no SQLite library is linked in.
//
// A conversation is one database. Each model call is a row of gen_metadata,
// rowids counting up from 0 with no gaps; the row's data is a protobuf
// CortexStepGeneratorMetadata (field numbers as agy 1.4.10's own descriptor
// names them):
//
//	#1 chat_model
//	   #3  model, an enum
//	   #4  usage: #2 input (cache not in it), #3 output (= #9 + #10),
//	       #4 cache write, #5 cache read, #9 thinking output,
//	       #10 response output, #11 response id
//	   #9  chat_start: #4 created_at {#1 seconds}
//	   #19 the model's API name, #21 its display name
//	#2 the step indices it produced, packed; the first is its planner step
//	#5 an error, on a call that failed
//
// Three of Tokscale's readings are not followed: usage #1 is the model enum
// (1000 + N), not a fixed prompt to add to input; #9 is thinking and #10 the
// answer, not the other way round; and a call with no response id counts for
// nothing, rather than its enum as input. A row is written once its call is
// done and later slimmed of its prompt, its usage untouched, so a file is read
// on from the row after the last one read; a response id counts once however
// many files or reads meet it. The time is the call's own when it says one,
// else the creation time of its planner step (steps.metadata #1), else the
// conversation's (trajectory_metadata_blob #2), else the file's.
//
// The databases are in WAL mode: the newest calls live in the -wal file until
// a checkpoint, and a file is only taken as read when the -wal file's header
// is the same after the read as before it.

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The protobuf wire: each field a key (number << 3 | type), then a varint
// (type 0), 8 bytes (1), a length and that many bytes (2) or 4 bytes (5).
// fn sees a varint's value or a length-delimited field's bytes; false stops.
// Answers false on a message that does not parse.
func pbEach(b []byte, fn func(num int, typ int, v uint64, data []byte) bool) bool {
	for i := 0; i < len(b); {
		key, n := pbVarint(b, i)
		if n == 0 {
			return false
		}
		i += n
		num, typ := int(key>>3), int(key&7)
		if num == 0 {
			return false
		}
		switch typ {
		case 0:
			v, n := pbVarint(b, i)
			if n == 0 {
				return false
			}
			i += n
			if !fn(num, typ, v, nil) {
				return true
			}
		case 1:
			if i+8 > len(b) {
				return false
			}
			i += 8
		case 2:
			l, n := pbVarint(b, i)
			if n == 0 || l > uint64(len(b)-i-n) {
				return false
			}
			i += n
			if !fn(num, typ, 0, b[i:i+int(l)]) {
				return true
			}
			i += int(l)
		case 5:
			if i+4 > len(b) {
				return false
			}
			i += 4
		default:
			return false
		}
	}
	return true
}

// A protobuf varint at b[i:] and its length; 0 for none.
func pbVarint(b []byte, i int) (uint64, int) {
	var v uint64
	for n := 0; n < 10 && i+n < len(b); n++ {
		c := b[i+n]
		v |= uint64(c&0x7f) << (7 * n)
		if c < 0x80 {
			return v, n + 1
		}
	}
	return 0, 0
}

// The first varint of field `num` at the top of message b, and whether it is
// there.
func pbUint(b []byte, num int) (uint64, bool) {
	var out uint64
	found := false
	pbEach(b, func(n, typ int, v uint64, _ []byte) bool {
		if n == num && typ == 0 {
			out, found = v, true
			return false
		}
		return true
	})
	return out, found
}

// The first length-delimited field `num` at the top of message b.
func pbBytes(b []byte, num int) ([]byte, bool) {
	var out []byte
	found := false
	pbEach(b, func(n, typ int, _ uint64, data []byte) bool {
		if n == num && typ == 2 {
			out, found = data, true
			return false
		}
		return true
	})
	return out, found
}

// A google.protobuf.Timestamp's seconds, 0 for none.
func pbSeconds(ts []byte) float64 {
	s, ok := pbUint(ts, 1)
	if !ok || s == 0 || s > 1<<40 {
		return 0
	}
	return float64(s)
}

// One model call as its row says it.
type agyCall struct {
	id      string // the response id
	model   string // the API name, "" when the row names none
	enum    uint64 // the model's enum
	at      float64
	step    int64 // its planner step, -1 for none
	in, out int64 // out as the vendor counts it: thinking included
	cr, cw  int64
	think   int64
	resp    int64
	hasOut  bool
	failed  bool
}

func agyParse(data []byte) (agyCall, bool) {
	c := agyCall{step: -1}
	chat, ok := pbBytes(data, 1)
	if !ok {
		return c, false
	}
	if steps, ok := pbBytes(data, 2); ok {
		if v, n := pbVarint(steps, 0); n > 0 && v < 1<<31 {
			c.step = int64(v)
		}
	}
	if _, ok := pbBytes(data, 5); ok {
		c.failed = true
	}
	c.enum, _ = pbUint(chat, 3)
	if m, ok := pbBytes(chat, 19); ok {
		c.model = strings.TrimSpace(string(m))
	}
	if start, ok := pbBytes(chat, 9); ok {
		if ts, ok := pbBytes(start, 4); ok {
			c.at = pbSeconds(ts)
		}
	}
	usage, ok := pbBytes(chat, 4)
	if !ok {
		return c, true
	}
	count := func(n int) int64 {
		v, _ := pbUint(usage, n)
		if v > uint64(tokCountMax) {
			return 0
		}
		return int64(v)
	}
	c.in, c.cw, c.cr = count(2), count(4), count(5)
	c.think, c.resp = count(9), count(10)
	if _, ok := pbUint(usage, 3); ok {
		c.out, c.hasOut = count(3), true
	}
	if id, ok := pbBytes(usage, 11); ok {
		c.id = string(id)
	}
	if c.enum == 0 {
		c.enum, _ = pbUint(usage, 1)
	}
	return c, true
}

// The call as the five kinds: output less the thinking it includes; where the
// row gives no total, the answer's own part.
func (c agyCall) cell() tokCell {
	out, rs := c.out, c.think
	if !c.hasOut {
		out = c.think + c.resp
	}
	rs = min(rs, out)
	return tokCell{in: c.in, out: out - rs, cr: c.cr, cw: c.cw, rs: rs, n: 1}
}

// The model names the enums went by, learned from the rows that say both,
// for a row that names only its enum.
var agyNames = map[uint64]string{}

// Antigravity's conversation directories that exist.
func agyRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	var out []string
	for _, d := range []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "conversations"),
		filepath.Join(home, ".gemini", "antigravity", "conversations"),
	} {
		if real, err := filepath.EvalSymlinks(d); err == nil {
			d = real
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, filepath.Clean(d))
		}
	}
	return out
}

// The conversations under a root written since `from` — the database or its
// -wal file — newest first.
func agyCandidates(root string, from time.Time) (out []candidate, partial bool) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, true
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			partial = true
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		mt := info.ModTime()
		if w, err := os.Lstat(filepath.Join(root, name+"-wal")); err == nil && w.Mode().IsRegular() && w.ModTime().After(mt) {
			mt = w.ModTime()
		}
		if mt.Before(from) {
			continue
		}
		out = append(out, candidate{name, mt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mtime.After(out[j].mtime) })
	return out, partial
}

// A conversation's account brought up to date: the calls after the last one
// read, within the pass's budget. False when it is not whole.
func tokAgyRead(r *os.Root, rel, id string, f *tokFile, spent *int64, hourFrom, dayFrom int64) bool {
	fh, err := r.Open(rel)
	if err != nil {
		tokReset(id, f)
		return false
	}
	st, err := fh.Stat()
	if err != nil || !st.Mode().IsRegular() {
		fh.Close()
		tokReset(id, f)
		return false
	}
	if n, known := openLinkCount(fh, st); !known || n != 1 {
		fh.Close()
		tokReset(id, f)
		return false
	}
	var wal *os.File
	var walSize int64
	sig := st.ModTime().UnixNano() ^ st.Size()
	if w, err := r.Open(rel + "-wal"); err == nil {
		if ws, err := w.Stat(); err == nil && ws.Mode().IsRegular() {
			if n, known := openLinkCount(w, ws); known && n == 1 {
				wal = w
				walSize = ws.Size()
				sig = sig*31 ^ ws.ModTime().UnixNano() ^ ws.Size()
			}
		}
		if wal == nil {
			w.Close()
		}
	}
	if f.info != nil && !os.SameFile(f.info, st) {
		tokReset(id, f)
	}
	if f.info != nil && f.sig == sig {
		fh.Close()
		if wal != nil {
			wal.Close()
		}
		return true // nothing written since
	}
	// The -wal file is read whole up to its last commit before any row: what
	// of it is new must fit what the pass has left, unless the pass has read
	// nothing yet (so a log of any size is read in the end).
	if wal != nil && *spent > 0 {
		from := int64(0)
		if f.wal != nil {
			from = f.wal.off
		}
		if walSize-from > tokPassBudget-*spent {
			fh.Close()
			wal.Close()
			return false
		}
	}
	db, err := sqlFrom(fh, wal, f.wal)
	if err != nil {
		f.wal = nil
		return false
	}
	defer db.Close()
	start := *spent
	whole, err := agyCalls(db, id, f, st.ModTime(), spent, hourFrom, dayFrom)
	*spent = start + db.read
	f.info = st
	f.wal = db.walRead
	if err != nil {
		f.wal = nil
		return false
	}
	if whole {
		f.sig = sig
	}
	return whole
}

// The calls read on from f.next into the account, when what was read is one
// state of the database; f.next moves on to the row after the last one taken.
func agyCalls(db *sqlDB, id string, f *tokFile, mtime time.Time, spent *int64, hourFrom, dayFrom int64) (bool, error) {
	gen, genCols, err := db.table("gen_metadata")
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil // a conversation with no call yet
		}
		return false, err
	}
	dataAt := agyColumn(genCols, "data", 1)
	// Rows gone from under the account (the database written anew): read it
	// again from its start.
	if f.next > 0 {
		if _, ok, err := db.row(gen, f.next-1); err != nil {
			return false, err
		} else if !ok {
			tokReset(id, f)
		}
	}
	type got struct {
		rowid int64
		call  agyCall
	}
	var calls []got
	whole := true
	err = db.scanFrom(gen, f.next, func(rowid int64, cols []any) bool {
		if len(calls) > 0 && (*spent+db.read >= tokPassBudget || db.read >= tokFileBudget) {
			whole = false
			return false
		}
		var data []byte
		if dataAt < len(cols) {
			data, _ = cols[dataAt].([]byte)
		}
		c, ok := agyParse(data)
		if !ok {
			c = agyCall{step: -1, failed: true}
		}
		calls = append(calls, got{rowid, c})
		return true
	})
	if err != nil {
		return false, err
	}
	if len(calls) == 0 {
		return whole && db.steady(), nil
	}
	// Each call's time where it does not carry its own.
	var steps uint32
	stepsMeta := -1
	if root, cols, err := db.table("steps"); err == nil {
		steps, stepsMeta = root, agyColumn(cols, "metadata", 4)
	}
	var started float64
	if root, cols, err := db.table("trajectory_metadata_blob"); err == nil {
		at := agyColumn(cols, "data", 1)
		db.scan(root, func(_ int64, row []any) bool {
			if at < len(row) {
				if b, ok := row[at].([]byte); ok {
					if ts, ok := pbBytes(b, 2); ok {
						started = pbSeconds(ts)
					}
				}
			}
			return false
		})
	}
	for i := range calls {
		c := &calls[i].call
		if c.at > 0 || c.failed || c.id == "" || steps == 0 || c.step < 0 {
			continue
		}
		if row, ok, err := db.row(steps, c.step); err == nil && ok && stepsMeta < len(row) {
			if meta, ok := row[stepsMeta].([]byte); ok {
				if ts, ok := pbBytes(meta, 1); ok {
					c.at = pbSeconds(ts)
				}
			}
		}
	}
	if !db.steady() {
		return false, nil // a checkpoint ran under the read: again next pass
	}
	for _, g := range calls {
		c := g.call
		f.next = g.rowid + 1
		if c.model != "" && c.model != "gemini-default" && c.enum != 0 {
			agyNames[c.enum] = c.model
		}
		if c.failed || c.id == "" {
			continue
		}
		cell := c.cell()
		if cell.total() == 0 {
			continue
		}
		h := fnv.New64a()
		h.Write([]byte("antigravity\x00"))
		h.Write([]byte(c.id))
		k := h.Sum64()
		if _, dup := tokState.owner[k]; dup {
			continue
		}
		tokState.owner[k] = id
		f.ids = append(f.ids, k)
		model := c.model
		if model == "" || model == "gemini-default" {
			model = agyNames[c.enum]
		}
		at := c.at
		if at <= 0 {
			at = started
		}
		if at <= 0 {
			at = float64(mtime.Unix())
		}
		f.put(at, tokKey{"antigravity", tokModel(model)}, cell, hourFrom, dayFrom)
	}
	return whole, nil
}

// A column's position by name, else where it is known to stand.
func agyColumn(cols []string, name string, fallback int) int {
	for i, c := range cols {
		if strings.EqualFold(c, name) {
			return i
		}
	}
	return fallback
}
