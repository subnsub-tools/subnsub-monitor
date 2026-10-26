package main

// Tokens the coding agents spent on this machine — by hour and by day, per
// tool and model, in five kinds — for the Agent usage card's token section
// (operator request 2026-10-27: the fleet's token usage at the detail a
// single-machine usage app shows: input, output, cache read, cache write and
// reasoning, by agent and by model). Counts only: no prompt, no reply, no
// path and no session id leave the machine; a model's name is the one string
// that does. MON_TOKENS=off turns the whole reading off.
//
// Read off the agents' own transcripts, the files session search already
// reads (sessionsources.go):
//
//   - Claude Code: <config>/projects/**/*.jsonl, every assistant line's
//     message.usage under message.model. A reply is written once per content
//     block, each of those lines carrying the same usage, and a resumed
//     session copies the replies before it into its new file; so a reply
//     counts once, by its message id and request id (the rule ccusage and
//     Tokscale both arrived at).
//   - Codex: sessions/**/rollout-*.jsonl and archived_sessions/, every
//     token_count event: the step in the session's running total, or the
//     turn's own usage where that step is not in view (codexactivity.go's
//     rule, kind by kind), under the model the turn_context line before it
//     names.
//
// The kinds, kept apart so the page can price each:
//
//	in   input not served from cache
//	out  output, less the reasoning it includes
//	cr   cache read
//	cw   cache write: Claude's cache creation; w1h is its one-hour share,
//	     which is priced above the five-minute one
//	rs   reasoning: Claude's thinking tokens, Codex's reasoning output
//	n    replies (Claude) or turns (Codex) counted
//
// No cost is computed here: a price changes without the helper changing, so
// the page puts one on (the site's /api/monitor-token-prices).
//
// Hours are UTC hours and days UTC days. The page adds up its reader's own
// today from the hours, which reach back past any time zone's midnight, and a
// longer span from the days.
//
// Read incrementally, forwards, whole lines only, bounded per file and per
// pass: a first pass over a month of transcripts (hundreds of megabytes on a
// busy machine) is spread over several collects, and the answer says
// `partial` until it is whole. A pass runs at most once a minute whatever
// asks for readings; between passes the last answer is served.
//
// Kept: the days and the last hours, in tokens.json beside the token, so a
// day outlives the transcript it was read from (Claude Code deletes a session
// thirty days after its last write unless told otherwise) and a restarted
// helper does not start today from nothing while it reads the month again. A
// bucket read again from the transcripts and a kept one are merged by keeping,
// per tool and model, the one with more tokens: a transcript that is gone
// lowers nothing, and one still being read raises the bucket as it goes.

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	tokVersion   = 1
	tokEvery     = 60.0 // seconds between passes
	tokHours     = 25   // hourly buckets on the wire, the current one included
	tokWireDays  = 32   // daily buckets on the wire, today included
	tokLiveDays  = 32   // days read from the transcripts
	tokKeepDays  = 400  // days kept in tokens.json
	tokMaxFiles  = 4000 // transcripts followed at once
	tokMaxKeys   = 48   // tool and model pairs on the wire
	tokHeadMax   = 256  // bytes kept of a file's start
	tokSaveEvery = 300.0
	tokModelMax  = 64
	tokCountMax  = 1e13 // a count past this is not a count
)

// Read sizes; variables so a test can make a pass run out.
var (
	tokChunk      int64 = 4 << 20   // one read
	tokFileBudget int64 = 64 << 20  // one file's share of a pass
	tokPassBudget int64 = 160 << 20 // what one pass may read
)

// TokenUsage is the wire block (Snapshot.Tokens, `tok`). Keys name each row's
// tool and model once; a row is [bucket, key, in, out, cr, cw, w1h, rs, n],
// the bucket an hour (unix seconds / 3600) in Hours and a day (unix seconds /
// 86400) in Days.
type TokenUsage struct {
	V       int         `json:"v"`
	At      int64       `json:"at"`
	Keys    [][2]string `json:"k"`
	Hours   [][9]int64  `json:"h"`
	Days    [][9]int64  `json:"d"`
	Partial bool        `json:"partial,omitempty"`
}

