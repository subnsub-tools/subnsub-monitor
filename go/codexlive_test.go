package main

// The live Codex leg, exercised against a stand-in app-server rather than the
// real one: these must pass on a machine with no Codex installed, and a test
// that shells out to somebody's actual CLI proves nothing repeatable anyway.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// What the real app-server answers on this plan: the account's own bucket and
// a second metered window with a name of its own.
const codexTwoBuckets = `{"id":2,"result":{` +
	`"rateLimits":{"limitId":"codex","limitName":null,` +
	`"primary":{"usedPercent":82,"windowDurationMins":10080,"resetsAt":1786165788},` +
	`"secondary":null,"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},` +
	`"planType":"prolite"},` +
	`"rateLimitsByLimitId":{` +
	`"codex":{"limitId":"codex","limitName":null,` +
	`"primary":{"usedPercent":82,"windowDurationMins":10080,"resetsAt":1786165788},` +
	`"secondary":null,"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},` +
	`"planType":"prolite"},` +
	`"codex_bengalfox":{"limitId":"codex_bengalfox","limitName":"GPT-5.3-Codex-Spark",` +
	`"primary":{"usedPercent":0,"windowDurationMins":10080,"resetsAt":1786551072},` +
	`"secondary":null,"credits":null,"planType":"prolite"}}}}`

// A stand-in `codex` whose only trick is the app-server handshake. The leading
// notification is not decoration: the real server talks over its own startup,
// and a reader that stopped at the first line it did not recognise would work
// here and fail there.
func fakeCodexBinary(t *testing.T, answer string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"method\":\"thread/started\",\"params\":{}}'\n" +
		"while IFS= read -r line; do\n" +
		"  case \"$line\" in\n" +
		"    *rateLimits*) printf '%s\\n' '" + answer + "'; exit 0 ;;\n" +
		"    *initialize*) printf '%s\\n' '{\"id\":1,\"result\":{}}' ;;\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexBinEnv, path)
	return path
}

// The cache is package state and these tests share a process, so a reading one
// test left behind would be served to the next one. Not a plain assignment:
// that copies a mutex, and vet is right to say so.
func resetCodexLiveCache(t *testing.T) {
	t.Helper()
	codexLiveCache.Lock()
	codexLiveCache.last, codexLiveCache.fail = nil, nil
	codexLiveCache.fetchedAt, codexLiveCache.attemptedAt = 0, 0
	codexLiveCache.Unlock()
}

func TestCodexLiveReadsEveryBucket(t *testing.T) {
	fakeCodexBinary(t, codexTwoBuckets)

	p := codexLiveFetch()
	if !p.OK {
		t.Fatalf("the live leg did not read: %+v", p)
	}
	if p.Source != "cli" {
		t.Errorf("source should say which leg answered, got %q", p.Source)
	}
	if len(p.Limits) != 2 {
		t.Fatalf("both metered windows should be limits, got %d: %+v", len(p.Limits), p.Limits)
	}

	// The account's own window keeps the key the log collector has always used,
	// so a machine switching legs does not look like a provider that grew a new
	// window overnight.
	if p.Limits[0].Key != "primary" || p.Limits[0].UsedPercent != 82 {
		t.Errorf("the account window changed shape: %+v", p.Limits[0])
	}
	if p.Limits[0].Scope != nil {
		t.Errorf("the account window should carry no scope, got %q", *p.Limits[0].Scope)
	}
	if p.Limits[0].WindowLabel == nil || *p.Limits[0].WindowLabel != "7d" {
		t.Errorf("10080 minutes should read as 7d: %+v", p.Limits[0])
	}

	// The second bucket is namespaced and wears the vendor's own name for the
	// meter, which is the row's second label on the card. Without it the page
	// would show two identical "7d" rows.
	if p.Limits[1].Key != "codex_bengalfox/p" {
		t.Errorf("the second bucket should be namespaced, got %q", p.Limits[1].Key)
	}
	if len(p.Limits[1].Key) > 24 {
		t.Errorf("the relay caps a limit key at 24 characters, got %d", len(p.Limits[1].Key))
	}
	if p.Limits[1].Scope == nil || *p.Limits[1].Scope != "GPT-5.3-Codex-Spark" {
		t.Errorf("the second bucket lost its name: %+v", p.Limits[1])
	}

	// Plan and balance belong to the account's bucket; the others carry nulls
	// for both and must not be allowed to overwrite it.
	if p.PlanType == nil || *p.PlanType != "prolite" {
		t.Errorf("plan came from the wrong bucket: %+v", p.PlanType)
	}
	if p.Credits == nil || p.Credits.Balance != "0" {
		t.Errorf("credits came from the wrong bucket: %+v", p.Credits)
	}
	if p.RecordedAt == nil {
		t.Error("a live reading must timestamp itself, or it cannot age on the card")
	}
}

