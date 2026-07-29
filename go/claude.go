package main

// Claude Code: quota costs a credential.
//
// Claude records no quota on disk at all — its transcripts carry token counts
// and a service_tier and nothing else (established by walking every nested key
// in recent transcripts, not just the obvious ones). So the numbers have to be
// asked for, and asking requires the token.
//
// The rules that come with holding one:
//
//   - Read four scalars out of claudeAiOauth and nothing else. That file also
//     holds MCP OAuth tokens for whatever third-party services the user has
//     connected — unrelated credentials this code has no business near.
//   - Never refresh, never write back. An expired token is reported and left
//     for Claude Code to renew; rewriting somebody's credential file to keep a
//     status panel green is not a trade worth making.
//   - No error text derived from an exception, the URL, or a header may reach
//     the return value. This collector's failures are PUBLISHED — they ride to
//     the relay, render on the page, and sit in the raw payload panel. A token
//     containing CR/LF makes the http layer produce an error quoting the whole
//     Authorization header, which is exactly the shape that must never escape.
//
// What travels: quota numbers, plus the plan and tier they belong to. So the
// honest claim is "no credential leaves the machine", not "only percentages".

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// How often the usage endpoint may actually be called.
//
// Codex costs nothing to read — it is a local file, and the push loop can run
// as often as it likes. Claude is a real request to someone else's service,
// and the push loop's 30s would be 2,880 calls a day from one machine. Run the
// helper on three boxes and it is a rude amount of traffic for a number that
// moves slowly. Found the hard way: enough verification runs in one session
// returned 429.
//
// Between calls the last answer is served as-is. That is not a lie about
// freshness — recorded_at carries the time the numbers were actually fetched,
// and the page renders the age from it, so a cached reading visibly ages.
const (
	claudeMinInterval = 120.0 // seconds between real calls
	claudeBackoffBase = 60.0  // first pause after a 429, doubling
	claudeBackoffMax  = 1800.0
)

var claudeCache struct {
	sync.Mutex
	last      *Provider // last successful reading, served while cooling down
	fetchedAt float64
	coolUntil float64
	strikes   float64
}

// Window kinds the usage endpoint reports, and the label each carries. The API
// gives a kind and a reset timestamp but no window length.
var claudeKinds = map[string][2]string{
	"session":       {"session", "5h"},
	"weekly_all":    {"weekly", "7d"},
	"weekly_scoped": {"weekly_scoped", "7d"},
}

type claudeAuth struct {
	token string
	plan  string
	tier  string
	// Unix MILLISECONDS in the credential file, unlike Codex's seconds.
	expiresAt float64
}

// A string from the credential file that we intend to publish.
//
// subscriptionType and rateLimitTier travel to the relay and onto other
// people's screens, and they come out of a file this program does not control.
// Nothing weird is expected there — but "nothing weird is expected" is not a
// reason to forward arbitrary bytes out of a credential store.
func tame(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) > limit {
		s = s[:limit]
	}
	for _, c := range s {
		ok := c == '-' || c == '_' || c == '.' || c == ' ' ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			return ""
		}
	}
	return s
}