type tokKey struct{ tool, model string }

type tokCell struct{ in, out, cr, cw, w1h, rs, n int64 }

func (c *tokCell) add(o tokCell) {
	c.in += o.in
	c.out += o.out
	c.cr += o.cr
	c.cw += o.cw
	c.w1h += o.w1h
	c.rs += o.rs
	c.n += o.n
}

func (c tokCell) total() int64 { return c.in + c.out + c.cr + c.cw + c.rs }

type tokSlot struct {
	at  int64
	key tokKey
}

// One transcript's account: how far it has been read (the end of its last
// whole line), what it added to each hour and day, and for Codex the running
// total and model it has reached.
type tokFile struct {
	tool  string
	info  os.FileInfo // the file read, to tell another put in its place
	head  []byte      // its first bytes, to tell it rewritten in place
	size  int64
	skip  bool // the read stopped inside a line too long to keep
	model string
	total tokCodexRaw
	have  bool
	hours map[tokSlot]tokCell
	days  map[tokSlot]tokCell
	ids   []uint64 // Claude: the replies this file counted
}

var tokRun sync.Mutex // one pass at a time

var tokState struct {
	files    map[string]*tokFile
	owner    map[uint64]string   // a Claude reply → the file that counted it
	kept     map[tokSlot]tokCell // days
	keptH    map[tokSlot]tokCell // hours
	keptRead bool
	dirty    bool
	savedAt  float64
}

var tokOut struct {
	sync.Mutex
	last *TokenUsage
	at   float64
}

// The token reading for this collect: the last pass's answer, after a new
// pass when that one is a minute old and no other pass is running.
func tokenSnapshot() *TokenUsage {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("MON_TOKENS")), "off") {
		return nil
	}
	t := now()
	tokOut.Lock()
	last, at := tokOut.last, tokOut.at
	tokOut.Unlock()
	if at > 0 && t-at < tokEvery {
		return last
	}
	if !tokRun.TryLock() {
		return last
	}
	defer tokRun.Unlock()
	tokOut.Lock()
	if tokOut.at > 0 && t-tokOut.at < tokEvery {
		last = tokOut.last
		tokOut.Unlock()
		return last
	}
	tokOut.Unlock()
	u := tokPassSafe(time.Unix(int64(t), 0))
	tokOut.Lock()
	tokOut.last, tokOut.at = u, t
	tokOut.Unlock()
	return u
}

// A pass, contained: these are other programs' files on machines this was
// never run on, and a panic in here must not take the reading down. Whatever
// the pass held is dropped, and the next one starts from the kept buckets.
func tokPassSafe(t time.Time) (u *TokenUsage) {
	defer func() {
		if recover() != nil {
			tokState.files, tokState.owner, tokState.keptRead = nil, nil, false
			u = nil
		}
	}()
	return tokPass(t)
}

type tokSource struct {
	tool  string
	root  string
	match func(rel string) bool
}

// Where the transcripts are: Claude Code's projects under each config
// directory it may use (CLAUDE_CONFIG_DIR, a comma-separated list, then the
// XDG one and the classic one), Codex's sessions and archived sessions. A
// directory reached twice under two names is read once.
func tokSources() []tokSource {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	var out []tokSource
	var stats []os.FileInfo
	addRoot := func(tool, root string, match func(string) bool) {
		if root == "" {
			return
		}
		// The real directory, not a link to it: the walk does not follow a
		// root that is a symlink, and one directory under two names is read
		// once (review 2026-10-27).
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		root = filepath.Clean(root)
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			return
		}
		for _, s := range stats {
			if os.SameFile(s, st) {
				return
			}
		}
		stats = append(stats, st)
		out = append(out, tokSource{tool, root, match})
	}
	for _, d := range strings.Split(os.Getenv("CLAUDE_CONFIG_DIR"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			addRoot("claude", filepath.Join(d, "projects"), sessClaudeMatch)
		}
	}
	addRoot("claude", filepath.Join(home, ".config", "claude", "projects"), sessClaudeMatch)
	addRoot("claude", filepath.Join(home, ".claude", "projects"), sessClaudeMatch)
	addRoot("codex", codexSessionsDir(), sessCodexMatch)
	addRoot("codex", filepath.Join(home, ".codex", "archived_sessions"), sessCodexMatch)
	return out
}

