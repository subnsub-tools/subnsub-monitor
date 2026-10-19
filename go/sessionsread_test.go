package main

// The reader's promises: `recent` lists newest-first with the first real
// user words as the title, `open` pages by transcript line with honest
// more_before/more_after at every edge, both verbs hold the search's read
// discipline (roots, link counts, redaction, the 16 KiB frame), and a search
// hit now carries the line an `open --around` can go back to.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func claudeUserLine(ts, cwd, text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user", "timestamp": ts, "cwd": cwd, "sessionId": "unused",
		"message": map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	})
	return string(b)
}

func claudeToolLine(ts, cwd, text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user", "timestamp": ts, "cwd": cwd,
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":    "tool_result",
				"content": []any{map[string]any{"type": "text", "text": text}},
			}},
		},
	})
	return string(b)
}

func TestSessionsSearchHitCarriesLine(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeLine("2026-08-17T10:00:00Z", "/p", "nothing here"),
		claudeLine("2026-08-17T10:01:00Z", "/p", "the needle line"))
	rep := sessionsSearch(sources, sessAsk("needle"))
	if len(rep.Matches) != 1 || rep.Matches[0].Line != 2 {
		t.Fatalf("the hit should carry its transcript line: %+v", rep.Matches)
	}
}

func TestSessionsRecentNewestFirstAcrossAgents(t *testing.T) {
	claude, codex, sources := sessTestWorld(t)
	now := time.Now()
	sessWrite(t, filepath.Join(claude, "-home-u-proj", "11111111-2222-3333-4444-555555555555.jsonl"),
		now.Add(-2*time.Hour),
		claudeUserLine("2026-08-17T10:00:00Z", "/home/u/proj", "fix the flux capacitor"))
	sessWrite(t, filepath.Join(codex, "2026", "08", "17", "rollout-x-99999999-8888-7777-6666-555555555555.jsonl"),
		now.Add(-1*time.Hour),
		codexMetaLine("2026-08-17T11:00:00Z", "/home/u/other"),
		codexMsgLine("2026-08-17T11:00:01Z", "please deploy the relay"))

	rep := sessionsRecent(sources, 10, sessFilter{}, time.Now().Add(time.Minute))
	if len(rep.Sessions) != 2 {
		t.Fatalf("want both sessions: %+v", rep.Sessions)
	}
	first, second := rep.Sessions[0], rep.Sessions[1]
	if first.Agent != "codex" || second.Agent != "claude" {
		t.Fatalf("newest first broken: %q then %q", first.Agent, second.Agent)
	}
	if first.Project != "other" || second.Project != "proj" {
		t.Fatalf("projects wrong: %q, %q", first.Project, second.Project)
	}
	if first.Title != "please deploy the relay" || second.Title != "fix the flux capacitor" {
		t.Fatalf("titles wrong: %q, %q", first.Title, second.Title)
	}
	if second.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("session id wrong: %q", second.SessionID)
	}
	if rep.Partial {
		t.Fatal("nothing here was partial")
	}
}

func TestSessionsRecentTitleSkipsPreludes(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	secret := "the api_key=verysecretvalue1 must hide"
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeUserLine("2026-08-17T10:00:00Z", "/p", "<command-name>/model</command-name>"),
		claudeUserLine("2026-08-17T10:00:01Z", "/p", "Caveat: the messages below were generated"),
		claudeLine("2026-08-17T10:00:02Z", "/p", "assistant words must not title"),
		claudeToolLine("2026-08-17T10:00:03Z", "/p", "tool output must not title"),
		claudeUserLine("2026-08-17T10:00:04Z", "/p", secret))
	rep := sessionsRecent(sources, 10, sessFilter{}, time.Now().Add(time.Minute))
	if len(rep.Sessions) != 1 {
		t.Fatalf("want one session: %+v", rep.Sessions)
	}
	title := rep.Sessions[0].Title
	if !strings.HasPrefix(title, "the ") || strings.Contains(title, "verysecretvalue1") {
		t.Fatalf("title should be the real prompt, redacted: %q", title)
	}
	if !strings.Contains(title, "[redacted]") {
		t.Fatalf("redaction should mark the title: %q", title)
	}
}

