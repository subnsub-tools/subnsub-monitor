package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// withRoom seeds the account room for one test and clears it after, so a
// test that never seeds sees what a `--local` run sees: no keys.
func withRoom(t *testing.T, token string) {
	t.Helper()
	setAccountRoom(token)
	t.Cleanup(func() { setAccountRoom("") })
}

var keyShape = regexp.MustCompile(`^[0-9a-f]{12}$`)

func TestAccountKeyIsAFleetScopedHMAC(t *testing.T) {
	exp := time.Now().Add(24 * time.Hour)
	withRoom(t, fakeTokenFor(0xA1, exp))
	k := accountKey("codex", "Someone@Example.com ")
	if !keyShape.MatchString(k) {
		t.Fatalf("key shape: %q", k)
	}
	if k != accountKey("codex", "someone@example.com") {
		t.Fatal("case and whitespace must not split one account")
	}
	if k == accountKey("claude", "someone@example.com") {
		t.Fatal("the same identity at two vendors is two accounts")
	}
	if k == accountKey("codex", "other@example.com") {
		t.Fatal("two identities, two keys")
	}
	// A renewed token keeps the account part, and so the key.
	setAccountRoom(fakeTokenFor(0xA1, exp.Add(48*time.Hour)))
	if k != accountKey("codex", "someone@example.com") {
		t.Fatal("a renewal must not move the key")
	}
	// Another fleet's token is another room: the same address hashes to
	// something else, so no address list runs across rooms.
	setAccountRoom(fakeTokenFor(0xB2, exp))
	if k == accountKey("codex", "someone@example.com") {
		t.Fatal("two rooms must not share a key for one identity")
	}
}

func TestAccountKeyNeedsARoomAndAnIdentity(t *testing.T) {
	setAccountRoom("")
	if accountKey("codex", "someone@example.com") != "" {
		t.Fatal("no token, no key")
	}
	withRoom(t, fakeToken(time.Now().Add(time.Hour)))
	if accountKey("codex", "  ") != "" || accountKey("", "x") != "" {
		t.Fatal("an empty side gives no key")
	}
	if accountKey("codex", strings.Repeat("a", identMaxLen+1)) != "" {
		t.Fatal("an identity past the bound is not an identity")
	}
}

func TestAccountKeyLegacyTokenIsUsedWhole(t *testing.T) {
	// The self-minted kind: 32 raw bytes, no account part. It never rotates,
	// so keying on the whole string is stable.
	legacy := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	if _, ok := tokenIdentity(legacy); ok {
		t.Fatal("fixture must not carry an account part")
	}
	withRoom(t, legacy)
	if !keyShape.MatchString(accountKey("amp", "someone@example.com")) {
		t.Fatal("a legacy token still keys")
	}
}

func jwtWith(t *testing.T, payload any) string {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	seg := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return seg([]byte(`{"alg":"none"}`)) + "." + seg(body) + ".sig"
}

func TestJwtClaimReadsFlatAndNestedClaims(t *testing.T) {
	tok := jwtWith(t, map[string]any{
		"sub":   " user_1 ",
		"email": "someone@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_9",
		},
		"n": 5,
	})
	if got := jwtClaim(tok, "sub"); got != "user_1" {
		t.Fatalf("sub: %q", got)
	}
	if got := jwtClaim(tok, "https://api.openai.com/auth|chatgpt_account_id"); got != "acct_9" {
		t.Fatalf("nested: %q", got)
	}
	if jwtClaim(tok, "n") != "" || jwtClaim(tok, "missing") != "" || jwtClaim(tok, "sub|x") != "" {
		t.Fatal("a non-string, a missing claim and a walk through a string are all empty")
	}
	for _, bad := range []string{"", "opaque-token", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("[]")) + ".c"} {
		if jwtClaim(bad, "sub") != "" {
			t.Fatalf("%q must not yield a claim", bad)
		}
	}
}

