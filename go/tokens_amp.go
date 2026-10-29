package main

// Amp's token counts (operator request 2026-10-29: add Antigravity and Amp the
// way Tokscale does), the way Tokscale (github.com/junhoyeo/tokscale, MIT) gets them:
// Amp keeps its threads on its own servers and stopped writing them to the
// disk in 2026-04, so they are asked of Amp's own CLI — `amp threads list
// --json --include-archived`, page by page, then `amp threads export <id>` for
// each thread whose `updated` moved since it was last read. The legacy thread
// files Amp wrote before that (~/.local/share/amp/threads/T-*.json, the same
// shape) are read off the disk for a thread the CLI does not name.
//
// The threads are the account's, every machine's it was used on, so a
// helper counts only the threads that ran on its machine — those whose
// env.initial names this machine's Amp installation (device-id.json) or its
// hostname — or two machines signed in to one account would each count the
// other's (review 2026-10-29). Another machine's thread is exported once to
// learn that, and not again.
//
// A thread's every message with a usage counts — the main agent's replies —
// and, which Tokscale leaves out, every inference its subagents (finder,
// oracle) ran, under the tool results' run.~debug.inferences[].usage. The
// input not served from cache is the input total less what the cache served
// and wrote: Claude's inputTokens leaves the cache out and OpenAI's counts it
// in, and totalInputTokens is that sum either way. Amp states no reasoning.
//
// Only counts are kept — by thread, hour and model — in amp-usage.json beside
// the token, so a restarted helper does not export a month of threads again.
// The CLI runs off the collect, in the background, at most once every
// ampTokEvery; a pass reads what the last run found.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ampTokEvery   = 600.0            // seconds between runs of the CLI
	ampTokPage    = 100              // threads a list page asks for
	ampTokPages   = 20               // list pages a run reads at most
	ampTokExports = 24               // threads a run exports at most
	ampTokRunMax  = 3 * time.Minute  // what one run may take in all
	ampTokCallMax = 60 * time.Second // one CLI call
	ampTokOutMax  = 96 << 20         // one export's JSON
	ampTokListMax = 4 << 20          // one list page's JSON
	ampTokFileMax = 64 << 20         // one legacy thread file
	ampTokThreads = 4000             // threads kept
	ampTokRowsMax = 2000             // a thread's hour × model rows
)

// A thread's counts: by hour and model.
type ampTokRow struct {
	hour  int64
	model string
	c     tokCell
}

type ampTokThread struct {
	updated string // as the list says it; "" for a legacy file's
	foreign bool   // run on another machine: counted there, not here
	rows    []ampTokRow
}

var ampTok struct {
	sync.Mutex
	threads  map[string]*ampTokThread // by thread id, from the CLI
	loaded   bool
	dirty    bool
	ranAt    float64
	complete bool // the last run listed and exported all it had to
	legacy   map[string]*ampTokLegacy
}

type ampTokLegacy struct {
	info os.FileInfo
	rows []ampTokRow
}

var ampTokBusy atomic.Bool

// The CLI as a pass may run it; a test puts its own in.
var ampTokRun = func(bin string, max int, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ampTokCallMax)
	defer cancel()
	cmd := toolCommand(ctx, bin, args...)
	cmd.Stdin = nil
	cmd.Env = ampEnv()
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	lo := &limitedWriter{w: &out, n: max}
	cmd.Stdout, cmd.Stderr = lo, &limitedWriter{w: &errb, n: 4 << 10}
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, errAmpTimeout
	}
	if lo.over {
		return nil, &ampErr{"output too large"}
	}
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var ampTokBinary = ampBinary

// One usage block as Amp writes it.
type ampTokUsage struct {
	Model     string `json:"model"`
	Timestamp string `json:"timestamp"`
	Input     any    `json:"inputTokens"`
	Output    any    `json:"outputTokens"`
	CR        any    `json:"cacheReadInputTokens"`
	CW        any    `json:"cacheCreationInputTokens"`
	Total     any    `json:"totalInputTokens"`
}

func (u *ampTokUsage) cell() tokCell {
	cr, cw := tokCount(u.CR), tokCount(u.CW)
	in := tokCount(u.Input)
	if tokHas(u.Total) {
		in = max(tokCount(u.Total)-cr-cw, 0)
	}
	return tokCell{in: in, out: tokCount(u.Output), cr: cr, cw: cw, n: 1}
}

type ampTokThreadDoc struct {
	ID      string `json:"id"`
	Created any    `json:"created"`
	Env     *struct {
		Initial *struct {
			Hostname string `json:"hostname"`
			Platform *struct {
				InstallationID string `json:"installationID"`
			} `json:"platform"`
		} `json:"initial"`
	} `json:"env"`
	Messages []struct {
		Role      string          `json:"role"`
		MessageID any             `json:"messageId"`
		Usage     *ampTokUsage    `json:"usage"`
		Content   json.RawMessage `json:"content"`
	} `json:"messages"`
}