// A response that parses and carries nothing. Reporting ok here would leave the
// page showing its previous gauge with a fresh timestamp on it.
func TestCodexLiveRefusesAnAnswerWithNoWindow(t *testing.T) {
	fakeCodexBinary(t, `{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":null,"secondary":null}}}`)

	if p := codexLiveFetch(); p.OK {
		t.Fatalf("an empty envelope was accepted as a reading: %+v", p)
	}
}

// The whole point of keeping the log collector: a machine with no Codex binary
// anywhere this is willing to run from still reports whatever its sessions
// recorded, rather than a permanent error for a leg it never had.
func TestCodexFallsBackToTheLogWhenTheLiveLegCannotRun(t *testing.T) {
	home := testHome(t)
	writeRollout(t, home)
	// An absolute path that does not exist: the override refuses rather than
	// falling through to the search, which is exactly the case being tested.
	t.Setenv(codexBinEnv, filepath.Join(home, "no-such-codex"))
	resetCodexLiveCache(t)
	t.Cleanup(func() { resetCodexLiveCache(t) })

	p := collectCodex()
	if !p.OK {
		t.Fatalf("the fallback did not read: %+v", p)
	}
	if p.Source != "local-log" {
		t.Errorf("a fallback reading must say it came from the log, got %q", p.Source)
	}
	if len(p.Limits) != 1 || p.Limits[0].UsedPercent != 12 {
		t.Errorf("the fallback read the wrong thing: %+v", p.Limits)
	}
}

// A live answer wins over a log that is sitting right there — the whole reason
// this leg exists is that the log can be a fortnight behind.
func TestCodexPrefersTheLiveLegOverTheLog(t *testing.T) {
	home := testHome(t)
	writeRollout(t, home)
	fakeCodexBinary(t, codexTwoBuckets)
	resetCodexLiveCache(t)
	t.Cleanup(func() { resetCodexLiveCache(t) })

	p := collectCodex()
	if p.Source != "cli" || len(p.Limits) == 0 || p.Limits[0].UsedPercent != 82 {
		t.Fatalf("the log won over a working live leg: %+v", p)
	}
}

// The hang this is built to survive: `codex` from npm is a Node script that
// spawns the real binary with inherited stdio, so killing the process we
// started leaves a grandchild holding the write end of our stdout pipe. No EOF
// ever arrives. Before the read had a deadline of its own this blocked forever
// with the collector's cache mutex held, which takes the whole helper off the
// air — every push waits on every collector.
func TestCodexLiveGivesUpWhenAGrandchildHoldsThePipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	// The subshell inherits stdout and outlives the process we kill, which is
	// exactly the npm launcher's shape.
	script := "#!/bin/sh\n(sleep 60) &\nexec sleep 60\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexBinEnv, path)

	old := codexRPCTimeout
	codexRPCTimeout = 300 * time.Millisecond
	t.Cleanup(func() { codexRPCTimeout = old })

	done := make(chan Provider, 1)
	go func() { done <- codexLiveFetch() }()
	select {
	case p := <-done:
		if p.OK {
			t.Fatalf("a server that never answered was read as a reading: %+v", p)
		}
		if p.Error != "unreachable" {
			t.Errorf("a silent server should read as unreachable, got %q/%q", p.Error, p.DetailCode)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the read never gave up — this is the hang that takes the helper off the air")
	}
}