func readClaudeAuth() (claudeAuth, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return claudeAuth{}, false
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return claudeAuth{}, false
	}
	// Decode only the subtree we want. The MCP nodes never become values in
	// this program.
	var doc struct {
		ClaudeAiOauth *struct {
			AccessToken      string  `json:"accessToken"`
			SubscriptionType string  `json:"subscriptionType"`
			RateLimitTier    string  `json:"rateLimitTier"`
			ExpiresAt        float64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.ClaudeAiOauth == nil || doc.ClaudeAiOauth.AccessToken == "" {
		return claudeAuth{}, false
	}
	o := doc.ClaudeAiOauth
	return claudeAuth{
		token:     o.AccessToken,
		plan:      tame(o.SubscriptionType, 32),
		tier:      tame(o.RateLimitTier, 32),
		expiresAt: o.ExpiresAt,
	}, true
}

// Limits are decoded one at a time so a single malformed entry is skipped
// rather than failing the whole response — the same tolerance the Python
// reference has. A strongly-typed slice would throw away three good windows
// because a fourth arrived with a percent as a string.
type claudeUsage struct {
	Limits []json.RawMessage `json:"limits"`
}

type claudeLimit struct {
	Kind     any `json:"kind"`
	Percent  any `json:"percent"`
	Severity any `json:"severity"`
	ResetsAt any `json:"resets_at"`
	IsActive any `json:"is_active"`
	Scope    *struct {
		Model *struct {
			DisplayName any `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

func collectClaude() Provider {
	claudeCache.Lock()
	defer claudeCache.Unlock()

	t := now()
	// Serve the cached reading rather than call again, whether we're merely
	// inside the polling interval or actively being told to back off. Handing
	// back a stale-but-real answer beats replacing a good number with an
	// error nobody can act on.
	if claudeCache.last != nil && (t < claudeCache.coolUntil || t-claudeCache.fetchedAt < claudeMinInterval) {
		cached := *claudeCache.last
		cached.CapturedAt = t // when we looked; RecordedAt still says when it was fetched
		return cached
	}
	if t < claudeCache.coolUntil {
		// Cooling down with nothing cached to fall back on.
		return Provider{ID: "claude", Name: "Claude Code", Source: "api", CapturedAt: t,
			Error: "rate-limited", Detail: "额度接口限流中，稍后重试"}
	}

	p := fetchClaude()
	if p.OK {
		claudeCache.last = &p
		claudeCache.fetchedAt = t
		claudeCache.strikes = 0
		return p
	}
	if p.Error == "rate-limited" {
		// Exponential, so a service that is genuinely unhappy is left alone
		// rather than poked every two minutes.
		claudeCache.strikes++
		pause := claudeBackoffBase * math.Pow(2, claudeCache.strikes-1)
		if pause > claudeBackoffMax {
			pause = claudeBackoffMax
		}
		claudeCache.coolUntil = t + pause
		if claudeCache.last != nil {
			cached := *claudeCache.last
			cached.CapturedAt = t
			return cached
		}
	}
	return p
}

func fetchClaude() Provider {
	p := Provider{ID: "claude", Name: "Claude Code", Source: "api", CapturedAt: now()}
	fail := func(err, detail string) Provider {
		p.Error, p.Detail = err, detail
		return p
	}

	auth, ok := readClaudeAuth()
	if !ok {
		// Literal path, never the absolute one — that carries the local
		// username, which would then travel to every watcher's screen.
		return fail("not-signed-in", "~/.claude/.credentials.json 里没有 claudeAiOauth")
	}
	if auth.expiresAt > 0 && auth.expiresAt/1000 < now() {
		return fail("token-expired", "凭据已过期；跑一次 Claude Code 让它续期。")
	}

	req, err := http.NewRequest("GET", claudeUsageURL, nil)
	if err != nil {
		// Type only. err.Error() here can quote the URL and headers.
		return fail("unreachable", "构造请求失败")
	}
	req.Header.Set("Authorization", "Bearer "+auth.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-meter")

	// Refuse redirects. Go's default client re-sends the request to whatever
	// Location names and, for a same-host hop, carries Authorization along —
	// a redirect from the usage endpoint, hostile or merely misconfigured,
	// would hand the token to another origin.
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirect refused")
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail("unreachable", "请求失败")
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fail("api-error", "额度接口试图重定向")
	}
	if resp.StatusCode == 401 {
		return fail("token-expired", "额度接口返回 401")
	}
	if resp.StatusCode == 429 {
		// Distinct from api-error: this one is our fault and self-corrects,
		// and the caller reacts to it by backing off rather than by telling
		// the user something is broken.
		return fail("rate-limited", "额度接口限流（429）")
	}
	if resp.StatusCode != 200 {
		return fail("api-error", "额度接口返回 "+itoa(resp.StatusCode))
	}

	var usage claudeUsage
	if json.NewDecoder(resp.Body).Decode(&usage) != nil {
		return fail("api-error", "额度接口返回的不是预期结构")
	}

	for _, raw := range usage.Limits {
		var l claudeLimit
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		pct := asNum(l.Percent)
		if pct == nil {
			continue
		}
		// An unrecognised kind is passed through bounded, not dropped — a new
		// window type should show up as itself rather than vanish.
		kind := asStr(l.Kind, 24)
		key, label := kind, ""
		if k, found := claudeKinds[kind]; found {
			key, label = k[0], k[1]
		}
		if key == "" {
			key = "unknown"
		}
		lim := Limit{Key: key, UsedPercent: round2(*pct), Active: bp(asBool(l.IsActive))}
		if label != "" {
			lim.WindowLabel = sp(label)
		}
		if e := isoToEpoch(asStr(l.ResetsAt, 64)); e > 0 {
			lim.ResetsAt = fp(e)
		}
		switch asStr(l.Severity, 16) {
		case "normal", "warning", "critical":
			lim.Severity = sp(asStr(l.Severity, 16))
		}
		if l.Scope != nil && l.Scope.Model != nil {
			if n := asStr(l.Scope.Model.DisplayName, 40); n != "" {
				lim.Scope = sp(n)
			}
		}
		p.Limits = append(p.Limits, lim)
	}
	if len(p.Limits) == 0 {
		return fail("no-readings", "额度接口没有返回可用的窗口")
	}

	p.OK = true
	// Live from the API, so the reading IS current — none of the staleness gap
	// Codex has, where numbers are only as fresh as your last actual call.
	p.RecordedAt = fp(now())
	if auth.plan != "" {
		p.PlanType = sp(auth.plan)
	}
	if auth.tier != "" {
		p.RateLimitTier = sp(auth.tier)
	}
	return p
}
