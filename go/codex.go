package main

// Codex: quota for free.
//
// Codex writes the rate-limit envelope the server hands back into its own
// session logs, so the numbers a menu-bar app shows are already on disk:
//
//	~/.codex/sessions/<Y>/<M>/<D>/rollout-*.jsonl
//	  {"type":"event_msg","payload":{"type":"token_count","rate_limits":{…}}}
//
// No credential, no network.
//
// This is the FALLBACK leg since 2026-08-06, not the only one. What it cannot
// do is say anything about usage that was never persisted, and `codex exec
// --ephemeral` persists nothing: a machine doing all its Codex work that way
// leaves this reading frozen at the last interactive session while the account
// drains. codexlive.go asks the CLI's own app-server first and lands here when
// that is unavailable — which is still every machine without a Codex binary in
// a place the helper will run from, so this leg is load-bearing.

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// How far back from a file's end to hunt. These rollouts reach tens of MB
	// and the record is essentially always in the last few KB; reading whole
	// files every 30s would make the helper the heaviest thing on an idle box.
	tailBudget = 4 << 20
	tailChunk  = 64 << 10

	// Backstop on files opened per collect. Bounds the pathological case of
	// thousands of never-used sessions; when it bites the reading says so.
	maxFiles = 400

	// Slack on the mtime early exit.
	//
	// The stop rule leans on "a file's mtime is never earlier than the last
	// record inside it" — true of a well-behaved filesystem under a monotonic
	// clock, and NOT a proof. Mtimes come from a scan taken before the walk,
	// so a rollout appended to mid-walk carries a stale one; coarse timestamp
	// granularity, restored backups and clock steps break it from the other
	// side. The margin makes it a forgiving heuristic: when it's wrong the
	// cost is opening a few more files, not serving a stale number.
	mtimeSlack = 600 * time.Second
)

// Why a candidate yielded nothing — which is not the same question as whether
// it holds a reading.
//
// A file we searched and found nothing in supports "no reading here". A file
// we refused before opening supports nothing at all, and reporting the two the
// same way is how a machine spent a day saying "no quota recorded yet" while
// every session file on it sat there holding one. Counting them separately is
// what lets the card name the actual reason.
type scanRefusal int

const (
	scanSearched  scanRefusal = iota // opened and read to the end of the budget
	scanManyNames                    // VERIFIED to have more than one name
	// Never looked at: could not be opened, is not a regular file, or the
	// platform would not say how many names it has. All three refuse the read
	// — the guard is fail-closed and stays that way — but none of them is
	// evidence that anything hard-linked anything, so they must not be told to
	// the reader as if they were.
	scanUnusable
)

// Everything that can vary is decoded as `any` and converted by hand.
//
// Strong types are the wrong tool against a file written by another program:
// one unrelated field arriving as a string — a window_minutes that became
// "10080", a balance that became a number — fails the whole line, and a line
// carrying a perfectly good used_percent gets thrown away over a field we do
// not even read. The Python reference tolerates each field independently, and
// so does this. Fields we never use are simply not declared.
type rateLimitWindow struct {
	UsedPercent   any `json:"used_percent"`
	WindowMinutes any `json:"window_minutes"`
	ResetsAt      any `json:"resets_at"`
}