func TestSessionsRecentUntitledStillListed(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeLine("2026-08-17T10:00:00Z", "/p", "assistant only, no user words"))
	rep := sessionsRecent(sources, 10, sessFilter{}, time.Now().Add(time.Minute))
	if len(rep.Sessions) != 1 || rep.Sessions[0].Title != "" {
		t.Fatalf("a title-less transcript still lists, untitled: %+v", rep.Sessions)
	}
	if rep.Sessions[0].Project != "p" {
		t.Fatalf("project should still come from the head: %+v", rep.Sessions[0])
	}
}

func TestSessionsRecentHonoursLimit(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 5; i++ {
		sessWrite(t, filepath.Join(claude, "p", fmt.Sprintf("s%d.jsonl", i)),
			base.Add(time.Duration(i)*time.Hour),
			claudeUserLine("2026-08-17T10:00:00Z", "/p", fmt.Sprintf("prompt %d", i)))
	}
	rep := sessionsRecent(sources, 2, sessFilter{}, time.Now().Add(time.Minute))
	if len(rep.Sessions) != 2 || rep.Files != 5 {
		t.Fatalf("limit or files wrong: %d of %d", len(rep.Sessions), rep.Files)
	}
	if rep.Sessions[0].Title != "prompt 4" || rep.Sessions[1].Title != "prompt 3" {
		t.Fatalf("the newest two should list: %+v", rep.Sessions)
	}
	if rep.Partial {
		t.Fatal("a filled limit is not partial")
	}
}

// A ten-message session read like the dashboard reads it: the tail first,
// then earlier from the tail's first line, then later from an inner line —
// and the more_* flags true exactly when the transcript continues that way.
func TestSessionsOpenTailAndPaging(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, claudeUserLine(fmt.Sprintf("2026-08-17T10:%02d:00Z", i), "/p", fmt.Sprintf("message %d", i)))
	}
	sessWrite(t, filepath.Join(claude, "p", "11111111-2222-3333-4444-555555555555.jsonl"), time.Now(), lines...)
	id := "11111111-2222-3333-4444-555555555555"
	deadline := time.Now().Add(time.Minute)

	rep := sessionsOpen(sources, id, "tail", 0, deadline)
	if rep.Error != "" || rep.Session == nil || rep.Session.Project != "p" || rep.Session.Agent != "claude" {
		t.Fatalf("session meta wrong: %+v", rep)
	}
	if len(rep.Messages) != 10 || rep.MoreBefore || rep.MoreAfter {
		t.Fatalf("ten messages fit one page: %d, before=%v after=%v", len(rep.Messages), rep.MoreBefore, rep.MoreAfter)
	}
	if rep.Messages[0].Line != 1 || rep.Messages[9].Line != 10 {
		t.Fatalf("lines wrong: %+v", rep.Messages)
	}
	if rep.Messages[3].Role != "user" || rep.Messages[3].Text != "message 3" || rep.Messages[3].Time != "2026-08-17T10:03:00Z" {
		t.Fatalf("message shape wrong: %+v", rep.Messages[3])
	}

	before := sessionsOpen(sources, id, "before", 4, deadline)
	if len(before.Messages) != 3 || before.Messages[0].Line != 1 || before.Messages[2].Line != 3 {
		t.Fatalf("before-4 should be lines 1..3: %+v", before.Messages)
	}
	if before.MoreBefore || !before.MoreAfter {
		t.Fatalf("before-4 edges wrong: before=%v after=%v", before.MoreBefore, before.MoreAfter)
	}

	after := sessionsOpen(sources, id, "after", 8, deadline)
	if len(after.Messages) != 2 || after.Messages[0].Line != 9 || after.MoreAfter || !after.MoreBefore {
		t.Fatalf("after-8 should be lines 9..10: %+v", after)
	}
}

