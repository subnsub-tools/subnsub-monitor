package main

// The archive pusher's promises: the cursor only ever moves over lines that
// arrived whole, a file that was replaced under the same name starts again
// rather than resuming into somebody else's conversation, a batch is bounded
// in bytes and in the memory one absurd line can cost, and nothing at all
// happens while the switch is off.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func syncTestState() *syncState {
	return &syncState{V: 1, Files: map[string]*syncFileState{}}
}

// What syncPass does to the cursor once the relay has stored a batch. Kept in
// one place so a test cannot accidentally advance it in a way production never
// would — the tail fingerprint in particular is what the next resume checks.
func syncStore(fs *syncFileState, b *syncBatch) {
	fs.Off = b.off
	fs.Line = b.line
	fs.Msgs += b.msgs
	fs.Seq++
	if b.tail != "" {
		fs.Tail = b.tail
	}
}

// One batch out of one file, with everything the pass would have set up.
func syncScanOne(t *testing.T, sources []sessSource, path string, st *syncState) (*syncBatch, bool, *syncFileState) {
	t.Helper()
	roots, files, _ := sessAllCandidates(sources, time.Now().Add(time.Minute))
	defer sessCloseRoots(roots)
	for _, f := range files {
		full := filepath.Join(sources[f.src].root, f.rel)
		if full != path {
			continue
		}
		key := syncKey(sources[f.src].agent, f.rel)
		fs := st.Files[key]
		if fs == nil {
			fs = &syncFileState{UID: syncUID(sources[f.src], f.rel)}
			st.Files[key] = fs
		}
		b, more, _ := syncScanFile(roots[f.src], sources[f.src], f, fs, time.Now().Add(time.Minute))
		return b, more, fs
	}
	t.Fatalf("no candidate matched %s", path)
	return nil, false, nil
}

// The metadata line and the message lines, split the way the relay splits them.
func syncParts(t *testing.T, b *syncBatch) (map[string]any, []syncMsgRec) {
	t.Helper()
	if b == nil {
		t.Fatal("no batch")
	}
	nl := strings.IndexByte(string(b.body), '\n')
	if nl < 0 {
		t.Fatalf("batch has no metadata line: %q", string(b.body))
	}
	var meta map[string]any
	if err := json.Unmarshal(b.body[:nl], &meta); err != nil {
		t.Fatalf("metadata line is not JSON: %v", err)
	}
	var msgs []syncMsgRec
	for _, line := range strings.Split(strings.TrimRight(string(b.body[nl+1:]), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m syncMsgRec
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("message line is not JSON: %v", err)
		}
		msgs = append(msgs, m)
	}
	return meta, msgs
}

func TestSyncSendsMessagesWithTranscriptLineNumbers(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "-home-u-proj", "11111111-2222-3333-4444-555555555555.jsonl")
	sessWrite(t, path, time.Now(),
		claudeUserLine("2026-08-18T10:00:00Z", "/home/u/proj", "archive this conversation"),
		claudeLine("2026-08-18T10:00:01Z", "/home/u/proj", "certainly"))

	st := syncTestState()
	b, more, fs := syncScanOne(t, sources, path, st)
	if more {
		t.Fatal("a two-line file should not report more to come")
	}
	meta, msgs := syncParts(t, b)
	if len(msgs) != 2 || msgs[0].N != 1 || msgs[1].N != 2 {
		t.Fatalf("line numbers must be the transcript's own: %+v", msgs)
	}
	if msgs[0].R != "user" || msgs[1].R != "assistant" {
		t.Fatalf("roles wrong: %+v", msgs)
	}
	if msgs[0].X != "archive this conversation" {
		t.Fatalf("text wrong: %q", msgs[0].X)
	}
	if meta["title"] != "archive this conversation" {
		t.Fatalf("title should be the first user words: %v", meta["title"])
	}
	if meta["project"] != "proj" {
		t.Fatalf("project label wrong: %v", meta["project"])
	}
	if meta["final"] != true {
		t.Fatalf("a batch that emptied the file is final: %v", meta["final"])
	}
	if fs.Title == "" || fs.Proj == "" {
		t.Fatal("the state should remember the title and project for later chunks")
	}
}

