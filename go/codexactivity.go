package main

// What Codex spent on this machine: the activity row (types.go Activity).
//
// Every model turn Codex keeps lands in its rollout as a token_count event, and
// that event's `info` carries the turn's usage (last_token_usage) beside the
// session's running total (total_token_usage) — the numbers Codex's own
// /status prints. The row says how many tokens the last 24 hours took, in the
// measure that status line leads with: input not served from cache, plus
// output (codex-rs TokenUsage::blended_total). Counting the cached prefix that
// every turn re-sends would make the figure a measure of how long the
// conversations were rather than of what they spent.
//
// A turn is the step in its session's running total, so an event written
// twice counts once; the first event read in a file, whose step from the one
// before is not in view, and a running total that went back (a new one)
// count the turn's own usage instead. Read incrementally: a rollout is
// append-only, so a file is read once back to the event before the window and
// afterwards only for what was appended, whole lines only — a line still
// being written waits for its newline. The steps kept are the ones inside the
// window, and leave as it moves.
//
// A reading short of what it needs says nothing rather than a smaller number:
// a sessions tree not wholly listed, a file in the window this would not open
// (the hard-link guard codex.go keeps, and for the same reason), or a first
// pass over a backlog bigger than one collect may read. The next collect picks
// up where this one stopped.
//
// Only on the native Codex tile (collectCodex): a machine whose Codex
// credential is CPA's sets that tile aside (mergeCPA), and a local log cannot
// say which of CPA's accounts it spent.

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"sync"
)

const (
	codexActWindow = 24 * 3600 // seconds
	codexActLabel  = "24h"
	// One file's first pass, back to the event before the window; and what
	// first passes may read in one collect, past which the rest wait for the
	// next one. A file's growth past the first is read as a first pass again.
	codexActFileBudget  = 64 << 20
	codexActRoundBudget = 128 << 20
	codexActMaxFiles    = 400
)

type codexActStep struct{ at, tokens float64 }

// One rollout's account: how far it has been read (to the end of its last
// whole line), the session's running total there, and the turns inside the
// window.
type codexActFile struct {
	size  int64
	total float64
	have  bool
	steps []codexActStep
}

var codexAct struct {
	sync.Mutex
	files map[string]*codexActFile
}

// A usage block, each field tolerated on its own as codex.go's are.
type codexUsage struct {
	Input  any `json:"input_tokens"`
	Cached any `json:"cached_input_tokens"`
	Output any `json:"output_tokens"`
}

// The tokens a usage block counts as Codex's status line does — input not
// served from cache, plus output — or -1 when it says nothing usable.
func (u *codexUsage) blended() float64 {
	if u == nil {
		return -1
	}
	in, out := asNum(u.Input), asNum(u.Output)
	if in == nil || out == nil || *in < 0 || *out < 0 {
		return -1
	}
	cached := 0.0
	if c := asNum(u.Cached); c != nil && *c > 0 {
		cached = math.Min(*c, *in)
	}
	return *in - cached + *out
}