// The transcripts under one root written since `from`, newest first.
func tokCandidates(src tokSource, from time.Time) (out []candidate, partial bool) {
	filepath.WalkDir(src.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			partial = true
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src.root, p)
		if err != nil {
			partial = true
			return nil
		}
		if !src.match(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			partial = true
			return nil
		}
		if !info.Mode().IsRegular() || info.ModTime().Before(from) {
			return nil
		}
		out = append(out, candidate{rel, info.ModTime()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].mtime.After(out[j].mtime) })
	return out, partial
}

// One pass at `t`: every transcript brought up to date within the budget, the
// windows moved on, the days merged with the kept ones, and the answer.
func tokPass(t time.Time) *TokenUsage {
	st := &tokState
	if st.files == nil {
		st.files = map[string]*tokFile{}
		st.owner = map[uint64]string{}
	}
	if !st.keptRead {
		st.kept, st.keptH = tokLoad()
		st.keptRead = true
	}
	unix := t.Unix()
	hourFrom := unix/3600 - tokHours + 1
	dayFrom := unix/86400 - tokLiveDays + 1
	from := time.Unix(dayFrom*86400, 0).Add(-mtimeSlack)

	srcs := tokSources()
	partial := false
	var spent int64
	buf := make([]byte, tokChunk)
	seen := map[string]bool{}
	n := 0
	for _, src := range srcs {
		r, err := os.OpenRoot(src.root)
		if err != nil {
			partial = true
			continue
		}
		cands, part := tokCandidates(src, from)
		if part {
			partial = true
		}
		for _, c := range cands {
			if n >= tokMaxFiles {
				partial = true
				break
			}
			n++
			id := src.tool + "\x00" + src.root + "\x00" + c.rel
			seen[id] = true
			f := st.files[id]
			if f == nil {
				f = &tokFile{tool: src.tool}
				st.files[id] = f
			}
			if !tokRead(r, c.rel, id, f, buf, &spent, hourFrom, dayFrom) {
				partial = true
			}
		}
		r.Close()
	}
	for id, f := range st.files {
		if !seen[id] {
			tokDrop(id, f)
			delete(st.files, id)
			continue
		}
		for s := range f.hours {
			if s.at < hourFrom {
				delete(f.hours, s)
			}
		}
		for s := range f.days {
			if s.at < dayFrom {
				delete(f.days, s)
			}
		}
	}
	if len(srcs) == 0 && len(st.kept) == 0 && len(st.keptH) == 0 {
		return nil
	}

	hours := map[tokSlot]tokCell{}
	live := map[tokSlot]tokCell{}
	for _, f := range st.files {
		for s, c := range f.hours {
			v := hours[s]
			v.add(c)
			hours[s] = v
		}
		for s, c := range f.days {
			v := live[s]
			v.add(c)
			live[s] = v
		}
	}
	if st.kept == nil {
		st.kept = map[tokSlot]tokCell{}
	}
	if st.keptH == nil {
		st.keptH = map[tokSlot]tokCell{}
	}
	if tokMerge(st.kept, live, unix/86400-tokKeepDays+1) {
		st.dirty = true
	}
	if tokMerge(st.keptH, hours, hourFrom) {
		st.dirty = true
	}
	if st.dirty && now()-st.savedAt >= tokSaveEvery {
		if tokSave(st.kept, st.keptH) == nil {
			st.dirty = false
		}
		st.savedAt = now()
	}
	return tokAnswer(unix, st.keptH, st.kept, partial)
}