func TestSyncResumesFromTheCursor(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "p", "a.jsonl")
	sessWrite(t, path, time.Now(),
		claudeUserLine("2026-08-18T10:00:00Z", "/p", "first question"))

	st := syncTestState()
	b, _, fs := syncScanOne(t, sources, path, st)
	if b == nil {
		t.Fatal("first pass sent nothing")
	}
	syncStore(fs, b)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(claudeLine("2026-08-18T10:00:05Z", "/p", "second answer") + "\n")
	f.Close()

	b2, _, _ := syncScanOne(t, sources, path, st)
	meta, msgs := syncParts(t, b2)
	if len(msgs) != 1 || msgs[0].N != 2 || msgs[0].X != "second answer" {
		t.Fatalf("the second pass should send only what was appended: %+v", msgs)
	}
	if meta["seq"] != float64(1) {
		t.Fatalf("chunk numbers must advance: %v", meta["seq"])
	}
	if meta["title"] != "first question" {
		t.Fatalf("the title has to ride every chunk, not just the first: %v", meta["title"])
	}
}

func TestSyncLeavesAHalfWrittenTailAlone(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "p", "a.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	whole := claudeUserLine("2026-08-18T10:00:00Z", "/p", "complete line") + "\n"
	half := `{"type":"user","timestamp":"2026-08-18T10:00:01Z","mess`
	if err := os.WriteFile(path, []byte(whole+half), 0o644); err != nil {
		t.Fatal(err)
	}

	st := syncTestState()
	b, _, _ := syncScanOne(t, sources, path, st)
	_, msgs := syncParts(t, b)
	if len(msgs) != 1 {
		t.Fatalf("only the terminated line may be sent: %+v", msgs)
	}
	if b.off != int64(len(whole)) {
		t.Fatalf("the cursor must stop at the last newline, got %d want %d", b.off, len(whole))
	}
	// Now the writer finishes the line.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`age":{"role":"user","content":[{"type":"text","text":"finished"}]}}` + "\n")
	f.Close()

	fs := st.Files[syncKey("claude", "p/a.jsonl")]
	syncStore(fs, b)
	b2, _, _ := syncScanOne(t, sources, path, st)
	_, msgs2 := syncParts(t, b2)
	if len(msgs2) != 1 || msgs2[0].X != "finished" || msgs2[0].N != 2 {
		t.Fatalf("the completed line must arrive whole on the next pass: %+v", msgs2)
	}
}

func TestSyncRestartsWhenTheFileWasReplaced(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "p", "a.jsonl")
	sessWrite(t, path, time.Now(),
		claudeUserLine("2026-08-18T10:00:00Z", "/p", "the original conversation"),
		claudeLine("2026-08-18T10:00:01Z", "/p", "the original answer"))

	st := syncTestState()
	b, _, fs := syncScanOne(t, sources, path, st)
	syncStore(fs, b)

	// Same path, different file: a restore, a rotation, a copy.
	sessWrite(t, path, time.Now(),
		claudeUserLine("2026-08-18T11:00:00Z", "/p", "a completely different conversation"),
		claudeLine("2026-08-18T11:00:01Z", "/p", "and its answer"))

	b2, _, fs2 := syncScanOne(t, sources, path, st)
	meta, msgs := syncParts(t, b2)
	if len(msgs) != 2 || msgs[0].N != 1 {
		t.Fatalf("a replaced file must be read from line one: %+v", msgs)
	}
	if meta["seq"] != float64(0) {
		t.Fatalf("a restart must begin a fresh chunk series: %v", meta["seq"])
	}
	if fs2.Msgs != 0 {
		t.Fatalf("the message tally restarts with the file: %d", fs2.Msgs)
	}
}

func TestSyncRestartsWhenTheFileShrank(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "p", "a.jsonl")
	sessWrite(t, path, time.Now(),
		claudeUserLine("2026-08-18T10:00:00Z", "/p", "one"),
		claudeUserLine("2026-08-18T10:00:01Z", "/p", "two"),
		claudeUserLine("2026-08-18T10:00:02Z", "/p", "three"))
	st := syncTestState()
	b, _, fs := syncScanOne(t, sources, path, st)
	syncStore(fs, b)

	// Truncated to its first line, head unchanged — only the size betrays it.
	sessWrite(t, path, time.Now(), claudeUserLine("2026-08-18T10:00:00Z", "/p", "one"))
	b2, _, _ := syncScanOne(t, sources, path, st)
	_, msgs := syncParts(t, b2)
	if len(msgs) != 1 || msgs[0].N != 1 {
		t.Fatalf("a shrunk file restarts: %+v", msgs)
	}
}