type codexTokenLine struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type string `json:"type"`
		Info *struct {
			Total *codexUsage `json:"total_token_usage"`
			Last  *codexUsage `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

// A token_count event out of one line: when, the session's running total and
// the turn's own usage (either -1 when not stated). Any other line is not one.
func codexTokenEvent(line []byte) (at, total, last float64, ok bool) {
	if !bytes.Contains(line, []byte(`"token_count"`)) {
		return
	}
	var r codexTokenLine
	if json.Unmarshal(line, &r) != nil || r.Payload.Type != "token_count" || r.Payload.Info == nil {
		return
	}
	if at = isoToEpoch(r.Timestamp); at <= 0 {
		return
	}
	total, last = r.Payload.Info.Total.blended(), r.Payload.Info.Last.blended()
	return at, total, last, total >= 0 || last >= 0
}

// One event into the account: the step in the running total, or the turn's
// own usage where that step is not in view. Only steps inside the window are
// kept; one before it still sets the total the next step is measured from.
func (a *codexActFile) add(at, total, last, from float64) {
	step := last
	if a.have && total >= 0 && total >= a.total {
		step = total - a.total
	}
	if total >= 0 {
		a.total, a.have = total, true
	}
	if step > 0 && at >= from {
		a.steps = append(a.steps, codexActStep{at, step})
	}
}

// A file's account from nothing: back from its last whole line to the first
// event before the window, or to the file's start. Returns the bytes read.
func codexActFirst(f *os.File, size int64, from float64) (*codexActFile, int64) {
	type event struct{ at, total, last float64 }
	var evs []event // newest first
	var tail []byte
	pos, read, end := size, int64(0), int64(-1)
	for pos > 0 && read < codexActFileBudget {
		step := int64(tailChunk)
		if step > pos {
			step = pos
		}
		pos -= step
		read += step
		buf := make([]byte, step)
		if _, err := f.ReadAt(buf, pos); err != nil {
			break
		}
		tail = append(buf, tail...)
		if end < 0 {
			// What this account covers ends at the last newline: a line
			// still being written is read when it is whole.
			i := bytes.LastIndexByte(tail, '\n')
			if i < 0 {
				continue
			}
			end = pos + int64(i) + 1
			tail = tail[:i+1]
		}
		lines := splitLines(tail, pos > 0)
		before := false
		for i := len(lines) - 1; i >= 0 && !before; i-- {
			at, total, last, ok := codexTokenEvent(lines[i])
			if ok {
				evs = append(evs, event{at, total, last})
				before = at < from
			}
		}
		if before {
			break
		}
		if pos > 0 {
			if idx := bytes.IndexByte(tail, '\n'); idx >= 0 {
				tail = tail[:idx+1]
			}
		}
	}
	if end < 0 {
		// No whole line at all: a file still writing its first one is read
		// again next time; one with no newline in a whole budget is not.
		end = 0
		if pos > 0 {
			end = size
		}
	}
	a := &codexActFile{size: end}
	for i := len(evs) - 1; i >= 0; i-- {
		a.add(evs[i].at, evs[i].total, evs[i].last, from)
	}
	return a, read
}

// What was appended since: the whole lines from the account's end to the last
// newline now. False when it could not be read, or is more than a first pass
// may read — the caller starts the account again.
func (a *codexActFile) grow(f *os.File, size int64, from float64) bool {
	n := size - a.size
	if n > codexActFileBudget {
		return false
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, a.size); err != nil {
		return false
	}
	i := bytes.LastIndexByte(buf, '\n')
	if i < 0 {
		return true
	}
	for _, line := range bytes.Split(buf[:i], []byte{'\n'}) {
		if at, total, last, ok := codexTokenEvent(line); ok {
			a.add(at, total, last, from)
		}
	}
	a.size += int64(i) + 1
	return true
}

// Tokens Codex spent on this machine in the 24 hours before `at`, or nil when
// there is nothing to say: no sessions tree, nothing spent, or a reading short
// of what it needs (see the top of this file).
func codexActivity(at float64) *Activity {
	root := codexSessionsDir()
	if root == "" {
		return nil
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer r.Close()
	cands, partial := codexCandidates(root)
	from := at - codexActWindow

	codexAct.Lock()
	defer codexAct.Unlock()
	if codexAct.files == nil {
		codexAct.files = map[string]*codexActFile{}
	}
	whole := !partial
	seen := map[string]bool{}
	var spent int64
	sum := 0.0
	for i, c := range cands {
		// Newest first, and a file last written before the window holds
		// nothing inside it (with codex.go's slack: an mtime can trail).
		if float64(c.mtime.Add(mtimeSlack).Unix()) < from {
			break
		}
		if i >= codexActMaxFiles {
			whole = false
			break
		}
		seen[c.rel] = true
		a, ok := codexActRead(r, c.rel, codexAct.files[c.rel], from, &spent)
		if !ok {
			delete(codexAct.files, c.rel)
			whole = false
			continue
		}
		codexAct.files[c.rel] = a
		kept := a.steps[:0]
		for _, s := range a.steps {
			if s.at >= from {
				kept = append(kept, s)
				sum += s.tokens
			}
		}
		a.steps = kept
	}
	for rel := range codexAct.files {
		if !seen[rel] {
			delete(codexAct.files, rel)
		}
	}
	if !whole || sum <= 0 {
		return nil
	}
	return &Activity{WindowLabel: sp(codexActLabel), Tokens: fp(math.Round(sum))}
}

// One file's account brought up to date, opened through the sessions root and
// past codex.go's hard-link guard. False when it could not be: refused, gone,
// or a first pass this collect has no budget left for.
func codexActRead(r *os.Root, rel string, a *codexActFile, from float64, spent *int64) (*codexActFile, bool) {
	f, err := r.Open(rel)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	if n, known := openLinkCount(f, st); !known || n != 1 {
		return nil, false
	}
	size := st.Size()
	// A file shorter than its account was replaced, not appended to.
	if a != nil && (size < a.size || (size > a.size && !a.grow(f, size, from))) {
		a = nil
	}
	if a == nil {
		if *spent >= codexActRoundBudget {
			return nil, false
		}
		var n int64
		a, n = codexActFirst(f, size, from)
		*spent += n
	}
	return a, true
}
