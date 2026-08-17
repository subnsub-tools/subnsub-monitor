package main

// Searching THIS machine's coding-agent sessions, when the dashboard's fleet
// search asks over the console channel — `subnsub-monitor sessions search`.
//
// The whole feature exists because of one ruling: a machine that granted the
// console has already granted arbitrary /bin/sh as this user, so the fleet
// search rides that one channel and nothing more sensitive is exposed by it.
// What leaves the machine is the snippets this prints — never a transcript.
//
// An earlier build of this file installed a third-party indexer instead, and
// the audit of that tool is why this is now a few hundred lines of our own:
// the indexer answered one narrow question — "which recent sessions mention
// this text" — by forking a resident daemon, rebuilding a plaintext copy of
// every transcript into its own database, and phoning two hosts by default.
// This file answers the same question by READING THE TRANSCRIPTS WHERE THEY
// ALREADY ARE. No index is built, no daemon starts, nothing is downloaded,
// no file is written, and the only network anywhere near this path is the
// console channel that carried the question in.
//
// The shape of the answer is the one the dashboard already renders: a JSON
// object with a `matches` list of {project, agent, timestamp, session_id,
// snippet}. Holding that shape is what let the indexer be swapped out without
// the far end noticing.
//
// Reading discipline is codex.go's, because the trees are the same kind of
// place: every open goes through os.Root so a symlinked component cannot walk
// the read out of the agent's own directory, and a file with more than one
// hard link is refused rather than read — a genuine second name inside the
// tree can point at somebody else's inode. Secrets that appear in matched
// text are best-effort redacted from the snippet before it is printed; the
// transcript itself is not touched.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// How many hits a search returns when the caller does not say. The
	// dashboard asks for 12 — about 3 KB of a 16 KB console pipe — and the cap
	// exists so a caller cannot ask this process to serialise its whole
	// history into one console frame.
	sessDefaultLimit = 12
	sessMaxLimit     = 50

	// Hits kept per session, newest last. One chatty session must not spend
	// the whole limit before the file after it is even opened.
	sessPerSession = 3

	// The whole search runs inside this. The console gives a command 30
	// seconds; stopping well short of that turns "the channel killed it and
	// reported nothing" into "an answer came back marked partial", which is
	// the difference between a timeout and a result.
	sessBudget = 15 * time.Second

	// The longest transcript line the scanner will hold. Lines beyond it are
	// real — a pasted image becomes megabytes of base64 on one line — but a
	// line this large is not something a snippet can usefully quote, and the
	// file it aborts is reported as partial rather than silently complete.
	sessMaxLineBytes = 8 << 20

	// Snippet length, in runes — what one hit contributes to the 16 KB pipe.
	sessSnippetRunes = 200

	// The ENCODED answer's ceiling, under the console's 16 KiB output cap
	// with room to spare. Runes are not bytes — 200 runes of CJK is 600
	// bytes, and encoding/json turns `<` into six — so the only honest
	// budget is on the marshalled document itself (review 2026-08-18). A
	// document over this drops its newest-last hits until it fits, marked
	// partial, rather than letting the channel cut it mid-JSON into
	// something the dashboard cannot read at all.
	sessMaxJSONBytes = 15 << 10

	// How deep sessCollectText follows nested structure before deciding the
	// line is not a transcript message. Real messages sit three or four
	// levels down; forty levels is a crafted file, not a conversation.
	sessMaxDepth = 12
)

// One agent tool whose transcripts this machine searches. The root is the
// tree the walk is confined to; match picks the file names that are
// transcripts rather than the tool's other droppings.
type sessSource struct {
	agent string
	root  string
	match func(name string) bool
}

// The tools searched, in the order their hits should tie-break. Local
// transcript trees only — a tool that is not installed simply has no
// directory, and that is a normal answer, not an error.
func sessSources() []sessSource {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []sessSource{
		{
			agent: "claude",
			root:  filepath.Join(home, ".claude", "projects"),
			match: func(n string) bool { return strings.HasSuffix(n, ".jsonl") },
		},
		{
			agent: "codex",
			root:  codexSessionsDir(),
			match: func(n string) bool {
				return strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, ".jsonl")
			},
		},
	}
}

