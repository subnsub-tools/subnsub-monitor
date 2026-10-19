package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The server reports what is LEFT; every other provider here reports what is
// USED, and the card draws a bar that fills as you spend. Getting this
// backwards would show a fresh account as exhausted and an exhausted one as
// fresh — wrong in the most convincing possible way.
func TestAgLimitsInvertsRemainingIntoUsed(t *testing.T) {
	body := `{"groups":[
	  {"displayName":"Gemini Models","buckets":[
	    {"bucketId":"FIVE_HOUR","remaining":{"remainingFraction":0.75},"resetTime":"2026-07-31T18:00:00Z"},
	    {"bucketId":"WEEKLY","remaining":{"remainingFraction":0.4},"resetTime":"2026-08-04T00:00:00Z"}]},
	  {"displayName":"Claude and GPT models","buckets":[
	    {"bucketId":"FIVE_HOUR","remaining":{"remainingFraction":1}}]}]}`
	var s agSummary
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	lims := agLimits(s.Groups)
	if len(lims) != 3 {
		t.Fatalf("want 3 limits, got %d", len(lims))
	}
	if lims[0].UsedPercent != 25 {
		t.Fatalf("0.75 remaining should be 25%% used, got %v", lims[0].UsedPercent)
	}
	if lims[1].UsedPercent != 60 {
		t.Fatalf("0.4 remaining should be 60%% used, got %v", lims[1].UsedPercent)
	}
	if lims[2].UsedPercent != 0 {
		t.Fatalf("a full bucket should be 0%% used, got %v", lims[2].UsedPercent)
	}

	// The window vocabulary has to match what Codex and Claude already put in
	// that column, or one card shows three spellings of the same idea.
	if lims[0].WindowLabel == nil || *lims[0].WindowLabel != "5h" {
		t.Fatalf("FIVE_HOUR window label: %v", lims[0].WindowLabel)
	}
	if lims[1].WindowLabel == nil || *lims[1].WindowLabel != "7d" {
		t.Fatalf("WEEKLY window label: %v", lims[1].WindowLabel)
	}
	// The group name is the row's scope, which is how two model families share
	// one provider block without becoming two providers.
	if lims[0].Scope == nil || *lims[0].Scope != "Gemini Models" {
		t.Fatalf("scope: %v", lims[0].Scope)
	}
	if lims[0].ResetsAt == nil || *lims[0].ResetsAt <= 0 {
		t.Fatal("ISO reset time was not parsed")
	}
	// A bucket with no reset time gets none, rather than a zero that would
	// render as a countdown that already expired.
	if lims[2].ResetsAt != nil {
		t.Fatalf("invented a reset time: %v", *lims[2].ResetsAt)
	}
}

// The wrapped shape, and the older flat remainingFraction, both have to work —
// they are the two forms this reply has been seen in.
func TestAgLimitsAcceptsBothShapes(t *testing.T) {
	var s agSummary
	body := `{"response":{"groups":[{"displayName":"Gemini Models",
	  "buckets":[{"bucketId":"WEEKLY","remainingFraction":0.1}]}]}}`
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s.Groups) != 0 || s.Response == nil {
		t.Fatal("expected the wrapped shape")
	}
	lims := agLimits(s.Response.Groups)
	if len(lims) != 1 || lims[1-1].UsedPercent != 90 {
		t.Fatalf("flat remainingFraction not read: %+v", lims)
	}
}

// A bucket that says nothing contributes nothing. The failure mode to avoid is
// a missing number rendering as zero — "0% used" is a claim, not a gap.
func TestAgLimitsSkipsBucketsWithNoNumber(t *testing.T) {
	var s agSummary
	body := `{"groups":[{"displayName":"Gemini Models","buckets":[
	  {"bucketId":"WEEKLY"},{"bucketId":"FIVE_HOUR","remaining":{}}]}]}`
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if lims := agLimits(s.Groups); len(lims) != 0 {
		t.Fatalf("invented %d readings from nothing: %+v", len(lims), lims)
	}
}