func TestSyncBatchStopsAtTheByteCap(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "p", "big.jsonl")
	// Each line carries ~2 KB of text, so the cap lands well inside the file.
	body := strings.Repeat("needle ", 300)
	lines := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		lines = append(lines, claudeUserLine("2026-08-18T10:00:00Z", "/p", body))
	}
	sessWrite(t, path, time.Now(), lines...)

	st := syncTestState()
	b, more, _ := syncScanOne(t, sources, path, st)
	if !more {
		t.Fatal("a file bigger than one batch must report more to come")
	}
	if len(b.body) > syncBatchBytes*2 {
		t.Fatalf("batch ran away: %d bytes", len(b.body))
	}
	meta, msgs := syncParts(t, b)
	if len(msgs) == 0 || len(msgs) >= 200 {
		t.Fatalf("the cap should take a slice, not all or nothing: %d", len(msgs))
	}
	if meta["final"] != false {
		t.Fatalf("a batch with more behind it is not final: %v", meta["final"])
	}
	if b.off <= 0 || b.line != len(msgs) {
		t.Fatalf("cursor and line count disagree: off=%d line=%d msgs=%d", b.off, b.line, len(msgs))
	}
}

func TestSyncAdvancesPastLinesThatCarryNoMessage(t *testing.T) {
	_, codex, sources := sessTestWorld(t)
	path := filepath.Join(codex, "2026", "rollout-x-99999999-8888-7777-6666-555555555555.jsonl")
	// Nothing but structure: a session_meta line has a cwd and no text.
	sessWrite(t, path, time.Now(), codexMetaLine("2026-08-18T10:00:00Z", "/home/u/other"))

	st := syncTestState()
	b, more, fs := syncScanOne(t, sources, path, st)
	if b != nil {
		t.Fatalf("a file of pure plumbing has nothing to send: %q", string(b.body))
	}
	if more {
		t.Fatal("and nothing left behind either")
	}
	if fs.Off == 0 || fs.Line != 1 {
		t.Fatalf("the cursor must still move past it: off=%d line=%d", fs.Off, fs.Line)
	}
	if fs.Proj != "other" {
		t.Fatalf("its cwd is still worth keeping: %q", fs.Proj)
	}
}

func TestSyncKeepsWholeMessagesNotTheReadersPreview(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "p", "a.jsonl")
	long := strings.Repeat("x", 2000)
	sessWrite(t, path, time.Now(), claudeUserLine("2026-08-18T10:00:00Z", "/p", long))

	st := syncTestState()
	b, _, _ := syncScanOne(t, sources, path, st)
	_, msgs := syncParts(t, b)
	if len(msgs) != 1 {
		t.Fatalf("want one message: %+v", msgs)
	}
	if len([]rune(msgs[0].X)) != 2000 {
		t.Fatalf("the archive keeps the message, not the 320-rune preview: %d runes", len([]rune(msgs[0].X)))
	}
	if msgs[0].C {
		t.Fatal("nothing was cut, so nothing should claim it was")
	}
}

func TestSyncRedactsBeforeTheArchiveEverSeesIt(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "p", "a.jsonl")
	sessWrite(t, path, time.Now(),
		claudeUserLine("2026-08-18T10:00:00Z", "/p", "deploy with sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKKKLLLL now"))

	st := syncTestState()
	b, _, _ := syncScanOne(t, sources, path, st)
	if strings.Contains(string(b.body), "AAAABBBBCCCC") {
		t.Fatalf("a credential reached the wire: %q", string(b.body))
	}
}

func TestSyncReadLineDoesNotHoldAnAbsurdLine(t *testing.T) {
	huge := strings.Repeat("z", 200000) + "\n"
	br := bufio.NewReaderSize(strings.NewReader(huge+"after\n"), 4096)
	line, n, complete, err := syncReadLine(br, 1000)
	if err != nil || !complete {
		t.Fatalf("the long line is still a complete line: complete=%v err=%v", complete, err)
	}
	if n != int64(len(huge)) {
		t.Fatalf("every byte of it must be consumed: %d want %d", n, len(huge))
	}
	if len(line) != 1000 {
		t.Fatalf("only the cap may be held: %d bytes", len(line))
	}
	line2, _, complete2, _ := syncReadLine(br, 1000)
	if !complete2 || string(line2) != "after" {
		t.Fatalf("the reader must land on the next line: %q", string(line2))
	}
}