// One hit, in exactly the field names the dashboard renders. `agent` is the
// tool, `project` the working directory's base name when the transcript
// recorded one, `session_id` something short enough to show and stable enough
// to look up by hand.
type sessHit struct {
	Project   string `json:"project"`
	Agent     string `json:"agent"`
	Timestamp string `json:"timestamp"`
	SessionID string `json:"session_id"`
	Snippet   string `json:"snippet"`
}

// The whole answer. Matches is initialised, never nil: the far end asks
// Array.isArray, and `"matches":null` would read as the tool having failed
// rather than having found nothing.
type sessReport struct {
	Matches []sessHit `json:"matches"`
	Files   int       `json:"files"`
	Scanned int       `json:"scanned"`
	Ms      int64     `json:"ms"`
	Partial bool      `json:"partial,omitempty"`
}

// What a search was asked to do. Terms arrive lowercased and non-empty; a hit
// is a single transcript line whose extracted text contains every one of
// them. The deadline is absolute so tests can put it in the past.
type sessQuery struct {
	terms    []string
	limit    int
	deadline time.Time
}

// The CLI entry: `sessions search [--limit N] [--json] [--] TEXT...`.
// Exit code discipline is load-bearing for the dashboard: a search that RAN
// exits 0 whatever it found, including nothing, so the far end can read any
// non-zero exit as "this helper does not know how to search" (an older build
// prints its usage and exits 2) and say "update the helper" instead of
// guessing. Only a caller error — no query, an unknown flag — exits non-zero
// here, and the dashboard never sends one.
func runSessionsSearchCLI(rest []string) int {
	limit := sessDefaultLimit
	asJSON := false
	var parts []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			parts = append(parts, rest[i+1:]...)
			i = len(rest)
		case a == "--json":
			asJSON = true
		case a == "--limit":
			if i+1 >= len(rest) {
				warnf("--limit needs a number")
				return 2
			}
			i++
			n, err := parsePositive(rest[i])
			if err != nil {
				warnf("--limit needs a number, not %q", rest[i])
				return 2
			}
			limit = n
		case strings.HasPrefix(a, "--limit="):
			n, err := parsePositive(a[len("--limit="):])
			if err != nil {
				warnf("--limit needs a number, not %q", a[len("--limit="):])
				return 2
			}
			limit = n
		case strings.HasPrefix(a, "-") && a != "-":
			warnf("unknown flag %q", a)
			return 2
		default:
			parts = append(parts, a)
		}
	}
	terms := sessTerms(strings.Join(parts, " "))
	if len(terms) == 0 {
		warnf("usage: subnsub-monitor sessions search [--limit N] [--json] [--] TEXT...")
		return 2
	}
	if limit < 1 {
		limit = 1
	}
	if limit > sessMaxLimit {
		limit = sessMaxLimit
	}

	rep := sessionsSearch(sessSources(), sessQuery{
		terms:    terms,
		limit:    limit,
		deadline: time.Now().Add(sessBudget),
	})

	if asJSON {
		out, err := sessEncodeCapped(&rep)
		if err != nil {
			// Nothing in the report can fail to marshal; saying so beats
			// printing half a document.
			warnf("could not encode the result: %v", err)
			return 2
		}
		fmt.Println(string(out))
		return 0
	}
	for _, h := range rep.Matches {
		when := h.Timestamp
		if len(when) >= 16 {
			when = strings.Replace(when[:16], "T", " ", 1)
		}
		id := h.SessionID
		if len(id) > 8 {
			id = id[:8]
		}
		line := []string{when, h.Agent}
		if h.Project != "" {
			line = append(line, h.Project)
		}
		line = append(line, id, h.Snippet)
		fmt.Println(strings.Join(line, "  "))
	}
	fmt.Printf("%d match(es) across %d file(s), %d scanned, %d ms", len(rep.Matches), rep.Files, rep.Scanned, rep.Ms)
	if rep.Partial {
		fmt.Print(" (partial — some files could not be fully searched)")
	}
	fmt.Println()
	return 0
}