type ampTokBlock struct {
	Type string `json:"type"`
	Run  *struct {
		Debug *struct {
			Inferences []struct {
				Usage *ampTokUsage `json:"usage"`
			} `json:"inferences"`
		} `json:"~debug"`
	} `json:"run"`
}

// Where a thread ran: the Amp installation and the hostname it began on.
type ampTokWhere struct{ inst, host string }

// This machine as Amp names it: its installation (Amp's device-id.json under
// its data directory) and its hostname.
func ampTokSelf() ampTokWhere {
	var w ampTokWhere
	w.host, _ = os.Hostname()
	var dirs []string
	if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		dirs = append(dirs, filepath.Join(x, "amp"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "amp"))
	}
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "device-id.json"))
		if err != nil || len(b) > 4<<10 {
			continue
		}
		var doc struct {
			InstallationID string `json:"installationID"`
		}
		if json.Unmarshal(b, &doc) == nil && strings.TrimSpace(doc.InstallationID) != "" {
			w.inst = strings.TrimSpace(doc.InstallationID)
			break
		}
	}
	return w
}

// Whether a thread that began at `t` ran on the machine `self`.
func (self ampTokWhere) has(t ampTokWhere) bool {
	return self.inst != "" && t.inst == self.inst || self.host != "" && strings.EqualFold(t.host, self.host)
}

// A thread's counts out of its JSON, by hour and model, and where it ran; ok
// false when it is not a thread, or not the one asked for (id "" asks for
// none in particular).
func ampTokParse(b []byte, id string) ([]ampTokRow, ampTokWhere, bool) {
	var d ampTokThreadDoc
	var where ampTokWhere
	if json.Unmarshal(b, &d) != nil || d.ID == "" || (id != "" && d.ID != id) {
		return nil, where, false
	}
	if d.Env != nil && d.Env.Initial != nil {
		where.host = d.Env.Initial.Hostname
		if d.Env.Initial.Platform != nil {
			where.inst = d.Env.Initial.Platform.InstallationID
		}
	}
	created := 0.0
	if n := asNum(d.Created); n != nil && *n > 0 {
		created = *n / 1000
	}
	sum := map[tokSlot]tokCell{}
	add := func(at float64, u *ampTokUsage) {
		c := u.cell()
		if at <= 0 || c.total() == 0 {
			return
		}
		s := tokSlot{int64(at) / 3600, tokKey{"amp", tokModel(u.Model)}}
		v := sum[s]
		v.add(c)
		sum[s] = v
	}
	last := created
	for _, m := range d.Messages {
		if m.Usage != nil && m.Role == "assistant" && strings.TrimSpace(m.Usage.Model) != "" {
			at := isoToEpoch(m.Usage.Timestamp)
			if at <= 0 && created > 0 {
				if n := asNum(m.MessageID); n != nil && *n >= 0 {
					at = created + *n
				}
			}
			if at > 0 {
				last = at
			}
			add(at, m.Usage)
		}
		// The subagents' inferences ride in the tool results, timed by the
		// reply that called them when they carry no time of their own.
		if len(m.Content) == 0 || m.Content[0] != '[' || !bytes.Contains(m.Content, []byte(`"inferences"`)) {
			continue
		}
		var blocks []ampTokBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, bl := range blocks {
			if bl.Type != "tool_result" || bl.Run == nil || bl.Run.Debug == nil {
				continue
			}
			for _, inf := range bl.Run.Debug.Inferences {
				if inf.Usage == nil || strings.TrimSpace(inf.Usage.Model) == "" {
					continue
				}
				at := isoToEpoch(inf.Usage.Timestamp)
				if at <= 0 {
					at = last
				}
				add(at, inf.Usage)
			}
		}
	}
	rows := make([]ampTokRow, 0, len(sum))
	for s, c := range sum {
		rows = append(rows, ampTokRow{s.at, s.key.model, c})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].hour != rows[j].hour {
			return rows[i].hour > rows[j].hour
		}
		return rows[i].model < rows[j].model
	})
	if len(rows) > ampTokRowsMax {
		rows = rows[:ampTokRowsMax]
	}
	return rows, where, true
}

