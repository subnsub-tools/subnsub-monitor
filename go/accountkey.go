package main

// Which ACCOUNT a quota reading belongs to — a key the page folds readings
// by, and the name it shows.
//
// The dashboard draws one row per account, not one per machine: three boxes
// signed into the same Codex plan share one quota, and three gauges of it
// are two too many. To fold them the page needs to
// know which readings are the same account, and nothing on the wire said so.
// What decides it is an HMAC of the identity under the fleet's own secret,
// 12 hex characters:
//
//	account_key = HMAC-SHA256(room, vendor + "\n" + identity)[:6 bytes]
//
// The room is the relay token's account part (renew.go tokenIdentity) — the
// 16 bytes the relay names a dashboard after, the same on every machine the
// token is pasted on and across the token's own renewals. So the key agrees
// across one fleet, and nobody without that fleet's token can run an address
// list against it: the hash of "someone@example.com" is a different string
// in every room. The relay already holds the token, and already sees every
// reading, so the key tells it nothing it could not know. A legacy token
// (the self-minted kind, no account part) is used whole; it never rotates.
//
// Without a token — `--local`, the direct tunnel on its own — there is no
// room and no key, and the page falls back to matching windows' reset times.
//
// AND THE NAME. A reading that comes
// without an Account of its own carries the name its tool is signed in
// with (identSourceFor) — the e-mail, or Copilot's login — read from
// the same files as the identity. The key still decides which readings are
// one account, from the identity below: for Codex that is the ChatGPT
// account, of which one e-mail can hold two, so a name is never a key.
//
// Where the identity comes from, per vendor, and what it is:
//
//	codex    auth.json tokens.account_id (the ChatGPT account; one sign-in
//	         can hold a personal and a team account, with separate quotas,
//	         so the account and not the e-mail); else the id_token's
//	         chatgpt_account_id claim; else its email claim — ~/.codex's,
//	         or CODEX_HOME's for the live reading (identSourceFor)
//	claude   .claude.json oauthAccount.emailAddress (the e-mail, so a CPA
//	         credential named for the same address folds with it); else
//	         oauthAccount.accountUuid — the file beside the credential the
//	         reading used (claudeConfigFile)
//	copilot  ~/.config/github-copilot/{apps,hosts}.json "user" (the login)
//	amp      the "Signed in as …" line of `amp usage` (amp.go keys it, and
//	         names the reading by it)
//	grok, kilo, kimi
//	         the OAuth token's `sub` claim when the token is a JWT; a token
//	         that is not one gives no key — an opaque token is not hashed in
//	         its place, because it rotates and a key that changes under a
//	         sign-in splits one account into two rows
//
// A reader names the account of the credential its collector WOULD read
// from disk. Where the collector takes its token from the environment
// instead (MON_COPILOT_TOKEN, MON_GROK_TOKEN / GROK_OAUTH_TOKEN,
// MON_KILO_TOKEN, MON_KIMI_TOKEN), the file on disk may be another account's
// and the reader says nothing: a key that names the wrong account is worse
// than none, because the page would fold two accounts into one row.
//	CPA, the local adapter rung
//	         the reading's own Account (cpa.go, local.go): the credential's
//	         e-mail or file name — except a Codex credential through CPA,
//	         keyed like ~/.codex/auth.json by the ChatGPT account its own
//	         request names (cpaAccountID), so the two fold
//	antigravity, kiro, the operator's own commands
//	         nothing this build can read; no key
//
// Every reader is bounded, opens nothing but a regular file, and decodes
// only the one subtree it names — ~/.claude.json in particular holds every
// project's history and is walked, not loaded. What a reader returns is
// kept in memory for identTTL and never logged or written.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	accountKeyHex = 12 // 48 bits: a fleet holds a handful of accounts
	// The most an identity file is read: auth.json files are a few KB;
	// ~/.claude.json is walked and this bounds the walk.
	identFileMax = 64 << 20
	identTTL     = 10 * time.Minute
	identMaxLen  = 200
)

var accountRoom struct {
	sync.RWMutex
	secret []byte
}

// setAccountRoom seeds the key from the token the push loop starts with.
// Called once per process: a renewal keeps the account part, so the seed
// does not move with it; and a process that never connects never seeds.
func setAccountRoom(token string) {
	tok := strings.TrimSpace(token)
	var secret []byte
	if tok != "" {
		if id, ok := tokenIdentity(tok); ok {
			secret = []byte("room:" + id)
		} else {
			secret = []byte("token:" + tok)
		}
	}
	accountRoom.Lock()
	accountRoom.secret = secret
	accountRoom.Unlock()
	identReset()
}

