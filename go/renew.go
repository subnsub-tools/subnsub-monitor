package main

// Keeping the relay token alive without ever holding a permanent secret.
//
// The token this helper pushes with expires. That is deliberate — it is a
// self-contained statement that an account was entitled to the relay when it
// was issued, and a statement nobody can revoke had better stop being true on
// its own. But the installer writes one token and walks away, so without
// something here every machine an account owns goes dark on the same day, and
// the dashboard reports it as "these machines stopped reporting" when the
// machines are fine.
//
// So: shortly before it expires, trade the current token for a fresh one. The
// site re-checks the subscription while doing it, which is the part that makes
// this more than a workaround — today a cancelled subscription keeps working
// until the token runs out because nobody ever looks again.
//
// WHAT THIS DELIBERATELY IS NOT. It is not a channel. The response is one
// string, checked against the same alphabet the relay enforces and rejected
// unless it actually extends our life. Nothing here can be told to change an
// interval, run a command, or fetch anything — the helper's habit of not
// parsing what the network says is worth keeping, and the way to keep it while
// still renewing is to make the one parsed value a token and nothing else.
//
// And it does not talk to the relay. The relay has no database and no idea who
// anyone is; entitlement lives on the site that issued the token in the first
// place. Renewing there keeps the relay a stateless verifier that never learns
// an identity, and keeps push responses unparsed — see transport.go.

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Where tokens are renewed. Baked in rather than passed through the service
	// definition, because every install that predates this has a unit file and a
	// plist naming only the relay, and those are not rewritten by an upgrade —
	// a value that only new installs could carry would leave exactly the
	// machines this feature exists for unable to use it.
	defaultSite = "https://tools.subnsub.com"
	siteEnv     = "SUBNSUB_MONITOR_SITE"
	renewPath   = "/api/monitor-token/renew"

	// Start trying with a week left. The server refuses renewals until a token
	// is inside its own, wider window, so this can be generous without
	// hammering: it is the point at which we begin, not a deadline.
	renewLead = 7 * 24 * time.Hour

	// After a failure, and why the floor is an hour rather than a few seconds:
	// every reason a renewal fails is slow to change. An expired subscription,
	// a machine that has been off too long, a site outage — none of them are
	// fixed by asking again in a minute, and a helper that retried at push
	// cadence would turn one lapsed account into 2,880 requests a day.
	renewRetryMin = time.Hour
	renewRetryMax = 12 * time.Hour

	// A renewal answer is two short fields. Anything larger is not one.
	maxRenewBody = 4 << 10

	// Where a renewed token is kept, beside the id and the name so that
	// --uninstall takes it with everything else.
	tokenFile = "token.current"
)

// The 44-byte token layout, mirrored from functions/_lib/monitortoken.js:
// 16 bytes of identity, 4 bytes of big-endian expiry, 8 of nonce, 16 of tag.
// Only the expiry is read here — the helper has no business interpreting the
// rest, and cannot verify the tag anyway.
const (
	tokenBytes   = 44
	tokenExpOff  = 16
	legacyTokLen = 43 // the self-minted tokens that predate signing: 32 raw bytes
)

// When this token stops being accepted, if it says.
//
// Tokens minted by the old `token` subcommand carry no expiry at all, and there
// are installs in the field holding one. They report ok=false and are simply
// never renewed — there is nothing to renew them from, and the relay's
// allow-list path they depend on does not expire either.
func tokenExpiry(tok string) (time.Time, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != tokenBytes {
		return time.Time{}, false
	}
	secs := binary.BigEndian.Uint32(raw[tokenExpOff : tokenExpOff+4])
	if secs == 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(secs), 0), true
}

// The alphabet the relay accepts, applied to anything the site hands back.
// Not a formality: this value is about to be written to disk and sent as an
// Authorization header, and a response that arrived with a newline in it would
// be the interesting kind of bug.
func cleanToken(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 24 || len(s) > 128 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return ""
		}
	}
	return s
}

func siteBase() string {
	if v := strings.TrimSpace(os.Getenv(siteEnv)); v != "" {
		if strings.HasPrefix(v, "https://") {
			return strings.TrimRight(v, "/")
		}
		// An http:// override would put the token on the wire in clear. Refuse
		// rather than honour it; the default is not a downgrade.
		warnf("ignoring %s: renewal requires an https:// URL", siteEnv)
	}
	return defaultSite
}

// ── where a renewed token lives ────────────────────────────────────────────
//
// NOT the file the installer wrote. That one is a systemd EnvironmentFile on
// Linux and is not read at all on macOS, where the token is inlined in the
// plist — so writing a renewal there would take effect on one platform, after a
// restart, and never on the other. A file the HELPER reads works the same
// everywhere and leaves the installer's formats alone.

func tokenPath() string {
	dir := configDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, tokenFile)
}

func storedToken() string {
	path := tokenPath()
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return cleanToken(string(b))
}