// The threads' accounts for a pass: each thread the CLI named, then each
// legacy file for a thread it did not, as accounts of their own; and the CLI
// set going again when its last run is old enough. Says whether what is
// known is whole.
func tokAmpPass(st *tokFiles, seen map[string]bool, hourFrom, dayFrom int64, from time.Time) bool {
	ampTok.Lock()
	if !ampTok.loaded {
		ampTok.loaded = true
		ampTok.threads = ampTokLoad()
	}
	threads := make(map[string]*ampTokThread, len(ampTok.threads))
	for id, t := range ampTok.threads {
		threads[id] = t
	}
	complete := ampTok.complete
	due := now()-ampTok.ranAt >= ampTokEvery
	ampTok.Unlock()

	bin := ""
	if due {
		bin = ampTokBinary()
	}
	if bin != "" && ampTokBusy.CompareAndSwap(false, true) {
		ampTok.Lock()
		ampTok.ranAt = now()
		ampTok.Unlock()
		go func() {
			defer ampTokBusy.Store(false)
			defer func() { recover() }()
			ampTokSync(bin, from)
		}()
	}
	if bin == "" && due {
		complete = true // no CLI here: nothing more to ask
	}

	whole := complete
	for id, t := range threads {
		if t.foreign {
			continue
		}
		key := "amp\x00" + id
		seen[key] = true
		f := (*st)[key]
		if f == nil || f.ampUpdated != t.updated || f.ampRows != len(t.rows) {
			f = &tokFile{tool: "amp", ampUpdated: t.updated, ampRows: len(t.rows)}
			for _, r := range t.rows {
				f.put(float64(r.hour*3600), tokKey{"amp", r.model}, r.c, hourFrom, dayFrom)
			}
			(*st)[key] = f
		}
	}
	if !tokAmpLegacy(st, seen, threads, hourFrom, dayFrom, from) {
		whole = false
	}
	return whole
}

type tokFiles = map[string]*tokFile

// The legacy thread files: a file read again whenever it changes, since Amp
// wrote a thread whole each time.
func tokAmpLegacy(st *tokFiles, seen map[string]bool, cli map[string]*ampTokThread, hourFrom, dayFrom int64, from time.Time) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return true
	}
	dir := filepath.Join(home, ".local", "share", "amp", "threads")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer r.Close()
	whole := true
	ampTok.Lock()
	defer ampTok.Unlock()
	if ampTok.legacy == nil {
		ampTok.legacy = map[string]*ampTokLegacy{}
	}
	here := map[string]bool{}
	defer func() {
		for id := range ampTok.legacy {
			if !here[id] {
				delete(ampTok.legacy, id)
			}
		}
	}()
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "T-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if _, ok := cli[id]; ok {
			continue // the CLI's copy is the one counted
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(from) || info.Size() > ampTokFileMax {
			continue
		}
		key := "amp\x00" + id
		seen[key] = true
		here[id] = true
		l := ampTok.legacy[id]
		if l == nil || !os.SameFile(l.info, info) || l.info.ModTime() != info.ModTime() || l.info.Size() != info.Size() {
			fh, err := r.Open(name)
			if err != nil {
				whole = false
				continue
			}
			fst, err := fh.Stat()
			if n, known := openLinkCount(fh, fst); err != nil || !known || n != 1 {
				fh.Close()
				continue
			}
			b, err := readAllMax(fh, ampTokFileMax)
			fh.Close()
			if err != nil {
				whole = false
				continue
			}
			rows, _, ok := ampTokParse(b, id)
			if !ok {
				continue
			}
			l = &ampTokLegacy{info: info, rows: rows}
			ampTok.legacy[id] = l
			delete(*st, key)
		}
		if (*st)[key] == nil {
			f := &tokFile{tool: "amp"}
			for _, row := range l.rows {
				f.put(float64(row.hour*3600), tokKey{"amp", row.model}, row.c, hourFrom, dayFrom)
			}
			(*st)[key] = f
		}
	}
	return whole
}

func readAllMax(f *os.File, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, &ampErr{"file too large"}
	}
	return b, nil
}