func accountRoomSecret() []byte {
	accountRoom.RLock()
	defer accountRoom.RUnlock()
	return accountRoom.secret
}

// accountKey is the wire form of one account at one vendor, or "" when
// either side is missing. Identity is trimmed and lower-cased first:
// e-mails compare case-insensitively, and the same address typed two ways
// must not become two accounts.
func accountKey(vendor, identity string) string {
	secret := accountRoomSecret()
	id := strings.ToLower(strings.TrimSpace(identity))
	vendor = strings.TrimSpace(vendor)
	if len(secret) == 0 || id == "" || vendor == "" || len(id) > identMaxLen {
		return ""
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(vendor))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil))[:accountKeyHex]
}

// The vendor a provider id names: `claude@2` is Claude's second account on
// this machine (cpa.go). The operator's own commands have no vendor here.
func vendorOfID(id string) string {
	if strings.HasPrefix(id, customProviderPrefix) {
		return ""
	}
	if i := strings.IndexByte(id, '@'); i >= 0 {
		return id[:i]
	}
	return id
}

// stampAccountKeys fills account_key on every provider that has none yet —
// from the reading's own Account where it names one, else from the account
// file its credential came with (identSourceFor), and only with a room to
// key it in — and then the name on every provider that came without one,
// room or not. In that order: the key of a native reading is its
// identity's, never the name just put on it. Run once per snapshot, after
// every collector has spoken.
func stampAccountKeys(ps []Provider) {
	keyed := len(accountRoomSecret()) > 0
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	for i := range ps {
		p := &ps[i]
		vendor := vendorOfID(p.ID)
		if vendor == "" {
			continue
		}
		src, ok := identSourceFor(vendor, p, home)
		if keyed && p.AccountKey == "" {
			if p.Account != "" {
				p.AccountKey = accountKey(vendor, p.Account)
			} else if ok {
				p.AccountKey = accountKey(vendor, identityOf(src.at+"\nid", src.id))
			}
		}
		if p.Account == "" && ok {
			p.Account = cleanText(identityOf(src.at+"\nname", src.name), 60)
		}
	}
}

// ── the readers ────────────────────────────────────────────────────────────

// Where one reading's account is read from: the identity its key is made
// of, the name its tile shows — the e-mail the tool signed in with, or
// Copilot's login; an id that is only an id (Claude's accountUuid, a
// token's bare `sub`) is no name — and the file they are cached under.
type identSource struct {
	at       string
	id, name func() string
}

// identSourceFor finds the account file of the credential a reading was
// taken with. It has to be that one: a name read off
// another sign-in would put one account's quota under another's address.
// Where that cannot be told there is no source — no key, no name.
//
//	codex    the live app-server (source "cli") is the child's, which reads
//	         the CODEX_HOME the helper hands it (codexEnv); the session logs
//	         (codex.go) are ~/.codex's, and so is their account
//	claude   the .claude.json that goes with the credential the local rung
//	         takes (claudeConfigFile)
//	copilot, grok, kilo, kimi
//	         the one file their collector reads, and nothing when a token
//	         handed in by name stands in for it
func identSourceFor(vendor string, p *Provider, home string) (identSource, bool) {
	if home == "" {
		return identSource{}, false
	}
	switch vendor {
	case "codex":
		dir := filepath.Join(home, ".codex")
		if p.Source == "cli" {
			if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); filepath.IsAbs(v) {
				dir = v
			}
		}
		return identSource{at: "codex\n" + dir,
			id:   func() string { return codexIdentityAt(dir) },
			name: func() string { return jwtClaim(codexIDToken(dir), "email") }}, true
	case "claude":
		f := claudeConfigFile(home)
		if f == "" {
			return identSource{}, false
		}
		return identSource{at: "claude\n" + f,
			id: func() string { return claudeIdentityAt(f) },
			name: func() string {
				if id := claudeIdentityAt(f); strings.Contains(id, "@") {
					return id
				}
				return ""
			}}, true
	case "copilot":
		read := func() string { return copilotIdentity(home) }
		return identSource{at: "copilot", id: read, name: read}, true
	case "grok", "kilo", "kimi":
		tok := map[string]func(string) string{"grok": grokIdentToken, "kilo": kiloIdentToken, "kimi": kimiIdentToken}[vendor]
		return identSource{at: vendor,
			id:   func() string { return jwtClaim(tok(home), "sub") },
			name: func() string { return jwtClaim(tok(home), "email") }}, true
	}
	return identSource{}, false
}