// The live buckets folded into the kept ones, each the larger of the two, and
// the kept ones older than `oldest` let go. Says whether anything changed.
func tokMerge(kept, live map[tokSlot]tokCell, oldest int64) bool {
	changed := false
	for s := range kept {
		if s.at < oldest {
			delete(kept, s)
			changed = true
		}
	}
	for s, c := range live {
		if s.at < oldest {
			continue
		}
		if k, ok := kept[s]; !ok || c.total() > k.total() || (c.total() == k.total() && c.n > k.n) {
			kept[s] = c
			changed = true
		}
	}
	return changed
}

// One file's account brought up to date, opened through its root and past the
// hard-link guard codex.go keeps, for the same reason. False when it is not
// whole: refused, gone, or more than this pass may read.
func tokRead(r *os.Root, rel, id string, f *tokFile, buf []byte, spent *int64, hourFrom, dayFrom int64) bool {
	fh, err := r.Open(rel)
	if err != nil {
		tokReset(id, f)
		return false
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil || !st.Mode().IsRegular() {
		tokReset(id, f)
		return false
	}
	if n, known := openLinkCount(fh, st); !known || n != 1 {
		tokReset(id, f)
		return false
	}
	size := st.Size()
	// Another file put in its place, one rewritten in place (its first bytes
	// differ), or one shorter than its account: read again from its start,
	// since what was read of it is no longer what it holds (review
	// 2026-10-27). Transcripts are only ever appended to.
	if f.info != nil && !os.SameFile(f.info, st) || size < f.size || !tokSameHead(fh, f.head) {
		tokReset(id, f)
	}
	f.info = st
	var mine int64
	for f.size < size {
		if *spent >= tokPassBudget || mine >= tokFileBudget {
			return false
		}
		want := size - f.size
		if want > int64(len(buf)) {
			want = int64(len(buf))
		}
		got, err := fh.ReadAt(buf[:want], f.size)
		if err != nil && !errors.Is(err, io.EOF) {
			return false
		}
		if got <= 0 {
			return false
		}
		*spent += int64(got)
		mine += int64(got)
		b := buf[:got]
		if f.size == 0 && len(f.head) == 0 {
			f.head = append([]byte(nil), b[:min(len(b), tokHeadMax)]...)
		}
		if f.skip {
			// Still inside a line too long to keep: on to its newline.
			i := bytes.IndexByte(b, '\n')
			if i < 0 {
				f.size += int64(got)
				continue
			}
			f.size += int64(i) + 1
			f.skip = false
			continue
		}
		i := bytes.LastIndexByte(b, '\n')
		if i < 0 {
			if int64(got) < int64(len(buf)) {
				// The last line is still being written; it is read once whole.
				return true
			}
			// A line longer than a whole read: passed over. A usage line is
			// never one; a pasted image or a tool's whole output can be.
			f.size += int64(got)
			f.skip = true
			continue
		}
		for _, line := range bytes.Split(b[:i], []byte{'\n'}) {
			tokEntry(id, f, line, hourFrom, dayFrom)
		}
		f.size += int64(i) + 1
	}
	return true
}

// Whether the file still starts with the bytes it started with when its
// reading began.
func tokSameHead(fh *os.File, head []byte) bool {
	if len(head) == 0 {
		return true
	}
	b := make([]byte, len(head))
	n, err := fh.ReadAt(b, 0)
	if n != len(b) && err != nil {
		return false
	}
	return bytes.Equal(b[:n], head)
}

func tokEntry(id string, f *tokFile, line []byte, hourFrom, dayFrom int64) {
	switch f.tool {
	case "claude":
		tokClaudeLine(id, f, line, hourFrom, dayFrom)
	case "codex":
		tokCodexLine(f, line, hourFrom, dayFrom)
	}
}

// A count out of a JSON value: a non-negative whole number, else 0.
func tokCount(v any) int64 {
	n := asNum(v)
	if n == nil || *n < 0 || *n > tokCountMax {
		return 0
	}
	return int64(*n)
}

func tokHas(v any) bool { return asNum(v) != nil }

// A model's name as it may travel: the characters model names are made of,
// at most tokModelMax of them; anything else is "other", and no name at all
// "unknown".
func tokModel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	if len(s) > tokModelMax {
		return "other"
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("._:/@+-[]", r)
		if !ok {
			return "other"
		}
	}
	return s
}