// One run of the CLI: the threads listed, newest first, down to the window's
// start, and each one whose `updated` moved exported and counted.
func ampTokSync(bin string, from time.Time) {
	deadline := time.Now().Add(ampTokRunMax)
	type listed struct {
		ID      string `json:"id"`
		Updated string `json:"updated"`
	}
	var all []listed
	listedAll := false
	for page := 0; page < ampTokPages && time.Now().Before(deadline); page++ {
		out, err := ampTokRun(bin, ampTokListMax, "threads", "list", "--json", "--include-archived",
			"--limit", strconv.Itoa(ampTokPage), "--offset", strconv.Itoa(page*ampTokPage))
		var got []listed
		if err != nil || json.Unmarshal(out, &got) != nil {
			// Signed out, offline, or a CLI without the command: nothing to
			// count until a later run says otherwise.
			if page == 0 {
				ampTok.Lock()
				ampTok.complete = true
				ampTok.Unlock()
			}
			return
		}
		all = append(all, got...)
		old := len(got) > 0 && isoToEpoch(got[len(got)-1].Updated) < float64(from.Unix())
		if len(got) < ampTokPage || old {
			listedAll = true
			break
		}
	}
	ampTok.Lock()
	known := make(map[string]string, len(ampTok.threads))
	foreign := map[string]bool{}
	for id, t := range ampTok.threads {
		known[id] = t.updated
		foreign[id] = t.foreign
	}
	ampTok.Unlock()

	var want []listed
	for _, t := range all {
		if !ampTokID(t.ID) || isoToEpoch(t.Updated) < float64(from.Unix()) {
			continue
		}
		// Another machine's thread stays another machine's.
		if known[t.ID] != t.Updated && !foreign[t.ID] {
			want = append(want, t)
		}
	}
	self := ampTokSelf()
	done := 0
	for _, t := range want {
		if done >= ampTokExports || time.Now().After(deadline) {
			break
		}
		out, err := ampTokRun(bin, ampTokOutMax, "threads", "export", t.ID)
		if err != nil {
			continue
		}
		rows, where, ok := ampTokParse(out, t.ID)
		if !ok {
			continue
		}
		done++
		th := &ampTokThread{updated: t.Updated, rows: rows}
		if !self.has(where) {
			th = &ampTokThread{updated: t.Updated, foreign: true}
		}
		ampTok.Lock()
		if ampTok.threads == nil {
			ampTok.threads = map[string]*ampTokThread{}
		}
		if len(ampTok.threads) < ampTokThreads || ampTok.threads[t.ID] != nil {
			ampTok.threads[t.ID] = th
			ampTok.dirty = true
		}
		ampTok.Unlock()
	}
	ampTok.Lock()
	// A thread goes once its last word is past the window. One gone from the
	// listing (deleted) stays until then: what it spent was spent, and the
	// kept buckets, which keep the larger of two counts, would otherwise
	// lose the next thread's usage in its hours (review 2026-10-29).
	for id, t := range ampTok.threads {
		if isoToEpoch(t.updated) < float64(from.Unix()) {
			delete(ampTok.threads, id)
			ampTok.dirty = true
		}
	}
	ampTok.complete = listedAll && done == len(want)
	if ampTok.dirty {
		if ampTokSave(ampTok.threads) == nil {
			ampTok.dirty = false
		}
	}
	ampTok.Unlock()
}

// Amp's thread ids: T- and a UUID's characters.
func ampTokID(id string) bool {
	if len(id) < 3 || len(id) > 64 || !strings.HasPrefix(id, "T-") {
		return false
	}
	for _, c := range id[2:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// amp-usage.json: { "v": 1, "threads": { id: { "u": updated, "r": [[hour,
// model, in, out, cr, cw, n], ...] } } }.
func ampTokPath() string {
	d := configDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, "amp-usage.json")
}

type ampTokDoc struct {
	V       int                       `json:"v"`
	Threads map[string]ampTokThreadKV `json:"threads"`
}

type ampTokThreadKV struct {
	U string  `json:"u"`
	F bool    `json:"f,omitempty"` // another machine's
	R [][]any `json:"r"`
}

func ampTokLoad() map[string]*ampTokThread {
	out := map[string]*ampTokThread{}
	p := ampTokPath()
	if p == "" {
		return out
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) > 32<<20 {
		return out
	}
	var doc ampTokDoc
	if json.Unmarshal(b, &doc) != nil || doc.V != 1 {
		return out
	}
	for id, t := range doc.Threads {
		if !ampTokID(id) || len(out) >= ampTokThreads {
			continue
		}
		th := &ampTokThread{updated: t.U, foreign: t.F}
		for _, r := range t.R {
			if len(r) != 7 || len(th.rows) >= ampTokRowsMax {
				continue
			}
			h := asNum(r[0])
			model, _ := r[1].(string)
			if h == nil || *h < 0 || *h > 1e7 || *h != float64(int64(*h)) || tokModel(model) != model {
				continue
			}
			th.rows = append(th.rows, ampTokRow{int64(*h), model,
				tokCell{in: tokCount(r[2]), out: tokCount(r[3]), cr: tokCount(r[4]), cw: tokCount(r[5]), n: tokCount(r[6])}})
		}
		out[id] = th
	}
	return out
}

func ampTokSave(threads map[string]*ampTokThread) error {
	p := ampTokPath()
	if p == "" {
		return os.ErrNotExist
	}
	doc := ampTokDoc{V: 1, Threads: map[string]ampTokThreadKV{}}
	for id, t := range threads {
		kv := ampTokThreadKV{U: t.updated, F: t.foreign, R: [][]any{}}
		for _, r := range t.rows {
			kv.R = append(kv.R, []any{r.hour, r.model, r.c.in, r.c.out, r.c.cr, r.c.cw, r.c.n})
		}
		doc.Threads[id] = kv
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p, b)
}