type rollutRecord struct {
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type       string `json:"type"`
		RateLimits *struct {
			Primary   *rateLimitWindow `json:"primary"`
			Secondary *rateLimitWindow `json:"secondary"`
			PlanType  any              `json:"plan_type"`
			Credits   *struct {
				HasCredits any `json:"has_credits"`
				Unlimited  any `json:"unlimited"`
				Balance    any `json:"balance"`
			} `json:"credits"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

// A finite float out of an arbitrary JSON value, or nil.
func asNum(v any) *float64 {
	f, ok := v.(float64)
	if !ok || !finite(f) {
		return nil
	}
	return &f
}

func asStr(v any, limit int) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}

func asBool(v any) bool { b, _ := v.(bool); return b }

func codexSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

type candidate struct {
	rel   string // path relative to the sessions root
	mtime time.Time
}

// Every rollout, newest first.
//
// Deliberately uncapped. An earlier version took the twelve most recent files,
// which is a correctness bug wearing an optimisation's clothes: twelve sessions
// opened but never used carry no rate_limits at all and would hide the real
// reading in the thirteenth. The walk is bounded on mtime instead, which is
// exact enough with the slack above and usually stops at the first file.
func codexCandidates(root string) (out []candidate, partial bool) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory means the list is incomplete, and the
			// caller must not turn an incomplete list into a confident "no
			// readings exist".
			partial = true
			return nil
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			partial = true // vanished or unreadable between walk and stat
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			partial = true
			return nil
		}
		out = append(out, candidate{rel, info.ModTime()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].mtime.After(out[j].mtime) })
	return out, partial
}

// Newest rate_limits record in one rollout, reading backwards from the end.
//
// The file is opened through os.Root, which confines every operation to the
// sessions tree: a symlinked component — including a PARENT directory swapped
// after the path was chosen — cannot walk this read out of the tree.
//
// Worth being precise, because an earlier version of this comment was not:
// os.Root is NOT the same guarantee as the Python's per-component O_NOFOLLOW.
// Root refuses to ESCAPE, but it will happily follow a symlink that stays
// inside the tree; the Python refused symlinked components outright. The
// security property we actually need — nothing outside ~/.codex/sessions can
// be read — holds either way, and Root gets it from the standard library
// rather than by hand. But "stricter" would be the wrong word for it.
//
// st_nlink is a separate concern Root does not cover at all: a hard link needs
// no race and no symlink, because it is a genuine name inside the tree
// pointing at somebody else's inode — auth.json, say.
//
// `truncated` means "the scan ran out of budget without finding anything", so
// a caller must not read a negative as final. Deliberately NOT set when a
// record was found: these logs are append-only, so everything further back is
// older, and there is no newer reading left to miss. (The Python reads to the
// budget regardless and can report truncated alongside a hit — same numbers,
// but it spends 4 MiB of reads to say so.)
func codexScanFile(root *os.Root, rel string) (rec *rollutRecord, truncated bool, why scanRefusal) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, false, scanUnusable
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, false, scanUnusable
	}
	// Refuse when the link count cannot be established, not just when it is
	// wrong: `count != 1` alone was fail-open, since a platform that cannot
	// answer would have switched the guard off silently. But the two refusals
	// are told apart — only a count we actually obtained can be reported to
	// the reader as a second name.
	// Zero is not "many": it is a file unlinked while this held it open, which
	// is a race with a cleanup, not a planted name.
	if n, known := openLinkCount(f, st); !known || n == 0 {
		return nil, false, scanUnusable
	} else if n != 1 {
		return nil, false, scanManyNames
	}

	size := st.Size()
	var tail []byte
	pos := size
	read := int64(0)
	for pos > 0 {
		if read >= tailBudget {
			// Stopped with a partial line unresolved, so anything earlier is
			// unread. Reporting "no rate_limits here" would state a negative
			// this never established.
			return nil, true, scanSearched
		}
		step := int64(tailChunk)
		if step > pos {
			step = pos
		}
		pos -= step
		read += step
		buf := make([]byte, step)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return nil, true, scanSearched
		}
		tail = append(buf, tail...)

		lines := splitLines(tail, pos > 0)
		for i := len(lines) - 1; i >= 0; i-- {
			var r rollutRecord
			if json.Unmarshal(lines[i], &r) != nil {
				continue
			}
			if r.Payload.Type == "token_count" && r.Payload.RateLimits != nil {
				return &r, false, scanSearched
			}
		}
		if pos > 0 {
			// First element may be a partial line whose head is further back.
			if idx := bytes.IndexByte(tail, '\n'); idx >= 0 {
				tail = tail[:idx+1]
			}
		}
	}
	return nil, false, scanSearched
}

// Complete lines out of the buffer. When more of the file remains to the left,
// the first line is partial and is dropped — its head has not been read yet.
func splitLines(buf []byte, dropFirst bool) [][]byte {
	parts := bytes.Split(buf, []byte{'\n'})
	if dropFirst && len(parts) > 0 {
		parts = parts[1:]
	}
	out := parts[:0]
	for _, p := range parts {
		if len(bytes.TrimSpace(p)) > 0 {
			out = append(out, p)
		}
	}
	return out
}

func collectCodexLog() Provider {
	p := Provider{ID: "codex", Name: "Codex", Source: "local-log", CapturedAt: now()}
	root := codexSessionsDir()
	if root == "" {
		p.failWith("no-sessions", "no-home")
		return p
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		p.failWith("no-sessions", "no-session-dir")
		return p
	}

	r, err := os.OpenRoot(root)
	if err != nil {
		p.failWith("no-sessions", "session-dir-closed")
		return p
	}
	defer r.Close()

	var best *rollutRecord
	var bestEpoch float64
	var scanned, refusedNames int
	var truncated, capped bool
	var sourceFile string

	cands, partial := codexCandidates(root)
	if partial {
		// A directory we could not read means "no readings" would be a claim
		// we never established.
		truncated = true
	}
	for _, c := range cands {
		// Stop once the reading in hand is comfortably newer than the next
		// file could be. Heuristic with a margin, not the exact bound an
		// earlier version claimed.
		if best != nil && bestEpoch >= float64(c.mtime.Add(mtimeSlack).Unix()) {
			break
		}
		if scanned >= maxFiles {
			capped = true
			break
		}
		scanned++
		rec, trunc, why := codexScanFile(r, c.rel)
		if trunc {
			truncated = true
		}
		if why != scanSearched {
			// Never looked at, so whatever is inside it is still unknown — the
			// same standing as a scan that ran out of budget, and the reason
			// "nothing newer exists" has to be hedged rather than claimed.
			truncated = true
			if why == scanManyNames {
				refusedNames++
			}
		}
		if rec == nil {
			continue
		}
		e := isoToEpoch(rec.Timestamp)
		if best == nil || e > bestEpoch {
			best, bestEpoch, sourceFile = rec, e, filepath.Base(c.rel)
		}
	}

	p.Truncated, p.Capped = truncated, capped
	if best == nil {
		switch {
		// Every file this OPENED was refused for having more than one name, so
		// nothing here is a statement about quota at all. Named on its own
		// because the cause is somewhere else entirely — a backup, a
		// de-duplicator or a test fixture that hard-linked the session tree —
		// and "the scan was cut short" sends whoever reads it looking in the
		// wrong place. Cost a day of hunting once.
		//
		// The count is what this LOOKED AT, and the sentence says exactly that:
		// the real case had 557 files and a 400-file backstop, so "all of them"
		// would have been a claim about 157 files nobody opened. Capped and
		// truncated still ride along on the reading for the same reason.
		case scanned > 0 && refusedNames == scanned:
			p.failWith("no-readings", "sessions-linked", itoa(refusedNames))
		case truncated || capped:
			p.failWith("no-readings", "scan-cut")
		default:
			p.failWith("no-readings", "scan-none")
		}
		return p
	}

	rl := best.Payload.RateLimits
	for _, w := range []struct {
		key string
		win *rateLimitWindow
	}{{"primary", rl.Primary}, {"secondary", rl.Secondary}} {
		if w.win == nil {
			continue
		}
		used := asNum(w.win.UsedPercent)
		if used == nil {
			continue
		}
		l := Limit{
			Key:         w.key,
			UsedPercent: round2(*used),
			// Codex reports no severity; the page colours these by percentage.
			Severity: nil, Scope: nil, Active: nil,
		}
		if m := asNum(w.win.WindowMinutes); m != nil {
			l.WindowMinutes = m
			l.WindowLabel = windowLabel(*m)
		}
		l.ResetsAt = asNum(w.win.ResetsAt)
		p.Limits = append(p.Limits, l)
	}
	if len(p.Limits) == 0 {
		// A rate_limits object with no usable window is not a reading. Saying
		// ok here would leave the page showing its previous gauge, freshly
		// stamped live — worse than showing nothing.
		p.failWith("no-readings", "latest-no-window")
		return p
	}

	p.OK = true
	p.RecordedAt = fp(bestEpoch)
	p.SourceFile = sourceFile
	if plan := asStr(rl.PlanType, 40); plan != "" {
		p.PlanType = sp(plan)
	}
	if c := rl.Credits; c != nil {
		p.Credits = &Credits{
			HasCredits: asBool(c.HasCredits),
			Unlimited:  asBool(c.Unlimited),
			Balance:    anyToString(c.Balance),
		}
	}
	return p
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// 10080 -> "7d". Codex expresses windows in minutes; humans do not.
func windowLabel(minutes float64) *string {
	if !finite(minutes) || minutes <= 0 {
		return nil
	}
	m := int64(minutes)
	switch {
	case m%1440 == 0:
		return sp(strconv.FormatInt(m/1440, 10) + "d")
	case m%60 == 0:
		return sp(strconv.FormatInt(m/60, 10) + "h")
	default:
		return sp(strconv.FormatInt(m, 10) + "m")
	}
}
