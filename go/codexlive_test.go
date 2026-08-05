package main

// The live Codex leg, exercised against a stand-in app-server rather than the
// real one: these must pass on a machine with no Codex installed, and a test
// that shells out to somebody's actual CLI proves nothing repeatable anyway.

import (
	"fmt"
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
//
// It insists on the handshake rather than merely surviving it. Matching loosely
// — `*initialize*`, `*rateLimits*` — makes a stand-in that answers the request
// this code SHOULD send and equally answers half a dozen it should not, so the
// tests pass whether or not the conversation is the right one. Here `initialized`
// is a notification and is answered with silence, `initialize` is a request and
// is answered once, the method name is matched in full, and asking for limits
// before the handshake finished is refused. Delete the `initialized` send from
// codexAskRateLimits and these tests go red, which is the point of them.
func fakeCodexBinary(t *testing.T, answer string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	script := strings.Replace(`#!/bin/sh
D=$(dirname "$0")
printf '%s\n' '{"method":"thread/started","params":{}}'
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialized"'*) : > "$D/ready" ;;
    *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}' ;;
    *'"method":"account/rateLimits/read"'*)
      if [ -f "$D/ready" ]; then
        printf '%s\n' '@ANSWER@'
      else
        printf '%s\n' '{"id":2,"error":{"code":-32002,"message":"not initialized"}}'
      fi
      exit 0 ;;
    *) printf '%s\n' '{"id":0,"error":{"code":-32601,"message":"unknown method"}}' ;;
  esac
done
`, "@ANSWER@", answer, 1)
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

// …but it wins on FRESHNESS, not on rank, and that is the half a rewrite would
// quietly drop. The cache keeps serving its last success for ten minutes after
// refreshes start failing, which is right — a stale real number beats an error —
// but it means an `ok` live reading can be minutes old while a session written
// since then sits unread on disk. Change collectCodex back to preferring live
// unconditionally and every other test in this file still passes; this is the
// one that goes red.
func TestCodexPrefersANewerLogOverAStaleCachedLiveReading(t *testing.T) {
	home := testHome(t)
	writeRollout(t, home)
	// An absolute path that does not exist, so every refresh from here fails and
	// the cache is left serving what it already had — the situation this is about.
	t.Setenv(codexBinEnv, filepath.Join(home, "no-such-codex"))
	resetCodexLiveCache(t)
	t.Cleanup(func() { resetCodexLiveCache(t) })

	logged := collectCodexLog()
	if !logged.OK || logged.RecordedAt == nil {
		t.Fatalf("the log leg is what this compares against: %+v", logged)
	}
	// Fetched recently enough that the cache still serves it, but READ from the
	// account a day before the session log was written. The two timestamps are
	// different questions and this test only works because they are.
	fetched := now() - (provMinInterval+provStaleMax)/2
	seed := Provider{
		ID: "codex", Name: "Codex", Source: "cli", OK: true,
		CapturedAt: fetched,
		RecordedAt: fp(*logged.RecordedAt - 86400),
		Limits:     []Limit{{Key: "primary", UsedPercent: 99}},
	}
	codexLiveCache.Lock()
	codexLiveCache.last, codexLiveCache.fetchedAt = &seed, fetched
	codexLiveCache.Unlock()

	p := collectCodex()
	if p.Source != "local-log" {
		t.Fatalf("a stale live reading outranked a log written after it: %+v", p)
	}
	if len(p.Limits) != 1 || p.Limits[0].UsedPercent != 12 {
		t.Errorf("the log reading did not come through: %+v", p.Limits)
	}
}

// The read budget covers the whole conversation, not each request in it. A
// server that talks it away during the handshake has nothing left for the
// answer, and the answer is refused rather than read. Reset the counters per
// await — the obvious-looking tidy-up — and a server can spend the budget as
// many times as it likes to send.
func TestCodexLiveSpendsOneReadBudgetAcrossTheWholeConversation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	// Just under the line by the end of the handshake: the startup notification,
	// this noise, and the initialize answer. The reading that follows is one line
	// too many. The exact arithmetic is not what is being tested — spending the
	// budget earlier only fails earlier — the regression is a budget that comes
	// back.
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' '{"method":"thread/started","params":{}}'
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      i=0
      while [ $i -lt %d ]; do printf '%%s\n' '{"method":"noise"}'; i=$((i+1)); done
      printf '%%s\n' '{"id":1,"result":{}}' ;;
    *'"method":"account/rateLimits/read"'*) printf '%%s\n' '%s'; exit 0 ;;
  esac
done
`, codexRPCMaxLines-2, codexTwoBuckets)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexBinEnv, path)

	p := codexLiveFetch()
	if p.OK {
		t.Fatalf("a server past its read budget was still read as a reading: %+v", p)
	}
	if p.DetailCode != "cli-shape" {
		t.Errorf("a budget refusal should read as a shape we do not know, got %q/%q", p.Error, p.DetailCode)
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
	// The background shell inherits stdout and outlives the process we kill,
	// which is exactly the npm launcher's shape. It also leaves a mark once a
	// second for as long as it lives, which is how the second half of this test
	// tells "collected" from "still out there".
	script := "#!/bin/sh\n" +
		"D=$(dirname \"$0\")\n" +
		"sh -c 'while :; do echo . >> \"$1/beat\"; sleep 1; done' _ \"$D\" &\n" +
		"exec sleep 60\n"
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

	// Giving up on the read is only half the job. Killing the process we started
	// leaves the one IT started running, still holding the pipe, and the cache
	// floor means another one every two minutes for as long as the failure lasts
	// — a leak measured in processes per hour on a machine nobody is watching.
	beat := filepath.Join(dir, "beat")
	size := func() int64 {
		fi, err := os.Stat(beat)
		if err != nil {
			return -1
		}
		return fi.Size()
	}
	for i := 0; i < 60 && size() < 0; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	before := size()
	if before < 0 {
		t.Fatal("the grandchild never ran, so this proves nothing about collecting it")
	}
	// Two of its beats: enough that a live one has certainly moved the file.
	time.Sleep(2500 * time.Millisecond)
	if after := size(); after != before {
		t.Errorf("the grandchild outlived the collector and kept working (%d → %d bytes) — "+
			"a kill that names one process does not reach what that process started", before, after)
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

// An entry under the account's name that carries no readable window is not an
// account reading, and preferring it because the KEY was there would lose the
// main quota to a technicality — silently, whenever an extra meter is present to
// keep the reading alive.
func TestCodexLiveFallsBackToTheFlatViewWhenTheAccountEntryHasNoWindow(t *testing.T) {
	fakeCodexBinary(t, `{"id":2,"result":{`+
		`"rateLimits":{"limitId":"codex","primary":{"usedPercent":55,"windowDurationMins":10080},`+
		`"planType":"prolite"},`+
		`"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":null,"secondary":null},`+
		`"codex_other":{"limitId":"codex_other","primary":{"usedPercent":5,"windowDurationMins":10080}}}}}`)

	p := codexLiveFetch()
	if !p.OK || len(p.Limits) != 2 {
		t.Fatalf("an empty account entry took the account quota with it: %+v", p)
	}
	if p.Limits[0].Key != "primary" || p.Limits[0].UsedPercent != 55 {
		t.Errorf("the flat view should have answered for the account: %+v", p.Limits[0])
	}
	if p.PlanType == nil || *p.PlanType != "prolite" {
		t.Errorf("the plan comes with the bucket that answered, got %v", p.PlanType)
	}
}

// No account bucket anywhere on the payload means nobody speaks for the account
// — not the extra meter that happens to sort first. Reading identity off
// position gives that meter the bare "primary" key, drops the scope that tells
// it apart, and lets it name the plan and the credit balance.
func TestCodexLiveDoesNotPromoteAnExtraMeterToTheAccount(t *testing.T) {
	fakeCodexBinary(t, `{"id":2,"result":{"rateLimitsByLimitId":{`+
		`"codex_bengalfox":{"limitId":"codex_bengalfox","limitName":"GPT-5.3-Codex-Spark",`+
		`"primary":{"usedPercent":30,"windowDurationMins":10080},`+
		`"credits":{"hasCredits":true,"unlimited":false,"balance":"5"},"planType":"prolite"}}}}`)

	p := codexLiveFetch()
	if !p.OK || len(p.Limits) != 1 {
		t.Fatalf("the extra meter should still be reported: %+v", p)
	}
	if p.Limits[0].Key != "codex_bengalfox/p" {
		t.Errorf("an extra meter must keep its namespaced key, got %q", p.Limits[0].Key)
	}
	if p.Limits[0].Scope == nil || *p.Limits[0].Scope != "GPT-5.3-Codex-Spark" {
		t.Errorf("an extra meter must keep the name that tells it apart: %+v", p.Limits[0])
	}
	if p.PlanType != nil {
		t.Errorf("the plan belongs to the account bucket, and there is none: %v", *p.PlanType)
	}
	if p.Credits != nil {
		t.Errorf("the balance belongs to the account bucket, and there is none: %+v", p.Credits)
	}
}

// The tolerance has to reach the objects, not only the scalars inside them. A
// vendor that reshapes one optional field must cost this leg that field — not
// the reading, and not the leg. Going dark here means falling back to a log
// that on this machine may be a fortnight old, which is the exact failure the
// live leg was written to end.
func TestCodexLiveSurvivesOneFieldArrivingInAStrangeShape(t *testing.T) {
	fakeCodexBinary(t, `{"id":2,"result":{`+
		`"rateLimits":{"limitId":"codex","primary":{"usedPercent":40,"windowDurationMins":10080},`+
		`"secondary":"who knows","credits":"not an object","planType":"prolite"},`+
		`"rateLimitsByLimitId":"not a map either"}}`)

	p := codexLiveFetch()
	if !p.OK {
		t.Fatalf("one odd field cost the whole reading: %+v", p)
	}
	if len(p.Limits) != 1 || p.Limits[0].UsedPercent != 40 {
		t.Fatalf("the window that parsed should still be reported: %+v", p.Limits)
	}
	if p.PlanType == nil || *p.PlanType != "prolite" {
		t.Errorf("a good field beside a bad one should survive it, got %v", p.PlanType)
	}
	if p.Credits != nil {
		t.Errorf("credits nobody can read should be absent, not invented: %+v", p.Credits)
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