// The .claude.json that goes with the credential a Claude reading is taken
// with. The local rung takes the first of $CLAUDE_SECURESTORAGE_CONFIG_DIR/
// .credentials.json, $CLAUDE_CONFIG_DIR/.credentials.json and
// ~/.claude/.credentials.json there is, then the keychain's default item
// (adapters.json; local.go localCredential), and Claude Code keeps its
// .claude.json in $CLAUDE_CONFIG_DIR when one is set, in the home when not.
// A secure-storage credential outside the config directory has no
// .claude.json known to be its own: "".
func claudeConfigFile(home string) string {
	present := func(spec string) bool {
		path := localExpand(spec)
		if path == "" {
			return false
		}
		st, err := os.Stat(path)
		return err == nil && st.Mode().IsRegular()
	}
	cfg := localExpand("$CLAUDE_CONFIG_DIR")
	if present("$CLAUDE_SECURESTORAGE_CONFIG_DIR/.credentials.json") {
		if cfg == "" || filepath.Clean(localExpand("$CLAUDE_SECURESTORAGE_CONFIG_DIR")) != filepath.Clean(cfg) {
			return ""
		}
		return filepath.Join(cfg, ".claude.json")
	}
	if present("$CLAUDE_CONFIG_DIR/.credentials.json") {
		return filepath.Join(cfg, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

// A reader's answer is kept for a while: the files do not change between
// one push and the next, and ~/.claude.json is not a file to walk every
// thirty seconds. Entries live in memory only, and are dropped when the
// room changes.
type identEntry struct {
	at  time.Time
	val string
}

var identCache struct {
	sync.Mutex
	m map[string]identEntry
}

func identReset() {
	identCache.Lock()
	identCache.m = nil
	identCache.Unlock()
}

func identityOf(key string, read func() string) string {
	identCache.Lock()
	defer identCache.Unlock()
	if e, ok := identCache.m[key]; ok && time.Since(e.at) < identTTL {
		return e.val
	}
	val := read()
	if identCache.m == nil {
		identCache.m = map[string]identEntry{}
	}
	identCache.m[key] = identEntry{at: time.Now(), val: val}
	return val
}

// identFile reads one regular file, bounded. A symlink or anything that is
// not a plain file is refused for the reason renew.go gives: this path is
// read as a credential's neighbour, and the tools only ever write regular
// files here.
func identFile(path string, max int64) []byte {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > max {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return nil
	}
	return b
}

// jwtClaim is one string claim out of a JWT's payload, unverified — the
// claim is hashed or shown, never trusted, and a forged one only ever
// mis-files or mislabels the forger's own reading. "" when the token is not three base64url parts with
// a JSON object in the middle, or the claim is not a string.
func jwtClaim(tok, claim string) string {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 || len(parts[1]) > 64<<10 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	// A dotted claim walks nested objects: "https://api.openai.com/auth" is
	// itself a key, so the walk splits on "." only where the caller asked.
	v := any(doc)
	for _, k := range strings.Split(claim, "|") {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

type codexTokens struct {
	IDToken   string `json:"id_token"`
	AccountID string `json:"account_id"`
}

// The tokens block of <dir>/auth.json, or nil.
func codexAuth(dir string) *codexTokens {
	raw := identFile(filepath.Join(dir, "auth.json"), 1<<20)
	if raw == nil {
		return nil
	}
	var doc struct {
		Tokens *codexTokens `json:"tokens"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return doc.Tokens
}

func codexIdentity(home string) string { return codexIdentityAt(filepath.Join(home, ".codex")) }

func codexIdentityAt(dir string) string {
	t := codexAuth(dir)
	if t == nil {
		return ""
	}
	if v := strings.TrimSpace(t.AccountID); v != "" {
		return v
	}
	if v := jwtClaim(t.IDToken, "https://api.openai.com/auth|chatgpt_account_id"); v != "" {
		return v
	}
	return jwtClaim(t.IDToken, "email")
}

// The id_token Codex signed in with, whose e-mail claim names the account.
func codexIDToken(dir string) string {
	if t := codexAuth(dir); t != nil {
		return t.IDToken
	}
	return ""
}

func claudeIdentity(home string) string { return claudeIdentityAt(filepath.Join(home, ".claude.json")) }

// claudeIdentityAt walks a .claude.json for its top-level oauthAccount and
// decodes nothing else: the file also carries every project's prompt
// history, which has no business becoming a value in this program.
func claudeIdentityAt(path string) string {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > identFileMax {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, identFileMax))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return ""
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return ""
		}
		key, _ := t.(string)
		if key != "oauthAccount" {
			if !skipJSONValue(dec) {
				return ""
			}
			continue
		}
		var acct struct {
			Email string `json:"emailAddress"`
			UUID  string `json:"accountUuid"`
		}
		if dec.Decode(&acct) != nil {
			return ""
		}
		if v := strings.TrimSpace(acct.Email); v != "" {
			return v
		}
		return strings.TrimSpace(acct.UUID)
	}
	return ""
}

// skipJSONValue steps over one value token by token, so a project
// history of any size passes through the decoder's buffer and is never
// held whole — the walk is bounded by the file, not by memory.
func skipJSONValue(dec *json.Decoder) bool {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return false
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return true
		}
	}
}

// copilotIdentity is the login beside the token copilot.go reads — the
// same files, the same github.com entries, newer file first. copilotToken
// takes the first github.com entry it meets in the first file that has
// one; map order is not a thing either side can rely on, so this names a
// login only when every github.com entry of that file agrees on it, and
// stops at that file as the collector does — an entry with a token and no
// login, or two logins, is an account this cannot name.
func copilotIdentity(home string) string {
	if strings.TrimSpace(os.Getenv(copilotTokenEnv)) != "" {
		return ""
	}
	for _, name := range []string{"apps.json", "hosts.json"} {
		raw := identFile(filepath.Join(home, ".config", "github-copilot", name), 1<<20)
		if raw == nil {
			continue
		}
		var doc map[string]struct {
			OauthToken string `json:"oauth_token"`
			User       string `json:"user"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		login, seen := "", false
		for key, entry := range doc {
			if entry.OauthToken == "" || !(key == "github.com" || strings.HasPrefix(key, "github.com:")) {
				continue
			}
			u := strings.TrimSpace(entry.User)
			if !seen {
				login, seen = u, true
			} else if u != login {
				login = ""
			}
		}
		if seen {
			return login
		}
	}
	return ""
}

// grokIdentity is the `sub` of the bearer grok.go would use, read from the
// same file by the same preference (the OIDC scope over the sign-in one).
// Environment tokens are not consulted: a token handed in by name is a
// credential, not an identity file.
func grokIdentity(home string) string { return jwtClaim(grokIdentToken(home), "sub") }

// The bearer grokIdentity reads, or "" — the name comes from it too.
func grokIdentToken(home string) string {
	for _, env := range []string{grokTokenEnv, grokCLITokenEnv} {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return ""
		}
	}
	dir := strings.TrimSpace(os.Getenv(grokHomeEnv))
	if dir == "" {
		dir = filepath.Join(home, ".grok")
	}
	raw := identFile(filepath.Join(dir, "auth.json"), 1<<20)
	if raw == nil {
		return ""
	}
	var doc map[string]struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	var oidc, legacy string
	for scope, entry := range doc {
		if entry.Key == "" {
			continue
		}
		if strings.HasPrefix(scope, "https://auth.x.ai::") {
			oidc = entry.Key
		} else if strings.Contains(scope, "/sign-in") {
			legacy = entry.Key
		}
	}
	if oidc != "" {
		return oidc
	}
	return legacy
}

func kiloIdentity(home string) string { return jwtClaim(kiloIdentToken(home), "sub") }

func kiloIdentToken(home string) string {
	if strings.TrimSpace(os.Getenv(kiloTokenEnv)) != "" {
		return ""
	}
	raw := identFile(filepath.Join(home, ".local", "share", "kilo", "auth.json"), 1<<20)
	if raw == nil {
		return ""
	}
	var doc struct {
		Kilo *struct {
			Access string `json:"access"`
		} `json:"kilo"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Kilo == nil {
		return ""
	}
	return strings.Trim(strings.TrimSpace(doc.Kilo.Access), `"'`)
}

func kimiIdentity(home string) string { return jwtClaim(kimiIdentToken(home), "sub") }

func kimiIdentToken(home string) string {
	if strings.TrimSpace(os.Getenv(kimiTokenEnv)) != "" {
		return ""
	}
	dir := strings.TrimSpace(os.Getenv(kimiHomeEnv))
	if dir == "" {
		dir = filepath.Join(home, ".kimi-code")
	}
	raw := identFile(filepath.Join(dir, "credentials", "kimi-code.json"), 1<<20)
	if raw == nil {
		return ""
	}
	var doc struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return doc.AccessToken
}
