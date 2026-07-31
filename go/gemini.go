package main

// Gemini CLI: quota costs a credential — and, uniquely here, a refresh.
//
// gemini-cli signs in with Google OAuth and stores the result at
// ~/.gemini/oauth_creds.json. Its quota lives behind the Cloud Code private
// API the CLI itself uses:
//
//	POST https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist
//	POST https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota
//	  → {"buckets":[{"remainingFraction":0.82,"resetTime":…,"modelId":…}]}
//
// The access token expires hourly, so unlike Claude — whose token the CLI
// keeps fresh as a side effect of being used — a reading between gemini runs
// usually needs a refresh. The refresh is IN MEMORY ONLY: the new token is
// held for its hour and oauth_creds.json is never written back. Rewriting
// somebody's credential file to keep a status panel green is the line
// claude.go drew, and it holds here; the refresh token itself never travels
// anywhere but Google's own token endpoint.
//
// The refresh needs the CLI's OAuth client id/secret. Those are not secrets
// in any meaningful sense — they ship inside every copy of the open-source
// CLI — but they are Google's identifiers for gemini-cli, not for this
// program, so they are read OUT OF THE INSTALLED CLI (dist/src/code_assist/
// oauth2.js) rather than baked in here: if they rotate, the installed CLI is
// the source of truth, and a box with no CLI has no business refreshing.
// GEMINI_OAUTH_CLIENT_ID/SECRET override for unusual installs — the same env
// names the CodexBar implementation this protocol was worked out from honours.
//
// What travels: per-model percentages, a plan id, reset times. Same honest
// claim as Claude's: "no credential leaves the machine", not "no requests".

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	geminiLoadURL  = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	geminiQuotaURL = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
	geminiTokenURL = "https://oauth2.googleapis.com/token"
)

var geminiCache provCache

// The refreshed access token, held for its lifetime so the 2-minute provider
// floor does not turn into thirty refresh calls an hour. `bad` records the
// last token an API answered 401 to: local expiry is a prediction, not the
// vendor's opinion, and a token Google revoked early would otherwise be
// re-selected — and re-refused — for the rest of its printed lifetime.
var geminiTok struct {
	sync.Mutex
	token string
	until float64
	bad   string
}

// An API said this token is no good, whatever its expiry claims. Invalidate
// so the next collection refreshes instead of replaying the refusal.
func geminiInvalidate(token string) {
	geminiTok.Lock()
	defer geminiTok.Unlock()
	if geminiTok.token == token {
		geminiTok.token, geminiTok.until = "", 0
	}
	geminiTok.bad = token
}

func collectGemini() Provider {
	return geminiCache.collect(fetchGemini)
}

func geminiDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini")
}

// Which way this install signs in. Only oauth-personal carries a Code Assist
// quota; an API-key or Vertex install meters elsewhere, and saying so beats a
// permanent unexplained failure.
func geminiAuthType() string {
	raw, err := os.ReadFile(filepath.Join(geminiDir(), "settings.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		SelectedAuthType string `json:"selectedAuthType"`
		Security         *struct {
			Auth *struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	if doc.Security != nil && doc.Security.Auth != nil && doc.Security.Auth.SelectedType != "" {
		return doc.Security.Auth.SelectedType
	}
	return doc.SelectedAuthType
}

type geminiCreds struct {
	access  string
	refresh string
	// Unix MILLISECONDS in the file, like Claude's and unlike Codex's.
	expiryMs float64
}

func readGeminiCreds() (geminiCreds, bool) {
	raw, err := os.ReadFile(filepath.Join(geminiDir(), "oauth_creds.json"))
	if err != nil {
		return geminiCreds{}, false
	}
	var doc struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiryDate   any    `json:"expiry_date"`
	}
	if json.Unmarshal(raw, &doc) != nil || (doc.AccessToken == "" && doc.RefreshToken == "") {
		return geminiCreds{}, false
	}
	c := geminiCreds{access: doc.AccessToken, refresh: doc.RefreshToken}
	if v := asNum(doc.ExpiryDate); v != nil {
		c.expiryMs = *v
	}
	return c, true
}

// The CLI's OAuth client, out of the CLI itself. Fixed candidate roots plus
// two bounded globs (nvm keeps one lib tree per node version, homebrew one
// per formula) — PATH is never consulted here for the same reason amp.go
// never consults it, and nothing found this way is ever executed.
var geminiOAuth2Candidates = func() []string {
	const tail = "dist/src/code_assist/oauth2.js"
	pkgs := []string{
		"@google/gemini-cli-core/" + tail,
		"@google/gemini-cli/node_modules/@google/gemini-cli-core/" + tail,
	}
	roots := []string{
		"/usr/lib/node_modules", "/usr/local/lib/node_modules",
		"/opt/homebrew/lib/node_modules",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots,
			filepath.Join(home, ".npm-global", "lib", "node_modules"),
			filepath.Join(home, ".local", "lib", "node_modules"),
			filepath.Join(home, ".local", "share", "gemini-cli", "node_modules"))
		if vs, err := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "lib", "node_modules")); err == nil {
			roots = append(roots, vs...)
		}
	}
	if vs, err := filepath.Glob("/opt/homebrew/Cellar/gemini-cli/*/libexec/lib/node_modules"); err == nil {
		roots = append(roots, vs...)
	}
	var out []string
	for _, r := range roots {
		for _, p := range pkgs {
			out = append(out, filepath.Join(r, p))
		}
	}
	return out
}()