// The report as JSON, guaranteed to fit the channel. Hits leave from the end
// — the end is the oldest-file tail of the list, so what survives is what the
// ordering already ranked first — and any drop is declared in `partial`.
func sessEncodeCapped(rep *sessReport) ([]byte, error) {
	out, err := json.Marshal(rep)
	for err == nil && len(out) > sessMaxJSONBytes && len(rep.Matches) > 0 {
		rep.Matches = rep.Matches[:len(rep.Matches)-1]
		rep.Partial = true
		out, err = json.Marshal(rep)
	}
	return out, err
}

// A small positive integer out of a flag value. strconv.Atoi accepts "+12"
// and "-3"; a limit does not.
func parsePositive(s string) (int, error) {
	if s == "" || len(s) > 4 {
		return 0, fmt.Errorf("not a small number")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// Query text into search terms: split on whitespace, lowercased ASCII-only —
// the SAME folding sessContainsFold applies, which is the point. A Unicode
// lower here would turn a query's Ärger into ärger and then match neither
// spelling, because the comparison below folds nothing outside A–Z (review
// 2026-08-18).
func sessTerms(q string) []string {
	fields := strings.Fields(q)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, sessLowerASCII(f))
	}
	return out
}

func sessLowerASCII(s string) string {
	b := []byte(s)
	changed := false
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
			changed = true
		}
	}
	if !changed {
		return s
	}
	return string(b)
}

// The terms the RAW-BYTE gate may use. A term holding a quote or a backslash
// can appear JSON-escaped in the file (`"foo"` as \"foo\", C:\tmp as
// C:\\tmp), so testing the raw line for it would throw away real hits before
// they were ever parsed (review 2026-08-18). Those terms are still enforced —
// on the extracted text, where escapes are gone — they just cannot serve as
// the cheap gate.
func sessGateTerms(terms []string) []string {
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if !strings.ContainsAny(t, "\"\\") {
			out = append(out, t)
		}
	}
	return out
}

// A transcript file waiting to be searched: which source it belongs to, where
// it lives under that source's root, and when it last changed — the sort key
// that decides what "the most recent matches" means.
type sessFile struct {
	src   int
	rel   string
	mtime time.Time
}

// Every transcript under one source, unordered. The walk pattern (and the
// reason `partial` exists) is codexCandidates': an unreadable directory means
// the list is incomplete, and an incomplete list must not become a confident
// "nothing here". The deadline covers the walk itself, because the budget is
// over the whole search and a tree pathological enough to take seconds to
// LIST would otherwise spend the scan's time before a single file was read
// (review 2026-08-18).
func sessCandidates(src sessSource, srcIdx int, deadline time.Time) (out []sessFile, partial bool) {
	filepath.WalkDir(src.root, func(p string, d fs.DirEntry, err error) error {
		if time.Now().After(deadline) {
			partial = true
			return fs.SkipAll
		}
		if err != nil {
			partial = true
			return nil
		}
		if d.IsDir() || !src.match(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			partial = true
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src.root, p)
		if err != nil {
			partial = true
			return nil
		}
		out = append(out, sessFile{srcIdx, rel, info.ModTime()})
		return nil
	})
	return out, partial
}