func TestAgResetAcceptsEveryUnitItIsSentIn(t *testing.T) {
	if got := agReset("2026-07-31T18:00:00Z"); got < 1.7e9 || got > 2.0e9 {
		t.Fatalf("ISO: %v", got)
	}
	if got := agReset(float64(1785500000)); got != 1785500000 {
		t.Fatalf("epoch seconds: %v", got)
	}
	// Milliseconds, the obvious mistake, must not land the reset 50,000 years
	// out and read as a countdown that never moves.
	if got := agReset(float64(1785500000000)); got != 1785500000 {
		t.Fatalf("epoch milliseconds: %v", got)
	}
	if got := agReset("not a time"); got != 0 {
		t.Fatalf("garbage: %v", got)
	}
	if got := agReset(nil); got != 0 {
		t.Fatalf("absent: %v", got)
	}
}

// The process matcher decides whether this collector talks to something at
// all, so the two directions both get a test: a real server matches, and
// anything that merely mentions the word does not.
func TestAgIsServerNeedsMoreThanTheWord(t *testing.T) {
	yes := [][]string{
		{"/opt/Antigravity/resources/app/extensions/language_server", "--app_data_dir", "/home/x/.antigravity", "--csrf_token", "abc123"},
		{"/opt/Antigravity/language_server", "--app_data_dir=/home/x/.antigravity"},
		{"/home/x/.antigravity-cli/bin/server", "--port", "0"},
		{"/opt/antigravity/bin/agy", "serve"},
	}
	for _, argv := range yes {
		if !agIsServer(argv) {
			t.Fatalf("should have matched: %v", argv)
		}
	}
	no := [][]string{
		{"/usr/bin/grep", "-r", "antigravity", "."},
		{"/opt/windsurf/language_server", "--app_data_dir", "/home/x/.windsurf"},
		{"/usr/bin/vim", "antigravity_notes.md"},
		// The marker has to be a FLAG VALUE. Another Codeium-derived editor
		// with a project called "antigravity" open is the realistic version of
		// this, and it used to match on the word alone.
		{"/opt/windsurf/language_server", "--app_data_dir", "/home/x/.windsurf", "--folder", "/home/x/src/antigravity"},
		// argv[0] is whatever a process says it is, so a two-letter name is
		// not identity on its own.
		{"/tmp/agy", "serve"},
		{},
		{""},
	}
	for _, argv := range no {
		if agIsServer(argv) {
			t.Fatalf("should NOT have matched: %v", argv)
		}
	}
}

// A fraction outside [0,1] is a protocol change or a bad reading, and clamping
// it would dress both up as a confident number — -1 saturating to "100% used"
// looks exactly like a genuinely exhausted quota.
func TestAgLimitsRefusesFractionsOutsideTheRange(t *testing.T) {
	var s agSummary
	body := `{"groups":[{"displayName":"Gemini Models","buckets":[
	  {"bucketId":"A","remaining":{"remainingFraction":-1}},
	  {"bucketId":"B","remaining":{"remainingFraction":2}},
	  {"bucketId":"C","remaining":{"remainingFraction":0.5}}]}]}`
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	lims := agLimits(s.Groups)
	if len(lims) != 1 || lims[0].Key != "c" || lims[0].UsedPercent != 50 {
		t.Fatalf("out-of-range fractions were not dropped: %+v", lims)
	}
}

// key and scope are rendered in every browser watching the account, and the
// server they came from was authenticated in no way at all.
func TestAgKeyAndScopeAreSafeToPublish(t *testing.T) {
	if got := agKey("FIVE_HOUR"); got != "five_hour" {
		t.Fatalf("ordinary id: %q", got)
	}
	for _, bad := range []string{"has space", "/home/someone/secret", "a\u202eb", strings.Repeat("x", 25)} {
		if got := agKey(bad); got != "quota" {
			t.Fatalf("%q should have become the generic word, got %q", bad, got)
		}
	}
	// The scope is free text meant for a human, so it is repaired rather than
	// refused — but the bidi override that can make a label render as
	// something it does not spell has to go.
	if got := agScope("Gemini\u202e Models\u0007"); got != "Gemini Models" {
		t.Fatalf("scope not cleaned: %q", got)
	}
}

