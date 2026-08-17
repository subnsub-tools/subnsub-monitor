package main

// The searcher's promises, each held by a test: hits come from every agent's
// tree at once and newest first, every term must live in ONE message, folding
// is ASCII-only, secrets leave redacted, a second hard name or an escaping
// link is refused rather than read, and the report's JSON is exactly the
// shape the dashboard renders — down to `"matches":[]` never being null.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake world: a claude projects tree and a codex sessions tree under one
// temp dir, with the same match rules production uses.
func sessTestWorld(t *testing.T) (claude, codex string, sources []sessSource) {
	t.Helper()
	base := t.TempDir()
	claude = filepath.Join(base, "claude-projects")
	codex = filepath.Join(base, "codex-sessions")
	for _, d := range []string{claude, codex} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sources = []sessSource{
		{agent: "claude", root: claude,
			match: func(n string) bool { return strings.HasSuffix(n, ".jsonl") }},
		{agent: "codex", root: codex,
			match: func(n string) bool {
				return strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, ".jsonl")
			}},
	}
	return claude, codex, sources
}

func claudeLine(ts, cwd, text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": ts, "cwd": cwd, "sessionId": "unused",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	})
	return string(b)
}

func codexMetaLine(ts, cwd string) string {
	b, _ := json.Marshal(map[string]any{
		"timestamp": ts, "type": "session_meta",
		"payload": map[string]any{"id": "m", "cwd": cwd},
	})
	return string(b)
}

func codexMsgLine(ts, text string) string {
	b, _ := json.Marshal(map[string]any{
		"timestamp": ts, "type": "response_item",
		"payload": map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": text}},
		},
	})
	return string(b)
}

func sessWrite(t *testing.T, path string, mtime time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func sessAsk(terms ...string) sessQuery {
	low := make([]string, len(terms))
	for i, s := range terms {
		low[i] = strings.ToLower(s)
	}
	return sessQuery{terms: low, limit: sessDefaultLimit, deadline: time.Now().Add(time.Minute)}
}

func TestSessionsSearchFindsAcrossAgents(t *testing.T) {
	claude, codex, sources := sessTestWorld(t)
	now := time.Now()
	sessWrite(t, filepath.Join(claude, "-home-u-proj", "11111111-2222-3333-4444-555555555555.jsonl"),
		now.Add(-1*time.Hour),
		claudeLine("2026-08-17T10:00:00Z", "/home/u/proj", "the flux capacitor hums"))
	sessWrite(t, filepath.Join(codex, "2026", "08", "17", "rollout-2026-08-17T09-00-00-99999999-8888-7777-6666-555555555555.jsonl"),
		now.Add(-2*time.Hour),
		codexMetaLine("2026-08-17T09:00:00Z", "/home/u/other"),
		codexMsgLine("2026-08-17T09:00:01Z", "please fix the flux capacitor"))

	rep := sessionsSearch(sources, sessAsk("flux", "capacitor"))
	if len(rep.Matches) != 2 {
		t.Fatalf("want 2 hits, got %d: %+v", len(rep.Matches), rep.Matches)
	}
	first, second := rep.Matches[0], rep.Matches[1]
	if first.Agent != "claude" || second.Agent != "codex" {
		t.Fatalf("newest-first across agents broken: %q then %q", first.Agent, second.Agent)
	}
	if first.Project != "proj" || second.Project != "other" {
		t.Fatalf("projects wrong: %q, %q", first.Project, second.Project)
	}
	if first.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("claude session id wrong: %q", first.SessionID)
	}
	if second.SessionID != "99999999-8888-7777-6666-555555555555" {
		t.Fatalf("codex uuid not pulled from the rollout name: %q", second.SessionID)
	}
	if first.Timestamp != "2026-08-17T10:00:00Z" {
		t.Fatalf("timestamp should come from the line: %q", first.Timestamp)
	}
	if !strings.Contains(first.Snippet, "flux capacitor") {
		t.Fatalf("snippet lost the match: %q", first.Snippet)
	}
	if rep.Partial {
		t.Fatal("nothing here was partial")
	}
}