func saveToken(tok string) error {
	dir := configDir()
	if dir == "" {
		return os.ErrNotExist
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, tokenFile)
	// Write-then-rename at 0600, the same shape the installer and the agent id
	// use: a plain create follows a symlink to wherever it points and can leave
	// half a secret behind if the process dies mid-write.
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(tok+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Which of the two tokens this machine holds is the live one.
//
// The installer's token is the bootstrap; a renewed one supersedes it. They are
// compared by expiry rather than by preferring the file, because the file is
// also what a REINSTALL has to be able to override: paste a fresh token from
// the panel and the install writes it to the environment, where it is newer
// than whatever renewal left on disk and therefore wins. A rule of "the file
// always wins" would make reinstalling silently do nothing.
func pickToken(envTok, fileTok string) string {
	envTok, fileTok = cleanToken(envTok), cleanToken(fileTok)
	if fileTok == "" {
		return envTok
	}
	if envTok == "" {
		return fileTok
	}
	fileExp, fileOK := tokenExpiry(fileTok)
	envExp, envOK := tokenExpiry(envTok)
	if !fileOK {
		return envTok
	}
	// An unsigned environment token (the pre-signing kind, no expiry) is not
	// something a dated file should displace: it works through the relay's
	// allow-list, which a renewed token would not be on.
	if !envOK {
		return envTok
	}
	if fileExp.After(envExp) {
		return fileTok
	}
	return envTok
}

// ── the renewal loop's state ───────────────────────────────────────────────

type renewer struct {
	site  string
	next  time.Time // earliest next attempt; zero means "decide from the token"
	fails int
}

func newRenewer() *renewer { return &renewer{site: siteBase()} }

// Should we try now? `forced` is the push loop reporting that the relay just
// refused us, which is the other moment a token is worth questioning.
//
// A forced attempt still respects the backoff. Without that, a relay answering
// 403 for a reason renewal cannot fix — a room that was never enabled, a
// subscription that really did lapse — would mean one renewal request per push,
// forever.
func (r *renewer) due(tok string, now time.Time, forced bool) bool {
	if now.Before(r.next) {
		return false
	}
	if forced {
		return true
	}
	exp, ok := tokenExpiry(tok)
	if !ok {
		// Nothing to renew from. Park it rather than asking again every push.
		r.next = now.Add(renewRetryMax)
		return false
	}
	return !now.Before(exp.Add(-renewLead))
}

// Try once. Returns the token to keep using — the new one on success, the one
// we came in with on any failure.
func (r *renewer) attempt(tok string, now time.Time) string {
	fresh, err := requestRenewal(r.site, tok)
	if err != nil {
		r.fails++
		backoff := renewRetryMin * time.Duration(1<<min(r.fails-1, 8))
		if backoff > renewRetryMax || backoff <= 0 {
			backoff = renewRetryMax
		}
		r.next = now.Add(backoff)
		warnf("could not renew the relay token: %v", err)
		return tok
	}
	r.fails = 0
	if err := saveToken(fresh); err != nil {
		// Held in memory regardless, exactly like a machine id that cannot be
		// written: this process keeps reporting, and a restart falls back to
		// the installed token — which is the situation we were already in.
		warnf("renewed the relay token but could not save it: it will be renewed again after a restart")
	}
	if exp, ok := tokenExpiry(fresh); ok {
		r.next = exp.Add(-renewLead)
	} else {
		r.next = now.Add(renewRetryMax)
	}
	return fresh
}

// One renewal request.
func requestRenewal(site, tok string) (string, error) {
	req, err := http.NewRequest("POST", site+renewPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", "subnsub-monitor")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{
		Timeout: 15 * time.Second,
		// Same refusal as the push path, for the same reason: Go's default
		// client keeps Authorization across a redirect to the same host or a
		// subdomain, judging by host alone without checking the scheme, so an
		// https→http hop would put the token on the wire in clear.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", renewError(resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRenewBody))
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", errString("renewal answer was not JSON")
	}
	fresh := cleanToken(out.Token)
	if fresh == "" {
		return "", errString("renewal answer carried no usable token")
	}
	// It has to actually be worth swapping to. A reply that hands back something
	// expiring no later than what we hold is either a bug or someone playing,
	// and taking it would reset nothing while overwriting a working token.
	newExp, newOK := tokenExpiry(fresh)
	if !newOK {
		return "", errString("renewed token carries no expiry")
	}
	if oldExp, ok := tokenExpiry(tok); ok && !newExp.After(oldExp) {
		return "", errString("renewed token expires no later than the current one")
	}
	return fresh, nil
}

// Turn a refusal into something the log can explain, without parsing anything
// we would then act on. The status is the whole decision; the code is a hint
// for a human reading stderr.
func renewError(resp *http.Response) error {
	var out struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRenewBody))
	_ = json.Unmarshal(body, &out)
	switch {
	case resp.StatusCode == 401:
		return errString("this machine's token is no longer valid — reinstall to reconnect it")
	case resp.StatusCode == 403 && out.Error == "plus-required":
		return errString("this account is no longer entitled to the relay")
	case resp.StatusCode == 403 && out.Error == "subject-unknown":
		return errString("this token predates renewal — open the Monitor panel once to link it")
	case resp.StatusCode == 429:
		return errString("too early to renew")
	default:
		return errString("renewal refused: " + itoa(resp.StatusCode))
	}
}

type errString string

func (e errString) Error() string { return string(e) }