func TestSessionsOpenAroundCentresTheAnchor(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, claudeUserLine("2026-08-17T10:00:00Z", "/p", fmt.Sprintf("message %d", i)))
	}
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(), lines...)
	rep := sessionsOpen(sources, "a", "around", 50, time.Now().Add(time.Minute))
	if len(rep.Messages) != sessPageMsgs {
		t.Fatalf("a long transcript fills the page: %d", len(rep.Messages))
	}
	if !rep.MoreBefore || !rep.MoreAfter {
		t.Fatal("both edges continue here")
	}
	seen := false
	for _, m := range rep.Messages {
		if m.Line == 50 {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the anchor line must be on its own page: %d..%d",
			rep.Messages[0].Line, rep.Messages[len(rep.Messages)-1].Line)
	}
	first, last := rep.Messages[0].Line, rep.Messages[len(rep.Messages)-1].Line
	if 50-first < 10 || last-50 < 10 {
		t.Fatalf("the anchor should sit near the middle: %d..%d", first, last)
	}
}

func TestSessionsOpenNotFound(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeUserLine("2026-08-17T10:00:00Z", "/p", "hello"))
	rep := sessionsOpen(sources, "no-such-id", "tail", 0, time.Now().Add(time.Minute))
	if rep.Error != "not-found" || len(rep.Messages) != 0 {
		t.Fatalf("an unknown id is a ran-and-answered miss: %+v", rep)
	}
	out, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(out), `"messages":[]`) {
		t.Fatalf("an empty answer must be [], not null: %s", out)
	}
}

func TestSessionsOpenRefusesSecondHardName(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	p := filepath.Join(claude, "p", "a.jsonl")
	sessWrite(t, p, time.Now(), claudeUserLine("2026-08-17T10:00:00Z", "/p", "linked words"))
	if err := os.Link(p, filepath.Join(claude, "p", "b.jsonl")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	rep := sessionsOpen(sources, "a", "tail", 0, time.Now().Add(time.Minute))
	if len(rep.Messages) != 0 || !rep.Partial {
		t.Fatalf("a file with two names must be refused and admitted: %+v", rep)
	}
}

func TestSessionsOpenCutsAndRedactsMessages(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	long := "the DB_PASSWORD=hunter2hunter2 sits here " + strings.Repeat("padding ", 200)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeUserLine("2026-08-17T10:00:00Z", "/p", long))
	rep := sessionsOpen(sources, "a", "tail", 0, time.Now().Add(time.Minute))
	if len(rep.Messages) != 1 {
		t.Fatalf("want the one message: %+v", rep)
	}
	m := rep.Messages[0]
	if !m.Cut || len([]rune(m.Text)) > sessMsgRunes {
		t.Fatalf("a long message is cut and says so: cut=%v len=%d", m.Cut, len([]rune(m.Text)))
	}
	if strings.Contains(m.Text, "hunter2") || !strings.Contains(m.Text, "[redacted]") {
		t.Fatalf("message text leaked: %q", m.Text)
	}
}

func TestSessRoleShapes(t *testing.T) {
	parse := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	cases := map[string]string{
		claudeUserLine("t", "/p", "x"):                "user",
		claudeLine("t", "/p", "x"):                    "assistant",
		claudeToolLine("t", "/p", "x"):                "tool",
		codexMsgLine("t", "x"):                        "user",
		`{"payload":{"type":"reasoning"}}`:            "assistant",
		`{"payload":{"type":"function_call_output"}}`: "tool",
		`{"type":"system","content":"note"}`:          "",
	}
	for line, want := range cases {
		if got := sessRole(parse(line)); got != want {
			t.Errorf("sessRole(%s) = %q, want %q", line, got, want)
		}
	}
}

func TestSessValidIDAndParseLine(t *testing.T) {
	for _, ok := range []string{"11111111-2222-3333-4444-555555555555", "rollout-2026-08-17T09-00-00-x", "notes", "a.b_c-d"} {
		if !sessValidID(ok) {
			t.Errorf("sessValidID(%q) should accept", ok)
		}
	}
	for _, bad := range []string{"", "a b", "x/y", "x'y", strings.Repeat("a", sessIDMaxLen+1)} {
		if sessValidID(bad) {
			t.Errorf("sessValidID(%q) should refuse", bad)
		}
	}
	if n, err := sessParseLine("123456"); err != nil || n != 123456 {
		t.Errorf("sessParseLine(123456) = %d, %v", n, err)
	}
	for _, bad := range []string{"", "0", "-1", "12a", "1234567890"} {
		if _, err := sessParseLine(bad); err == nil {
			t.Errorf("sessParseLine(%q) should refuse", bad)
		}
	}
}