func writeIdent(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexIdentityPrefersTheAccountID(t *testing.T) {
	home := t.TempDir()
	if codexIdentity(home) != "" {
		t.Fatal("no file, no identity")
	}
	idTok := jwtWith(t, map[string]any{
		"email":                       "someone@example.com",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_claim"},
	})
	auth := filepath.Join(home, ".codex", "auth.json")
	writeIdent(t, auth, `{"OPENAI_API_KEY":null,"tokens":{"id_token":"`+idTok+`","access_token":"x","refresh_token":"y","account_id":"acct_field"}}`)
	if got := codexIdentity(home); got != "acct_field" {
		t.Fatalf("account_id first: %q", got)
	}
	writeIdent(t, auth, `{"tokens":{"id_token":"`+idTok+`"}}`)
	if got := codexIdentity(home); got != "acct_claim" {
		t.Fatalf("then the claim: %q", got)
	}
	plain := jwtWith(t, map[string]any{"email": "someone@example.com"})
	writeIdent(t, auth, `{"tokens":{"id_token":"`+plain+`"}}`)
	if got := codexIdentity(home); got != "someone@example.com" {
		t.Fatalf("then the e-mail: %q", got)
	}
	writeIdent(t, auth, `{"OPENAI_API_KEY":"sk-..."}`)
	if codexIdentity(home) != "" {
		t.Fatal("an API key is not an account")
	}
}

func TestClaudeIdentityWalksOnlyTheAccountSubtree(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	// A big, early project history the walk must step over, then the account.
	history := strings.Repeat(`{"display":"prompt text","pastedContents":{}},`, 2000)
	writeIdent(t, path, `{"numStartups":3,"projects":{"/x":{"history":[`+history+`{}]}},`+
		`"oauthAccount":{"accountUuid":"uuid-1","emailAddress":" Someone@Example.com ","organizationUuid":"org-1"},"tail":1}`)
	if got := claudeIdentity(home); got != "Someone@Example.com" {
		t.Fatalf("e-mail first: %q", got)
	}
	writeIdent(t, path, `{"oauthAccount":{"accountUuid":"uuid-1"}}`)
	if got := claudeIdentity(home); got != "uuid-1" {
		t.Fatalf("then the uuid: %q", got)
	}
	writeIdent(t, path, `{"projects":{}}`)
	if claudeIdentity(home) != "" {
		t.Fatal("no account, no identity")
	}
	writeIdent(t, path, `[1,2]`)
	if claudeIdentity(home) != "" {
		t.Fatal("not an object, no identity")
	}
	// Skipped values of every shape, nested, with the account last.
	writeIdent(t, path, `{"a":[[{"b":[1,{"c":null}]}],"x"],"d":{"e":{"f":[]}},"g":"s","h":true,"i":1.5,"oauthAccount":{"accountUuid":"deep"}}`)
	if got := claudeIdentity(home); got != "deep" {
		t.Fatalf("after nested skips: %q", got)
	}
	writeIdent(t, path, `{"a":[1,2`)
	if claudeIdentity(home) != "" {
		t.Fatal("a truncated file gives nothing")
	}
}

func TestCopilotIdentityIsTheGithubLogin(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "github-copilot")
	writeIdent(t, filepath.Join(dir, "hosts.json"), `{"github.com":{"user":"octocat","oauth_token":"gho_x"},"ghe.example.com":{"user":"other","oauth_token":"gho_y"}}`)
	if got := copilotIdentity(home); got != "octocat" {
		t.Fatalf("hosts.json: %q", got)
	}
	writeIdent(t, filepath.Join(dir, "apps.json"), `{"github.com:Iv1.abc":{"user":"newer","oauth_token":"gho_z"}}`)
	if got := copilotIdentity(home); got != "newer" {
		t.Fatalf("apps.json first: %q", got)
	}
	writeIdent(t, filepath.Join(dir, "apps.json"), `{"github.com:Iv1.abc":{"oauth_token":"gho_z"}}`)
	if got := copilotIdentity(home); got != "" {
		t.Fatalf("the collector takes apps.json's token, whose login is unknown — no fall-through: %q", got)
	}
	writeIdent(t, filepath.Join(dir, "apps.json"), `{"github.com:Iv1.abc":{"user":"a","oauth_token":"gho_z"},"github.com:Iv1.def":{"user":"b","oauth_token":"gho_w"}}`)
	if got := copilotIdentity(home); got != "" {
		t.Fatalf("two logins in the file the token comes from is an account this cannot name: %q", got)
	}
	writeIdent(t, filepath.Join(dir, "apps.json"), `{"github.com:Iv1.abc":{"user":"a","oauth_token":"gho_z"},"github.com:Iv1.def":{"user":"a","oauth_token":"gho_w"}}`)
	if got := copilotIdentity(home); got != "a" {
		t.Fatalf("two entries, one login: %q", got)
	}
	t.Setenv(copilotTokenEnv, "gho_env")
	if copilotIdentity(home) != "" {
		t.Fatal("a token from the environment is not the file's account")
	}
}