func TestSyncReadLineHandlesCRLF(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("one\r\ntwo\r\n"), 64)
	line, _, complete, _ := syncReadLine(br, 100)
	if !complete || string(line) != "one" {
		t.Fatalf("carriage returns are not part of the line: %q", string(line))
	}
}

func TestSyncUIDIsStableAndDistinguishesSameStem(t *testing.T) {
	a := syncUID(sessSource{agent: "claude"}, "proj-one/11111111-2222-3333-4444-555555555555.jsonl")
	again := syncUID(sessSource{agent: "claude"}, "proj-one/11111111-2222-3333-4444-555555555555.jsonl")
	b := syncUID(sessSource{agent: "claude"}, "proj-two/11111111-2222-3333-4444-555555555555.jsonl")
	c := syncUID(sessSource{agent: "codex"}, "proj-one/11111111-2222-3333-4444-555555555555.jsonl")
	if a != again {
		t.Fatal("the same file must always key the same")
	}
	if a == b || a == c || b == c {
		t.Fatalf("same stem in different places must not collide: %q %q %q", a, b, c)
	}
	if !strings.HasPrefix(a, "11111111-2222-3333-4444-555555555555-") {
		t.Fatalf("the displayed id should still lead: %q", a)
	}
}

func TestSyncStateRoundTripsAndPrunes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if configDir() == "" {
		t.Skip("no config dir on this platform")
	}
	st := syncTestState()
	st.Files["claude/a.jsonl"] = &syncFileState{UID: "u1", Off: 42, Line: 3, Seq: 1, Seen: time.Now().Unix()}
	st.Files["claude/old.jsonl"] = &syncFileState{UID: "u2", Off: 7, Seen: time.Now().Add(-90 * 24 * time.Hour).Unix()}
	if err := saveSyncState(st); err != nil {
		t.Fatal(err)
	}
	back := loadSyncState()
	if back.Files["claude/a.jsonl"] == nil || back.Files["claude/a.jsonl"].Off != 42 {
		t.Fatalf("cursor lost in the round trip: %+v", back.Files)
	}
	if back.Files["claude/old.jsonl"] != nil {
		t.Fatal("an entry older than the TTL should have been pruned on save")
	}
	if fi, err := os.Stat(syncStatePath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the state file must not be world readable: %v %v", fi, err)
	}
}

func TestSyncSwitchIsOffUntilTurnedOn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(syncEnvVar, "")
	if configDir() == "" {
		t.Skip("no config dir on this platform")
	}
	if syncEnabled() {
		t.Fatal("a fresh install must not archive")
	}
	if err := setSync(true); err != nil {
		t.Fatal(err)
	}
	if !syncEnabled() {
		t.Fatal("the switch did not take")
	}
	// Off leaves the cursors alone: the archive has not forgotten anything.
	st := syncTestState()
	st.Files["claude/a.jsonl"] = &syncFileState{UID: "u", Off: 9, Seen: time.Now().Unix()}
	if err := saveSyncState(st); err != nil {
		t.Fatal(err)
	}
	if err := setSync(false); err != nil {
		t.Fatal(err)
	}
	if syncEnabled() {
		t.Fatal("off did not take")
	}
	if loadSyncState().Files["claude/a.jsonl"] == nil {
		t.Fatal("turning it off must not discard what the archive already holds")
	}
}

func TestSyncEnvironmentOverridesTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv(syncEnvVar, "1")
	if !syncEnabled() {
		t.Fatal("MON_SYNC=1 should turn it on without a file")
	}
	t.Setenv(syncEnvVar, "off")
	if syncEnabled() {
		t.Fatal("MON_SYNC=off should win over anything on disk")
	}
}