func TestSessionsSearchAllTermsMustShareOneMessage(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeLine("2026-08-17T10:00:00Z", "/p", "alpha alone here"),
		claudeLine("2026-08-17T10:01:00Z", "/p", "beta alone there"))
	if rep := sessionsSearch(sources, sessAsk("alpha", "beta")); len(rep.Matches) != 0 {
		t.Fatalf("terms split across messages must not match: %+v", rep.Matches)
	}
	if rep := sessionsSearch(sources, sessAsk("alpha", "alone")); len(rep.Matches) != 1 {
		t.Fatal("terms in one message must match")
	}
}

func TestSessionsSearchFoldAndCJK(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeLine("2026-08-17T10:00:00Z", "/p", "Deploy THE Relay 部署中继完成"))
	for _, q := range [][]string{{"deploy"}, {"RELAY"}, {"部署中继"}} {
		if rep := sessionsSearch(sources, sessAsk(q...)); len(rep.Matches) != 1 {
			t.Fatalf("query %v should hit once", q)
		}
	}
	if rep := sessionsSearch(sources, sessAsk("部署完成")); len(rep.Matches) != 0 {
		t.Fatal("non-adjacent CJK must not match as one term")
	}
}

// The terms live in the line's JSON plumbing (a path, a key) but not in any
// message text — the raw-byte gate lets it through and the text check must
// then throw it back.
func TestSessionsSearchIgnoresPlumbingOnlyMatches(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeLine("2026-08-17T10:00:00Z", "/home/u/zanzibar", "nothing relevant"))
	if rep := sessionsSearch(sources, sessAsk("zanzibar")); len(rep.Matches) != 0 {
		t.Fatalf("a cwd-only match is not a message match: %+v", rep.Matches)
	}
}

func TestSessionsSearchNewestFirstLimitAndPerSessionCap(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	base := time.Now().Add(-24 * time.Hour)
	// One old session stuffed with matches, several newer ones with one each.
	var noisy []string
	for i := 0; i < 10; i++ {
		noisy = append(noisy, claudeLine(fmt.Sprintf("2026-08-16T09:%02d:00Z", i), "/old", "needle again"))
	}
	sessWrite(t, filepath.Join(claude, "old", "old.jsonl"), base, noisy...)
	for i := 0; i < 3; i++ {
		sessWrite(t, filepath.Join(claude, "new", fmt.Sprintf("n%d.jsonl", i)),
			base.Add(time.Duration(i+1)*time.Hour),
			claudeLine("2026-08-17T10:00:00Z", "/new", "needle here"))
	}

	q := sessAsk("needle")
	q.limit = 5
	rep := sessionsSearch(sources, q)
	if len(rep.Matches) != 5 {
		t.Fatalf("limit not held: %d", len(rep.Matches))
	}
	// The three fresh sessions first (newest first), then the old one capped.
	for i := 0; i < 3; i++ {
		if rep.Matches[i].Project != "new" {
			t.Fatalf("hit %d should be from a newer session: %+v", i, rep.Matches[i])
		}
	}
	fromOld := 0
	for _, h := range rep.Matches {
		if h.Project == "old" {
			fromOld++
		}
	}
	if fromOld != 2 {
		t.Fatalf("the old session should contribute only up to the leftover limit, got %d", fromOld)
	}

	q.limit = sessMaxLimit
	rep = sessionsSearch(sources, q)
	fromOld = 0
	for _, h := range rep.Matches {
		if h.Project == "old" {
			fromOld++
		}
	}
	if fromOld != sessPerSession {
		t.Fatalf("per-session cap broken: %d hits from one session", fromOld)
	}
}

