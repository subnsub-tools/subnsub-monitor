package main

// The local rung: the credential file, and the flow with a stand-in vendor.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCred(t *testing.T, rel string, doc any) string {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalExpand(t *testing.T) {
	t.Setenv("HOME", "/h")
	t.Setenv("GROK_HOME", "")
	t.Setenv("KIMI_HOME", "/k")
	if got := localExpand("~/.grok/auth.json"); got != "/h/.grok/auth.json" {
		t.Errorf("tilde: %q", got)
	}
	if got := localExpand("$GROK_HOME/auth.json"); got != "" {
		t.Errorf("unset variable must yield no path, got %q", got)
	}
	if got := localExpand("$KIMI_HOME/credentials/x.json"); got != "/k/credentials/x.json" {
		t.Errorf("set variable: %q", got)
	}
	if got := localExpand("relative/path"); got != "" {
		t.Errorf("relative paths are not opened, got %q", got)
	}
}

func TestLocalPrepareReadsTheToolsOwnFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GROK_HOME", "")
	claude := vendorOf(t, "claude")
	xai := vendorOf(t, "xai")

	// Nothing on disk: not signed in, and the error names the table's own
	// spelling of the path, never the resolved one.
	if _, _, _, fail := localPrepare(claude, "claude"); fail == nil || fail.DetailCode != "creds-missing" || fail.DetailArg != "~/.claude/.credentials.json" {
		t.Errorf("missing file: %+v", fail)
	}

	// Claude's file, live.
	writeCred(t, ".claude/.credentials.json", map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "tok-claude", "expiresAt": float64(now()*1000 + 3600e3), "subscriptionType": "max", "rateLimitTier": "default_claude_max_20x"}})
	cred, token, account, fail := localPrepare(claude, "claude")
	if fail != nil || token != "tok-claude" || account != "" || cred == nil {
		t.Fatalf("claude prepare: %q %q %+v", token, account, fail)
	}
	if plan := adapterPick(claude.Credential.Plan, nil, cred); plan != "Max" {
		t.Errorf("plan from the file: %q", plan)
	}
	if tier := adapterPick(claude.Credential.Tier, nil, cred); tier != "20x" {
		t.Errorf("tier from the file: %q", tier)
	}

	// Expired: reported, never refreshed.
	writeCred(t, ".claude/.credentials.json", map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "tok-claude", "expiresAt": float64(1000)}})
	if _, _, _, fail := localPrepare(claude, "claude"); fail == nil || fail.DetailCode != "creds-expired" || fail.DetailArg != "claude" {
		t.Errorf("expired: %+v", fail)
	}

	// Grok's file is keyed by issuer and account id; `*` reads whichever.
	writeCred(t, ".grok/auth.json", map[string]any{"https://auth.x.ai::abc": map[string]any{
		"key": "tok-grok", "email": "g@example.com", "user_id": "u-1", "expires_at": "2099-01-01T00:00:00Z"}})
	cred, token, account, fail = localPrepare(xai, "grok")
	if fail != nil || token != "tok-grok" || account != "g@example.com" {
		t.Fatalf("grok prepare: %q %q %+v", token, account, fail)
	}
	if got := adapterTemplate(xai.Requests[0].Headers["x-userid"], adapterCtx{cred}, cred); got != "u-1" {
		t.Errorf("x-userid from the file: %q", got)
	}
}

