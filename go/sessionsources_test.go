package main

// What each newly-read tool has to keep working, held by a test apiece: a
// transcript that never says where it ran still gets a project, a tool that
// names every file the same still gets distinct ids, a tool that writes one
// document instead of one line per message is still searchable and readable,
// and a tool that keeps its system prompt in the transcript does not answer
// every query with it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cursorLine(role, text string) string {
	b, _ := json.Marshal(map[string]any{
		"role": role,
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	})
	return string(b)
}

func grokLine(kind, text string) string {
	if kind == "system" || kind == "assistant" {
		b, _ := json.Marshal(map[string]any{"type": kind, "content": text})
		return string(b)
	}
	b, _ := json.Marshal(map[string]any{
		"type":    kind,
		"content": []any{map[string]any{"type": "text", "text": text}},
	})
	return string(b)
}

func kiroLine(kind, text string) string {
	b, _ := json.Marshal(map[string]any{
		"version": "v1", "kind": kind,
		"data": map[string]any{
			"message_id": "m",
			"content":    []any{map[string]any{"kind": "text", "data": text}},
		},
	})
	return string(b)
}

func ampDoc(t *testing.T, cwd string, texts ...string) string {
	t.Helper()
	msgs := make([]any, 0, len(texts))
	for i, tx := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]any{
			"role": role, "messageId": i,
			"content": []any{map[string]any{"type": "text", "text": tx}},
		})
	}
	doc := map[string]any{
		"id": "T-x", "messages": msgs,
		"env": map[string]any{"initial": map[string]any{
			"trees": []any{map[string]any{"uri": "file://" + cwd}},
		}},
	}
	// Written the way Amp writes it — indented, so a line-oriented reader
	// would see fragments rather than messages. That is the whole point of
	// the `doc` hook and this test would pass by accident on one line.
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A world holding one transcript from each of the four tools added in
// 2026.08.19, in the shape and at the path each one really uses.
func sessNewWorld(t *testing.T) (base string, sources []sessSource) {
	t.Helper()
	base = t.TempDir()
	old := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	cursor := filepath.Join(base, "cursor-projects")
	sessWrite(t, filepath.Join(cursor, "home-tester-widgets", "agent-transcripts",
		"11111111-2222-3333-4444-555555555555", "11111111-2222-3333-4444-555555555555.jsonl"),
		old,
		cursorLine("user", "<timestamp>Tue</timestamp>\n<user_query>\nthe kettle is boiling\n</user_query>"),
		cursorLine("assistant", "cursor says the kettle boiled"))
	// A subagent's own transcript, one level deeper.
	sessWrite(t, filepath.Join(cursor, "home-tester-widgets", "agent-transcripts",
		"11111111-2222-3333-4444-555555555555", "subagents",
		"99999999-8888-7777-6666-555555555555.jsonl"),
		old, cursorLine("assistant", "a subagent mentions the kettle too"))

	grok := filepath.Join(base, "grok-sessions")
	sessWrite(t, filepath.Join(grok, "%2Fhome%2Ftester%2Fwidgets",
		"22222222-2222-3333-4444-555555555555", "chat_history.jsonl"), old,
		grokLine("system", "You are an interactive CLI tool. The kettle is never mentioned by users."),
		grokLine("user", "grok was asked about the kettle"))
	// The sibling this must NOT read: the same prompts a second time.
	sessWrite(t, filepath.Join(grok, "%2Fhome%2Ftester%2Fwidgets", "prompt_history.jsonl"), old,
		`{"timestamp":"2026-08-01T12:00:00Z","prompt":"grok was asked about the kettle"}`)

	kiro := filepath.Join(base, "kiro-cli")
	sessWrite(t, filepath.Join(kiro, "33333333-2222-3333-4444-555555555555.jsonl"), old,
		kiroLine("Prompt", "kiro heard about the kettle"))
	side, _ := json.Marshal(map[string]any{"session_id": "x", "cwd": "/home/tester/widgets"})
	sessWrite(t, filepath.Join(kiro, "33333333-2222-3333-4444-555555555555.json"), old, string(side))

	amp := filepath.Join(base, "amp-threads")
	sessWrite(t, filepath.Join(amp, "T-44444444-2222-3333-4444-555555555555.json"), old,
		ampDoc(t, "/home/tester/widgets", "amp was told the kettle boiled", "amp agreed"))

	home := "/home/tester"
	sources = []sessSource{
		{agent: "cursor", root: cursor,
			match: func(rel string) bool {
				p := sessPathParts(rel)
				return len(p) >= 4 && p[1] == "agent-transcripts" && strings.HasSuffix(p[len(p)-1], ".jsonl")
			},
			cwd: sessCursorCwd(home)},
		{agent: "grok", root: grok,
			match: func(rel string) bool {
				p := sessPathParts(rel)
				return len(p) == 3 && p[2] == "chat_history.jsonl"
			},
			id: func(rel string) string {
				p := sessPathParts(rel)
				if len(p) >= 2 {
					return p[len(p)-2]
				}
				return ""
			},
			cwd: func(_ *os.Root, rel string) string {
				p := sessPathParts(rel)
				if len(p) == 0 {
					return ""
				}
				return sessUnescapePath(p[0])
			}},
		{agent: "kiro", root: kiro, match: sessSuffixMatch(".jsonl"), cwd: sessKiroCwd},
		{agent: "amp", root: amp,
			match: func(rel string) bool {
				n := filepath.Base(rel)
				return strings.HasPrefix(n, "T-") && strings.HasSuffix(n, ".json")
			},
			doc: sessAmpDoc},
	}
	return base, sources
}

