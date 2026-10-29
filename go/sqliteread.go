package main

// A read-only reader of SQLite database files — just enough to walk a
// table's rows — for the token counts another program keeps in SQLite
// (tokens_antigravity.go): no SQLite library is linked into this helper, and
// one table scan does not justify the megabytes one would add.
//
// It follows the file format (sqlite.org/fileformat2.html): the header's page
// size and reserved bytes, table b-trees (interior 0x05, leaf 0x0d pages),
// payloads spilled onto overflow pages, records of serial types. A database
// in WAL mode keeps its newest pages in the -wal file beside it until a
// checkpoint copies them back, so the -wal file is read too: its frames up to
// the last commit whose salts and running checksum hold, the newest copy of
// each page winning. Pages are read on demand, not the whole file.
//
// The files belong to another program that may be writing them as they are
// read: every offset and length is checked, a page out of range or a cycle is
// an error rather than a panic, and the caller reads again on its next pass.

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"strings"
)

var errSQL = errors.New("sqlite: malformed")

type sqlDB struct {
	f        *os.File
	wal      *os.File
	pageSize int
	usable   int
	pages    uint32            // pages in the database, the WAL's last commit included
	walPage  map[uint32]int64  // page → offset of its newest committed copy in the -wal file
	walHead  []byte            // the -wal file's header as read, to tell a checkpoint since
	walRead  *sqlWAL           // how far the -wal file was read, for the next open to go on from
	cache    map[uint32][]byte // pages read this scan
	read     int64             // bytes read, for the caller's budget
	counter  uint32            // the header's change counter as read
}

// How far a -wal file was read, to its last commit: its header, the offset
// after that commit's frame and the checksum running to it, and the pages the
// commits hold. While the header stays the same the log has only grown past
// that commit, and the next read goes on from there instead of checksumming
// every frame again. Not from the last frame read: frames after the last
// commit belong to a transaction that may roll back, and SQLite writes the
// next one over them without touching the header.
type sqlWAL struct {
	head         []byte
	big          bool
	salt1, salt2 uint32
	off          int64
	s0, s1       uint32
	committed    map[uint32]int64
	pages        uint32
}

// sqlOpen opens path and, when there is one, path-wal. The caller closes it.
func sqlOpen(path string) (*sqlDB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	w, _ := os.Open(path + "-wal")
	return sqlFrom(f, w, nil)
}