// The flat `rateLimits` is the account's own bucket in backward-compatible
// form, not a lesser copy to drop the moment the map exists. A payload with
// extra meters and no `codex` entry must not lose the main quota — nor read the
// plan and balance off whichever extra meter sorts first.
func TestCodexLiveKeepsTheFlatBucketWhenTheMapHasNoAccountEntry(t *testing.T) {
	fakeCodexBinary(t, `{"id":2,"result":{`+
		`"rateLimits":{"limitId":"codex","primary":{"usedPercent":40,"windowDurationMins":10080},`+
		`"credits":{"hasCredits":true,"unlimited":false,"balance":"7"},"planType":"prolite"},`+
		`"rateLimitsByLimitId":{"codex_other":{"limitId":"codex_other","limitName":null,`+
		`"primary":{"usedPercent":5,"windowDurationMins":10080},"credits":null,"planType":null}}}}`)

	p := codexLiveFetch()
	if !p.OK || len(p.Limits) != 2 {
		t.Fatalf("the account bucket was dropped: %+v", p)
	}
	if p.Limits[0].Key != "primary" || p.Limits[0].UsedPercent != 40 {
		t.Errorf("the account window should lead and keep its key: %+v", p.Limits[0])
	}
	if p.PlanType == nil || *p.PlanType != "prolite" {
		t.Errorf("plan should come from the account bucket, got %v", p.PlanType)
	}
	if p.Credits == nil || p.Credits.Balance != "7" {
		t.Errorf("credits should come from the account bucket, got %+v", p.Credits)
	}
	// A nameless extra bucket still has to be distinguishable: both rows are
	// labelled by their window, and both windows are 7d.
	if p.Limits[1].Scope == nil || *p.Limits[1].Scope != "codex_other" {
		t.Errorf("a nameless bucket should fall back to its id for scope: %+v", p.Limits[1])
	}
}

// Two ids sharing a long prefix must not collide into one key — a card cannot
// tell two rows with the same key apart.
func TestCodexLimitKeyStaysUniqueWhenTruncated(t *testing.T) {
	long := "codex_a_very_long_meter_identifier_"
	a := codexLimitKey(long+"one", "primary")
	b := codexLimitKey(long+"two", "primary")
	if a == b {
		t.Fatalf("two ids collapsed to one key: %q", a)
	}
	for _, k := range []string{a, b} {
		if len(k) > 24 {
			t.Errorf("key %q is over the relay's 24-character cap", k)
		}
	}
	if got := codexLimitKey("codex_bengalfox", "primary"); got != "codex_bengalfox/p" {
		t.Errorf("a short id should pass through untouched, got %q", got)
	}
}

// nvm names its directories by version, and "v9" sorts after "v22" as a string
// — then "v22.9.0" sorts after "v22.19.0" one level down. A plain reverse sort
// gets both wrong and hands a machine an older CLI than it has.
func TestCodexPrefersTheNewestNodeVersion(t *testing.T) {
	home := t.TempDir()
	for _, v := range []string{"v9.11.2", "v22.19.0", "v20.11.0", "v22.9.0"} {
		dir := filepath.Join(home, ".nvm", "versions", "node", v, "bin")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := codexNodeVersionCandidates(home)
	if len(got) != 4 {
		t.Fatalf("every install should be a candidate, got %v", got)
	}
	if !strings.Contains(got[0], "v22.19.0") {
		t.Errorf("the newest version should come first, got %v", got)
	}
	if !strings.Contains(got[1], "v22.9.0") {
		t.Errorf("minor versions compare as numbers too, got %v", got)
	}
}