// The byte cap sheds the edge farther from the anchor and admits it in the
// more_* flags; the anchored line survives every shed.
func TestSessEncodeOpenCapped(t *testing.T) {
	rep := sessOpenReport{Messages: []sessMsg{}}
	for i := 1; i <= sessPageMsgs; i++ {
		rep.Messages = append(rep.Messages, sessMsg{
			Line: i, Role: "user", Time: "2026-08-17T10:00:00Z",
			Text: strings.Repeat("\u591a", sessMsgRunes),
		})
	}
	out, err := sessEncodeOpenCapped(&rep, "around", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > sessMaxJSONBytes {
		t.Fatalf("encoded answer still %d bytes", len(out))
	}
	if len(rep.Messages) == 0 || len(rep.Messages) >= sessPageMsgs {
		t.Fatalf("capping should shed some messages: %d left", len(rep.Messages))
	}
	if !rep.MoreAfter {
		t.Fatal("an anchor at the head sheds the tail, and must say so")
	}
	found := false
	for _, m := range rep.Messages {
		if m.Line == 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the anchor must survive the cap: %+v", rep.Messages[0])
	}
	var back sessOpenReport
	if err := json.Unmarshal(out, &back); err != nil || len(back.Messages) != len(rep.Messages) {
		t.Fatalf("capped document must still parse whole: %v", err)
	}
}

// A directory can be NAMED like a credential, and the project label must
// wear the same mask the message text does.
func TestSessProjectLabelRedacts(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeUserLine("2026-08-17T10:00:00Z", "/home/u/api_key=verysecretname9", "needle words"))
	rep := sessionsRecent(sources, 10, sessFilter{}, time.Now().Add(time.Minute))
	if len(rep.Sessions) != 1 || strings.Contains(rep.Sessions[0].Project, "verysecretname9") {
		t.Fatalf("the project label leaked: %+v", rep.Sessions)
	}
	srep := sessionsSearch(sources, sessAsk("needle"))
	if len(srep.Matches) != 1 || strings.Contains(srep.Matches[0].Project, "verysecretname9") {
		t.Fatalf("the search hit's project leaked: %+v", srep.Matches)
	}
}

// A user whose genuine first words open with `<` keeps their title; only the
// exact machinery preludes are skipped.
func TestSessionsRecentTitleKeepsAngleBracketPrompts(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeUserLine("2026-08-17T10:00:00Z", "/p", "<div> is overflowing on mobile, why?"))
	rep := sessionsRecent(sources, 10, sessFilter{}, time.Now().Add(time.Minute))
	if len(rep.Sessions) != 1 || !strings.HasPrefix(rep.Sessions[0].Title, "<div> is overflowing") {
		t.Fatalf("a real prompt starting with < must keep its title: %+v", rep.Sessions)
	}
}

// An anchored page whose scan was cut before it ever reached the anchor must
// come back empty rather than stitch a distant prefix onto the transcript.
func TestSessionsOpenCutBeforeAnchorServesNoPage(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a >8MB line")
	}
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeUserLine("2026-08-17T10:00:00Z", "/p", "message one"),
		claudeUserLine("2026-08-17T10:01:00Z", "/p", "message two"),
		claudeLine("2026-08-17T10:02:00Z", "/p", strings.Repeat("x", sessMaxLineBytes)),
		claudeUserLine("2026-08-17T10:03:00Z", "/p", "past the monster"))
	rep := sessionsOpen(sources, "a", "before", 1000, time.Now().Add(time.Minute))
	if len(rep.Messages) != 0 || !rep.Partial || !rep.MoreBefore {
		t.Fatalf("a cut short of the anchor must serve nothing and keep the button: %+v", rep)
	}
}

