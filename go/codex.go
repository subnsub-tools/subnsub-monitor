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
	"syscall"
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
func codexScanFile(root *os.Root, rel string) (rec *rollutRecord, truncated bool) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	// Refuse when the link count cannot be established, not just when it is
	// wrong. `ok && Nlink != 1` was fail-open: on any platform where the type
	// assertion misses, the hard-link guard silently switched itself off.
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Nlink != 1 {
		return nil, false // hard link, or a link count we cannot verify
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
			return nil, true
		}
		step := int64(tailChunk)
		if step > pos {
			step = pos
		}
		pos -= step
		read += step
		buf := make([]byte, step)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return nil, false
		}
		tail = append(buf, tail...)

		lines := splitLines(tail, pos > 0)
		for i := len(lines) - 1; i >= 0; i-- {
			var r rollutRecord
			if json.Unmarshal(lines[i], &r) != nil {
				continue
			}
			if r.Payload.Type == "token_count" && r.Payload.RateLimits != nil {
				return &r, false
			}
		}
		if pos > 0 {
			// First element may be a partial line whose head is further back.
			if idx := bytes.IndexByte(tail, '\n'); idx >= 0 {
				tail = tail[:idx+1]
			}
		}
	}
	return nil, false
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

func collectCodex() Provider {
	p := Provider{ID: "codex", Name: "Codex", Source: "local-log", CapturedAt: now()}
	root := codexSessionsDir()
	if root == "" {
		p.Error, p.Detail = "no-sessions", "找不到 home 目录"
		return p
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		p.Error = "no-sessions"
		p.Detail = "~/.codex/sessions 不存在——Codex CLI 装了并登录过吗？"
		return p
	}

	r, err := os.OpenRoot(root)
	if err != nil {
		p.Error, p.Detail = "no-sessions", "无法打开 sessions 目录"
		return p
	}
	defer r.Close()

	var best *rollutRecord
	var bestEpoch float64
	var scanned int
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
		rec, trunc := codexScanFile(r, c.rel)
		if trunc {
			truncated = true
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
		p.Error = "no-readings"
		p.Detail = "扫描了若干 session 文件，还没有额度记录。"
		if truncated || capped {
			p.Detail = "扫描被截断，这不是一个确定的否定结论。"
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
		p.Error, p.Detail = "no-readings", "最新记录里没有可用的额度窗口。"
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

