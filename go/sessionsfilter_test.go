package main

// What a filter promises: the same narrowing for `search` and for `recent`,
// a clock bound that is paid for by NOT reading the file rather than by
// discarding it afterwards, and a project match that refuses to file an
// unlabelled session under a named project.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sessFilterAsk(fl sessFilter, terms ...string) sessQuery {
	q := sessAsk(terms...)
	q.filter = fl
	return q
}

// Two transcripts, a day and a project apart, is the whole fixture every test
// below needs.
func sessFilterWorld(t *testing.T) (sources []sessSource, now time.Time) {
	t.Helper()
	claude, codex, sources := sessTestWorld(t)
	now = time.Now()
	sessWrite(t, filepath.Join(claude, "-home-u-alpha", "11111111-2222-3333-4444-555555555555.jsonl"),
		now.Add(-2*time.Hour),
		claudeLine("2026-08-18T10:00:00Z", "/home/u/alpha", "the beacon is lit"))
	sessWrite(t, filepath.Join(codex, "2026", "08", "10", "rollout-2026-08-10T09-00-00-99999999-8888-7777-6666-555555555555.jsonl"),
		now.Add(-20*24*time.Hour),
		codexMetaLine("2026-08-10T09:00:00Z", "/home/u/beta"),
		codexMsgLine("2026-08-10T09:00:01Z", "the beacon is lit here too"))
	return sources, now
}

func TestFilterParsesTheTimesPeopleType(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"30m", now.Add(-30 * time.Minute)},
		{"24h", now.Add(-24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"4w", now.Add(-28 * 24 * time.Hour)},
		{"7D", now.Add(-7 * 24 * time.Hour)},
		{"2026-08-01", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-08-01T06:30:00Z", time.Date(2026, 8, 1, 6, 30, 0, 0, time.UTC)},
	} {
		got, day, err := sessParseWhen(c.in, now)
		if err != nil {
			t.Fatalf("%q should parse: %v", c.in, err)
		}
		if !got.Equal(c.want) {
			t.Fatalf("%q -> %v, want %v", c.in, got, c.want)
		}
		// Only a bare date names a day; a full stamp and a relative form name
		// instants, and --until must not stretch those to a day's end.
		if want := c.in == "2026-08-01"; day != want {
			t.Fatalf("%q dayOnly=%v, want %v", c.in, day, want)
		}
	}
	// A count wide enough to wrap a Duration is refused rather than silently
	// turned into a moment in the far past or future.
	for _, bad := range []string{"", "d", "0d", "-1d", "7y", "99999w", "yesterday", "2026-13-01", "7 d"} {
		if _, _, err := sessParseWhen(bad, now); err == nil {
			t.Fatalf("%q should not parse as a time", bad)
		}
	}
}

func TestFilterUntilADateCoversThatWholeDay(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	var fl sessFilter
	args := []string{"--until", "2026-08-01"}
	i := 0
	if handled, err := sessFilterFlag(&fl, args, &i, now); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	// Taken literally a bare date is that day's FIRST midnight, which would
	// exclude all but the first instant of the day somebody asked for.
	lateThatDay := time.Date(2026, 8, 1, 23, 59, 0, 0, time.UTC)
	if !fl.wantsTime(lateThatDay) {
		t.Fatalf("--until 2026-08-01 dropped 23:59 on the first of August")
	}
	if fl.wantsTime(time.Date(2026, 8, 2, 0, 0, 1, 0, time.UTC)) {
		t.Fatal("--until 2026-08-01 must not reach into the second")
	}
	// A full stamp still means exactly what it says.
	var exact sessFilter
	stamp := []string{"--until", "2026-08-01T06:00:00Z"}
	j := 0
	if _, err := sessFilterFlag(&exact, stamp, &j, now); err != nil {
		t.Fatal(err)
	}
	if exact.wantsTime(time.Date(2026, 8, 1, 6, 0, 1, 0, time.UTC)) {
		t.Fatal("a full --until stamp must not be stretched to the day's end")
	}
}