func TestSyncRestartsWhenTheFileWasRewrittenPastItsOldLength(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "p", "a.jsonl")
	// Same opening line in both versions, and the replacement is LONGER than
	// the cursor — so neither the head fingerprint nor the size can tell them
	// apart. Only the bytes ending at the cursor can.
	head := claudeUserLine("2026-08-18T10:00:00Z", "/p", "the shared opening")
	sessWrite(t, path, time.Now(), head,
		claudeLine("2026-08-18T10:00:01Z", "/p", "the original second line"))

	st := syncTestState()
	b, _, fs := syncScanOne(t, sources, path, st)
	syncStore(fs, b)
	if fs.Tail == "" {
		t.Fatal("a stored batch must leave a cursor fingerprint behind")
	}

	sessWrite(t, path, time.Now(), head,
		claudeLine("2026-08-18T11:00:01Z", "/p", "a completely different second line, longer than before"),
		claudeLine("2026-08-18T11:00:02Z", "/p", "and a third"))

	b2, _, fs2 := syncScanOne(t, sources, path, st)
	meta, msgs := syncParts(t, b2)
	if len(msgs) != 3 || msgs[0].N != 1 {
		t.Fatalf("a rewritten file must be read from line one: %+v", msgs)
	}
	if meta["seq"] != float64(0) {
		t.Fatalf("and under a fresh chunk series: %v", meta["seq"])
	}
	if fs2.Msgs != 0 {
		t.Fatalf("with its tallies reset: %d", fs2.Msgs)
	}
}

func TestSyncRestartDropsTheOldSessionsTitle(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "old-proj", "a.jsonl")
	sessWrite(t, path, time.Now(),
		claudeUserLine("2026-08-18T10:00:00Z", "/home/u/old-proj", "the first conversation"))
	st := syncTestState()
	b, _, fs := syncScanOne(t, sources, path, st)
	syncStore(fs, b)
	if fs.Title != "the first conversation" || fs.Proj != "old-proj" {
		t.Fatalf("setup wrong: %q %q", fs.Title, fs.Proj)
	}

	sessWrite(t, path, time.Now(),
		claudeUserLine("2026-08-18T11:00:00Z", "/home/u/new-proj", "the second conversation"))
	b2, _, _ := syncScanOne(t, sources, path, st)
	meta, _ := syncParts(t, b2)
	if meta["title"] != "the second conversation" {
		t.Fatalf("a new series must not wear the old one's title: %v", meta["title"])
	}
	if meta["project"] != "new-proj" {
		t.Fatalf("nor its project: %v", meta["project"])
	}
}

func TestSyncChunkCountsAreNotCumulative(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	path := filepath.Join(claude, "p", "big.jsonl")
	body := strings.Repeat("chatter ", 300)
	lines := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		lines = append(lines, claudeUserLine("2026-08-18T10:00:00Z", "/p", body))
	}
	sessWrite(t, path, time.Now(), lines...)

	st := syncTestState()
	b1, _, fs := syncScanOne(t, sources, path, st)
	m1, msgs1 := syncParts(t, b1)
	if int(m1["msgs"].(float64)) != len(msgs1) {
		t.Fatalf("chunk 0 miscounts itself: %v vs %d", m1["msgs"], len(msgs1))
	}
	syncStore(fs, b1)

	b2, _, _ := syncScanOne(t, sources, path, st)
	m2, msgs2 := syncParts(t, b2)
	if int(m2["msgs"].(float64)) != len(msgs2) {
		t.Fatalf("chunk 1 counts earlier chunks too: %v vs %d", m2["msgs"], len(msgs2))
	}
	if int(m2["total_msgs"].(float64)) != len(msgs1)+len(msgs2) {
		t.Fatalf("the running total should be the sum: %v vs %d", m2["total_msgs"], len(msgs1)+len(msgs2))
	}
}

func TestSyncSendsAnIdTheRelayWillAccept(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	// A transcript nobody generated but somebody can create: spaces in a stem
	// the relay's id shape refuses. The batch must still be storable.
	path := filepath.Join(claude, "p", "notes 1.jsonl")
	sessWrite(t, path, time.Now(),
		claudeUserLine("2026-08-18T10:00:00Z", "/p", "a hand named transcript"))

	st := syncTestState()
	b, _, fs := syncScanOne(t, sources, path, st)
	meta, _ := syncParts(t, b)
	sid, _ := meta["sid"].(string)
	if !sessValidID(sid) {
		t.Fatalf("the sid must be a shape the relay accepts: %q", sid)
	}
	if !strings.HasPrefix(fs.UID, sid+"-") {
		t.Fatalf("key and label must agree on the file: uid=%q sid=%q", fs.UID, sid)
	}
}