// macOS keeps Claude Code's document in the login keychain; the table names
// the item and the rung reads it when none of the files exists — through
// the seam, since the test machine has no keychain. The file, where it
// exists, comes first; and with neither the error names the table's file.
func TestLocalCredentialReadsTheKeychainWhenTheFileIsAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	claude := vendorOf(t, "claude")
	if claude.Credential.Keychain != "Claude Code-credentials" {
		t.Fatalf("the table names the item: %q", claude.Credential.Keychain)
	}
	saved := readKeychain
	defer func() { readKeychain = saved }()
	asked := ""
	readKeychain = func(service string) ([]byte, bool) {
		asked = service
		raw, _ := json.Marshal(map[string]any{
			"claudeAiOauth": map[string]any{"accessToken": "tok-keychain", "expiresAt": float64(now()*1000 + 3600e3), "subscriptionType": "max"},
			"mcpOAuth":      map[string]any{"x": map[string]any{"accessToken": "never-read"}},
		})
		return raw, true
	}
	cred, token, _, fail := localPrepare(claude, "claude")
	if fail != nil || token != "tok-keychain" || asked != "Claude Code-credentials" {
		t.Fatalf("keychain prepare: %q asked=%q %+v", token, asked, fail)
	}
	if plan := adapterPick(claude.Credential.Plan, nil, cred); plan != "Max" {
		t.Errorf("plan from the keychain document: %q", plan)
	}
	// The native collector reads the same item.
	if auth, ok := readClaudeAuth(); !ok || auth.token != "tok-keychain" || auth.plan != "max" {
		t.Errorf("native: %+v %v", auth, ok)
	}
	// The file, where it exists, is the answer and the keychain is not asked.
	path := writeCred(t, ".claude/.credentials.json", map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "tok-file", "expiresAt": float64(now()*1000 + 3600e3)}})
	asked = ""
	if _, token, _, fail := localPrepare(claude, "claude"); fail != nil || token != "tok-file" || asked != "" {
		t.Errorf("file first: %q asked=%q %+v", token, asked, fail)
	}
	// Neither: not signed in, named by the file the table spells.
	os.Remove(path)
	readKeychain = func(string) ([]byte, bool) { return nil, false }
	if _, _, _, fail := localPrepare(claude, "claude"); fail == nil || fail.DetailCode != "creds-missing" || fail.DetailArg != "~/.claude/.credentials.json" {
		t.Errorf("nothing anywhere: %+v", fail)
	}
	// A keychain item that is not the document is no credential either.
	readKeychain = func(string) ([]byte, bool) { return []byte("not json"), true }
	if _, _, _, fail := localPrepare(claude, "claude"); fail == nil || fail.DetailCode != "creds-missing" {
		t.Errorf("garbage item: %+v", fail)
	}
}

// The keychain tool's environment is an allowlist, and an allowlist with
// nothing in it must still be one: nil would mean "inherit everything".
func TestKeychainEnvIsNeverNil(t *testing.T) {
	for _, k := range []string{"HOME", "USER", "LOGNAME", "TMPDIR"} {
		t.Setenv(k, "x") // so the cleanup restores it
		os.Unsetenv(k)
	}
	t.Setenv("SUBNSUB_MONITOR_TOKEN", "secret")
	env := keychainEnv()
	if env == nil || len(env) != 0 {
		t.Fatalf("with none of the four set: %#v", env)
	}
	t.Setenv("HOME", "/h")
	env = keychainEnv()
	if len(env) != 1 || env[0] != "HOME=/h" {
		t.Fatalf("HOME alone: %#v", env)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "SUBNSUB_") {
			t.Fatalf("the relay token reached the tool's environment: %q", kv)
		}
	}
}

func TestLocalSenderFillsTheToken(t *testing.T) {
	// The sender is exercised through adapterFetch with the real table and a
	// vendor that only checks what it was sent.
	t.Setenv("HOME", t.TempDir())
	writeCred(t, ".claude/.credentials.json", map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "tok-claude", "expiresAt": float64(now()*1000 + 3600e3), "subscriptionType": "pro"}})
	claude := vendorOf(t, "claude")
	cred, token, _, fail := localPrepare(claude, "claude")
	if fail != nil {
		t.Fatal(fail.Detail)
	}
	seen := map[string]string{}
	send := func(method, target string, header map[string]string, data string) (int, []byte, *Provider) {
		seen[target] = header["Authorization"]
		if _, beta := header["anthropic-beta"]; beta {
			t.Error("the beta header is the proxy's; sent locally it earns a 429")
		}
		if !strings.HasPrefix(header["User-Agent"], "claude-cli/") {
			t.Errorf("the reset grants answer only the CLI's own agent: %q", header["User-Agent"])
		}
		if target == "https://api.anthropic.com/api/oauth/usage?cedar_ember=1" {
			return 200, []byte(`{"limits":[{"kind":"session","percent":7}]}`), nil
		}
		return 500, nil, nil
	}
	// The table leaves `$TOKEN$`; the local sender replaces it before the
	// request leaves (localSender does the same with provRequest behind it).
	wrapped := func(method, target string, header map[string]string, data string) (int, []byte, *Provider) {
		hdr := map[string]string{}
		for k, v := range header {
			hdr[k] = replaceToken(v, token)
		}
		return send(method, target, hdr, data)
	}
	p := adapterFetch(Provider{ID: "claude", Name: "Claude Code", Source: "api"}, claude, cred, "local", wrapped)
	if !p.OK || len(p.Limits) != 1 || p.Limits[0].UsedPercent != 7 {
		t.Fatalf("reading: %+v", p)
	}
	if seen["https://api.anthropic.com/api/oauth/usage?cedar_ember=1"] != "Bearer tok-claude" {
		t.Errorf("token not filled in: %q", seen["https://api.anthropic.com/api/oauth/usage?cedar_ember=1"])
	}
	if _, asked := seen["https://api.anthropic.com/api/oauth/profile"]; asked {
		t.Error("the profile call is the proxy's; the local rung must not make it")
	}
	if p.PlanType == nil || *p.PlanType != "Pro" {
		t.Errorf("plan falls back to the file when the profile call failed: %+v", p.PlanType)
	}
}