var (
	geminiClientIDRe     = regexp.MustCompile(`OAUTH_CLIENT_ID\s*=\s*['"]([\w\-.]+)['"]`)
	geminiClientSecretRe = regexp.MustCompile(`OAUTH_CLIENT_SECRET\s*=\s*['"]([\w\-]+)['"]`)
)

func geminiClient() (id, secret string) {
	id = strings.TrimSpace(os.Getenv("GEMINI_OAUTH_CLIENT_ID"))
	secret = strings.TrimSpace(os.Getenv("GEMINI_OAUTH_CLIENT_SECRET"))
	if id != "" && secret != "" {
		return id, secret
	}
	paths := geminiOAuth2Candidates
	if p := strings.TrimSpace(os.Getenv("GEMINI_OAUTH2_JS_PATH")); p != "" && filepath.IsAbs(p) {
		paths = []string{p}
	}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || st.Size() > 4<<20 {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		mID := geminiClientIDRe.FindSubmatch(raw)
		mSec := geminiClientSecretRe.FindSubmatch(raw)
		if mID != nil && mSec != nil {
			return string(mID[1]), string(mSec[1])
		}
	}
	return "", ""
}

// A usable access token: the file's if it still has a minute to live, the
// cached refreshed one next, a fresh refresh last. Empty string plus an error
// slug when none of the three can be had.
func geminiAccessToken(c geminiCreds) (string, string, string) {
	t := now()
	geminiTok.Lock()
	defer geminiTok.Unlock()
	if c.access != "" && c.access != geminiTok.bad && c.expiryMs/1000 > t+60 {
		return c.access, "", ""
	}
	if geminiTok.token != "" && geminiTok.until > t+60 {
		return geminiTok.token, "", ""
	}
	if c.refresh == "" {
		return "", "token-expired", "凭据已过期且没有 refresh token；跑一次 gemini 让它续期。"
	}
	id, secret := geminiClient()
	if id == "" || secret == "" {
		return "", "not-supported", "找不到 gemini-cli 的 OAuth client（装了 CLI 吗？也可设 GEMINI_OAUTH_CLIENT_ID/SECRET）。"
	}
	form := url.Values{
		"client_id":     {id},
		"client_secret": {secret},
		"refresh_token": {c.refresh},
		"grant_type":    {"refresh_token"},
	}
	status, body, err := provRequest("POST", geminiTokenURL,
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		[]byte(form.Encode()))
	if err != nil {
		return "", "unreachable", "刷新令牌失败"
	}
	if status == 400 || status == 401 {
		// Only invalid_grant means the refresh token itself is dead. A 400 can
		// also be invalid_client (our extracted client id has rotated) or a
		// malformed request — telling the user to re-login cannot fix either.
		var oe struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &oe) == nil && oe.Error == "invalid_grant" {
			return "", "token-expired", "refresh token 已失效；跑一次 gemini 重新登录。"
		}
		return "", "api-error", "令牌接口拒绝了刷新请求（" + itoa(status) + "）"
	}
	if status != 200 {
		return "", "api-error", "令牌接口返回 " + itoa(status)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   any    `json:"expires_in"`
	}
	if json.Unmarshal(body, &tok) != nil || tok.AccessToken == "" {
		return "", "api-error", "令牌接口返回的不是预期结构"
	}
	ttl := 3600.0
	if v := asNum(tok.ExpiresIn); v != nil && *v > 60 {
		ttl = *v
	}
	geminiTok.token, geminiTok.until = tok.AccessToken, t+ttl-60
	return tok.AccessToken, "", ""
}

