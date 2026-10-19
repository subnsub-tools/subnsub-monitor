package main

// The local rung: the table's requests, with the token read from the file
// the vendor's own tool left on this machine.
//
// A machine with no CLI Proxy API still has credentials on it — the ones
// each tool wrote when the user signed in (~/.claude/.credentials.json,
// ~/.grok/auth.json, …), which the native collectors have read since the
// beginning. This reads the same files and then does exactly what the CPA
// rung does with a token the proxy filled in: the same adapters.json, the
// same requests, the same windows. What it buys is that the vendor-specific
// knowledge for these providers is now in the table on every machine, and a
// signed newer table fixes a moved field everywhere without a release; what
// it costs is nothing the native collectors did not already cost — the
// helper holds the vendor's own token, per vendor, exactly as before (operator
// ruling 2026-09-09: keep both roads — through CPA where there is one,
// straight from the local credential file where there is not).
//
// Two rules carried over from the native collectors, because they were
// learned there: the credential file is never written (an expired token is
// reported, not refreshed — the tool refreshes it the next time it runs), and
// no error line ever carries the path as it was resolved on this machine
// (the literal spelling from the table, with its `~`, is what travels).
//
// Precedence, per vendor: CPA when the proxy holds a credential for it, else
// this rung when the table knows the file, else the native collector — and
// the native collector also catches a table that read the file but found no
// window in the answer, so a wrong table row costs nothing the old code was
// already getting right (collectLocalOr).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const localCredMax = 256 * 1024

// How a macOS keychain item is read (keychain_darwin.go; nothing anywhere
// else). A variable so the tests can stand in for the keychain.
var readKeychain = keychainPassword