func sessSearchNew(sources []sessSource, terms ...string) sessReport {
	return sessionsSearch(sources, sessQuery{
		terms: terms, limit: 20, deadline: time.Now().Add(sessBudget),
	})
}

func TestEveryNewToolIsSearchable(t *testing.T) {
	_, sources := sessNewWorld(t)
	rep := sessSearchNew(sources, "kettle")
	seen := map[string]int{}
	for _, m := range rep.Matches {
		seen[m.Agent]++
	}
	for _, want := range []string{"cursor", "grok", "kiro", "amp"} {
		if seen[want] == 0 {
			t.Errorf("no hit from %s: %+v", want, rep.Matches)
		}
	}
}

func TestEveryNewToolKnowsItsProject(t *testing.T) {
	_, sources := sessNewWorld(t)
	rep := sessSearchNew(sources, "kettle")
	for _, m := range rep.Matches {
		if m.Project != "widgets" {
			t.Errorf("%s hit says project %q, want widgets", m.Agent, m.Project)
		}
	}
	// And the same label has to narrow a search, or the filter is decorative.
	got := sessionsSearch(sources, sessQuery{
		terms:    []string{"kettle"},
		filter:   sessFilter{project: "widgets"},
		limit:    20,
		deadline: time.Now().Add(sessBudget),
	})
	if len(got.Matches) != len(rep.Matches) {
		t.Errorf("project widgets found %d of %d hits", len(got.Matches), len(rep.Matches))
	}
	none := sessionsSearch(sources, sessQuery{
		terms:    []string{"kettle"},
		filter:   sessFilter{project: "gadgets"},
		limit:    20,
		deadline: time.Now().Add(sessBudget),
	})
	if len(none.Matches) != 0 {
		t.Errorf("project gadgets should match nothing, got %+v", none.Matches)
	}
}

func TestGrokSystemPromptIsNotAnAnswer(t *testing.T) {
	_, sources := sessNewWorld(t)
	// A word that appears ONLY in the system prompt.
	rep := sessSearchNew(sources, "interactive")
	for _, m := range rep.Matches {
		if m.Agent == "grok" {
			t.Fatalf("the system prompt answered a search: %+v", m)
		}
	}
	// The user's own line is still found.
	if len(sessSearchNew(sources, "grok was asked").Matches) == 0 {
		t.Fatal("skipping the system prompt must not skip the conversation")
	}
}