func fetchGemini() Provider {
	p := Provider{ID: "gemini", Name: "Gemini CLI", Source: "api", CapturedAt: now()}
	fail := func(err, detail string) Provider {
		p.Error, p.Detail = err, detail
		return p
	}

	creds, ok := readGeminiCreds()
	if !ok {
		return fail("not-signed-in", "~/.gemini/oauth_creds.json 不存在——gemini-cli 装了并登录过吗？")
	}
	switch at := geminiAuthType(); at {
	case "", "oauth-personal":
		// The quota below is the personal Code Assist quota.
	case "api-key", "gemini-api-key", "vertex-ai":
		return fail("not-supported", "这台机器的 gemini-cli 用 "+at+" 认证，额度不在个人配额接口上。")
	}

	token, errSlug, detail := geminiAccessToken(creds)
	if token == "" {
		return fail(errSlug, detail)
	}
	hdr := map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}

	// Which project the quota question is even about — and the plan id
	// (currentTier upstream), which becomes the plan chip. A managed no-cost
	// account answers with its own project.
	var project, planID string
	if status, body, err := provRequest("POST", geminiLoadURL, hdr,
		[]byte(`{"metadata":{"ideType":"GEMINI_CLI","pluginType":"GEMINI"}}`)); err == nil && status == 200 {
		var la struct {
			CurrentTier *struct {
				ID string `json:"id"`
			} `json:"currentTier"`
			CloudAICompanionProject any    `json:"cloudaicompanionProject"`
			ProjectID               string `json:"projectId"`
		}
		if json.Unmarshal(body, &la) == nil {
			switch v := la.CloudAICompanionProject.(type) {
			case string:
				project = v
			case map[string]any:
				project = asStr(v["id"], 128)
			}
			if project == "" {
				project = la.ProjectID
			}
			if la.CurrentTier != nil {
				planID = la.CurrentTier.ID
			}
		}
	} else if err == nil && status == 401 {
		// The vendor's opinion of this token beats its printed expiry. Mark it
		// so the NEXT collection refreshes instead of replaying the refusal
		// for the rest of the hour.
		geminiInvalidate(token)
		return fail("token-expired", "Code Assist 接口返回 401")
	}

	quotaBody := []byte(`{}`)
	if project != "" {
		if b, err := json.Marshal(map[string]string{"project": project}); err == nil {
			quotaBody = b
		}
	}
	status, body, err := provRequest("POST", geminiQuotaURL, hdr, quotaBody)
	if err != nil {
		return fail("unreachable", "请求失败")
	}
	switch {
	case status == 401:
		geminiInvalidate(token)
		return fail("token-expired", "配额接口返回 401")
	case status == 403:
		// Not conflated with 401: a 403 is "this account may not", which no
		// amount of refreshing changes, and burning the refresh token's
		// goodwill on it would be pure noise.
		return fail("api-error", "配额接口拒绝访问（403）")
	case status == 429:
		return fail("rate-limited", "配额接口限流（429）")
	case status != 200:
		return fail("api-error", "配额接口返回 "+itoa(status))
	}

	var qr struct {
		Buckets []struct {
			RemainingFraction any    `json:"remainingFraction"`
			ResetTime         string `json:"resetTime"`
			ModelID           string `json:"modelId"`
		} `json:"buckets"`
	}
	if json.Unmarshal(body, &qr) != nil {
		return fail("api-error", "配额接口返回的不是预期结构")
	}

	// One row per model, worst bucket wins — a model's tightest window is the
	// one that will actually refuse the next request. Same grouping the CLI's
	// own /stats view (and CodexBar) applies.
	type worst struct {
		fraction float64
		reset    string
	}
	perModel := map[string]worst{}
	var order []string
	for _, b := range qr.Buckets {
		f := asNum(b.RemainingFraction)
		if f == nil || b.ModelID == "" {
			continue
		}
		w, seen := perModel[b.ModelID]
		if !seen {
			order = append(order, b.ModelID)
			perModel[b.ModelID] = worst{*f, b.ResetTime}
		} else if *f < w.fraction {
			perModel[b.ModelID] = worst{*f, b.ResetTime}
		}
	}
	for _, id := range order {
		w := perModel[id]
		used := 100 - w.fraction*100
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		l := Limit{Key: tame(id, 24), UsedPercent: round2(used)}
		if l.Key == "" {
			l.Key = "model"
		}
		if e := isoToEpoch(w.reset); e > 0 {
			l.ResetsAt = fp(e)
		}
		p.Limits = append(p.Limits, l)
	}
	if len(p.Limits) == 0 {
		return fail("no-readings", "配额接口没有返回可用的额度窗口")
	}

	p.OK = true
	p.RecordedAt = fp(now())
	if plan := tame(planID, 32); plan != "" {
		p.PlanType = sp(plan)
	}
	return p
}