// One counted turn into a file's hours and days, by when it happened.
func (f *tokFile) put(at float64, key tokKey, c tokCell, hourFrom, dayFrom int64) {
	if at <= 0 {
		return
	}
	sec := int64(at)
	if d := sec / 86400; d >= dayFrom {
		if f.days == nil {
			f.days = map[tokSlot]tokCell{}
		}
		s := tokSlot{d, key}
		v := f.days[s]
		v.add(c)
		f.days[s] = v
	}
	if h := sec / 3600; h >= hourFrom {
		if f.hours == nil {
			f.hours = map[tokSlot]tokCell{}
		}
		s := tokSlot{h, key}
		v := f.hours[s]
		v.add(c)
		f.hours[s] = v
	}
}

type tokClaudeUsage struct {
	Input    any `json:"input_tokens"`
	Output   any `json:"output_tokens"`
	CW       any `json:"cache_creation_input_tokens"`
	CR       any `json:"cache_read_input_tokens"`
	Creation *struct {
		H1 any `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	Details *struct {
		Thinking any `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type tokClaudeEntry struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	APIError  bool   `json:"isApiErrorMessage"`
	Message   *struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage *tokClaudeUsage `json:"usage"`
	} `json:"message"`
}

func tokClaudeLine(id string, f *tokFile, line []byte, hourFrom, dayFrom int64) {
	if !bytes.Contains(line, []byte(`"usage"`)) || !bytes.Contains(line, []byte(`"assistant"`)) {
		return
	}
	var e tokClaudeEntry
	if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.APIError || e.Message == nil || e.Message.Usage == nil {
		return
	}
	u := e.Message.Usage
	if !tokHas(u.Input) || !tokHas(u.Output) || strings.TrimSpace(e.Message.Model) == "<synthetic>" {
		return
	}
	at := isoToEpoch(e.Timestamp)
	if at <= 0 || int64(at)/86400 < dayFrom {
		return
	}
	if e.Message.ID != "" {
		h := fnv.New64a()
		h.Write([]byte(e.Message.ID))
		h.Write([]byte{0})
		h.Write([]byte(e.RequestID))
		k := h.Sum64()
		if _, dup := tokState.owner[k]; dup {
			return
		}
		tokState.owner[k] = id
		f.ids = append(f.ids, k)
	}
	c := tokCell{in: tokCount(u.Input), cr: tokCount(u.CR), cw: tokCount(u.CW), n: 1}
	out := tokCount(u.Output)
	if u.Details != nil {
		c.rs = min(tokCount(u.Details.Thinking), out)
	}
	c.out = out - c.rs
	if u.Creation != nil {
		c.w1h = min(tokCount(u.Creation.H1), c.cw)
	}
	f.put(at, tokKey{"claude", tokModel(e.Message.Model)}, c, hourFrom, dayFrom)
}

// A Codex usage block's five figures, as Codex states them: input counts the
// cached part (and any cache write) inside it, and output the reasoning.
type tokCodexRaw struct{ input, cached, cw, output, reason int64 }

type tokCodexUsage struct {
	Input  any `json:"input_tokens"`
	Cached any `json:"cached_input_tokens"`
	CW     any `json:"cache_write_input_tokens"`
	Output any `json:"output_tokens"`
	Reason any `json:"reasoning_output_tokens"`
}

func (u *tokCodexUsage) raw() (tokCodexRaw, bool) {
	if u == nil || !tokHas(u.Input) || !tokHas(u.Output) {
		return tokCodexRaw{}, false
	}
	return tokCodexRaw{tokCount(u.Input), tokCount(u.Cached), tokCount(u.CW), tokCount(u.Output), tokCount(u.Reason)}, true
}

func (a tokCodexRaw) atLeast(b tokCodexRaw) bool {
	return a.input >= b.input && a.cached >= b.cached && a.cw >= b.cw && a.output >= b.output && a.reason >= b.reason
}