func TestGrokSessionsDoNotAllShareOneID(t *testing.T) {
	base, sources := sessNewWorld(t)
	old := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	sessWrite(t, filepath.Join(base, "grok-sessions", "%2Fhome%2Ftester%2Fwidgets",
		"77777777-2222-3333-4444-555555555555", "chat_history.jsonl"), old,
		grokLine("user", "a second grok session about the kettle"))
	ids := map[string]bool{}
	for _, m := range sessSearchNew(sources, "kettle").Matches {
		if m.Agent == "grok" {
			ids[m.SessionID] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("two grok sessions must have two ids, got %v", ids)
	}
	if ids["chat_history"] {
		t.Fatal("the id is the session, not the file name every session shares")
	}
}

func TestGrokPromptHistoryIsNotSearchedTwice(t *testing.T) {
	_, sources := sessNewWorld(t)
	n := 0
	for _, m := range sessSearchNew(sources, "grok was asked").Matches {
		n++
		_ = m
	}
	if n != 1 {
		t.Fatalf("one grok line, one hit — got %d", n)
	}
}

func TestAmpDocumentReadsAsMessages(t *testing.T) {
	_, sources := sessNewWorld(t)
	rep := sessSearchNew(sources, "amp was told")
	if len(rep.Matches) != 1 {
		t.Fatalf("want one amp hit, got %+v", rep.Matches)
	}
	// Position inside the document is the paging coordinate, exactly as a line
	// number is for every other tool.
	if rep.Matches[0].Line != 1 {
		t.Errorf("amp hit is at message %d, want 1", rep.Matches[0].Line)
	}
	// And it must be readable back through `open` at that coordinate.
	id := rep.Matches[0].SessionID
	got := sessionsOpen(sources, id, "around", 1, time.Now().Add(sessBudget))
	if got.Error != "" || len(got.Messages) == 0 {
		t.Fatalf("open %q: err=%q msgs=%d", id, got.Error, len(got.Messages))
	}
	if !strings.Contains(got.Messages[0].Text, "amp was told") {
		t.Errorf("opened the wrong message: %+v", got.Messages[0])
	}
	if got.Session == nil || got.Session.Project != "widgets" {
		t.Errorf("opened amp session lost its project: %+v", got.Session)
	}
}

func TestADocumentTranscriptIsArchivedToo(t *testing.T) {
	// A whole-document transcript has no byte offsets, so the archive resumes
	// it by MESSAGE INDEX instead. It has to reach the archive all the same:
	// the archive's whole purpose is the machine that is gone, and a tool left
	// out of it is a tool whose history dies with the box.
	base, sources := sessNewWorld(t)
	path := filepath.Join(base, "amp-threads", "T-44444444-2222-3333-4444-555555555555.json")
	st := syncTestState()
	b, more, fs := syncScanOne(t, sources, path, st)
	if b == nil {
		t.Fatal("an amp thread produced no batch")
	}
	if more {
		t.Fatal("two messages should not fill a batch")
	}
	meta, msgs := syncParts(t, b)
	if len(msgs) != 2 {
		t.Fatalf("want two messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].N != 1 || msgs[1].N != 2 {
		t.Errorf("positions are the document's own: %+v", msgs)
	}
	if meta["tool"] != "amp" {
		t.Errorf("tool %v", meta["tool"])
	}
	if meta["project"] != "widgets" {
		t.Errorf("project %v — a document names its own workspace", meta["project"])
	}
	if fs.Sig == "" || !strings.HasPrefix(fs.Sig, "doc:") {
		t.Errorf("a document resumes on its identity, not its opening bytes: %q", fs.Sig)
	}
	syncStore(fs, b)
	// Nothing appended: no second batch, and the cheap size test now holds.
	if b2, _, _ := syncScanOne(t, sources, path, st); b2 != nil {
		t.Fatalf("nothing changed; want no batch, got %d msgs", b2.msgs)
	}
}

func TestADocumentThatGrowsSendsOnlyWhatIsNew(t *testing.T) {
	base, sources := sessNewWorld(t)
	path := filepath.Join(base, "amp-threads", "T-44444444-2222-3333-4444-555555555555.json")
	st := syncTestState()
	b, _, fs := syncScanOne(t, sources, path, st)
	if b == nil {
		t.Fatal("no first batch")
	}
	syncStore(fs, b)
	// The thread gains a message, keeping its id — the same conversation.
	sessWrite(t, path, time.Now(),
		ampDoc(t, "/home/tester/widgets", "amp was told the kettle boiled", "amp agreed", "and one more thing"))
	b2, _, fs2 := syncScanOne(t, sources, path, st)
	if b2 == nil {
		t.Fatal("a grown thread sent nothing")
	}
	_, msgs := syncParts(t, b2)
	if len(msgs) != 1 || msgs[0].N != 3 {
		t.Fatalf("want only message 3, got %+v", msgs)
	}
	if fs2.Seq == 0 {
		t.Log("seq advances on store, not on scan")
	}
}

func TestADifferentThreadAtTheSamePathStartsOver(t *testing.T) {
	// The identity is the document's, not its opening bytes: Amp writes a
	// revision counter into its own head, so a head fingerprint would call
	// every write a different conversation.
	base, sources := sessNewWorld(t)
	path := filepath.Join(base, "amp-threads", "T-44444444-2222-3333-4444-555555555555.json")
	st := syncTestState()
	b, _, fs := syncScanOne(t, sources, path, st)
	if b == nil {
		t.Fatal("no first batch")
	}
	syncStore(fs, b)
	before := fs.Sig
	// Same id, more messages — NOT a replacement.
	sessWrite(t, path, time.Now(),
		ampDoc(t, "/home/tester/widgets", "amp was told the kettle boiled", "amp agreed", "third"))
	if _, _, fs2 := syncScanOne(t, sources, path, st); fs2.Sig != before || fs2.Line == 0 {
		t.Errorf("a thread that merely grew was treated as new: sig %q line %d", fs2.Sig, fs2.Line)
	}
	// A different id at the same path IS a replacement.
	other := strings.Replace(ampDoc(t, "/home/tester/widgets", "someone else entirely"),
		`"id": "T-x"`, `"id": "T-other"`, 1)
	sessWrite(t, path, time.Now(), other)
	if _, _, fs3 := syncScanOne(t, sources, path, st); fs3.Sig == before {
		t.Errorf("a different conversation kept the old identity: %q", fs3.Sig)
	}
}

func TestKiroTextLivesUnderDataNotText(t *testing.T) {
	var out []string
	var m map[string]any
	if err := json.Unmarshal([]byte(kiroLine("Prompt", "hello there")), &m); err != nil {
		t.Fatal(err)
	}
	sessCollectText(m, 0, &out)
	if strings.Join(out, "|") != "hello there" {
		t.Fatalf("collected %q", out)
	}
	// A `data` that is NOT text must stay plumbing — otherwise every tool
	// argument reads as something somebody said.
	var tool map[string]any
	if err := json.Unmarshal([]byte(
		`{"kind":"AssistantMessage","data":{"content":[{"kind":"toolUse","data":{"name":"web_fetch","url":"http://x"}}]}}`,
	), &tool); err != nil {
		t.Fatal(err)
	}
	out = nil
	sessCollectText(tool, 0, &out)
	if len(out) != 0 {
		t.Fatalf("tool plumbing was quoted as text: %q", out)
	}
}

func TestCursorTitleIsWhatWasTypedNotTheWrapper(t *testing.T) {
	_, sources := sessNewWorld(t)
	rep := sessionsRecent(sources, 20, sessFilter{}, time.Now().Add(sessBudget))
	for _, s := range rep.Sessions {
		if s.Agent != "cursor" {
			continue
		}
		if strings.Contains(s.Title, "<user_query>") || strings.HasPrefix(s.Title, "<timestamp>") {
			t.Fatalf("cursor title is the envelope: %q", s.Title)
		}
		if s.Title == "the kettle is boiling" {
			return
		}
	}
	t.Fatal("no cursor session listed with the words that were typed")
}

func TestTitleUnwrapLeavesOrdinaryMessagesAlone(t *testing.T) {
	for in, want := range map[string]string{
		"<timestamp>Tue</timestamp>\n<user_query>\nhi\n</user_query>": "hi",
		"<user_query>hi</user_query>":                                 "hi",
		"plain words":                                                 "plain words",
		// A message that only MENTIONS the tag keeps its own words.
		"why does <user_query> show up in my logs": "why does <user_query> show up in my logs",
		// An empty envelope is not an improvement on the envelope.
		"<user_query></user_query>": "<user_query></user_query>",
	} {
		if got := sessTitleUnwrap(in); got != want {
			t.Errorf("sessTitleUnwrap(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCursorSlugReadsBackAsAProjectName(t *testing.T) {
	f := sessCursorCwd("/home/tester")
	for in, want := range map[string]string{
		"home-tester-widgets": "widgets",
		"home-tester":         "tester",
		// Outside the home directory nothing can be inverted, and the whole
		// slug is the honest label.
		"opt-builds-thing": "opt-builds-thing",
	} {
		if got := f(nil, in+"/agent-transcripts/a/a.jsonl"); got != want {
			t.Errorf("cursor slug %q read back as %q, want %q", in, got, want)
		}
	}
}

func TestPercentDecodingIsForFilenamesNotURLs(t *testing.T) {
	for in, want := range map[string]string{
		"%2Fhome%2Fa": "/home/a",
		// A plus is a plus in a directory name, not a space.
		"a+b": "a+b",
		// A broken escape is left as written rather than swallowed.
		"100%":     "100%",
		"a%zz":     "a%zz",
		"no-marks": "no-marks",
	} {
		if got := sessUnescapePath(in); got != want {
			t.Errorf("sessUnescapePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestASourceCannotBeReadOutsideItsTree(t *testing.T) {
	// The oldest promise in this file, re-made for the sidecar read the new
	// sources brought in: a sidecar that points out of the tree is not read.
	base := t.TempDir()
	root := filepath.Join(base, "kiro")
	outside := filepath.Join(base, "outside.json")
	sessWrite(t, filepath.Join(root, "a.jsonl"), time.Now(), kiroLine("Prompt", "hi"))
	sessWrite(t, outside, time.Now(), `{"cwd":"/somewhere/else"}`)
	if err := os.Symlink(outside, filepath.Join(root, "a.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if got := sessKiroCwd(r, "a.jsonl"); got != "" {
		t.Fatalf("a sidecar outside the tree was read: %q", got)
	}
}

/* ── what the 2026-08-19 review turned up ─────────────────────────── */

func TestToolPlumbingIsNotQuotedAsSomethingSaid(t *testing.T) {
	// A tool call's arguments really do carry keys called `text` and
	// `content`. The kind says the whole item is machinery, and that has to be
	// believed before the keys inside it are read.
	for _, line := range []string{
		`{"kind":"toolUse","data":{"text":"needle","name":"edit"}}`,
		`{"kind":"toolUse","data":{"input":{"content":"needle"}}}`,
		`{"kind":"toolResult","data":{"content":[{"kind":"text","data":"needle"}]}}`,
	} {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		var out []string
		sessCollectText(m, 0, &out)
		if len(out) != 0 {
			t.Errorf("%s collected %q", line, out)
		}
	}
	// And the thing that IS text still comes through.
	var m map[string]any
	json.Unmarshal([]byte(kiroLine("Prompt", "needle")), &m)
	var out []string
	sessCollectText(m, 0, &out)
	if strings.Join(out, "") != "needle" {
		t.Errorf("real text lost: %q", out)
	}
}

func TestFileURLBecomesAPath(t *testing.T) {
	for in, want := range map[string]string{
		"file:///home/u/plain":        "/home/u/plain",
		"file:///home/u/My%20Project": "/home/u/My Project",
		"file:///home/u/%E4%B8%AD":    "/home/u/\u4e2d",
		"file:///C:/work/thing":       "C:/work/thing",
		"file://server/share/x":       "//server/share/x",
		"/already/a/path":             "/already/a/path",
	} {
		if got := sessFileURLPath(in); got != want {
			t.Errorf("sessFileURLPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestArchivedRowsOfPathShapedToolsCarryTheirProject(t *testing.T) {
	// The archive's `project:` filter reads a column the SYNC writes. A source
	// whose transcript never names its own working directory has to be asked
	// the same way the live readers ask it, or the archive and the machine
	// disagree about which project a session belongs to (review 2026-08-19).
	base, sources := sessNewWorld(t)
	st := syncTestState()
	for _, c := range []struct{ agent, path, project, title string }{
		{"cursor", filepath.Join(base, "cursor-projects", "home-tester-widgets",
			"agent-transcripts", "11111111-2222-3333-4444-555555555555",
			"11111111-2222-3333-4444-555555555555.jsonl"), "widgets", "the kettle is boiling"},
		{"grok", filepath.Join(base, "grok-sessions", "%2Fhome%2Ftester%2Fwidgets",
			"22222222-2222-3333-4444-555555555555", "chat_history.jsonl"), "widgets", "grok was asked about the kettle"},
		{"kiro", filepath.Join(base, "kiro-cli",
			"33333333-2222-3333-4444-555555555555.jsonl"), "widgets", "kiro heard about the kettle"},
	} {
		_, _, fs := syncScanOne(t, sources, c.path, st)
		if fs.Proj != c.project {
			t.Errorf("%s archived with project %q, want %q", c.agent, fs.Proj, c.project)
		}
		// And the title is the one the listing shows, envelope removed.
		if fs.Title != c.title {
			t.Errorf("%s archived title %q, want %q", c.agent, fs.Title, c.title)
		}
	}
}

func TestAFullyReadTranscriptStillLearnsItsProject(t *testing.T) {
	// The nothing-new exit comes before the scan loop. A transcript archived
	// once and never appended to again would otherwise keep an empty project
	// for as long as it existed.
	base, sources := sessNewWorld(t)
	path := filepath.Join(base, "kiro-cli", "33333333-2222-3333-4444-555555555555.jsonl")
	st := syncTestState()
	b, _, fs := syncScanOne(t, sources, path, st)
	if b == nil {
		t.Fatal("first pass produced no batch")
	}
	syncStore(fs, b)
	fs.Proj = "" // as a row written before the source knew where to look
	if b2, _, fs2 := syncScanOne(t, sources, path, st); b2 != nil {
		t.Fatalf("nothing was appended; want no batch, got %d msgs", b2.msgs)
	} else if fs2.Proj != "widgets" {
		t.Errorf("a finished transcript never learned its project: %q", fs2.Proj)
	}
}
