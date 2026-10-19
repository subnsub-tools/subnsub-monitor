package main

// What Codex spent in the last 24 hours, out of its own session logs
// (codexactivity.go): the steps in each session's running total inside the
// window, in the measure Codex's status line leads with — input not served
// from cache, plus output.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A usage block: input, of which cached, and output.
type tokUse [3]float64

func (u tokUse) json() string {
	return fmt.Sprintf(`{"input_tokens":%g,"cached_input_tokens":%g,"output_tokens":%g,"reasoning_output_tokens":0,"total_tokens":%g}`,
		u[0], u[1], u[2], u[0]+u[2])
}

// One token_count line as Codex writes it, the running total and the turn.
func tokLine(at float64, total, last tokUse) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":%s,"last_token_usage":%s,"model_context_window":272000},"rate_limits":null}}`+"\n",
		time.Unix(int64(at), 0).UTC().Format(time.RFC3339Nano), total.json(), last.json())
}

func writeSession(t *testing.T, home, name, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "10", "07")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func appendTo(t *testing.T, file, body string) {
	t.Helper()
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
}

func resetCodexAct(t *testing.T) {
	codexAct.Lock()
	codexAct.files = nil
	codexAct.Unlock()
	t.Cleanup(func() {
		codexAct.Lock()
		codexAct.files = nil
		codexAct.Unlock()
	})
}

func actTokens(a *Activity) float64 {
	if a == nil || a.Tokens == nil {
		return -1
	}
	return *a.Tokens
}

func TestCodexActivityCountsWhatTheWindowSpent(t *testing.T) {
	home := testHome(t)
	resetCodexAct(t)
	at := float64(time.Now().Unix())
	h := 3600.0

	// A session inside the window: its first turn counts its own usage, each
	// later one the step in the running total (250 → 820 → 1200), and an
	// event written twice once. Other lines are not turns.
	a := writeSession(t, home, "rollout-2026-10-07T09-00-00-a.jsonl",
		`{"timestamp":"2026-10-07T09:00:00Z","type":"session_meta","payload":{"id":"a"}}`+"\n"+
			tokLine(at-3*h, tokUse{1000, 800, 50}, tokUse{1000, 800, 50})+
			`{"timestamp":"2026-10-07T09:30:00Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":3}}}}`+"\n"+
			tokLine(at-2*h, tokUse{2500, 1800, 120}, tokUse{1500, 1000, 70})+
			tokLine(at-2*h+1, tokUse{2500, 1800, 120}, tokUse{1500, 1000, 70})+
			tokLine(at-1*h, tokUse{4000, 3000, 200}, tokUse{1500, 1200, 80}))
	// A session begun the day before: what it had spent by the window's
	// start is not the window's — only the step after it is, measured from
	// the running total there (11000 → 12500) even where the turn's own
	// figure says less (a turn between that was never written).
	writeSession(t, home, "rollout-2026-10-06T07-00-00-b.jsonl",
		tokLine(at-30*h, tokUse{10000, 0, 1000}, tokUse{10000, 0, 1000})+
			tokLine(at-20*h, tokUse{12000, 1000, 1500}, tokUse{500, 0, 100}))

	got := codexActivity(at)
	if actTokens(got) != 1200+1500 || got.WindowLabel == nil || *got.WindowLabel != "24h" {
		t.Fatalf("24h activity: %+v (tokens %v)", got, actTokens(got))
	}

	// A line still being written waits for its newline, and is then counted
	// once, from where the account stopped.
	line := tokLine(at-600, tokUse{5000, 3500, 300}, tokUse{1000, 500, 100})
	appendTo(t, a, line[:len(line)-1])
	if got := actTokens(codexActivity(at)); got != 2700 {
		t.Errorf("a half-written line counted: %v", got)
	}
	appendTo(t, a, "\n")
	if got := actTokens(codexActivity(at)); got != 2700+600 {
		t.Errorf("the appended turn: %v", got)
	}
	if got := actTokens(codexActivity(at)); got != 3300 {
		t.Errorf("read again, the same file counts the same: %v", got)
	}

	// The window moves on: the day-old session's step and the first turns
	// leave it.
	if got := actTokens(codexActivity(at + 21.5*h)); got != 570+380+600 {
		t.Errorf("a later window: %v", got)
	}
}