func TestEnvTokensSilenceTheFileReaders(t *testing.T) {
	home := t.TempDir()
	jwt := jwtWith(t, map[string]any{"sub": "u-1"})
	writeIdent(t, filepath.Join(home, ".grok", "auth.json"), `{"https://auth.x.ai::openid":{"key":"`+jwt+`"}}`)
	writeIdent(t, filepath.Join(home, ".local", "share", "kilo", "auth.json"), `{"kilo":{"access":"`+jwt+`"}}`)
	writeIdent(t, filepath.Join(home, ".kimi-code", "credentials", "kimi-code.json"), `{"access_token":"`+jwt+`"}`)
	for _, env := range []string{grokHomeEnv, kimiHomeEnv, grokTokenEnv, grokCLITokenEnv, kiloTokenEnv, kimiTokenEnv} {
		t.Setenv(env, "")
	}
	if grokIdentity(home) != "u-1" || kiloIdentity(home) != "u-1" || kimiIdentity(home) != "u-1" {
		t.Fatal("fixture: the files name u-1")
	}
	for _, c := range []struct {
		env  string
		read func(string) string
	}{{grokTokenEnv, grokIdentity}, {grokCLITokenEnv, grokIdentity}, {kiloTokenEnv, kiloIdentity}, {kimiTokenEnv, kimiIdentity}} {
		t.Setenv(c.env, "override")
		if c.read(home) != "" {
			t.Fatalf("%s set: the file's account is not the token's", c.env)
		}
		t.Setenv(c.env, "")
	}
}

func TestJWTBackedIdentitiesReadSubOrNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv(grokHomeEnv, "")
	t.Setenv(kimiHomeEnv, "")
	jwt := jwtWith(t, map[string]any{"sub": "u-77"})

	writeIdent(t, filepath.Join(home, ".grok", "auth.json"),
		`{"https://accounts.x.ai/sign-in":{"key":"`+jwtWith(t, map[string]any{"sub": "legacy"})+`"},"https://auth.x.ai::openid":{"key":"`+jwt+`"}}`)
	if got := grokIdentity(home); got != "u-77" {
		t.Fatalf("grok prefers the OIDC scope: %q", got)
	}
	writeIdent(t, filepath.Join(home, ".grok", "auth.json"), `{"https://auth.x.ai::openid":{"key":"opaque"}}`)
	if grokIdentity(home) != "" {
		t.Fatal("an opaque grok token gives no identity — it is not hashed in place")
	}

	writeIdent(t, filepath.Join(home, ".local", "share", "kilo", "auth.json"), `{"kilo":{"access":"\"`+jwt+`\""}}`)
	if got := kiloIdentity(home); got != "u-77" {
		t.Fatalf("kilo (quoted access): %q", got)
	}
	writeIdent(t, filepath.Join(home, ".kimi-code", "credentials", "kimi-code.json"), `{"access_token":"`+jwt+`","expires_at":1}`)
	if got := kimiIdentity(home); got != "u-77" {
		t.Fatalf("kimi: %q", got)
	}
}

func TestIdentityReadersRefuseSymlinksAndOversize(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, "real.json")
	writeIdent(t, real, `{"tokens":{"account_id":"acct"}}`)
	link := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here")
	}
	if codexIdentity(home) != "" {
		t.Fatal("a symlinked identity file is refused")
	}
	if identFile(real, 4) != nil {
		t.Fatal("a file past the bound is refused, not truncated")
	}
}

func TestStampAccountKeysUsesTheAccountThenTheReader(t *testing.T) {
	withRoom(t, fakeToken(time.Now().Add(time.Hour)))
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"} {
		t.Setenv(k, "")
	}
	identReset()
	writeIdent(t, filepath.Join(home, ".codex", "auth.json"), `{"tokens":{"account_id":"acct_1"}}`)

	ps := []Provider{
		{ID: "codex", OK: true},
		{ID: "claude@2", Account: "someone@example.com", OK: true},
		{ID: "claude", OK: false, Error: "not-signed-in"},
		{ID: "amp", AccountKey: "deadbeefcafe"},
		{ID: customProviderPrefix + "bill", Account: "x"},
		{ID: "kiro", OK: true},
	}
	stampAccountKeys(ps)
	if ps[0].AccountKey != accountKey("codex", "acct_1") {
		t.Fatalf("codex from the reader: %q", ps[0].AccountKey)
	}
	if ps[1].AccountKey != accountKey("claude", "someone@example.com") {
		t.Fatalf("CPA from its account, under the vendor not the id: %q", ps[1].AccountKey)
	}
	if ps[2].AccountKey != "" {
		t.Fatal("no ~/.claude.json, no key")
	}
	if ps[3].AccountKey != "deadbeefcafe" {
		t.Fatal("a key the collector set stands")
	}
	if ps[4].AccountKey != "" || ps[5].AccountKey != "" {
		t.Fatal("the operator's own commands and vendors without a reader carry no key")
	}

	// The reader's answer is cached: a file that changes under it is read
	// again only after the TTL, or when the room changes.
	writeIdent(t, filepath.Join(home, ".codex", "auth.json"), `{"tokens":{"account_id":"acct_2"}}`)
	q := []Provider{{ID: "codex", OK: true}}
	stampAccountKeys(q)
	if q[0].AccountKey != accountKey("codex", "acct_1") {
		t.Fatal("cached within the TTL")
	}
	identReset()
	q = []Provider{{ID: "codex", OK: true}}
	stampAccountKeys(q)
	if q[0].AccountKey != accountKey("codex", "acct_2") {
		t.Fatal("re-read after a reset")
	}

	// And none of it without a room.
	setAccountRoom("")
	r := []Provider{{ID: "codex", OK: true}, {ID: "claude@2", Account: "someone@example.com"}}
	stampAccountKeys(r)
	if r[0].AccountKey != "" || r[1].AccountKey != "" {
		t.Fatal("no token, no keys")
	}
}