// The search: newest transcripts first, across every source at once, stopping
// at the limit or the deadline, whichever comes first. Partial means exactly
// "some transcript was not fully searched" — a directory that would not list,
// a file that would not read, a line too long to hold, or time running out —
// and never "the limit filled", which is the search working as designed.
func sessionsSearch(sources []sessSource, q sessQuery) sessReport {
	started := time.Now()
	rep := sessReport{Matches: []sessHit{}}

	roots := make([]*os.Root, len(sources))
	var files []sessFile
	for i, src := range sources {
		if src.root == "" {
			continue
		}
		r, err := os.OpenRoot(src.root)
		if err != nil {
			// No such directory is the tool not being installed — normal, and
			// not partial: there are no transcripts being missed.
			if !os.IsNotExist(err) {
				rep.Partial = true
			}
			continue
		}
		defer r.Close()
		roots[i] = r
		cs, partial := sessCandidates(src, i, q.deadline)
		if partial {
			rep.Partial = true
		}
		files = append(files, cs...)
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mtime.Equal(files[j].mtime) {
			return files[i].mtime.After(files[j].mtime)
		}
		// A stable tie-break so equal mtimes cannot reorder between runs.
		if files[i].src != files[j].src {
			return files[i].src < files[j].src
		}
		return files[i].rel < files[j].rel
	})
	rep.Files = len(files)

	gate := sessGateTerms(q.terms)
	for _, f := range files {
		if len(rep.Matches) >= q.limit {
			break
		}
		if time.Now().After(q.deadline) {
			rep.Partial = true
			break
		}
		hits, trouble := sessScanFile(roots[f.src], sources[f.src], f, q, gate)
		if trouble {
			rep.Partial = true
		}
		rep.Scanned++
		// When the limit has room for only part of this file's hits, keep the
		// TAIL: hits are chronological, so the tail is the newest, and newest
		// is the promise the whole ordering makes (review 2026-08-18).
		if remaining := q.limit - len(rep.Matches); len(hits) > remaining {
			hits = hits[len(hits)-remaining:]
		}
		rep.Matches = append(rep.Matches, hits...)
	}
	rep.Ms = time.Since(started).Milliseconds()
	return rep
}