// The token goes into an HTTP header, so anything that could split one is
// refused rather than trimmed.
func TestAgCsrfRefusesAnythingHeaderUnsafe(t *testing.T) {
	if got := agCsrf([]string{"srv", "--csrf_token", "abc123"}); got != "abc123" {
		t.Fatalf("space form: %q", got)
	}
	if got := agCsrf([]string{"srv", "--csrf_token=xyz789"}); got != "xyz789" {
		t.Fatalf("equals form: %q", got)
	}
	if got := agCsrf([]string{"srv", "--csrf_token", "bad\r\nX-Evil: 1"}); got != "" {
		t.Fatalf("header injection accepted: %q", got)
	}
	if got := agCsrf([]string{"srv"}); got != "" {
		t.Fatalf("absent: %q", got)
	}
}

// The summary in the shapes its three sources send (CodexBar
// AntigravityQuotaSummaryParser, steipete/CodexBar@62acfd8ead): a `summary`
// wrapper and `id` / `name` spellings from the CLI's report; a stated
// `window` that overrides the id; a `disabled` bucket that says nothing.
func TestAgLimitsReadsTheCLIReportShape(t *testing.T) {
	var s agSummary
	body := `{"summary":{"groups":[{"name":"Gemini Models","buckets":[
	  {"id":"gemini-weekly","name":"Weekly Limit Remaining","window":"weekly","remainingFraction":0.86,"resetTime":"2026-09-17T18:40:27Z"},
	  {"id":"gemini-5h","name":"Five Hour Limit Remaining","window":"5h","remainingFraction":0.95},
	  {"id":"3p-5h","window":"5h","remainingFraction":0.5,"disabled":true}]}]}}`
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	lims := agLimits(s.groups())
	if len(lims) != 2 {
		t.Fatalf("two live buckets, one disabled: %+v", lims)
	}
	if lims[0].Key != "gemini-weekly" || lims[0].UsedPercent != 14 || lims[0].WindowLabel == nil || *lims[0].WindowLabel != "7d" ||
		lims[0].Scope == nil || *lims[0].Scope != "Gemini Models" || lims[0].ResetsAt == nil {
		t.Errorf("weekly: %+v", lims[0])
	}
	if lims[1].Key != "gemini-5h" || *lims[1].WindowLabel != "5h" {
		t.Errorf("5h: %+v", lims[1])
	}
}