// agy's token file (Linux, and wherever the OS keyring is unavailable):
// `{"token":{"access_token","token_type","refresh_token","expiry"},
// "auth_method","id_token"}`, the expiry an RFC 3339 stamp with nanoseconds
// and a zone offset, as the CLI writes it.
func TestLocalPrepareReadsAgyTokenFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag := vendorOf(t, "antigravity")
	if _, _, _, fail := localPrepare(ag, "antigravity"); fail == nil || fail.DetailCode != "creds-missing" || fail.DetailArg != "~/.gemini/antigravity-cli/antigravity-oauth-token" {
		t.Errorf("missing file: %+v", fail)
	}
	future := time.Now().Add(50 * time.Minute).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02T15:04:05.999999999-07:00")
	writeCred(t, ".gemini/antigravity-cli/antigravity-oauth-token", map[string]any{
		"token":       map[string]any{"access_token": "tok-agy", "token_type": "Bearer", "refresh_token": "never-read", "expiry": future},
		"auth_method": "consumer", "id_token": "eyJ.never.read"})
	_, token, account, fail := localPrepare(ag, "antigravity")
	if fail != nil || token != "tok-agy" || account != "" {
		t.Fatalf("agy prepare: %q %q %+v", token, account, fail)
	}
	writeCred(t, ".gemini/antigravity-cli/antigravity-oauth-token", map[string]any{
		"token": map[string]any{"access_token": "tok-agy", "expiry": "2026-10-14T10:47:58.177583237+08:00"}})
	if _, _, _, fail := localPrepare(ag, "antigravity"); fail == nil || fail.DetailCode != "creds-expired" || fail.DetailArg != "agy" {
		t.Errorf("expired (the CLI refreshes it on its next run, never this): %+v", fail)
	}
}

// The local channel asks for the quota without a project — Google answers
// retrieveUserQuotaSummary without one (verified 2026-10-14) and agy's file
// names none — while the proxy's request keeps the project its credential
// carries. One quota call per channel, never both.
func TestLocalAntigravityAsksTheQuotaWithoutAProject(t *testing.T) {
	ag := vendorOf(t, "antigravity")
	quota := `{"groups":[{"displayName":"Gemini Models","buckets":[
	  {"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","resetTime":"2026-10-15T01:15:46Z","remainingFraction":0.6895451},
	  {"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","resetTime":"2026-10-14T06:15:46Z","remainingFraction":1}]}]}`
	assist := `{"currentTier":{"id":"free-tier","name":"Antigravity"}}`
	var bodies []string
	send := func(method, target string, header map[string]string, data string) (int, []byte, *Provider) {
		// The channel's sender fills $TOKEN$ (localSender; the proxy for the
		// CPA rung) — the table hands it on as written.
		if header["Authorization"] != "Bearer $TOKEN$" {
			t.Errorf("the token placeholder was not handed on: %q", header["Authorization"])
		}
		switch {
		case strings.HasSuffix(target, ":retrieveUserQuotaSummary"):
			bodies = append(bodies, data)
			return 200, []byte(quota), nil
		case strings.HasSuffix(target, ":loadCodeAssist"):
			return 200, []byte(assist), nil
		}
		t.Errorf("unexpected call %s", target)
		return 404, nil, nil
	}
	p := adapterFetch(Provider{ID: "antigravity", Name: "Antigravity", Source: "api"}, ag,
		map[string]any{"token": map[string]any{"access_token": "tok-agy"}}, "local", send)
	if !p.OK || len(p.Limits) != 2 || p.PlanType == nil || *p.PlanType != "Free" {
		t.Fatalf("local reading: %+v", p)
	}
	if len(bodies) != 1 || bodies[0] != "{}" {
		t.Errorf("the local channel asks once, with no project: %q", bodies)
	}
	if p.Limits[0].Key != "gemini-weekly" || p.Limits[0].UsedPercent != 31.05 || p.Limits[0].Scope == nil || *p.Limits[0].Scope != "Gemini Models" {
		t.Errorf("weekly: %+v", p.Limits[0])
	}
	bodies = nil
	p = adapterFetch(Provider{ID: "antigravity", Name: "Antigravity", Source: "cpa"}, ag,
		map[string]any{"access_token": "tok-cpa", "project_id": "persuasive-truth-c8gvj"}, "cpa", send)
	if !p.OK || len(bodies) != 1 || bodies[0] != `{"project":"persuasive-truth-c8gvj"}` {
		t.Errorf("the proxy's channel carries the project: %q %+v", bodies, p)
	}
}