// One transcript, line by line. Returns the last few matching messages in the
// order they happened, and whether anything kept the scan from being
// complete. The open goes through os.Root and the link-count guard for the
// reasons codexScanFile spells out; a file that fails either is reported as
// trouble, never read.
func sessScanFile(root *os.Root, src sessSource, f sessFile, q sessQuery, gate []string) (hits []sessHit, trouble bool) {
	if root == nil {
		return nil, true
	}
	file, err := root.Open(f.rel)
	if err != nil {
		return nil, true
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, true
	}
	if n, known := openLinkCount(file, st); !known || n != 1 {
		return nil, true
	}

	// The working directory this transcript records, once one is seen. Claude
	// stamps it on every line; codex only on the head of the file — so the
	// head lines are given a cheap look even when they match nothing.
	fileCwd := ""

	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 64*1024), sessMaxLineBytes)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		// Every few lines rather than every few hundred: lines here can be
		// megabytes each, and a check that comes around only after 256 of
		// them can overshoot the budget so far the console's own timeout
		// kills the command — which returns NOTHING, where this returns a
		// partial answer (review 2026-08-18). Break, not return: the hits
		// already found still deserve their timestamps and project below.
		if lineNo%16 == 0 && time.Now().After(q.deadline) {
			trouble = true
			break
		}
		line := sc.Bytes()
		if fileCwd == "" && lineNo <= 4 && len(line) < 512*1024 && bytes.Contains(line, []byte(`"cwd"`)) {
			var head struct {
				Cwd     string `json:"cwd"`
				Payload struct {
					Cwd string `json:"cwd"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &head) == nil {
				if head.Cwd != "" {
					fileCwd = head.Cwd
				} else if head.Payload.Cwd != "" {
					fileCwd = head.Payload.Cwd
				}
			}
		}

		// Cheap gate first: a line whose raw bytes do not hold every GATE
		// term cannot produce a message text that does, and almost every
		// line of almost every file stops here without being parsed. Terms
		// that JSON escaping could disguise are not in the gate (see
		// sessGateTerms) — a query made only of those parses every line,
		// which is slower and correct.
		ok := true
		for _, t := range gate {
			if !sessContainsFold(line, t) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}

		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		var texts []string
		sessCollectText(m, 0, &texts)
		if len(texts) == 0 {
			continue
		}
		joined := strings.Join(texts, "\n")
		ok = true
		for _, t := range q.terms {
			if sessIndexFold(joined, t) < 0 {
				ok = false
				break
			}
		}
		if !ok {
			// The terms lived in the line's plumbing — keys, ids, paths — not
			// in anything a person said. Not a hit.
			continue
		}

		hit := sessHit{
			Agent:     src.agent,
			SessionID: sessIDFromName(f.rel),
			Snippet:   sessRedact(sessSnippet(joined, q.terms[0])),
		}
		if ts, ok := m["timestamp"].(string); ok {
			if _, err := time.Parse(time.RFC3339, ts); err == nil {
				hit.Timestamp = ts
			}
		}
		if cwd, ok := m["cwd"].(string); ok && cwd != "" {
			fileCwd = cwd
		}
		// Project is filled in after the loop, once the file has had every
		// chance to reveal its cwd.
		hits = append(hits, hit)
		if len(hits) > sessPerSession {
			hits = hits[1:]
		}
	}
	if sc.Err() != nil {
		trouble = true
	}
	project := ""
	if fileCwd != "" {
		// Bounded like everything else that rides the pipe: a directory can
		// be NAMED five kilobytes, and one hit must not be able to spend the
		// whole frame on its label.
		project = sessCapRunes(filepath.Base(fileCwd), 64)
	}
	for i := range hits {
		if hits[i].Timestamp == "" {
			hits[i].Timestamp = f.mtime.UTC().Format(time.RFC3339)
		}
		hits[i].Project = project
	}
	return hits, trouble
}

// The first n runes of s, whole runes only.
func sessCapRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// The human-readable text out of one parsed transcript line. Collection is by
// key, not by position: `text` and `thinking` hold what somebody typed or the
// model said, in every format this searches (Claude's content items, codex's
// input_text/output_text items), and `content` is the older claude shape
// where a user message is one plain string. Everything else — ids, paths,
// base64 payloads under other names — is structure to walk through, not text
// to quote. Keys are visited in sorted order so the same line always yields
// the same joined text, which is what keeps snippets stable between runs.
func sessCollectText(v any, depth int, out *[]string) {
	if depth > sessMaxDepth {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := t[k]
			switch k {
			case "text", "thinking":
				if s, ok := val.(string); ok && s != "" {
					*out = append(*out, s)
				}
			case "content":
				if s, ok := val.(string); ok {
					if s != "" {
						*out = append(*out, s)
					}
				} else {
					sessCollectText(val, depth+1, out)
				}
			default:
				switch val.(type) {
				case map[string]any, []any:
					sessCollectText(val, depth+1, out)
				}
			}
		}
	case []any:
		for _, e := range t {
			sessCollectText(e, depth+1, out)
		}
	}
}

// Case-insensitive substring, ASCII fold only, no allocation. The needle
// arrives already lowercased. Folding stops at ASCII deliberately: transcript
// search here is for code and command fragments and English/CJK prose, and a
// full Unicode fold would buy Å≈å at the price of lowercasing megabytes of
// line into fresh allocations. CJK has no case and is unaffected.
func sessContainsFold(hay []byte, needle string) bool {
	n := len(needle)
	if n == 0 {
		return true
	}
	if n > len(hay) {
		return false
	}
	c := needle[0]
	C := c
	if c >= 'a' && c <= 'z' {
		C = c - ('a' - 'A')
	}
	for i := 0; i+n <= len(hay); i++ {
		if b := hay[i]; b != c && b != C {
			continue
		}
		j := 1
		for ; j < n; j++ {
			h := hay[i+j]
			if h >= 'A' && h <= 'Z' {
				h += 'a' - 'A'
			}
			if h != needle[j] {
				break
			}
		}
		if j == n {
			return true
		}
	}
	return false
}

// The byte index of the needle in a string, same folding rules, -1 when
// absent. Kept separate from sessContainsFold so the hot path above never
// pays for an index it does not want.
func sessIndexFold(hay string, needle string) int {
	n := len(needle)
	if n == 0 {
		return 0
	}
	if n > len(hay) {
		return -1
	}
	c := needle[0]
	C := c
	if c >= 'a' && c <= 'z' {
		C = c - ('a' - 'A')
	}
	for i := 0; i+n <= len(hay); i++ {
		if b := hay[i]; b != c && b != C {
			continue
		}
		j := 1
		for ; j < n; j++ {
			h := hay[i+j]
			if h >= 'A' && h <= 'Z' {
				h += 'a' - 'A'
			}
			if h != needle[j] {
				break
			}
		}
		if j == n {
			return i
		}
	}
	return -1
}

// A window of the matched text around the first term, whitespace collapsed,
// capped in runes, with ellipses marking where it was cut. The window is
// taken from the raw text FIRST and collapsed after, so a multi-megabyte
// message costs a few hundred bytes of work, not a full rewrite.
func sessSnippet(text, firstTerm string) string {
	idx := sessIndexFold(text, firstTerm)
	if idx < 0 {
		idx = 0
	}
	start := idx - 300
	if start < 0 {
		start = 0
	}
	end := idx + len(firstTerm) + 300
	if end > len(text) {
		end = len(text)
	}
	for start > 0 && start < len(text) && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	window := strings.Join(strings.Fields(text[start:end]), " ")
	cutHead := start > 0
	cutTail := end < len(text)

	// Cap in runes, centred on the term, so whatever else the cut takes it
	// never takes the one word the reader searched for.
	runes := []rune(window)
	if len(runes) > sessSnippetRunes {
		termAt := sessIndexFold(window, firstTerm)
		if termAt < 0 {
			termAt = 0
		}
		tr := utf8.RuneCountInString(window[:termAt])
		lo := tr - sessSnippetRunes/2
		if lo < 0 {
			lo = 0
		}
		hi := lo + sessSnippetRunes
		if hi > len(runes) {
			hi = len(runes)
			lo = hi - sessSnippetRunes
		}
		if lo > 0 {
			cutHead = true
		}
		if hi < len(runes) {
			cutTail = true
		}
		window = string(runes[lo:hi])
	}
	if cutHead {
		window = "…" + window
	}
	if cutTail {
		window = window + "…"
	}
	return window
}

// Best-effort masking of credentials that appear in a snippet — the one part
// of a transcript that leaves the machine. Patterns over cleverness: the
// well-known prefixes vendors stamp on their tokens, JWTs by their shape, key
// material by its armour, and `key=value` assignments whose key says secret.
// This is defence in depth for a channel the operator already holds both ends
// of, not data-loss prevention; the transcript on disk is untouched.
var sessRedactRes = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`(?:gh[pousr]|github_pat)_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`AKIA[A-Z0-9]{16}`),
	regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}[A-Za-z0-9_.-]*`),
	regexp.MustCompile(`-----BEGIN[A-Z ]{0,32}PRIVATE KEY-----[^-]*(?:-----END[A-Z ]{0,32}PRIVATE KEY-----)?`),
	regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`),
}