// The CLI's own report (`agy -p /usage --output-format json`, agy 1.3.4 as
// observed on 2026-10-14): the `command.data` wrapper, snake_case buckets,
// a banner before the document — and the failure ladder when what came
// back is not a report.
func TestParseAgyReportReadsThePrintShape(t *testing.T) {
	p := Provider{ID: "antigravity", Name: "Antigravity", Source: "cli"}
	report := `{"conversation_id":"","status":"SUCCESS","response":"Gemini Models\tWeekly Limit Remaining\t100%\t2026-10-21T01:25:17Z\n","duration_seconds":0,"num_turns":0,` +
		`"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0},"command":{"name":"usage","data":{"description":"Within each group, models share a weekly limit and a 5-hour limit.","groups":[` +
		`{"name":"Gemini Models","description":"Models within this group: Gemini Flash, Gemini Pro","buckets":[` +
		`{"id":"gemini-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.86,"reset_time":"2026-10-21T01:25:17Z"},` +
		`{"id":"gemini-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":1,"reset_time":"2026-10-14T06:25:17Z"}]},` +
		`{"name":"Claude and GPT models","description":"Models within this group: Claude Opus, Claude Sonnet, GPT-OSS","buckets":[` +
		`{"id":"3p-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.5,"reset_time":"2026-10-21T01:25:17Z"},` +
		`{"id":"3p-5h","name":"Five Hour Limit Remaining","window":"5h","remaining_fraction":0.25,"disabled":true}]}]}}}`
	got := parseAgyReport(p, "A new agy is available.\n"+report+"\n", false)
	if !got.OK || got.Source != "cli" || len(got.Limits) != 3 || got.RecordedAt == nil {
		t.Fatalf("report not read: %+v", got)
	}
	w := got.Limits[0]
	if w.Key != "gemini-weekly" || w.UsedPercent != 14 || w.WindowLabel == nil || *w.WindowLabel != "7d" ||
		w.Scope == nil || *w.Scope != "Gemini Models" || w.ResetsAt == nil || *w.ResetsAt != isoToEpoch("2026-10-21T01:25:17Z") {
		t.Errorf("gemini weekly: %+v", w)
	}
	if h := got.Limits[1]; h.Key != "gemini-5h" || h.UsedPercent != 0 || *h.WindowLabel != "5h" {
		t.Errorf("gemini 5h: %+v", h)
	}
	if c := got.Limits[2]; c.Key != "3p-weekly" || c.UsedPercent != 50 || *c.Scope != "Claude and GPT models" {
		t.Errorf("claude/gpt weekly (and the disabled 5h bucket dropped): %+v", c)
	}

	// Not a report: the sign-in phrase claims that diagnosis, a failing exit
	// or a failed status is reported as that, a clean exit with no report is
	// a shape this build does not know, and a report with no bucket is empty.
	if f := parseAgyReport(p, "Error: You are not logged into Antigravity.\n", true); f.OK || f.Error != "not-signed-in" || f.DetailCode != "cli-signin" || f.DetailArg != "agy" {
		t.Errorf("signed out: %+v", f)
	}
	if f := parseAgyReport(p, `{"status":"FAILURE","response":"something broke","command":{"name":"usage"}}`, false); f.OK || f.DetailCode != "cli-exit" {
		t.Errorf("failed status: %+v", f)
	}
	if f := parseAgyReport(p, "panic: something\n", true); f.OK || f.DetailCode != "cli-exit" {
		t.Errorf("non-zero exit: %+v", f)
	}
	if f := parseAgyReport(p, `{"status":"SUCCESS","command":{"name":"help","data":{}}}`, false); f.OK || f.DetailCode != "cli-shape" {
		t.Errorf("not the usage command: %+v", f)
	}
	if f := parseAgyReport(p, `{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[]}}}`, false); f.OK || f.DetailCode != "cli-nolines" {
		t.Errorf("empty report: %+v", f)
	}
}

// A stated window names the period whatever the id says; a window this
// build does not know is no period, not a guess from the id.
func TestAgWindowStatedBeatsTheID(t *testing.T) {
	cases := []struct{ window, id, want string }{
		{"", "gemini-5h", "5h"},
		{"", "WEEKLY", "7d"},
		{"weekly", "gemini-5h", "7d"},
		{"5h", "gemini-weekly", "5h"},
		{"Five Hour Limit", "gemini-weekly", "5h"},
		{"unknown", "gemini-5h", ""},
	}
	for _, c := range cases {
		if got := agWindow(c.window, c.id); got != c.want {
			t.Errorf("window %q id %q: got %q want %q", c.window, c.id, got, c.want)
		}
	}
}

// The desktop app ships as Gemini.app since 2026-08 (CodexBar
// AntigravityStatusProbe, @a2444e62ec): its server's path is identity enough.
func TestAgIsServerKnowsTheGeminiBundle(t *testing.T) {
	if !agIsServer([]string{"/applications/gemini.app/contents/resources/bin/language_server", "--csrf_token", "abc"}) {
		t.Fatal("the Gemini.app bundle's server should match")
	}
	if agIsServer([]string{"/applications/gemini.app/contents/resources/bin/other_tool", "--csrf_token", "abc"}) {
		t.Fatal("only the server's own name, even in the bundle")
	}
}