func TestFilterFlagsTakeBothSpellings(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	for _, args := range [][]string{
		{"--since", "7d", "--project", "alpha"},
		{"--since=7d", "--project=alpha"},
	} {
		var fl sessFilter
		for i := 0; i < len(args); i++ {
			handled, err := sessFilterFlag(&fl, args, &i, now)
			if !handled || err != nil {
				t.Fatalf("%v: handled=%v err=%v", args, handled, err)
			}
		}
		if fl.project != "alpha" || !fl.since.Equal(now.Add(-7*24*time.Hour)) {
			t.Fatalf("%v parsed to %+v", args, fl)
		}
	}
	// A value that is not a time is a caller error, not a silently ignored flag.
	var fl sessFilter
	args := []string{"--since", "soonish"}
	i := 0
	if handled, err := sessFilterFlag(&fl, args, &i, now); !handled || err == nil {
		t.Fatalf("a bad --since must be handled AND refused, got handled=%v err=%v", handled, err)
	}
	// Somebody else's flag is left for somebody else's loop.
	other := []string{"--limit=5"}
	j := 0
	if handled, _ := sessFilterFlag(&fl, other, &j, now); handled {
		t.Fatal("--limit is not a filter flag")
	}
}

func TestFilterProjectNarrowsSearchToOneTree(t *testing.T) {
	sources, _ := sessFilterWorld(t)
	if rep := sessionsSearch(sources, sessAsk("beacon")); len(rep.Matches) != 2 {
		t.Fatalf("unfiltered should see both, got %d", len(rep.Matches))
	}
	rep := sessionsSearch(sources, sessFilterAsk(sessFilter{project: "alpha"}, "beacon"))
	if len(rep.Matches) != 1 || rep.Matches[0].Project != "alpha" {
		t.Fatalf("want the one alpha hit, got %+v", rep.Matches)
	}
	// Substring and case-folded, so the label a listing shows is enough to type.
	if rep := sessionsSearch(sources, sessFilterAsk(sessFilter{project: "lph"}, "beacon")); len(rep.Matches) != 1 {
		t.Fatalf("a substring of the label should match, got %d", len(rep.Matches))
	}
	if rep := sessionsSearch(sources, sessFilterAsk(sessFilter{project: "gamma"}, "beacon")); len(rep.Matches) != 0 {
		t.Fatalf("a project nobody worked in should match nothing, got %d", len(rep.Matches))
	}
}

func TestFilterSinceSkipsTheFileEntirely(t *testing.T) {
	sources, now := sessFilterWorld(t)
	rep := sessionsSearch(sources, sessFilterAsk(sessFilter{since: now.Add(-24 * time.Hour)}, "beacon"))
	if len(rep.Matches) != 1 || rep.Matches[0].Project != "alpha" {
		t.Fatalf("want only the recent hit, got %+v", rep.Matches)
	}
	// The point of a clock filter is the reading it avoids: the old transcript
	// must not even be opened, which `Scanned` is what reports.
	if rep.Scanned != 1 {
		t.Fatalf("the filtered-out transcript was still read: scanned %d", rep.Scanned)
	}
	if rep.Files != 2 {
		t.Fatalf("Files should still count every candidate, got %d", rep.Files)
	}
}

func TestFilterUntilBoundsTheOtherSide(t *testing.T) {
	sources, now := sessFilterWorld(t)
	rep := sessionsSearch(sources, sessFilterAsk(sessFilter{until: now.Add(-24 * time.Hour)}, "beacon"))
	if len(rep.Matches) != 1 || rep.Matches[0].Project != "beta" {
		t.Fatalf("want only the old hit, got %+v", rep.Matches)
	}
	// Both sides at once is a window, and a window with nothing in it is empty
	// rather than everything.
	rep = sessionsSearch(sources, sessFilterAsk(sessFilter{
		since: now.Add(-19 * 24 * time.Hour),
		until: now.Add(-24 * time.Hour),
	}, "beacon"))
	if len(rep.Matches) != 0 {
		t.Fatalf("an empty window should hold nothing, got %+v", rep.Matches)
	}
}

