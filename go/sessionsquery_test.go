package main

// What the richer query has to mean. A phrase is not two words, an exclusion
// removes a message rather than a session, a pattern is case-insensitive and
// linear, and a query of nothing but exclusions is refused rather than
// answered with everything.

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func queryWorld(t *testing.T) []sessSource {
	t.Helper()
	claude, _, sources := sessTestWorld(t)
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	sessWrite(t, filepath.Join(claude, "p", "11111111-2222-3333-4444-555555555555.jsonl"), at,
		claudeLine("2026-08-01T09:00:00Z", "/w/p", "the red kettle boiled"),
		claudeLine("2026-08-01T09:01:00Z", "/w/p", "the kettle is not red, it is blue"),
		claudeLine("2026-08-01T09:02:00Z", "/w/p", "retry 42 attempts"))
	return sources
}

func run(sources []sessSource, q sessQuery) sessReport {
	q.limit = 20
	q.deadline = time.Now().Add(sessBudget)
	return sessionsSearch(sources, q)
}

func TestAPhraseIsNotTwoWords(t *testing.T) {
	sources := queryWorld(t)
	loose := run(sources, sessQuery{terms: []string{"red", "kettle"}})
	if len(loose.Matches) != 2 {
		t.Fatalf("two loose words should find both messages, got %d", len(loose.Matches))
	}
	phrase := run(sources, sessQuery{terms: []string{"red kettle"}})
	if len(phrase.Matches) != 1 {
		t.Fatalf("the phrase should find one message, got %d: %+v", len(phrase.Matches), phrase.Matches)
	}
}

func TestAnExclusionRemovesTheMessageItIsIn(t *testing.T) {
	sources := queryWorld(t)
	got := run(sources, sessQuery{terms: []string{"kettle"}, nots: []string{"blue"}})
	if len(got.Matches) != 1 {
		t.Fatalf("want one kettle without blue, got %+v", got.Matches)
	}
	// The excluded message is gone; the session it lives in is not.
	if got.Matches[0].Line != 1 {
		t.Errorf("kept the wrong message: %+v", got.Matches[0])
	}
}

func TestAPatternMatchesTheWayATermDoes(t *testing.T) {
	sources := queryWorld(t)
	re, err := sessCompile(`retry [0-9]+`)
	if err != nil {
		t.Fatal(err)
	}
	got := run(sources, sessQuery{res: []*regexp.Regexp{re}})
	if len(got.Matches) != 1 {
		t.Fatalf("want the retry line, got %+v", got.Matches)
	}
	// Patterns fold case like every other term here.
	up, err := sessCompile(`RETRY [0-9]+`)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(run(sources, sessQuery{res: []*regexp.Regexp{up}}).Matches); n != 1 {
		t.Fatalf("a pattern must be case-insensitive, got %d hits", n)
	}
}

func TestAPatternOnlyQueryStillGetsAUsefulSnippet(t *testing.T) {
	sources := queryWorld(t)
	re, _ := sessCompile(`retry [0-9]+`)
	got := run(sources, sessQuery{res: []*regexp.Regexp{re}})
	if len(got.Matches) != 1 || got.Matches[0].Snippet == "" {
		t.Fatalf("a pattern hit needs a snippet: %+v", got.Matches)
	}
}

func TestExclusionsAloneAreNotASearch(t *testing.T) {
	q := sessQuery{nots: []string{"kettle"}}
	if !q.empty() {
		t.Fatal("a query of nothing but exclusions must be refused, not answered with everything")
	}
	if (sessQuery{terms: []string{"a"}}).empty() {
		t.Fatal("a term is a search")
	}
	re, _ := sessCompile("a")
	if (sessQuery{res: []*regexp.Regexp{re}}).empty() {
		t.Fatal("a pattern is a search")
	}
}