// A running total that went back is a new one: the turn's own usage counts.
func TestCodexActivityTakesATotalThatWentBackAsANewOne(t *testing.T) {
	home := testHome(t)
	resetCodexAct(t)
	at := float64(time.Now().Unix())
	writeSession(t, home, "rollout-2026-10-07T10-00-00-c.jsonl",
		tokLine(at-7200, tokUse{500, 0, 0}, tokUse{500, 0, 0})+
			tokLine(at-3600, tokUse{100, 0, 0}, tokUse{100, 0, 0}))
	if got := actTokens(codexActivity(at)); got != 600 {
		t.Errorf("total went back: %v", got)
	}
}

// Short of what it needs, it says nothing rather than a smaller number: a
// file in the window behind a second name is refused (codex.go's guard), and
// with it the count.
func TestCodexActivitySaysNothingShortOfTheWholeWindow(t *testing.T) {
	home := testHome(t)
	resetCodexAct(t)
	at := float64(time.Now().Unix())
	writeSession(t, home, "rollout-2026-10-07T10-00-00-d.jsonl", tokLine(at-60, tokUse{100, 0, 10}, tokUse{100, 0, 10}))
	linked := writeSession(t, home, "rollout-2026-10-07T11-00-00-e.jsonl", tokLine(at-30, tokUse{100, 0, 10}, tokUse{100, 0, 10}))
	if got := actTokens(codexActivity(at)); got != 220 {
		t.Fatalf("before the link: %v", got)
	}
	if err := os.Link(linked, filepath.Join(home, "second-name")); err != nil {
		t.Skip("no hard links here:", err)
	}
	if got := codexActivity(at); got != nil {
		t.Errorf("a refused file in the window still gave a count: %+v", got)
	}
}

// Nothing spent is nothing to say, and so is a tree that is not there.
func TestCodexActivityNothingSpentIsNoRow(t *testing.T) {
	home := testHome(t)
	resetCodexAct(t)
	at := float64(time.Now().Unix())
	if got := codexActivity(at); got != nil {
		t.Errorf("no sessions tree: %+v", got)
	}
	writeSession(t, home, "rollout-2026-10-07T10-00-00-f.jsonl", oneReading)
	if got := codexActivity(at); got != nil {
		t.Errorf("rate limits alone are not activity: %+v", got)
	}
}

// The tile carries it whichever leg read the quota.
func TestCodexTileCarriesItsActivity(t *testing.T) {
	home := testHome(t)
	resetCodexAct(t)
	at := float64(time.Now().Unix())
	writeSession(t, home, "rollout-2026-10-07T10-00-00-g.jsonl",
		`{"timestamp":`+fmt.Sprintf("%q", time.Unix(int64(at-60), 0).UTC().Format(time.RFC3339))+`,"type":"event_msg","payload":{"type":"token_count",`+
			`"info":{"total_token_usage":{"input_tokens":900,"cached_input_tokens":400,"output_tokens":100},"last_token_usage":{"input_tokens":900,"cached_input_tokens":400,"output_tokens":100}},`+
			`"rate_limits":{"primary":{"used_percent":12.0,"window_minutes":300,"resets_at":1785258178}}}}`+"\n")
	t.Setenv(codexBinEnv, filepath.Join(home, "no-such-codex"))
	resetCodexLiveCache(t)
	t.Cleanup(func() { resetCodexLiveCache(t) })
	p := collectCodex()
	if !p.OK || actTokens(p.Activity) != 600 {
		t.Fatalf("the tile's activity: %+v / %+v", p, p.Activity)
	}
}