func (a tokCodexRaw) minus(b tokCodexRaw) tokCodexRaw {
	return tokCodexRaw{a.input - b.input, a.cached - b.cached, a.cw - b.cw, a.output - b.output, a.reason - b.reason}
}

func (a tokCodexRaw) cell() tokCell {
	cached := min(a.cached, a.input)
	cw := min(a.cw, a.input-cached)
	rs := min(a.reason, a.output)
	return tokCell{in: a.input - cached - cw, cr: cached, cw: cw, out: a.output - rs, rs: rs, n: 1}
}

type tokCodexEntry struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Info  *struct {
			Total *tokCodexUsage `json:"total_token_usage"`
			Last  *tokCodexUsage `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

func tokCodexLine(f *tokFile, line []byte, hourFrom, dayFrom int64) {
	turn := bytes.Contains(line, []byte(`"turn_context"`))
	if !turn && !bytes.Contains(line, []byte(`"token_count"`)) {
		return
	}
	var e tokCodexEntry
	if json.Unmarshal(line, &e) != nil {
		return
	}
	if e.Type == "turn_context" {
		if m := strings.TrimSpace(e.Payload.Model); m != "" {
			f.model = tokModel(m)
		}
		return
	}
	if e.Payload.Type != "token_count" || e.Payload.Info == nil {
		return
	}
	total, haveTotal := e.Payload.Info.Total.raw()
	last, haveLast := e.Payload.Info.Last.raw()
	var step tokCodexRaw
	switch {
	case haveTotal && f.have && total.atLeast(f.total):
		step = total.minus(f.total)
	case haveLast:
		step = last
	default:
		if haveTotal {
			f.total, f.have = total, true
		}
		return
	}
	if haveTotal {
		f.total, f.have = total, true
	}
	if step == (tokCodexRaw{}) {
		return // the same total written twice
	}
	model := f.model
	if model == "" {
		model = "unknown"
	}
	f.put(isoToEpoch(e.Timestamp), tokKey{"codex", model}, step.cell(), hourFrom, dayFrom)
}

// A file's replies let go of, so another file holding the same ones may count
// them once this one is gone.
func tokDrop(id string, f *tokFile) {
	for _, k := range f.ids {
		if tokState.owner[k] == id {
			delete(tokState.owner, k)
		}
	}
	f.ids = nil
}

// A file read again from its start.
func tokReset(id string, f *tokFile) {
	tokDrop(id, f)
	*f = tokFile{tool: f.tool}
}

// The wire block: this hour and the 24 before it, today and the 31 days before
// it, and the keys they use (the most-used tokMaxKeys, the rest of a tool's
// folded into its "other"). Nothing to say is nil, unless a pass that is not
// whole has yet to find anything.
func tokAnswer(unix int64, hours, days map[tokSlot]tokCell, partial bool) *TokenUsage {
	hourFrom := unix/3600 - tokHours + 1
	dayFrom := unix/86400 - tokWireDays + 1
	use := map[tokKey]int64{}
	for s, c := range hours {
		if s.at >= hourFrom {
			use[s.key] += c.total() + c.n
		}
	}
	for s, c := range days {
		if s.at >= dayFrom {
			use[s.key] += c.total() + c.n
		}
	}
	keys := make([]tokKey, 0, len(use))
	for k := range use {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if use[keys[i]] != use[keys[j]] {
			return use[keys[i]] > use[keys[j]]
		}
		return keys[i].tool+"\x00"+keys[i].model < keys[j].tool+"\x00"+keys[j].model
	})
	fold := map[tokKey]tokKey{}
	if len(keys) > tokMaxKeys {
		// Room among the keys kept for an "other" for each of the two tools.
		keep := tokMaxKeys - 2
		for _, k := range keys[keep:] {
			fold[k] = tokKey{k.tool, "other"}
		}
		rest := keys[keep:]
		keys = keys[:keep]
		for _, k := range rest {
			keys = append(keys, tokKey{k.tool, "other"})
		}
	}
	idx := map[tokKey]int64{}
	u := &TokenUsage{V: tokVersion, At: unix, Partial: partial, Keys: [][2]string{}, Hours: [][9]int64{}, Days: [][9]int64{}}
	for _, k := range keys {
		if _, ok := idx[k]; ok {
			continue
		}
		idx[k] = int64(len(u.Keys))
		u.Keys = append(u.Keys, [2]string{k.tool, k.model})
	}
	rows := func(m map[tokSlot]tokCell, from int64) [][9]int64 {
		sum := map[tokSlot]tokCell{}
		for s, c := range m {
			if s.at < from {
				continue
			}
			if to, ok := fold[s.key]; ok {
				s.key = to
			}
			v := sum[s]
			v.add(c)
			sum[s] = v
		}
		out := make([][9]int64, 0, len(sum))
		for s, c := range sum {
			if c.total() == 0 && c.n == 0 {
				continue
			}
			out = append(out, [9]int64{s.at, idx[s.key], c.in, c.out, c.cr, c.cw, c.w1h, c.rs, c.n})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i][0] != out[j][0] {
				return out[i][0] < out[j][0]
			}
			return out[i][1] < out[j][1]
		})
		return out
	}
	u.Hours = rows(hours, hourFrom)
	u.Days = rows(days, dayFrom)
	if len(u.Hours) == 0 && len(u.Days) == 0 && !partial {
		return nil
	}
	return u
}

// tokens.json: the kept days and hours, one row each, [bucket, tool, model,
// in, out, cr, cw, w1h, rs, n].
type tokFileDoc struct {
	V     int     `json:"v"`
	Days  [][]any `json:"days"`
	Hours [][]any `json:"hours"`
}

func tokPath() string {
	d := configDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "tokens.json")
}

func tokLoad() (days, hours map[tokSlot]tokCell) {
	days, hours = map[tokSlot]tokCell{}, map[tokSlot]tokCell{}
	p := tokPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) > 8<<20 {
		return
	}
	var doc tokFileDoc
	if json.Unmarshal(b, &doc) != nil || doc.V != tokVersion {
		return
	}
	tokLoadRows(days, doc.Days)
	tokLoadRows(hours, doc.Hours)
	return
}

func tokLoadRows(out map[tokSlot]tokCell, rows [][]any) {
	for _, r := range rows {
		if len(r) != 10 {
			continue
		}
		tool, _ := r[1].(string)
		model, _ := r[2].(string)
		if (tool != "claude" && tool != "codex") || tokModel(model) != model {
			continue
		}
		at := asNum(r[0])
		if at == nil || *at < 0 || *at > 1e9 || *at != float64(int64(*at)) {
			continue
		}
		c := tokCell{tokCount(r[3]), tokCount(r[4]), tokCount(r[5]), tokCount(r[6]), tokCount(r[7]), tokCount(r[8]), tokCount(r[9])}
		out[tokSlot{int64(*at), tokKey{tool, model}}] = c
	}
}

func tokRows(m map[tokSlot]tokCell) [][]any {
	slots := make([]tokSlot, 0, len(m))
	for s := range m {
		slots = append(slots, s)
	}
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].at != slots[j].at {
			return slots[i].at < slots[j].at
		}
		return slots[i].key.tool+"\x00"+slots[i].key.model < slots[j].key.tool+"\x00"+slots[j].key.model
	})
	out := make([][]any, 0, len(slots))
	for _, s := range slots {
		c := m[s]
		out = append(out, []any{s.at, s.key.tool, s.key.model, c.in, c.out, c.cr, c.cw, c.w1h, c.rs, c.n})
	}
	return out
}

func tokSave(days, hours map[tokSlot]tokCell) error {
	p := tokPath()
	if p == "" {
		return errors.New("no config directory")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(tokFileDoc{V: tokVersion, Days: tokRows(days), Hours: tokRows(hours)})
	if err != nil {
		return err
	}
	return writeFileAtomic(p, b)
}