func TestPatternsThatCannotBeCompiledAreRefused(t *testing.T) {
	for _, bad := range []string{"", "(", "[a-", `a{2,1}`} {
		if _, err := sessCompile(bad); err == nil {
			t.Errorf("sessCompile(%q) should have failed", bad)
		}
	}
	long := make([]byte, sessMaxPattern+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := sessCompile(string(long)); err == nil {
		t.Error("an over-long pattern should be refused")
	}
}

func TestQueryFlagsTakeBothSpellings(t *testing.T) {
	cases := [][]string{
		{"--term", "red kettle", "--not", "blue", "--re", "retry [0-9]+", "--not-re", "^x"},
		{"--term=red kettle", "--not=blue", "--re=retry [0-9]+", "--not-re=^x"},
	}
	for _, args := range cases {
		var q sessQuery
		for i := 0; i < len(args); i++ {
			handled, err := sessQueryFlag(&q, args, &i)
			if !handled || err != nil {
				t.Fatalf("%v: handled=%v err=%v", args, handled, err)
			}
		}
		if len(q.terms) != 1 || q.terms[0] != "red kettle" {
			t.Errorf("%v: terms %q", args, q.terms)
		}
		if len(q.nots) != 1 || q.nots[0] != "blue" {
			t.Errorf("%v: nots %q", args, q.nots)
		}
		if len(q.res) != 1 || len(q.nres) != 1 {
			t.Errorf("%v: res %d nres %d", args, len(q.res), len(q.nres))
		}
	}
}

func TestQueryFlagsRefuseEmptyValues(t *testing.T) {
	for _, args := range [][]string{
		{"--term", "   "}, {"--not", ""}, {"--re", ""}, {"--term"},
	} {
		var q sessQuery
		i := 0
		handled, err := sessQueryFlag(&q, args, &i)
		if !handled || err == nil {
			t.Errorf("%v should be a refusal, got handled=%v err=%v", args, handled, err)
		}
	}
}

func TestQueryFlagsLeaveOtherArgumentsAlone(t *testing.T) {
	var q sessQuery
	i := 0
	if handled, _ := sessQueryFlag(&q, []string{"--limit"}, &i); handled {
		t.Fatal("--limit is not a query flag")
	}
	i = 0
	if handled, _ := sessQueryFlag(&q, []string{"kettle"}, &i); handled {
		t.Fatal("a plain word is not a query flag")
	}
}

func TestTermsAreFoldedTheSameWayTheScanFolds(t *testing.T) {
	sources := queryWorld(t)
	var q sessQuery
	args := []string{"--term", "RED KETTLE"}
	i := 0
	if handled, err := sessQueryFlag(&q, args, &i); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if len(run(sources, q).Matches) != 1 {
		t.Fatal("a phrase typed in capitals must find the same message")
	}
}

func TestPatternLengthIsCountedInCharactersNotBytes(t *testing.T) {
	// The panel and the relay count code points. A helper counting UTF-8 bytes
	// would refuse a hundred Chinese characters the page had already accepted,
	// and the page would report this machine as too old to search.
	cjk := strings.Repeat("\u4e2d", sessMaxPattern)
	if _, err := sessCompile(cjk); err != nil {
		t.Errorf("%d characters should be accepted: %v", sessMaxPattern, err)
	}
	if _, err := sessCompile(strings.Repeat("\u4e2d", sessMaxPattern+1)); err == nil {
		t.Error("one character past the cap should be refused")
	}
}

func TestAMessageKeepsItsOwnClockWhereverItIsWritten(t *testing.T) {
	// Three spellings, because the tools disagree. Without all three, every
	// hit in a session shares the file's mtime — for a thread spanning months
	// that puts every message at the one minute it last moved.
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	for name, line := range map[string]string{
		"claude/codex, a top-level string": `{"timestamp":"2026-08-01T09:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		"amp, unix milliseconds":           `{"role":"user","meta":{"sentAt":1785574800000},"content":[{"type":"text","text":"hi"}]}`,
		"kiro, unix seconds":               `{"kind":"Prompt","data":{"meta":{"timestamp":1785574800},"content":[{"kind":"text","data":"hi"}]}}`,
	} {
		m, _, ok := sessParseMsg([]byte(line), 1)
		if !ok {
			t.Errorf("%s: not parsed as a message", name)
			continue
		}
		got, err := time.Parse(time.RFC3339, m.Time)
		if err != nil || !got.Equal(at) {
			t.Errorf("%s: time %q, want %s", name, m.Time, at.Format(time.RFC3339))
		}
	}
	// A number that is not a clock stays unstamped rather than becoming 1970 —
	// a listing sorts by this column.
	for _, line := range []string{
		`{"role":"user","meta":{"sentAt":3},"content":[{"type":"text","text":"hi"}]}`,
		`{"kind":"Prompt","data":{"meta":{"timestamp":99999999999},"content":[{"kind":"text","data":"hi"}]}}`,
		`{"role":"user","meta":{"sentAt":"yesterday"},"content":[{"type":"text","text":"hi"}]}`,
	} {
		if m, _, ok := sessParseMsg([]byte(line), 1); ok && m.Time != "" {
			t.Errorf("%s produced a time: %q", line, m.Time)
		}
	}
}
