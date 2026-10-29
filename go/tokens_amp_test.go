package main

// Amp's token counts (tokens_amp.go), with the CLI stood in for: threads
// listed and exported, a thread exported again only when its `updated`
// moves, the main agent's replies and the subagents' inferences counted, the
// input that is not cache out of either way of stating it, the counts kept
// across a restart, another machine's thread exported once and never counted
// here, a thread gone from the listing keeping what it spent, and a legacy
// thread file counted only when the CLI does not name its thread.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const ampT0 = 1793200000

func ampStamp(sec int64) string { return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z") }

const ampHere = "11111111-2222-4333-8444-555555555555"

// A thread as `amp threads export` writes it, begun on the Amp installation
// `inst`: a user turn, a Claude reply, a tool result carrying a subagent's
// inference, a GPT reply that counts its cache inside its input.
func ampThread(id string, at int64) string { return ampThreadOn(id, at, ampHere) }

func ampThreadOn(id string, at int64, inst string) string {
	return fmt.Sprintf(`{"v":3,"id":%q,"created":%d,"title":"t","env":{"initial":{"hostname":"elsewhere-host","platform":{"os":"linux","installationID":%q}}},"messages":[
{"role":"user","messageId":0,"content":[{"type":"text","text":"hi"}]},
{"role":"assistant","messageId":1,"content":[{"type":"text","text":"ok"}],"usage":{"model":"claude-opus-4-6","timestamp":%q,"inputTokens":10,"outputTokens":100,"cacheReadInputTokens":5000,"cacheCreationInputTokens":200,"totalInputTokens":5210}},
{"role":"user","messageId":2,"content":[{"type":"tool_result","run":{"status":"done","~debug":{"inferences":[{"usage":{"model":"gemini-3-flash-preview","inputTokens":0,"outputTokens":7,"cacheReadInputTokens":100,"cacheCreationInputTokens":50,"totalInputTokens":150}}]}}}]},
{"role":"assistant","messageId":3,"content":"plain words","usage":{"model":"gpt-5.4","timestamp":%q,"inputTokens":4335,"outputTokens":50,"cacheReadInputTokens":4096,"cacheCreationInputTokens":null,"totalInputTokens":4335}}
]}`, id, at*1000, inst, ampStamp(at), ampStamp(at+10))
}

type ampFake struct {
	list    []map[string]string
	threads map[string]string
	exports []string
}

func (f *ampFake) run(_ string, _ int, args ...string) ([]byte, error) {
	switch {
	case len(args) >= 2 && args[0] == "threads" && args[1] == "list":
		off := 0
		for i, a := range args {
			if a == "--offset" && i+1 < len(args) {
				fmt.Sscan(args[i+1], &off)
			}
		}
		page := []map[string]string{}
		for i := off; i < len(f.list) && i < off+ampTokPage; i++ {
			page = append(page, f.list[i])
		}
		return json.Marshal(page)
	case len(args) == 3 && args[0] == "threads" && args[1] == "export":
		f.exports = append(f.exports, args[2])
		if b, ok := f.threads[args[2]]; ok {
			return []byte(b), nil
		}
		return nil, errors.New("no such thread")
	}
	return nil, errors.New("unexpected " + strings.Join(args, " "))
}

func ampReset(t *testing.T) *ampFake {
	t.Helper()
	f := &ampFake{threads: map[string]string{}}
	run, bin := ampTokRun, ampTokBinary
	ampTokRun = f.run
	ampTokBinary = func() string { return "" } // the pass never sets one going
	wipe := func() {
		ampTok.Lock()
		ampTok.threads, ampTok.loaded, ampTok.dirty, ampTok.ranAt, ampTok.complete, ampTok.legacy = nil, false, false, 0, false, nil
		ampTok.Unlock()
	}
	wipe()
	t.Cleanup(func() {
		ampTokRun, ampTokBinary = run, bin
		wipe()
	})
	return f
}

func TestTokensAmp(t *testing.T) {
	home := testHome(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	tkuReset(t)
	fake := ampReset(t)
	tkuWrite(t, filepath.Join(home, ".local", "share", "amp", "device-id.json"), `{"installationID":"`+ampHere+`"}`)
	now := time.Unix(ampT0+3600, 0)
	from := time.Unix((now.Unix()/86400-tokLiveDays+1)*86400, 0)
	fake.list = []map[string]string{
		{"id": "T-aaaa", "updated": ampStamp(ampT0 + 20), "title": "a"},
		{"id": "T-dddd", "updated": ampStamp(ampT0 + 15), "title": "the laptop's"},
		{"id": "T-bbbb", "updated": ampStamp(ampT0 - 40*86400), "title": "old"},
		{"id": "../etc", "updated": ampStamp(ampT0), "title": "bad"},
	}
	fake.threads["T-aaaa"] = ampThread("T-aaaa", ampT0)
	fake.threads["T-dddd"] = ampThreadOn("T-dddd", ampT0, "99999999-0000-4000-8000-000000000000")
	fake.threads["T-bbbb"] = ampThread("T-bbbb", ampT0-40*86400)

	ampTokSync("amp", from)
	if strings.Join(fake.exports, ",") != "T-aaaa,T-dddd" {
		t.Fatalf("exported %v: only the threads inside the window, by a real id", fake.exports)
	}
	u := tokPass(now)
	if u == nil || u.Partial {
		t.Fatalf("answer = %+v", u)
	}
	if got, want := tkuSum(u, "amp", "claude-opus-4-6", true), (tokCell{in: 10, out: 100, cr: 5000, cw: 200, n: 1}); got != want {
		t.Fatalf("opus = %+v, want %+v", got, want)
	}
	// GPT's input holds the cache it read: what is left is the input.
	if got, want := tkuSum(u, "amp", "gpt-5.4", true), (tokCell{in: 239, out: 50, cr: 4096, n: 1}); got != want {
		t.Fatalf("gpt = %+v, want %+v", got, want)
	}
	// The subagent's inference, at the time of the reply before it.
	if got, want := tkuSum(u, "amp", "gemini-3-flash-preview", false), (tokCell{out: 7, cr: 100, cw: 50, n: 1}); got != want {
		t.Fatalf("finder = %+v, want %+v", got, want)
	}

	// Another machine's thread counts there, not here (one account, two
	// helpers): every model's count is this machine's thread's alone.
	if got := tkuSum(u, "amp", "claude-opus-4-6", true); got.n != 1 {
		t.Fatalf("another machine's thread counted: %+v", got)
	}

	// Listed again unchanged: nothing exported; moved on: exported again —
	// but another machine's thread, moved on, is not asked for again.
	ampTokSync("amp", from)
	if len(fake.exports) != 2 {
		t.Fatalf("exports = %v", fake.exports)
	}
	fake.list[0]["updated"] = ampStamp(ampT0 + 30)
	fake.list[1]["updated"] = ampStamp(ampT0 + 35)
	ampTokSync("amp", from)
	if strings.Join(fake.exports, ",") != "T-aaaa,T-dddd,T-aaaa" {
		t.Fatalf("exports = %v", fake.exports)
	}
	u = tokPass(now)
	if got := tkuSum(u, "amp", "claude-opus-4-6", true); got.n != 1 {
		t.Fatalf("a thread exported again counts once: %+v", got)
	}

	// A restarted helper reads its counts back instead of exporting again.
	ampTok.Lock()
	ampTok.threads, ampTok.loaded = nil, false
	ampTok.Unlock()
	tokState.files = nil
	u = tokPass(now)
	if got := tkuSum(u, "amp", "claude-opus-4-6", true); got.n != 1 {
		t.Fatalf("after a restart = %+v", got)
	}
	ampTokSync("amp", from)
	if len(fake.exports) != 3 {
		t.Fatalf("exported again after a restart: %v", fake.exports)
	}

	// Legacy files: one for a thread the CLI does not name counts, one for
	// a thread it does is left to the CLI's copy.
	dir := filepath.Join(home, ".local", "share", "amp", "threads")
	tkuWrite(t, filepath.Join(dir, "T-cccc.json"), ampThread("T-cccc", ampT0-3*3600))
	tkuWrite(t, filepath.Join(dir, "T-aaaa.json"), ampThread("T-aaaa", ampT0))
	u = tokPass(now)
	if got := tkuSum(u, "amp", "claude-opus-4-6", true); got.n != 2 {
		t.Fatalf("with legacy files = %+v", got)
	}

	// A thread gone from the listing (deleted) keeps what it spent until it
	// is past the window: the next thread's usage in its hours still adds.
	fake.list = fake.list[1:]
	ampTokSync("amp", from)
	u = tokPass(now)
	if _, ok := tokState.files["amp\x00T-aaaa"]; !ok {
		t.Fatal("T-aaaa's account let go")
	}
	if got := tkuSum(u, "amp", "claude-opus-4-6", true); got.n != 2 {
		t.Fatalf("after T-aaaa went = %+v", got)
	}
}

func TestTokensAmpSignedOut(t *testing.T) {
	home := testHome(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	tkuReset(t)
	ampReset(t)
	ampTokRun = func(string, int, ...string) ([]byte, error) { return nil, errors.New("not signed in") }
	ampTokSync("amp", time.Unix(ampT0-86400, 0))
	ampTok.Lock()
	complete := ampTok.complete
	ampTok.Unlock()
	if !complete {
		t.Fatal("a CLI that cannot list leaves nothing to wait for")
	}
	if _, err := os.Stat(ampTokPath()); err == nil {
		t.Fatal("nothing to keep, nothing written")
	}
}