// A credential straddling the 320-rune cut must be masked before the cut can
// leave a recognisable prefix behind.
func TestSessParseMsgRedactsBeforeCutting(t *testing.T) {
	text := strings.Repeat("a", sessMsgRunes-10) + " DB_PASSWORD=supersecretvalue99 and much more after it"
	m, _, ok := sessParseMsg([]byte(claudeUserLine("2026-08-17T10:00:00Z", "/p", text)), 1)
	if !ok || !m.Cut {
		t.Fatalf("the long message should parse and be cut: ok=%v cut=%v", ok, m.Cut)
	}
	if strings.Contains(m.Text, "supersec") {
		t.Fatalf("the cut leaked a credential prefix: %q", m.Text[len(m.Text)-60:])
	}
}

// Codex tool replies live under payload.output, which search's collector
// ignores; the reader must still show them.
func TestSessParseMsgReadsCodexToolOutput(t *testing.T) {
	line, _ := json.Marshal(map[string]any{
		"timestamp": "2026-08-17T10:00:00Z", "type": "response_item",
		"payload": map[string]any{
			"type":    "function_call_output",
			"call_id": "c1",
			"output":  "exit 0: all tests passed",
		},
	})
	m, _, ok := sessParseMsg(line, 3)
	if !ok || m.Role != "tool" || !strings.Contains(m.Text, "all tests passed") {
		t.Fatalf("codex tool output should read as a tool message: ok=%v %+v", ok, m)
	}
}

// A scan that never reached the end of the file must not report the end of
// the transcript: the unreadable monster line stops the scanner, and the
// page it leaves behind has to keep its Later edge.
func TestSessionsOpenCutScanKeepsLater(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a >8MB line")
	}
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeUserLine("2026-08-17T10:00:00Z", "/p", "before the monster"),
		claudeLine("2026-08-17T10:01:00Z", "/p", strings.Repeat("x", sessMaxLineBytes)))
	rep := sessionsOpen(sources, "a", "tail", 0, time.Now().Add(time.Minute))
	if !rep.Partial || !rep.MoreAfter {
		t.Fatalf("a cut scan is partial and cannot deny a continuation: partial=%v more_after=%v", rep.Partial, rep.MoreAfter)
	}
}

// An unknown id off an INCOMPLETE candidate walk is not "not-found" — that
// verdict needs the whole list to have been seen.
func TestSessionsOpenPartialWalkIsNotNotFound(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeUserLine("2026-08-17T10:00:00Z", "/p", "here"))
	rep := sessionsOpen(sources, "a", "tail", 0, time.Now().Add(-time.Second))
	if rep.Error == "not-found" || !rep.Partial {
		t.Fatalf("an expired walk must answer partial, never not-found: %+v", rep)
	}
}

// The byte cap must hold for the FRAME the document rides in, where every
// quote and backslash is escaped a second time.
func TestSessEncodeOpenCappedByFrameCost(t *testing.T) {
	rep := sessOpenReport{Messages: []sessMsg{}}
	for i := 1; i <= sessPageMsgs; i++ {
		rep.Messages = append(rep.Messages, sessMsg{
			Line: i, Role: "assistant",
			Text: strings.Repeat("\"\\\n", sessMsgRunes/3),
		})
	}
	out, err := sessEncodeOpenCapped(&rep, "tail", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cost := sessFrameCost(out); cost > sessMaxJSONBytes {
		t.Fatalf("frame cost still %d bytes", cost)
	}
	if len(rep.Messages) == 0 || len(rep.Messages) >= sessPageMsgs {
		t.Fatalf("escape-heavy pages should shed messages: %d left", len(rep.Messages))
	}
}

func TestSessRecentReportJSONShape(t *testing.T) {
	out, err := json.Marshal(sessRecentReport{Sessions: []sessRecentEntry{}})
	if err != nil || !strings.Contains(string(out), `"sessions":[]`) {
		t.Fatalf("an empty listing must be [], not null: %s", out)
	}
	one, _ := json.Marshal(sessRecentEntry{Project: "p", Agent: "a", Timestamp: "t", SessionID: "s", Title: "x"})
	for _, key := range []string{`"project"`, `"agent"`, `"timestamp"`, `"session_id"`, `"title"`} {
		if !strings.Contains(string(one), key) {
			t.Fatalf("the dashboard renders %s and it is missing: %s", key, one)
		}
	}
}