// Assignments keep their key and lose their value, so the snippet still shows
// WHAT was set — which is usually why the line matched — without showing what
// it was set to. The key may carry prefixes and suffixes around the sensitive
// word (AWS_SECRET_ACCESS_KEY, DB_PASSWORD, CLIENT_SECRET) — a plain \b
// before the word would miss all of those, because an underscore IS a word
// character (review 2026-08-18). RE2 has no lookaround, so the shape is
// spelled out: optional `..._`-style prefix, the word, optional `_...`-style
// suffix. `tokens:` stays untouched — a bare `s` fits neither the suffix nor
// the separator. Values may be quoted (spaces and all) or bare.
var sessRedactAssign = regexp.MustCompile(
	`(?i)((?:[A-Za-z0-9_-]*[_-])?(?:api[_-]?key|apikey|access[_-]?key|secret|token|passw(?:or)?d|credentials?|authorization)(?:[_-][A-Za-z0-9_-]*)?["']?\s*[:=]\s*)("[^"]{4,}"|'[^']{4,}'|[^\s"']{6,})`)

func sessRedact(s string) string {
	for _, re := range sessRedactRes {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return sessRedactAssign.ReplaceAllString(s, "${1}[redacted]")
}

// The id worth showing for a transcript file. Claude names files by the
// session's uuid; codex wraps the uuid in a rollout-<stamp>- prefix. When a
// trailing uuid is there, it IS the id; otherwise the stem is the honest
// answer.
func sessIDFromName(rel string) string {
	stem := strings.TrimSuffix(filepath.Base(rel), ".jsonl")
	if len(stem) >= 36 && sessLooksUUID(stem[len(stem)-36:]) {
		return stem[len(stem)-36:]
	}
	return stem
}

func sessLooksUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			hex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !hex {
				return false
			}
		}
	}
	return true
}