func TestAmpAccountKeyAndNameFromTheAddress(t *testing.T) {
	setAccountRoom("")
	if p := parseAmpFixture(t, ampReal); p.AccountKey != "" || p.Account != "someone@example.com" {
		t.Fatalf("no room: no key, the name all the same (%q, %q)", p.AccountKey, p.Account)
	}
	withRoom(t, fakeToken(time.Now().Add(time.Hour)))
	p := parseAmpFixture(t, ampReal)
	if p.AccountKey != accountKey("amp", "someone@example.com") {
		t.Fatalf("key: %q", p.AccountKey)
	}
	q := parseAmpFixture(t, ampTierReal)
	if q.AccountKey != p.AccountKey || q.Account != p.Account {
		t.Fatal("the same address with an org suffix is the same account")
	}
	blob := string(dump(p))
	if !strings.Contains(blob, `"account":"someone@example.com"`) || !strings.Contains(blob, `"account_key":"`+p.AccountKey+`"`) {
		t.Fatalf("the name and the key travel: %s", blob)
	}
	if ampAccountKey("Signed in as nobody-here\n") != "" || ampAddress("Signed in as nobody-here\n") != "" {
		t.Fatal("a sign-in line without an address keys and names nothing")
	}
}

// A reading that comes without an account is named by its tool's sign-in,
// with or without a room; the key
// still comes from the identity — for Codex the ChatGPT account, never the
// e-mail the name is.
func TestStampAccountNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(copilotTokenEnv, "")
	for _, k := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"} {
		t.Setenv(k, "")
	}
	setAccountRoom("")
	identReset()
	idTok := jwtWith(t, map[string]any{"email": "someone@example.com"})
	writeIdent(t, filepath.Join(home, ".codex", "auth.json"), `{"tokens":{"id_token":"`+idTok+`","account_id":"acct_1"}}`)
	writeIdent(t, filepath.Join(home, ".claude.json"), `{"projects":{"/x":{"history":[1,2]}},"oauthAccount":{"emailAddress":"Other@Example.com","accountUuid":"uuid-1"}}`)
	ps := []Provider{
		{ID: "codex", OK: true},
		{ID: "claude", OK: true},
		{ID: "claude@2", Account: "cpa@example.com", OK: true},
		{ID: "kiro", OK: true},
		{ID: customProviderPrefix + "bill"},
	}
	stampAccountKeys(ps)
	if ps[0].Account != "someone@example.com" || ps[1].Account != "Other@Example.com" {
		t.Fatalf("named by the sign-in, without a room too: %q, %q", ps[0].Account, ps[1].Account)
	}
	if ps[2].Account != "cpa@example.com" {
		t.Fatal("a credential's own name stands")
	}
	if ps[3].Account != "" || ps[4].Account != "" || ps[0].AccountKey != "" {
		t.Fatal("nothing to read, no name; no room, no key")
	}

	withRoom(t, fakeToken(time.Now().Add(time.Hour)))
	q := []Provider{{ID: "codex", OK: true}, {ID: "claude", OK: true}}
	stampAccountKeys(q)
	if q[0].AccountKey != accountKey("codex", "acct_1") || q[0].Account != "someone@example.com" {
		t.Fatalf("codex: keyed by the account, named by the e-mail: %+v", q[0])
	}
	if q[1].AccountKey != accountKey("claude", "other@example.com") {
		t.Fatalf("claude: keyed by its e-mail as before: %+v", q[1])
	}

	// An id that is only an id is no name.
	identReset()
	writeIdent(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"accountUuid":"uuid-1"}}`)
	r := []Provider{{ID: "claude", OK: true}}
	stampAccountKeys(r)
	if r[0].Account != "" || r[0].AccountKey != accountKey("claude", "uuid-1") {
		t.Fatalf("a uuid keys, it does not name: %+v", r[0])
	}
}

// A machine signed in twice — the default directories as one account, a
// CODEX_HOME or CLAUDE_CONFIG_DIR as another — names and keys each reading
// by the sign-in it was taken with, and by nothing where that cannot be
// told.
func TestStampAccountFollowsTheReadingsOwnSignIn(t *testing.T) {
	withRoom(t, fakeToken(time.Now().Add(time.Hour)))
	home := t.TempDir()
	t.Setenv("HOME", home)
	identReset()
	tok := func(email string) string { return jwtWith(t, map[string]any{"email": email}) }
	writeIdent(t, filepath.Join(home, ".codex", "auth.json"), `{"tokens":{"id_token":"`+tok("a@example.com")+`","account_id":"acct_a"}}`)
	other := t.TempDir()
	writeIdent(t, filepath.Join(other, "auth.json"), `{"tokens":{"id_token":"`+tok("b@example.com")+`","account_id":"acct_b"}}`)
	t.Setenv("CODEX_HOME", other)
	ps := []Provider{{ID: "codex", Source: "cli", OK: true}, {ID: "codex", Source: "local-log", OK: true}}
	stampAccountKeys(ps)
	if ps[0].Account != "b@example.com" || ps[0].AccountKey != accountKey("codex", "acct_b") {
		t.Fatalf("the live reading is CODEX_HOME's: %+v", ps[0])
	}
	if ps[1].Account != "a@example.com" || ps[1].AccountKey != accountKey("codex", "acct_a") {
		t.Fatalf("the session logs are ~/.codex's: %+v", ps[1])
	}

	writeIdent(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"emailAddress":"a@example.com"}}`)
	cfg := t.TempDir()
	writeIdent(t, filepath.Join(cfg, ".claude.json"), `{"oauthAccount":{"emailAddress":"b@example.com"}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	claude := func() Provider {
		identReset()
		q := []Provider{{ID: "claude", Source: "api", OK: true}}
		stampAccountKeys(q)
		return q[0]
	}
	if p := claude(); p.Account != "a@example.com" {
		t.Fatalf("no credential in the config directory: the keychain's or ~/.claude's, the home's file: %+v", p)
	}
	writeIdent(t, filepath.Join(cfg, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"x"}}`)
	if p := claude(); p.Account != "b@example.com" || p.AccountKey != accountKey("claude", "b@example.com") {
		t.Fatalf("the config directory's credential, its .claude.json: %+v", p)
	}
	sec := t.TempDir()
	writeIdent(t, filepath.Join(sec, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"y"}}`)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", sec)
	if p := claude(); p.Account != "" || p.AccountKey != "" {
		t.Fatalf("a secure-storage credential with no .claude.json of its own: nothing: %+v", p)
	}
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", cfg)
	if p := claude(); p.Account != "b@example.com" {
		t.Fatalf("secure storage in the config directory is that directory's: %+v", p)
	}
}

// The same ChatGPT account through CPA and from ~/.codex/auth.json is one
// key: the credential's Chatgpt-Account-Id, as its own request fills it —
// from an id_token object or from the JWT — and not the e-mail CPA names
// the credential after.
func TestCPACodexKeyedLikeItsFile(t *testing.T) {
	withRoom(t, fakeToken(time.Now().Add(time.Hour)))
	ad := currentAdapters().Vendors["codex"]
	if ad == nil {
		t.Fatal("the table has a codex adapter")
	}
	home := t.TempDir()
	writeIdent(t, filepath.Join(home, ".codex", "auth.json"), `{"tokens":{"account_id":"acct_9"}}`)
	want := accountKey("codex", codexIdentity(home))
	jwt := jwtWith(t, map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_9"}})
	for _, raw := range []map[string]any{
		{"email": "someone@example.com", "id_token": map[string]any{"chatgpt_account_id": "acct_9"}},
		{"email": "someone@example.com", "id_token": jwt},
	} {
		if id := cpaAccountID(ad, raw); accountKey("codex", id) != want {
			t.Fatalf("%v: %q", raw, id)
		}
	}
	if cpaAccountID(ad, map[string]any{"email": "someone@example.com"}) != "" {
		t.Fatal("a credential that names no account gives none")
	}
	if cpaAccountID(currentAdapters().Vendors["claude"], map[string]any{"id_token": map[string]any{"chatgpt_account_id": "x"}}) != "" {
		t.Fatal("only Codex is keyed this way")
	}
}