// What the keychain tool is allowed to see: HOME, which is how it finds the
// login keychain, and the three names beside it — nothing of this helper's
// own. NEVER nil: an exec.Cmd with a nil Env inherits this whole process's
// environment, relay token included, and a session with none of these four
// set would have handed exactly that over (review 2026-10-14).
func keychainEnv() []string {
	env := []string{}
	for _, k := range []string{"HOME", "USER", "LOGNAME", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

var localCache struct {
	sync.Mutex
	per map[string]*provCache
}

func localCacheFor(id string) *provCache {
	localCache.Lock()
	defer localCache.Unlock()
	if localCache.per == nil {
		localCache.per = map[string]*provCache{}
	}
	c := localCache.per[id]
	if c == nil {
		c = &provCache{pace: paceMark{id: id}}
		localCache.per[id] = c
	}
	return c
}

// The adapter for a provider id, when the table has one with a credential
// spec — the local rung's precondition.
func localAdapterFor(id string) *vendorAdapter {
	for _, ad := range currentAdapters().Vendors {
		if ad.Provider == id && ad.Credential != nil && ad.Credential.Token != "" && len(ad.Credential.Files) > 0 {
			return ad
		}
	}
	return nil
}

// `~` is the home directory; `$NAME` is that environment variable. Nothing
// else is interpreted; a variable that is not set, or a path that is not
// absolute once expanded, is a path this does not open.
func localExpand(spec string) string {
	if strings.HasPrefix(spec, "~") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		spec = home + spec[1:]
	}
	if strings.Contains(spec, "$") {
		missing := false
		spec = os.Expand(spec, func(name string) string {
			v := strings.TrimSpace(os.Getenv(name))
			if v == "" {
				missing = true
			}
			return v
		})
		if missing || strings.Contains(spec, "$") {
			return ""
		}
	}
	if !filepath.IsAbs(spec) {
		return ""
	}
	return spec
}

// The first credential file the spec names that exists, parsed. The spelling
// returned is the table's own, for error lines.
func localCredential(spec *adapterCredential) (map[string]any, string, bool) {
	for _, f := range spec.Files {
		path := localExpand(f)
		if path == "" {
			continue
		}
		raw, err := readRegular(path, localCredMax)
		if err != nil {
			continue
		}
		var doc map[string]any
		if json.Unmarshal(raw, &doc) != nil || doc == nil {
			continue
		}
		return doc, f, true
	}
	// macOS keeps the same document in the login keychain where the other
	// platforms keep a file (Claude Code writes ~/.claude/.credentials.json
	// only where the keychain is unavailable): the table names the item's
	// service, and it is read when none of the files exists. The spelling
	// for an error line is the service's — the name the user would look for.
	if spec.Keychain != "" {
		if raw, ok := readKeychain(spec.Keychain); ok && len(raw) <= localCredMax {
			var doc map[string]any
			if json.Unmarshal(raw, &doc) == nil && doc != nil {
				return doc, spec.Keychain, true
			}
		}
	}
	return nil, "", false
}

// The spelling of the credential path for an error line: the first the
// table names that this machine could open — one whose variables are set —
// so a line does not name `$CLAUDE_CONFIG_DIR/…` to someone who never set it.
func localSpelled(files []string) string {
	for _, f := range files {
		if localExpand(f) != "" {
			return f
		}
	}
	if len(files) > 0 {
		return files[0]
	}
	return ""
}

// What the local rung needs before it can ask anything: the credential
// document, the token in it, and the account it names — or the failure
// that stands in for the reading.
func localPrepare(ad *vendorAdapter, id string) (cred map[string]any, token, account string, fail *Provider) {
	p := Provider{ID: id, Name: ad.Name, Source: "api", CapturedAt: now()}
	spec := ad.Credential
	failWith := func(err, code string, arg ...string) *Provider {
		p.failWith(err, code, arg...)
		return &p
	}
	cred, spelled, ok := localCredential(spec)
	if !ok {
		return nil, "", "", failWith("not-signed-in", "creds-missing", localSpelled(spec.Files))
	}
	credCtx := adapterCtx{cred}
	token = strings.TrimSpace(adapterStr(credCtx.get(spec.Token), 4096))
	if token == "" {
		return nil, "", "", failWith("not-signed-in", "creds-missing", spelled)
	}
	if spec.Expires != "" {
		if exp := adapterReset(credCtx.get(spec.Expires), spec.ExpiresFormat, 0); exp > 0 && exp < now() {
			signin := spec.Signin
			if signin == "" {
				signin = ad.Name
			}
			return nil, "", "", failWith("token-expired", "creds-expired", signin)
		}
	}
	if spec.Account != "" {
		account = cleanText(adapterStr(credCtx.get(spec.Account), 60), 60)
	}
	return cred, token, account, nil
}

func replaceToken(s, token string) string { return strings.ReplaceAll(s, "$TOKEN$", token) }

// A sender that puts the token where the table left `$TOKEN$`, and asks the
// vendor itself — provhttp's rules: no redirects, bounded body, no transport
// text in the error.
func localSender(p Provider, token string) adapterSender {
	return func(method, target string, header map[string]string, data string) (int, []byte, *Provider) {
		hdr := make(map[string]string, len(header)+1)
		for k, v := range header {
			hdr[k] = replaceToken(v, token)
		}
		if _, has := hdr["Content-Type"]; !has && data != "" {
			hdr["Content-Type"] = "application/json"
		}
		var body []byte
		if data != "" {
			body = []byte(replaceToken(data, token))
		}
		status, out, err := provRequest(method, target, hdr, body)
		if err != nil {
			q := p
			if err == errProvRedirect {
				q.failWith("api-error", "http-redirect")
			} else {
				q.failWith("unreachable", "req-failed")
			}
			return 0, nil, &q
		}
		return status, out, nil
	}
}

// The reading for one vendor through the local rung.
func localFetch(ad *vendorAdapter, id string) Provider {
	cred, token, account, fail := localPrepare(ad, id)
	if fail != nil {
		return *fail
	}
	p := Provider{ID: id, Name: ad.Name, Source: "api", CapturedAt: now(), Account: account}
	out := adapterFetch(p, ad, cred, "local", localSender(p, token))
	adapterKeep("local:"+id, &out, now())
	return out
}

// The local rung when the table knows the vendor's credential file, else the
// native collector; and the native collector again when the table read the
// file but the answer held no window for it — a table row that is wrong
// about a field must not cost a reading the old code still gets.
//
// The native road is taken only in a collection where the table's road was
// REALLY asked (a refresh press, the first ask of an empty cache, the floor
// while watched), and what it answered is kept and served until the next one. The
// two keep separate caches, and the table's failure surfaces here only once
// its last success has aged past the stale window — half an hour after the
// press that produced it. Calling the native collector then would find its
// cache empty, or holding a press it never saw, and ask the vendor with
// nobody pressing anything (review 2026-09-28).
// Two ways of handing over to the native road, named for the vendor shapes
// that need them.
type localMode int

const (
	// Hand over when the table READ the credential but could not read the
	// answer (no-window, http-shape), and only in a collection that really
	// asked: the native road reads the same credential and would say the
	// same about a missing or expired one, and its cache is its own.
	localOrNative localMode = iota
	// Hand over on ANY failure of the table's FETCH, every collection: for a
	// native road that needs no credential of ours (collectLocalElse).
	localElseNative
)

func collectLocalOr(id string, native func() Provider) Provider {
	return collectLocalWith(id, native, localOrNative)
}

// collectLocalElse is collectLocalOr for a vendor whose native road needs no
// credential of ours at all. Antigravity's is its CLI's own report
// (antigravity.go): agy refreshes its token itself, while the file this rung
// reads holds the token as of the CLI's last run and expires an hour later,
// and on macOS there is no file at all. So ANY failure of the table's road
// hands over — a missing or expired file, a 401, a window it could not read
// — and the native road is never worse off than before this rung existed,
// while a fresh file still saves launching the CLI (operator ruling 2026-10-14: Linux
// must read directly without CPA installed, too).
func collectLocalElse(id string, native func() Provider) Provider {
	return collectLocalWith(id, native, localElseNative)
}

// The table's road, then the native one by the mode's rule.
func collectLocalWith(id string, native func() Provider, mode localMode) Provider {
	ad := localAdapterFor(id)
	if ad == nil {
		return native()
	}
	var fetched *Provider
	p, asked := localCacheFor(id).collectAsked(func() Provider {
		r := localFetch(ad, id)
		fetched = &r
		return r
	})
	if mode == localElseNative && native != nil {
		// The judgement is the FETCH's, not the served reading's. The cache
		// serves a success up to half an hour old over a fresh failure, which
		// is right for a vendor with one road; here a failure is the signal
		// that the other road should answer, fresh, now — a token that
		// expired since the last success, a 401 (review 2026-10-14). And the
		// native road is asked every time the table's cannot stand, press or
		// no press: it paces itself (its own provCache), so a press the
		// table's 429 backoff consumed still reaches it, and a quiet
		// collection costs it a cache read and nothing else.
		failed := !p.OK || (asked && fetched != nil && !fetched.OK)
		if !failed {
			return p
		}
		n := native()
		if n.OK {
			// What the table did read stays: the plan chip, which the CLI's
			// report has no call for. The gauge is the native road's own.
			if n.PlanType == nil && p.PlanType != nil {
				n.PlanType = p.PlanType
			}
			if n.RateLimitTier == nil && p.RateLimitTier != nil {
				n.RateLimitTier = p.RateLimitTier
			}
			return n
		}
		if p.OK {
			// The table's stale-but-real reading beats the native road's
			// refusal, as it would with one road.
			return p
		}
		return n
	}
	if p.OK {
		localFallbackSet(id, nil)
		return p
	}
	switch p.DetailCode {
	case "no-window", "http-shape":
		if native == nil {
			break
		}
		if asked {
			n := native()
			// What the table did read stays: the plan chip, which the
			// native road may have no call for (Grok's settings call is the
			// table's). The gauge is the native road's own — a percentage
			// is never carried from one answer into another (CodexBar
			// GrokWebBillingSnapshot.completing).
			if n.OK {
				if n.PlanType == nil && p.PlanType != nil {
					n.PlanType = p.PlanType
				}
				if n.RateLimitTier == nil && p.RateLimitTier != nil {
					n.RateLimitTier = p.RateLimitTier
				}
			}
			localFallbackSet(id, &n)
			return n
		}
		if held := localFallbackGet(id); held != nil {
			out := *held
			out.CapturedAt = now() // when we looked; RecordedAt still says when fetched
			return out
		}
	}
	return p
}

// What the native road last answered for a vendor whose table road failed,
// kept so the collections between two real asks have it to serve.
var localFallback struct {
	sync.Mutex
	per map[string]*Provider
}

func localFallbackSet(id string, p *Provider) {
	localFallback.Lock()
	defer localFallback.Unlock()
	if p == nil {
		delete(localFallback.per, id)
		return
	}
	if localFallback.per == nil {
		localFallback.per = map[string]*Provider{}
	}
	localFallback.per[id] = p
}

func localFallbackGet(id string) *Provider {
	localFallback.Lock()
	defer localFallback.Unlock()
	return localFallback.per[id]
}