// sqlFrom reads a database from its open file and its open -wal file (nil
// when there is none), going on in the -wal file from `prev` (nil: from its
// start), and owns both files from here: closed on an error, or by Close.
func sqlFrom(f, wal *os.File, prev *sqlWAL) (*sqlDB, error) {
	fail := func(err error) (*sqlDB, error) {
		f.Close()
		if wal != nil {
			wal.Close()
		}
		return nil, err
	}
	db := &sqlDB{f: f, wal: wal, cache: map[uint32][]byte{}}
	var h [100]byte
	if _, err := f.ReadAt(h[:], 0); err != nil {
		return fail(err)
	}
	if string(h[:16]) != "SQLite format 3\x00" {
		return fail(errSQL)
	}
	ps := int(binary.BigEndian.Uint16(h[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps > 65536 || ps&(ps-1) != 0 {
		return fail(errSQL)
	}
	db.pageSize = ps
	db.counter = binary.BigEndian.Uint32(h[24:28])
	db.usable = ps - int(h[20])
	if db.usable < 480 {
		return fail(errSQL)
	}
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	db.pages = uint32(st.Size() / int64(ps))
	if wal != nil {
		if err := db.readWAL(prev); err != nil {
			// A -wal file that does not parse holds nothing committed that
			// can be trusted: read the database as the last checkpoint left it.
			db.walPage = nil
		}
	}
	return db, nil
}

// steady says whether what was read is one state of the database. Pages
// come from the -wal file's commits up to the one read, and from the database
// for the rest; a writer can only have changed those rest under the read by
// committing something newer (a checkpoint copies committed frames back, and
// a rollback-journal writer bumps the header's change counter). So: the
// change counter as it was, the -wal file's header as it was, and no commit
// in it past the last one read. Not steady, the caller reads again next pass.
func (db *sqlDB) steady() bool {
	var h [28]byte
	if _, err := db.f.ReadAt(h[:], 0); err != nil || binary.BigEndian.Uint32(h[24:28]) != db.counter {
		return false
	}
	if db.wal == nil {
		return true
	}
	head := make([]byte, 32)
	n, _ := db.wal.ReadAt(head, 0)
	w := db.walRead
	if len(db.walHead) == 0 || w == nil {
		return n == 0
	}
	if n != 32 || string(head) != string(db.walHead) {
		return false
	}
	_, committed := db.walFrames(w, w.off, w.s0, w.s1, nil)
	return !committed
}

func (db *sqlDB) Close() {
	db.f.Close()
	if db.wal != nil {
		db.wal.Close()
	}
}

// The -wal file's committed frames: a 32-byte header, then frames of a
// 24-byte header and a page each. A frame belongs when its salts are the
// header's and the checksum running from the header through it holds; the
// pages count up to the last commit frame (one naming the database's size).
func (db *sqlDB) readWAL(prev *sqlWAL) error {
	h := make([]byte, 32)
	if _, err := db.wal.ReadAt(h, 0); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty: nothing beyond the checkpoint
		}
		return err
	}
	db.walHead = h
	magic := binary.BigEndian.Uint32(h[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return errSQL
	}
	if int(binary.BigEndian.Uint32(h[8:12])) != db.pageSize {
		return errSQL
	}
	w := prev
	if w == nil || string(w.head) != string(h) {
		big := magic == 0x377f0683
		s0, s1 := walSum(h[:24], 0, 0, big)
		if s0 != binary.BigEndian.Uint32(h[24:28]) || s1 != binary.BigEndian.Uint32(h[28:32]) {
			return errSQL
		}
		w = &sqlWAL{head: h, big: big, salt1: binary.BigEndian.Uint32(h[16:20]), salt2: binary.BigEndian.Uint32(h[20:24]),
			off: 32, s0: s0, s1: s1, committed: map[uint32]int64{}}
	}
	db.walFrames(w, w.off, w.s0, w.s1, w)
	db.walRead = w
	if len(w.committed) > 0 {
		db.walPage = w.committed
		db.pages = w.pages
	}
	return nil
}

// The frames from `off`, the checksum running to it s0 and s1, while they
// belong: each commit's pages folded into into.committed and into's place
// moved past that commit when into is not nil. Says whether any commit was
// met.
func (db *sqlDB) walFrames(w *sqlWAL, off int64, s0, s1 uint32, into *sqlWAL) (int64, bool) {
	frame := make([]byte, 24+db.pageSize)
	pending := map[uint32]int64{}
	met := false
	for ; ; off += int64(len(frame)) {
		if _, err := db.wal.ReadAt(frame, off); err != nil {
			break
		}
		db.read += int64(len(frame))
		fh := frame[:24]
		if binary.BigEndian.Uint32(fh[8:12]) != w.salt1 || binary.BigEndian.Uint32(fh[12:16]) != w.salt2 {
			break
		}
		a, b := walSum(fh[:8], s0, s1, w.big)
		a, b = walSum(frame[24:], a, b, w.big)
		if a != binary.BigEndian.Uint32(fh[16:20]) || b != binary.BigEndian.Uint32(fh[20:24]) {
			break
		}
		pg := binary.BigEndian.Uint32(fh[0:4])
		if pg == 0 {
			break
		}
		s0, s1 = a, b
		pending[pg] = off + 24
		if size := binary.BigEndian.Uint32(fh[4:8]); size != 0 {
			met = true
			if into == nil {
				break
			}
			for p, o := range pending {
				into.committed[p] = o
			}
			clear(pending)
			into.pages = size
			into.off, into.s0, into.s1 = off+int64(len(frame)), s0, s1
		}
	}
	return off, met
}

// The WAL's checksum (sqlite.org/fileformat2.html#walformat): over 32-bit
// words read in the byte order the magic names.
func walSum(b []byte, s0, s1 uint32, big bool) (uint32, uint32) {
	for i := 0; i+8 <= len(b); i += 8 {
		var x0, x1 uint32
		if big {
			x0, x1 = binary.BigEndian.Uint32(b[i:]), binary.BigEndian.Uint32(b[i+4:])
		} else {
			x0, x1 = binary.LittleEndian.Uint32(b[i:]), binary.LittleEndian.Uint32(b[i+4:])
		}
		s0 += x0 + s1
		s1 += x1 + s0
	}
	return s0, s1
}

func (db *sqlDB) page(n uint32) ([]byte, error) {
	if n == 0 || n > db.pages {
		return nil, errSQL
	}
	if p, ok := db.cache[n]; ok {
		return p, nil
	}
	p := make([]byte, db.pageSize)
	var err error
	if off, ok := db.walPage[n]; ok {
		_, err = db.wal.ReadAt(p, off)
	} else {
		_, err = db.f.ReadAt(p, int64(n-1)*int64(db.pageSize))
	}
	if err != nil {
		return nil, err
	}
	db.read += int64(len(p))
	// Kept for the scan's descents, up to 8 MiB whatever the page size.
	if len(db.cache)*db.pageSize < 8<<20 {
		db.cache[n] = p
	}
	return p, nil
}

// A varint (1–9 bytes, big-endian, the ninth byte whole) at b[i:].
func sqlVarint(b []byte, i int) (int64, int, bool) {
	var v uint64
	for n := 0; n < 9; n++ {
		if i+n >= len(b) {
			return 0, 0, false
		}
		c := b[i+n]
		if n == 8 {
			return int64(v<<8 | uint64(c)), 9, true
		}
		v = v<<7 | uint64(c&0x7f)
		if c < 0x80 {
			return int64(v), n + 1, true
		}
	}
	return 0, 0, false
}

// table answers the root page of the table named `name` and its columns'
// names in order, from the schema on page 1.
func (db *sqlDB) table(name string) (uint32, []string, error) {
	var root uint32
	var sql string
	err := db.scan(1, func(_ int64, cols []any) bool {
		if len(cols) < 5 {
			return true
		}
		typ, _ := cols[0].(string)
		n, _ := cols[1].(string)
		if typ == "table" && strings.EqualFold(n, name) {
			if r, ok := cols[3].(int64); ok && r > 0 && r <= math.MaxUint32 {
				root = uint32(r)
			}
			sql, _ = cols[4].(string)
			return false
		}
		return true
	})
	if err != nil {
		return 0, nil, err
	}
	if root == 0 {
		return 0, nil, os.ErrNotExist
	}
	return root, sqlColumns(sql), nil
}

// The column names of a CREATE TABLE statement, in order: each top-level
// definition's first word, unquoted, leaving out the table constraints.
func sqlColumns(sql string) []string {
	open, end := strings.IndexByte(sql, '('), strings.LastIndexByte(sql, ')')
	if open < 0 || end <= open {
		return nil
	}
	var out []string
	depth, start := 0, open+1
	body := sql[:end]
	for i := open + 1; i <= len(body); i++ {
		if i < len(body) {
			switch body[i] {
			case '(':
				depth++
				continue
			case ')':
				depth--
				continue
			case ',':
				if depth > 0 {
					continue
				}
			default:
				continue
			}
		}
		def := strings.TrimSpace(body[start:i])
		start = i + 1
		if def == "" {
			continue
		}
		word := def
		if q := def[0]; q == '`' || q == '"' || q == '[' || q == '\'' {
			closer := q
			if q == '[' {
				closer = ']'
			}
			if j := strings.IndexByte(def[1:], closer); j >= 0 {
				out = append(out, def[1:1+j])
				continue
			}
		}
		if j := strings.IndexAny(word, " \t\n\r("); j >= 0 {
			word = word[:j]
		}
		switch strings.ToUpper(word) {
		case "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT":
			continue
		}
		out = append(out, word)
	}
	return out
}

// scan walks the table b-tree at root in rowid order, calling fn with each
// row's rowid and columns (int64, float64, string, []byte or nil) until fn
// says stop. An INTEGER PRIMARY KEY column holds nil: its value is the rowid.
func (db *sqlDB) scan(root uint32, fn func(rowid int64, cols []any) bool) error {
	return db.scanFrom(root, math.MinInt64, fn)
}

// scanFrom is scan from the first row whose rowid is at least `from`: a
// subtree whose keys all fall short is passed over unread, and so is the
// payload of a row before it, so reading on from where the last pass stopped
// reads the tree's right edge and the new rows.
func (db *sqlDB) scanFrom(root uint32, from int64, fn func(rowid int64, cols []any) bool) error {
	seen := map[uint32]bool{}
	_, err := db.walk(root, from, seen, fn)
	return err
}

// row answers one row by its rowid, down the tree's keys.
func (db *sqlDB) row(root uint32, rowid int64) ([]any, bool, error) {
	var got []any
	found := false
	err := db.scanFrom(root, rowid, func(id int64, cols []any) bool {
		if id == rowid {
			got, found = cols, true
		}
		return false
	})
	return got, found, err
}

func (db *sqlDB) walk(n uint32, from int64, seen map[uint32]bool, fn func(int64, []any) bool) (bool, error) {
	if seen[n] || len(seen) > 1<<20 {
		return false, errSQL
	}
	seen[n] = true
	p, err := db.page(n)
	if err != nil {
		return false, err
	}
	hdr := 0
	if n == 1 {
		hdr = 100
	}
	if hdr+8 > len(p) {
		return false, errSQL
	}
	kind := p[hdr]
	cells := int(binary.BigEndian.Uint16(p[hdr+3:]))
	switch kind {
	case 0x05: // interior: cells of [left child, key], then the right-most child
		if hdr+12 > len(p) {
			return false, errSQL
		}
		right := binary.BigEndian.Uint32(p[hdr+8:])
		ptrs := hdr + 12
		if ptrs+2*cells > len(p) {
			return false, errSQL
		}
		for i := 0; i < cells; i++ {
			c := int(binary.BigEndian.Uint16(p[ptrs+2*i:]))
			if c+4 > len(p) {
				return false, errSQL
			}
			// The key is the largest rowid in the left child.
			key, _, ok := sqlVarint(p, c+4)
			if !ok {
				return false, errSQL
			}
			if key < from {
				continue
			}
			more, err := db.walk(binary.BigEndian.Uint32(p[c:]), from, seen, fn)
			if err != nil || !more {
				return more, err
			}
		}
		return db.walk(right, from, seen, fn)
	case 0x0d: // leaf: cells of [payload size, rowid, payload]
		ptrs := hdr + 8
		if ptrs+2*cells > len(p) {
			return false, errSQL
		}
		for i := 0; i < cells; i++ {
			c := int(binary.BigEndian.Uint16(p[ptrs+2*i:]))
			size, k, ok := sqlVarint(p, c)
			if !ok || size < 0 || size > 1<<30 {
				return false, errSQL
			}
			rowid, k2, ok := sqlVarint(p, c+k)
			if !ok {
				return false, errSQL
			}
			if rowid < from {
				continue
			}
			payload, err := db.payload(p, c+k+k2, int(size))
			if err != nil {
				return false, err
			}
			cols, err := sqlRecord(payload)
			if err != nil {
				return false, err
			}
			if !fn(rowid, cols) {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, errSQL
	}
}

// A leaf cell's payload: what the page holds, then the overflow chain.
func (db *sqlDB) payload(p []byte, at, size int) ([]byte, error) {
	u := db.usable
	x := u - 35
	local := size
	if size > x {
		m := (u-12)*32/255 - 23
		k := m + (size-m)%(u-4)
		if k <= x {
			local = k
		} else {
			local = m
		}
	}
	if at+local > len(p) || at < 0 {
		return nil, errSQL
	}
	// No more than the pages it could spill onto could hold: a length a
	// broken cell makes up is not an allocation.
	if int64(size-local) > int64(db.pages)*int64(u-4) {
		return nil, errSQL
	}
	out := make([]byte, 0, size)
	out = append(out, p[at:at+local]...)
	if local == size {
		return out, nil
	}
	if at+local+4 > len(p) {
		return nil, errSQL
	}
	next := binary.BigEndian.Uint32(p[at+local:])
	met := map[uint32]bool{}
	for len(out) < size {
		if next == 0 || met[next] {
			return nil, errSQL
		}
		met[next] = true
		op, err := db.page(next)
		if err != nil {
			return nil, err
		}
		if u > len(op) {
			return nil, errSQL
		}
		next = binary.BigEndian.Uint32(op[:4])
		take := min(u-4, size-len(out))
		out = append(out, op[4:4+take]...)
	}
	return out, nil
}

// A record: a header of serial types, then the values they say.
func sqlRecord(b []byte) ([]any, error) {
	hl, n, ok := sqlVarint(b, 0)
	if !ok || hl < int64(n) || hl > int64(len(b)) {
		return nil, errSQL
	}
	var types []int64
	for i := n; i < int(hl); {
		t, k, ok := sqlVarint(b, i)
		if !ok || t < 0 {
			return nil, errSQL
		}
		types = append(types, t)
		i += k
	}
	cols := make([]any, 0, len(types))
	at := int(hl)
	for _, t := range types {
		var size int
		switch {
		case t == 0 || t == 8 || t == 9:
			size = 0
		case t >= 1 && t <= 4:
			size = int(t)
		case t == 5:
			size = 6
		case t == 6 || t == 7:
			size = 8
		case t >= 12:
			if (t-12)/2 > int64(len(b)) {
				return nil, errSQL
			}
			size = int((t - 12) / 2)
		default:
			return nil, errSQL
		}
		if size > len(b)-at {
			return nil, errSQL
		}
		v := b[at : at+size]
		at += size
		switch {
		case t == 0:
			cols = append(cols, nil)
		case t == 8:
			cols = append(cols, int64(0))
		case t == 9:
			cols = append(cols, int64(1))
		case t <= 6:
			var x int64
			for _, c := range v {
				x = x<<8 | int64(c)
			}
			if size < 8 && v[0]&0x80 != 0 {
				x -= 1 << (8 * size)
			}
			cols = append(cols, x)
		case t == 7:
			cols = append(cols, math.Float64frombits(binary.BigEndian.Uint64(v)))
		case t%2 == 0:
			cols = append(cols, append([]byte(nil), v...))
		default:
			cols = append(cols, string(v))
		}
	}
	return cols, nil
}
