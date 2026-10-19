package main

// `where`'s promises: the directory comes from the transcript's head (or the
// source's own hook), only ever absolute, with an honest note when it is gone;
// not-found only off a complete walk; and Cursor's lossy slug is resolved
// against the real filesystem, with the CLI's own chat store choosing between
// readings and deciding whether the chat can be resumed at all.

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sessWhereSoon() time.Time { return time.Now().Add(time.Minute) }

func TestSessionsWhereClaudeFromHead(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	proj := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "11111111-2222-3333-4444-555555555555"
	sessWrite(t, filepath.Join(claude, "-x-proj", id+".jsonl"), time.Now(),
		`{"type":"summary","summary":"no cwd on this line"}`,
		claudeUserLine("2026-08-17T10:00:00Z", proj, "hello"),
		claudeLine("2026-08-17T10:00:01Z", "/somewhere/else", "later lines do not move it"))
	rep := sessionsWhere(sources, id, sessWhereSoon())
	if rep.Error != "" || rep.Session == nil || rep.Session.Agent != "claude" {
		t.Fatalf("want the claude session: %+v", rep)
	}
	if rep.Cwd != proj || !rep.Here || rep.Session.Project != "proj" {
		t.Fatalf("want %q present, got %+v", proj, rep)
	}
}

func TestSessionsWhereCodexMetaAndGoneDirectory(t *testing.T) {
	_, codex, sources := sessTestWorld(t)
	gone := filepath.Join(t.TempDir(), "deleted-since")
	id := "99999999-8888-7777-6666-555555555555"
	sessWrite(t, filepath.Join(codex, "2026", "08", "17", "rollout-x-"+id+".jsonl"), time.Now(),
		codexMetaLine("2026-08-17T11:00:00Z", gone),
		codexMsgLine("2026-08-17T11:00:01Z", "deploy"))
	rep := sessionsWhere(sources, id, sessWhereSoon())
	if rep.Cwd != gone || rep.Here {
		t.Fatalf("a directory that no longer exists is still named, and said to be gone: %+v", rep)
	}
}

func TestSessionsWhereRefusesRelativeCwd(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	sessWrite(t, filepath.Join(claude, "p", "a.jsonl"), time.Now(),
		claudeUserLine("2026-08-17T10:00:00Z", "relative/dir", "hi"))
	rep := sessionsWhere(sources, "a", sessWhereSoon())
	if rep.Cwd != "" || rep.Here || rep.Session == nil {
		t.Fatalf("a relative cwd is nowhere a terminal can stand: %+v", rep)
	}
}

func TestSessionsWhereNotFoundAndPartial(t *testing.T) {
	_, _, sources := sessTestWorld(t)
	if rep := sessionsWhere(sources, "nope", sessWhereSoon()); rep.Error != "not-found" || rep.Partial {
		t.Fatalf("a complete walk may say not-found: %+v", rep)
	}
	rep := sessionsWhere(sources, "nope", time.Now().Add(-time.Second))
	if rep.Error != "" || !rep.Partial {
		t.Fatalf("an incomplete walk must not: %+v", rep)
	}
}

// Cursor's source as the helper really builds it, pointed at a test tree:
// the real matcher, so the fixture cannot drift from the rule.
func sessCursorTestSource(t *testing.T) (projects string, sources []sessSource) {
	t.Helper()
	projects = filepath.Join(t.TempDir(), ".cursor", "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessSources() {
		if s.agent == "cursor" {
			s.root = projects
			return projects, []sessSource{s}
		}
	}
	t.Fatal("no cursor source")
	return "", nil
}

func sessCursorSlug(dir string) string {
	return strings.TrimPrefix(sessSlugName(filepath.ToSlash(dir)), "-")
}

func sessCursorChat(t *testing.T, projects, dir, id string) {
	t.Helper()
	sum := md5.Sum([]byte(dir))
	if err := os.MkdirAll(filepath.Join(filepath.Dir(projects), "chats", hex.EncodeToString(sum[:]), id), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSessionsWhereCursorResolvesSlug(t *testing.T) {
	projects, sources := sessCursorTestSource(t)
	dir := filepath.Join(t.TempDir(), "my.app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	sessWrite(t, filepath.Join(projects, sessCursorSlug(dir), "agent-transcripts", id, id+".jsonl"), time.Now(),
		`{"role":"user","message":{"content":[{"type":"text","text":"hi"}]}}`)

	rep := sessionsWhere(sources, id, sessWhereSoon())
	if rep.Cwd != dir || !rep.Here || !rep.EditorOnly {
		t.Fatalf("with no CLI chat it is an editor chat, still placed: %+v", rep)
	}
	sessCursorChat(t, projects, dir, id)
	rep = sessionsWhere(sources, id, sessWhereSoon())
	if rep.Cwd != dir || rep.EditorOnly {
		t.Fatalf("the CLI's chat store makes it resumable: %+v", rep)
	}
}

func TestSessionsWhereCursorChatBreaksTheTie(t *testing.T) {
	projects, sources := sessCursorTestSource(t)
	base := t.TempDir()
	flat := filepath.Join(base, "x-y")
	nested := filepath.Join(base, "x", "y")
	for _, d := range []string{flat, nested} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if sessCursorSlug(flat) != sessCursorSlug(nested) {
		t.Fatal("fixture: the two must slug alike")
	}
	id := "12121212-3434-5656-7878-909090909090"
	sessWrite(t, filepath.Join(projects, sessCursorSlug(flat), "agent-transcripts", id, id+".jsonl"), time.Now(),
		`{"role":"user","message":{"content":[{"type":"text","text":"hi"}]}}`)

	if rep := sessionsWhere(sources, id, sessWhereSoon()); rep.Cwd != flat {
		t.Fatalf("with nothing to decide, the shallow reading: %+v", rep)
	}
	sessCursorChat(t, projects, nested, id)
	if rep := sessionsWhere(sources, id, sessWhereSoon()); rep.Cwd != nested || rep.EditorOnly {
		t.Fatalf("the chat store names the real one: %+v", rep)
	}
}

func TestSessSlugName(t *testing.T) {
	for in, want := range map[string]string{
		"web-app": "web-app",
		".config": "-config",
		"a_b.c d": "a-b-c-d",
		"DLR":     "DLR",
		"café":    "caf-",
	} {
		if got := sessSlugName(in); got != want {
			t.Errorf("sessSlugName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunSessionsWhereCLIRefusesBadID(t *testing.T) {
	if code := runSessionsWhereCLI([]string{"--id", "../etc"}); code != 2 {
		t.Fatalf("an id that is not one: exit %d", code)
	}
	if code := runSessionsWhereCLI([]string{"--bogus"}); code != 2 {
		t.Fatalf("an unknown flag: exit %d", code)
	}
}