func TestSessionsSearchRedactsSecrets(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		// The token shapes are deliberately one size above OUR redaction
		// threshold and below every real vendor's length, so nothing here
		// trips a secret scanner while still proving the masking works.
		claudeLine("2026-08-17T10:00:00Z", "/p",
			"set the wombat key sk-fakefakefake01234 and GH ghp_abcdefghijklmnop1234 "+
				"and api_key=supersecretvalue and Bearer abcdefghijklmnopqrstuv done"))
	rep := sessionsSearch(sources, sessAsk("wombat"))
	if len(rep.Matches) != 1 {
		t.Fatal("the message should match")
	}
	s := rep.Matches[0].Snippet
	for _, leak := range []string{"sk-fakefake", "ghp_abcdef", "supersecretvalue", "abcdefghijklmnopqrstuv"} {
		if strings.Contains(s, leak) {
			t.Fatalf("snippet leaked %q: %q", leak, s)
		}
	}
	if !strings.Contains(s, "[redacted]") || !strings.Contains(s, "wombat") {
		t.Fatalf("redaction should mark, not erase the context: %q", s)
	}
}

func TestSessionsSearchRefusesSecondHardName(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	p := filepath.Join(claude, "p", "a.jsonl")
	sessWrite(t, p, time.Now(), claudeLine("2026-08-17T10:00:00Z", "/p", "needle in a linked file"))
	if err := os.Link(p, filepath.Join(claude, "p", "b.jsonl")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	rep := sessionsSearch(sources, sessAsk("needle"))
	if len(rep.Matches) != 0 {
		t.Fatalf("a file with two names must be refused: %+v", rep.Matches)
	}
	if !rep.Partial {
		t.Fatal("refusing a file must be reported as partial")
	}
}

func TestSessionsSearchSymlinksNeverFollowed(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	sessWrite(t, filepath.Join(outside, "secret.jsonl"), time.Now(),
		claudeLine("2026-08-17T10:00:00Z", "/x", "needle beyond the fence"))
	if err := os.Symlink(outside, filepath.Join(claude, "linkdir")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(claude, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.jsonl"), filepath.Join(claude, "p", "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	rep := sessionsSearch(sources, sessAsk("needle"))
	if len(rep.Matches) != 0 {
		t.Fatalf("nothing behind a symlink may be read: %+v", rep.Matches)
	}
}

func TestSessionsSearchDeadlineMarksPartial(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeLine("2026-08-17T10:00:00Z", "/p", "needle"))
	q := sessAsk("needle")
	q.deadline = time.Now().Add(-time.Second)
	rep := sessionsSearch(sources, q)
	if len(rep.Matches) != 0 || !rep.Partial {
		t.Fatalf("an expired deadline is a partial empty answer: %+v", rep)
	}
}

func TestSessionsSearchMissingRootsAreNormal(t *testing.T) {
	base := t.TempDir()
	sources := []sessSource{
		{agent: "claude", root: filepath.Join(base, "not-there"),
			match: func(n string) bool { return true }},
		{agent: "codex", root: "",
			match: func(n string) bool { return true }},
	}
	rep := sessionsSearch(sources, sessAsk("anything"))
	if rep.Partial || rep.Files != 0 || len(rep.Matches) != 0 {
		t.Fatalf("absent tools are a normal empty answer: %+v", rep)
	}
}

func TestSessionsSearchTimestampFallsBackToMtime(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	when := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	line, _ := json.Marshal(map[string]any{
		"cwd": "/p",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "needle with no clock"}},
		},
	})
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), when, string(line))
	rep := sessionsSearch(sources, sessAsk("needle"))
	if len(rep.Matches) != 1 {
		t.Fatal("should match")
	}
	if rep.Matches[0].Timestamp != "2026-08-15T12:00:00Z" {
		t.Fatalf("mtime fallback wrong: %q", rep.Matches[0].Timestamp)
	}
}

func TestSessionsSearchOversizedLineMarksPartial(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a >8MB line")
	}
	claude, _, sources := sessTestWorld(t)
	big := claudeLine("2026-08-17T10:00:00Z", "/p", strings.Repeat("x", sessMaxLineBytes))
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		big,
		claudeLine("2026-08-17T10:01:00Z", "/p", "needle after the monster"))
	rep := sessionsSearch(sources, sessAsk("needle"))
	if !rep.Partial {
		t.Fatal("a line too long to hold means the file was not fully searched")
	}
}