// Antigravity hands over on ANY failure of the table's road: its native road
// needs no credential (the CLI refreshes its own), so a missing or stale file
// must not cost the reading the old code got.
func TestCollectLocalElseHandsOverOnAnyFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	calls := 0
	native := func() Provider { calls++; return Provider{ID: "antigravity", OK: true, Source: "cli", Name: "native"} }
	localCache.Lock()
	saved := localCache.per
	localCache.per = nil
	localCache.Unlock()
	localFallbackSet("antigravity", nil)
	t.Cleanup(func() {
		localCache.Lock()
		localCache.per = saved
		localCache.Unlock()
		localFallbackSet("antigravity", nil)
	})
	// No file: collectLocalOr would report "not signed in"; this hands over.
	p := collectLocalElse("antigravity", native)
	if !p.OK || p.Name != "native" || calls != 1 {
		t.Fatalf("missing file must hand over: calls=%d %+v", calls, p)
	}
	// An expired file: the same.
	localCache.Lock()
	localCache.per = nil
	localCache.Unlock()
	writeCred(t, ".gemini/antigravity-cli/antigravity-oauth-token", map[string]any{
		"token": map[string]any{"access_token": "tok-agy", "expiry": "2026-10-14T10:47:58.177583237+08:00"}})
	p = collectLocalElse("antigravity", native)
	if !p.OK || p.Name != "native" || calls != 2 {
		t.Fatalf("expired file must hand over: calls=%d %+v", calls, p)
	}
	// And collectLocalOr keeps its narrower rule for the vendors that have it.
	localCache.Lock()
	localCache.per = nil
	localCache.Unlock()
	kimiCalls := 0
	p = collectLocalOr("kimi", func() Provider { kimiCalls++; return Provider{ID: "kimi", OK: true} })
	if p.OK || p.DetailCode != "creds-missing" || kimiCalls != 0 {
		t.Errorf("collectLocalOr unchanged: calls=%d %+v", kimiCalls, p)
	}
}

// A success the table once had must not paper over a token that has since
// expired: the cache serves that success for half an hour, and the
// judgement is the fetch's — a press finds the CLI's report, fresh, not the
// table's reading from before the token lapsed.
func TestCollectLocalElseJudgesTheFetchNotTheServedReading(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	paceDaemon(t)
	calls := 0
	native := func() Provider { calls++; return Provider{ID: "antigravity", OK: true, Source: "cli", Name: "native"} }
	stale := Provider{ID: "antigravity", OK: true, Source: "api", Name: "table", PlanType: sp("Pro"), RecordedAt: fp(now() - 200)}
	localCache.Lock()
	saved := localCache.per
	localCache.per = map[string]*provCache{"antigravity": {last: &stale, fetchedAt: now() - 200, attemptedAt: now() - 200, pace: paceMark{id: "antigravity"}}}
	localCache.Unlock()
	t.Cleanup(func() {
		localCache.Lock()
		localCache.per = saved
		localCache.Unlock()
	})
	writeCred(t, ".gemini/antigravity-cli/antigravity-oauth-token", map[string]any{
		"token": map[string]any{"access_token": "tok-agy", "expiry": "2026-10-14T10:47:58.177583237+08:00"}})
	// Not due: the table's success stands, nothing is fetched or asked.
	p := collectLocalElse("antigravity", native)
	if !p.OK || p.Name != "table" || calls != 0 {
		t.Fatalf("held success: calls=%d %+v", calls, p)
	}
	// A press: the fetch finds the token expired; the cache would serve the
	// old success, the judgement hands over — and the plan chip the table
	// read rides along.
	quotaForce()
	p = collectLocalElse("antigravity", native)
	if !p.OK || p.Name != "native" || calls != 1 || p.PlanType == nil || *p.PlanType != "Pro" {
		t.Fatalf("expired since the last success must hand over: calls=%d %+v", calls, p)
	}
}