func TestFilterWillNotFileAnUnlabelledSessionUnderAProject(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	// A transcript that never records a cwd: real, searchable, and belonging to
	// no named project.
	sessWrite(t, filepath.Join(claude, "-unknown", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.jsonl"),
		time.Now().Add(-time.Hour),
		claudeLine("2026-08-18T10:00:00Z", "", "the beacon is lit"))
	if rep := sessionsSearch(sources, sessAsk("beacon")); len(rep.Matches) != 1 {
		t.Fatalf("unfiltered it is still a hit, got %d", len(rep.Matches))
	}
	if rep := sessionsSearch(sources, sessFilterAsk(sessFilter{project: "alpha"}, "beacon")); len(rep.Matches) != 0 {
		t.Fatalf("no project is not every project, got %+v", rep.Matches)
	}
}

func TestFilterAppliesToRecentTheSameWay(t *testing.T) {
	sources, now := sessFilterWorld(t)
	if rep := sessionsRecent(sources, 10, sessFilter{}, now.Add(time.Minute)); len(rep.Sessions) != 2 {
		t.Fatalf("unfiltered listing should see both, got %d", len(rep.Sessions))
	}
	rep := sessionsRecent(sources, 10, sessFilter{project: "beta"}, now.Add(time.Minute))
	if len(rep.Sessions) != 1 || rep.Sessions[0].Project != "beta" {
		t.Fatalf("want the one beta session, got %+v", rep.Sessions)
	}
	rep = sessionsRecent(sources, 10, sessFilter{since: now.Add(-24 * time.Hour)}, now.Add(time.Minute))
	if len(rep.Sessions) != 1 || rep.Sessions[0].Project != "alpha" {
		t.Fatalf("want the one recent session, got %+v", rep.Sessions)
	}
}

func TestFilterDescribesItselfInTheSummary(t *testing.T) {
	if s := (sessFilter{}).describe(); s != "" {
		t.Fatalf("an off filter says nothing, got %q", s)
	}
	fl := sessFilter{project: "alpha", since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	s := fl.describe()
	if !strings.Contains(s, "project alpha") || !strings.Contains(s, "since 2026-08-01T00:00:00Z") {
		t.Fatalf("summary should name the narrowing, got %q", s)
	}
}

func TestRoleNarrowsToWhoSaidIt(t *testing.T) {
	claude, _, sources := sessTestWorld(t)
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	sessWrite(t, filepath.Join(claude, "p", "11111111-2222-3333-4444-555555555555.jsonl"), at,
		`{"type":"user","timestamp":"2026-08-01T09:00:00Z","cwd":"/w/p","message":{"role":"user","content":[{"type":"text","text":"the kettle asked"}]}}`,
		`{"type":"assistant","timestamp":"2026-08-01T09:01:00Z","cwd":"/w/p","message":{"role":"assistant","content":[{"type":"text","text":"the kettle answered"}]}}`,
		`{"type":"user","timestamp":"2026-08-01T09:02:00Z","cwd":"/w/p","message":{"role":"user","content":[{"type":"tool_result","text":"the kettle reported"}]}}`)
	run := func(role string) int {
		return len(sessionsSearch(sources, sessQuery{
			terms:    []string{"kettle"},
			filter:   sessFilter{role: role},
			limit:    20,
			deadline: time.Now().Add(sessBudget),
		}).Matches)
	}
	for role, want := range map[string]int{"": 3, "user": 1, "assistant": 1, "tool": 1} {
		if got := run(role); got != want {
			t.Errorf("role %q found %d, want %d", role, got, want)
		}
	}
}

func TestRoleFlagOnlyTakesRolesATranscriptCanName(t *testing.T) {
	for _, args := range [][]string{
		{"--role", "user"}, {"--role=assistant"}, {"--role", "TOOL"},
	} {
		var fl sessFilter
		i := 0
		if handled, err := sessFilterFlag(&fl, args, &i, time.Now()); !handled || err != nil {
			t.Errorf("%v: handled=%v err=%v", args, handled, err)
		} else if !sessValidRole(fl.role) {
			t.Errorf("%v: role %q", args, fl.role)
		}
	}
	for _, args := range [][]string{
		{"--role", "usr"}, {"--role", "system"}, {"--role", ""}, {"--role"},
	} {
		var fl sessFilter
		i := 0
		handled, err := sessFilterFlag(&fl, args, &i, time.Now())
		if !handled || err == nil {
			t.Errorf("%v should be refused, got handled=%v err=%v", args, handled, err)
		}
	}
}

func TestAnUnownedMessageIsNotFiledUnderAnyRole(t *testing.T) {
	// sessRole returns "" for a line it cannot attribute. Filing those under
	// whichever role was asked for is the one answer that makes the filter
	// worse than not having it.
	if !(sessFilter{}).wantsRole("") {
		t.Fatal("no filter must want everything")
	}
	if (sessFilter{role: "user"}).wantsRole("") {
		t.Fatal("an unowned message is not a user message")
	}
	if !(sessFilter{role: "user"}).wantsRole("user") {
		t.Fatal("a user message is a user message")
	}
}