func TestSessReportJSONShape(t *testing.T) {
	out, err := json.Marshal(sessReport{Matches: []sessHit{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"matches":[]`) {
		t.Fatalf("an empty answer must be [], not null: %s", out)
	}
	one, _ := json.Marshal(sessHit{Project: "p", Agent: "a", Timestamp: "t", SessionID: "s", Snippet: "x"})
	for _, key := range []string{`"project"`, `"agent"`, `"timestamp"`, `"session_id"`, `"snippet"`} {
		if !strings.Contains(string(one), key) {
			t.Fatalf("the dashboard renders %s and it is missing: %s", key, one)
		}
	}
}

func TestSessContainsFold(t *testing.T) {
	cases := []struct {
		hay, needle string
		want        bool
	}{
		{"Hello World", "hello", true},
		{"HELLO", "hello", true},
		{"hell", "hello", false},
		{"", "", true},
		{"abc", "", true},
		{"部署完成", "部署", true},
		{"部署完成", "完成了", false},
		{"Ärger", "ärger", false}, // ASCII fold only, by design
		{"xxAbCxx", "abc", true},
	}
	for _, c := range cases {
		if got := sessContainsFold([]byte(c.hay), c.needle); got != c.want {
			t.Errorf("sessContainsFold(%q, %q) = %v", c.hay, c.needle, got)
		}
		if got := sessIndexFold(c.hay, c.needle) >= 0; got != c.want {
			t.Errorf("sessIndexFold(%q, %q) found=%v", c.hay, c.needle, got)
		}
	}
}

func TestSessSnippet(t *testing.T) {
	long := strings.Repeat("padding words here ", 100) + "the NEEDLE sits" + strings.Repeat(" and more tail", 100)
	s := sessSnippet(long, "needle")
	if !strings.Contains(strings.ToLower(s), "needle") {
		t.Fatalf("snippet lost the term: %q", s)
	}
	if !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") {
		t.Fatalf("a middle cut shows both ellipses: %q", s)
	}
	if n := len([]rune(s)); n > sessSnippetRunes+2 {
		t.Fatalf("snippet too long: %d runes", n)
	}
	if strings.Contains(s, "  ") || strings.Contains(s, "\n") {
		t.Fatalf("whitespace not collapsed: %q", s)
	}
	if short := sessSnippet("just this", "just"); short != "just this" {
		t.Fatalf("a short text is itself: %q", short)
	}
	cjk := sessSnippet(strings.Repeat("多字节字符", 200)+"目标词条", "目标词条")
	if !strings.Contains(cjk, "目标词条") || !utf8ValidString(cjk) {
		t.Fatalf("CJK snippet broken: %q", cjk)
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestSessIDFromName(t *testing.T) {
	cases := map[string]string{
		"a/11111111-2222-3333-4444-555555555555.jsonl":                                      "11111111-2222-3333-4444-555555555555",
		"2026/08/17/rollout-2026-08-17T09-00-00-99999999-8888-7777-6666-555555555555.jsonl": "99999999-8888-7777-6666-555555555555",
		"p/notes.jsonl": "notes",
	}
	for in, want := range cases {
		if got := sessIDFromName(in); got != want {
			t.Errorf("sessIDFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePositive(t *testing.T) {
	for s, want := range map[string]int{"1": 1, "12": 12, "9999": 9999} {
		if n, err := parsePositive(s); err != nil || n != want {
			t.Errorf("parsePositive(%q) = %d, %v", s, n, err)
		}
	}
	for _, s := range []string{"", "-1", "+2", "12a", "10000", "1e3"} {
		if _, err := parsePositive(s); err == nil {
			t.Errorf("parsePositive(%q) should refuse", s)
		}
	}
}