// The table's 429 backoff consumes presses and serves its refusal; the
// native road is asked regardless — it paces itself — so a reading the CLI
// can give is not held behind a vendor that is saying no to the table.
func TestCollectLocalElseAsksNativeWhileTheTableCools(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	paceDaemon(t)
	calls := 0
	native := func() Provider { calls++; return Provider{ID: "antigravity", OK: true, Source: "cli", Name: "native"} }
	limited := Provider{ID: "antigravity"}
	limited.failWith("rate-limited", "http-429")
	localCache.Lock()
	saved := localCache.per
	localCache.per = map[string]*provCache{"antigravity": {fail: &limited, attemptedAt: now() - 10, coolUntil: now() + 600, strikes: 1, pace: paceMark{id: "antigravity"}}}
	localCache.Unlock()
	t.Cleanup(func() {
		localCache.Lock()
		localCache.per = saved
		localCache.Unlock()
	})
	for i := 1; i <= 2; i++ {
		p := collectLocalElse("antigravity", native)
		if !p.OK || p.Name != "native" || calls != i {
			t.Fatalf("round %d: the native road must be asked while the table cools: calls=%d %+v", i, calls, p)
		}
	}
	// The native road's refusal does not displace a table reading still held.
	localCache.Lock()
	held := Provider{ID: "antigravity", OK: true, Source: "api", Name: "table"}
	localCache.per["antigravity"] = &provCache{last: &held, fetchedAt: now() - 300, fail: &limited, attemptedAt: now() - 10, coolUntil: now() + 600, pace: paceMark{id: "antigravity"}}
	localCache.Unlock()
	refusing := func() Provider { f := Provider{ID: "antigravity"}; f.failWith("not-installed", "ls-none"); return f }
	if p := collectLocalElse("antigravity", refusing); !p.OK || p.Name != "table" {
		t.Fatalf("stale-but-real beats the native refusal: %+v", p)
	}
}

func TestCollectLocalOrFallsBackToNative(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	native := func() Provider { return Provider{ID: "kimi", OK: true, Source: "api", Name: "native"} }
	// No file on disk: the table's answer is "not signed in", which stands —
	// it is a real answer, and the native collector would say the same.
	localCache.Lock()
	localCache.per = nil
	localCache.Unlock()
	p := collectLocalOr("kimi", native)
	if p.OK || p.DetailCode != "creds-missing" {
		t.Errorf("missing file should be reported by the table path: %+v", p)
	}
	// A vendor the table has no row for goes straight to the native collector.
	p = collectLocalOr("copilot", native)
	if !p.OK || p.Name != "native" {
		t.Errorf("no table row: %+v", p)
	}
}

// The table's failure surfaces only once its last success has aged out —
// long after the press that produced it. The native road must not be taken
// then: its cache is its own, and empty or holding an unseen press it would
// ask the vendor with nobody pressing anything.
func TestCollectLocalOrDoesNotAskOnAgeAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	paceDaemon(t)
	calls := 0
	native := func() Provider { calls++; return Provider{ID: "kimi", OK: true, Source: "api", Name: "native"} }
	failed := Provider{ID: "kimi"}
	failed.failWith("api-error", "no-window")
	localCache.Lock()
	saved := localCache.per
	localCache.per = map[string]*provCache{"kimi": {fail: &failed, attemptedAt: now() - 7200}}
	localCache.Unlock()
	localFallbackSet("kimi", nil)
	t.Cleanup(func() {
		localCache.Lock()
		localCache.per = saved
		localCache.Unlock()
		localFallbackSet("kimi", nil)
	})

	// A press was made and consumed elsewhere; this cache has not seen it.
	// Age alone brought the failure out, so nothing is asked.
	for i := 0; i < 3; i++ {
		p := collectLocalOr("kimi", native)
		if calls != 0 || p.OK || p.DetailCode != "no-window" {
			t.Fatalf("an aged failure must not start the native road: calls=%d %+v", calls, p)
		}
	}
	// What the native road answered when it WAS asked is what is served.
	held := Provider{ID: "kimi", OK: true, Name: "held", RecordedAt: fp(now() - 600)}
	localFallbackSet("kimi", &held)
	p := collectLocalOr("kimi", native)
	if calls != 0 || !p.OK || p.Name != "held" {
		t.Errorf("the kept native reading must be served without asking: calls=%d %+v", calls, p)
	}
}
