package main

// Codex, live.
//
// The account's rate limits as the Codex CLI itself sees them, asked over the
// app-server's stdio JSON-RPC:
//
//	codex app-server
//	  → {"id":1,"method":"initialize","params":{"clientInfo":{…}}}
//	  → {"method":"initialized"}
//	  → {"id":2,"method":"account/rateLimits/read","params":null}
//	  ← {"id":2,"result":{"rateLimits":{…},"rateLimitsByLimitId":{…}}}
//
// Why this exists when codex.go already reads the number off the disk: the log
// holds only what a PERSISTED session recorded, and a machine whose Codex usage
// is all `codex exec --ephemeral` records nothing at all. One of ours reported
// 88% left for two weeks while the account was down to 19% — the reading was
// faithful to the file, and the file had stopped moving. A collector that can
// only be as fresh as your last interactive session is not a quota gauge.
//
// It also answers something the log cannot. `rateLimitsByLimitId` carries one
// bucket per metered limit — the account's own, and on this plan a separate
// weekly window for GPT-5.3-Codex-Spark. The logged envelope is a single flat
// snapshot with primary/secondary, so a second bucket has nowhere to go and
// simply never appears no matter how fresh the file is.
//
// Costs, measured rather than assumed: ~1.5s wall and ~90 MB peak RSS for the
// child, once per cache floor rather than once per push. It writes no session
// file and does not touch auth.json. Its rows in Codex's own log database are
// real but bounded — that database prunes on its own retention and was 99% free
// pages when this was written, so these land on space it already carries.
//
// What it gives up, which is the honest part: the log collector could say "no
// credential, no network". This one still holds no credential — it never opens
// auth.json — but it does ask the vendor's own tool to make a call on our
// behalf, exactly as amp.go does, and that tool may refresh the credential it
// owns while doing so. That is the `cli` rung, and the panel prints "via CLI"
// instead of "local log", so the difference is on screen rather than only in
// this comment.
//
// Upstream marks the app-server experimental, so this is a PREFERENCE and never
// a requirement. No binary, a protocol that moved, a machine that is signed out
// — every one of them falls through to the log scan, which is unchanged. The
// fallback is not decoration: on a box with no Codex binary in a place this is
// willing to run from, it is the only leg there is.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Generous next to the 1.5s a warm run takes, because the first spawn on a cold
// page cache is a different animal and a timeout here costs a whole cycle's
// reading. Still well inside collectAll's own budget.
//
// A var rather than a const only so the hang test can shorten it. Nothing else
// writes it.
var codexRPCTimeout = 20 * time.Second

const (
	codexBinEnv = "MON_CODEX_BIN"

	// The app-server is chatty — notifications about threads, plugins and
	// startup arrive interleaved with our answers — so the read loop needs a
	// budget rather than a promise. Both bite before memory does; whichever
	// comes first refuses the reading rather than parsing what arrived.
	codexRPCMaxBytes = 1 << 20
	codexRPCMaxLines = 4000
	codexRPCMaxLine  = 512 << 10

	// Limit keys are capped at 24 characters by the relay. A bucket key is
	// built rather than passed through, so it is cut HERE, where the cut can be
	// seen, instead of silently downstream.
	codexKeyMax = 24
)

var codexLiveCache provCache

// Everything that can vary is decoded as `any` for the same reason codex.go
// does it: this is another program's output, and one unrelated field arriving
// in an unexpected shape must not throw away a perfectly good usedPercent
// sitting next to it.
type codexLiveWindow struct {
	UsedPercent any `json:"usedPercent"`
	WindowMins  any `json:"windowDurationMins"`
	ResetsAt    any `json:"resetsAt"`
}

type codexLiveCredits struct {
	HasCredits any `json:"hasCredits"`
	Unlimited  any `json:"unlimited"`
	Balance    any `json:"balance"`
}

type codexLiveBucket struct {
	LimitID   any               `json:"limitId"`
	LimitName any               `json:"limitName"`
	Primary   *codexLiveWindow  `json:"primary"`
	Secondary *codexLiveWindow  `json:"secondary"`
	PlanType  any               `json:"planType"`
	Credits   *codexLiveCredits `json:"credits"`
}

type codexLiveResult struct {
	// The backward-compatible single-bucket view, same shape the session log
	// carries. Kept as the fallback for a build that predates the map.
	RateLimits *codexLiveBucket `json:"rateLimits"`
	// One entry per metered limit, keyed by limitId.
	ByLimitID map[string]*codexLiveBucket `json:"rateLimitsByLimitId"`
}

type codexRPCMessage struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

var (
	errCodexNoBinary = errors.New("no codex binary")
	errCodexSpawn    = errors.New("app-server would not start")
	errCodexTimeout  = errors.New("app-server timed out")
	errCodexProtocol = errors.New("app-server answered in an unknown shape")
	errCodexRefused  = errors.New("app-server refused the request")
)

// Where the Codex CLI is allowed to be.
//
// PATH is not consulted to FIND it, for the reason spelled out at length in
// amp.go: PATH is inherited from whatever launched the helper, and a service
// that runs "the first thing called codex on PATH" is one prepended directory
// away from executing somebody else's file.
//
// Worth being exact about what that does and does not buy, because the
// equivalent comment on the other CLI collectors reads as a stronger promise
// than it is. `codex` from npm is a script beginning `#!/usr/bin/env node`, so
// choosing the script from a fixed list still leaves the kernel resolving
// `node` through PATH at exec time. Pinning an interpreter instead is not an
// option worth the trade: the node that belongs to this install is the one
// beside it under a version manager, naming it would mean hard-coding one
// layout, and a helper that cannot find node stops reporting rather than
// reports safely. What the fixed list actually guarantees is that the TOOL is
// the one the user installed; the interpreter is trusted exactly as far as this
// account's PATH is, which is the same footing every other CLI-rung collector
// here has always stood on.
//
// The node version managers are the difference from vendorCandidates' fixed
// list. Codex ships as an npm package, so on a great many machines it lives
// under a per-version directory whose name cannot be written down in advance.
// A glob over ONE fixed shape is still a fixed set of directories — it is not
// PATH, and nothing outside ~/.nvm/versions/node/*/bin can answer to it.
func codexBinary() string {
	if v := strings.TrimSpace(os.Getenv(codexBinEnv)); v != "" {
		// An override that does not hold up is a refusal, not a reason to go
		// looking elsewhere: someone pointed us at a specific file on purpose.
		if filepath.IsAbs(v) && usableBinary(v) {
			return v
		}
		return ""
	}
	cands := vendorCandidates("codex")
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cands = append(cands,
			filepath.Join(home, ".bun", "bin", "codex"),
			filepath.Join(home, ".volta", "bin", "codex"),
			filepath.Join(home, ".npm-global", "bin", "codex"))
		cands = append(cands, codexNodeVersionCandidates(home)...)
	}
	for _, c := range cands {
		if usableBinary(c) {
			return c
		}
	}
	return ""
}

// nvm's installs, newest version first.
//
// Compared as NUMBERS, component by component, and not as strings — the whole
// point of this function. "v9" sorts after "v22" lexically, and one level down
// the same trap sits again: "v22.9.0" sorts after "v22.19.0". Getting only the
// major right would still hand a machine an older CLI than the one it has, and
// an older CLI is one that may not speak the protocol this asks for.
func codexNodeVersionCandidates(home string) []string {
	matches, err := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin", "codex"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	// …/node/<version>/bin/codex → [major, minor, patch]. An unparseable
	// component reads as -1, which sorts it below every real version rather
	// than letting it win by accident.
	parts := func(p string) [3]int {
		v := strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(p))), "v")
		out := [3]int{-1, -1, -1}
		for i, f := range strings.SplitN(v, ".", 3) {
			if i > 2 {
				break
			}
			if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
				out[i] = n
			}
		}
		return out
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := parts(matches[i]), parts(matches[j])
		for k := 0; k < 3; k++ {
			if a[k] != b[k] {
				return a[k] > b[k]
			}
		}
		return matches[i] > matches[j]
	})
	return matches
}

// One app-server conversation: start it, initialize, ask, leave.
//
// THE DEADLINE IS ON THE READ, not only on the context, and that distinction is
// the whole reason this function is shaped the way it is.
//
// `codex` from npm is a Node script that spawns the real Codex binary with
// inherited stdio. Cancelling the context kills the Node process this started
// and nothing else, so the grandchild still holds the write end of our stdout
// pipe: no EOF ever arrives, a Scan() blocked on it stays blocked, and
// cmd.WaitDelay cannot help because it only bounds Wait(), which we would not
// reach until after the read we are stuck in. The collector holds its cache
// mutex throughout, and collectAll waits on every collector, so one hung
// app-server would take the entire helper off the air rather than cost one
// provider one cycle.
//
// os.Pipe with a read deadline ends that: the read fails on time no matter who
// else is holding the pipe open. Closing stdin then gives the grandchild the
// EOF a stdio server shuts down on, which is what actually collects it —
// killing Node alone would leave it running.
func codexAskRateLimits(bin string) (*codexLiveResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), codexRPCTimeout)
	defer cancel()

	cmd := toolCommand(ctx, bin, "app-server")
	cmd.Env = vendorEnv([]string{"CODEX_"})
	// nil is the null device, which is what we want for stderr: the app-server
	// writes progress there and nobody reads it, and an unread pipe that fills
	// is a deadlock rather than a missing log line.
	cmd.Stderr = nil
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errCodexSpawn
	}
	// Deliberately NOT cmd.StdoutPipe(): that hands back a pipe whose lifetime
	// belongs to Wait(), and an *os.File we made ourselves is the only one we
	// can put a deadline on.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, errCodexSpawn
	}
	cmd.Stdout = pw
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, errCodexSpawn
	}
	// The child holds its own copy now. Ours has to go, or the read end never
	// sees EOF even when every process in the tree has exited.
	pw.Close()
	pr.SetReadDeadline(time.Now().Add(codexRPCTimeout))
	defer func() {
		// Order matters. stdin first: EOF is how a stdio server is asked to
		// leave, and it reaches the grandchild that a kill does not. Then the
		// read end, so nothing is left blocked on it. Then the process this
		// actually started, then exactly one Wait to reap it.
		stdin.Close()
		pr.Close()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
	}()
	stdout := pr

	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return errCodexProtocol
		}
		if _, err := stdin.Write(append(b, '\n')); err != nil {
			return errCodexSpawn
		}
		return nil
	}

	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 0, 64<<10), codexRPCMaxLine)
	read := 0
	seen := 0
	// The answer to one id, skipping everything else the server says on the
	// way. Notifications are not errors here — they are the normal traffic of a
	// server that has just started up.
	await := func(id int64) (json.RawMessage, error) {
		for lines.Scan() {
			b := lines.Bytes()
			read += len(b)
			seen++
			if read > codexRPCMaxBytes || seen > codexRPCMaxLines {
				return nil, errCodexProtocol
			}
			var m codexRPCMessage
			if json.Unmarshal(b, &m) != nil || m.ID == nil || *m.ID != id {
				continue
			}
			if len(m.Error) > 0 && string(m.Error) != "null" {
				// Deliberately not quoting it. This travels to the relay and is
				// published, and an error from somebody's account endpoint is
				// the last thing that should be repeated verbatim on a page.
				return nil, errCodexRefused
			}
			if len(m.Result) == 0 {
				return nil, errCodexProtocol
			}
			return m.Result, nil
		}
		// The read deadline is the one that fires when a grandchild is holding
		// the pipe open, so it is checked before the context: ctx may not be
		// done yet when the pipe already gave up.
		if err := lines.Err(); errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, errCodexTimeout
		}
		if ctx.Err() == context.DeadlineExceeded {
			return nil, errCodexTimeout
		}
		// Clean EOF without our answer: a server that started and left, which
		// is what a signed-out or half-installed CLI looks like from here. A
		// line past the budget lands here too, and means the same thing to a
		// caller — this is not the protocol we know.
		return nil, errCodexProtocol
	}

	if err := send(map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]any{"name": "subnsub-monitor", "version": helperVersion},
		},
	}); err != nil {
		return nil, err
	}
	if _, err := await(1); err != nil {
		return nil, err
	}
	// The handshake's second half is a notification, so there is nothing to
	// wait for; the request after it is what we actually came for.
	if err := send(map[string]any{"method": "initialized"}); err != nil {
		return nil, err
	}
	if err := send(map[string]any{"id": 2, "method": "account/rateLimits/read", "params": nil}); err != nil {
		return nil, err
	}
	raw, err := await(2)
	if err != nil {
		return nil, err
	}
	var res codexLiveResult
	if json.Unmarshal(raw, &res) != nil {
		return nil, errCodexProtocol
	}
	if res.RateLimits == nil && len(res.ByLimitID) == 0 {
		return nil, errCodexProtocol
	}
	return &res, nil
}

// The account's own bucket, by name. Everything else on the payload — the plan,
// the credit balance — belongs to this one; the extra buckets are windows on
// other meters and carry their own nulls for the rest.
const codexPrimaryLimitID = "codex"

// A bucket and the name it was filed under. The map key is carried rather than
// re-derived from the bucket's own limitId: the two are the same today, the
// protocol promises only the key, and a bucket whose embedded id is null would
// otherwise lose the one identifier it had.
type codexBucketRef struct {
	id string
	b  *codexLiveBucket
}

// Buckets in a stable order: the account's own first, then the rest by id.
// Stable because the card rebuilds itself whenever the shape of its rows
// changes, and a map iterated in Go's order would change that shape every
// thirty seconds.
//
// The flat `rateLimits` is not a lesser copy to be dropped as soon as the map
// exists — the protocol defines it as the backward-compatible view of the
// account's own bucket, so it is the fallback for the account WHENEVER the map
// does not carry one. An earlier version took "map is non-empty" as licence to
// ignore it entirely, which on a payload holding extra meters and no `codex`
// entry would have thrown away the main quota and then read the plan and credit
// balance off whichever extra meter happened to sort first.
func codexOrderedBuckets(res *codexLiveResult) []codexBucketRef {
	var out []codexBucketRef
	primaryID := codexPrimaryLimitID
	primary := res.ByLimitID[codexPrimaryLimitID]
	if primary == nil {
		primary = res.RateLimits
		if primary != nil {
			if id := asStr(primary.LimitID, 64); id != "" {
				primaryID = id
			}
		}
	}
	if primary != nil {
		out = append(out, codexBucketRef{primaryID, primary})
	}
	ids := make([]string, 0, len(res.ByLimitID))
	for id := range res.ByLimitID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if id == primaryID || id == codexPrimaryLimitID {
			continue
		}
		if b := res.ByLimitID[id]; b != nil {
			out = append(out, codexBucketRef{id, b})
		}
	}
	return out
}

// A limit key for a bucket that is not the account's own, inside the 24
// characters the relay allows.
//
// Truncating a long id would invite two meters to arrive under one key, and a
// card cannot tell two rows with the same key apart. The head stays readable
// and the tail carries the difference. Cut by RUNE — a byte cut can leave half
// a character on the wire.
func codexLimitKey(id, window string) string {
	suffix := "/" + window[:1]
	budget := codexKeyMax - len(suffix)
	if id == "" {
		id = "limit"
	}
	if len(id) <= budget {
		return id + suffix
	}
	sum := sha256.Sum256([]byte(id))
	tag := "~" + hex.EncodeToString(sum[:2])
	var head strings.Builder
	for _, r := range id {
		if head.Len()+len(string(r)) > budget-len(tag) {
			break
		}
		head.WriteRune(r)
	}
	return head.String() + tag + suffix
}

// One bucket's windows as Limits.
//
// The account's own bucket keeps the keys the log collector has always used —
// "primary" and "secondary" — so a machine switching legs does not look to the
// page like a provider that grew new windows. Other buckets are namespaced and
// carry a scope, which is the row's second label on the card.
//
// The scope falls back to the bucket's id when the vendor supplies no name for
// the meter, which the protocol allows. Without that fallback two nameless
// weekly buckets both render as "7d" and nothing on the card says which is
// which — the row label is the window, and the window is what they share.
func codexBucketLimits(ref codexBucketRef, isPrimaryBucket bool) []Limit {
	b := ref.b
	if b == nil {
		return nil
	}
	scope := asStr(b.LimitName, 40)
	if scope == "" {
		scope = ref.id
	}
	var out []Limit
	for _, w := range []struct {
		key string
		win *codexLiveWindow
	}{{"primary", b.Primary}, {"secondary", b.Secondary}} {
		if w.win == nil {
			continue
		}
		used := asNum(w.win.UsedPercent)
		if used == nil {
			continue
		}
		key := w.key
		if !isPrimaryBucket {
			key = codexLimitKey(ref.id, w.key)
		}
		l := Limit{
			Key:         key,
			UsedPercent: round2(*used),
			// Codex reports no severity; the page colours these by percentage.
			Severity: nil, Active: nil,
		}
		if !isPrimaryBucket && scope != "" {
			l.Scope = sp(scope)
		}
		if m := asNum(w.win.WindowMins); m != nil {
			l.WindowMinutes = m
			l.WindowLabel = windowLabel(*m)
		}
		l.ResetsAt = asNum(w.win.ResetsAt)
		out = append(out, l)
	}
	return out
}

// The live reading, or a failure to be told about. Nothing here falls back —
// collectCodex does that, because "the log is fresher than nothing" is a
// decision about the provider, not about this request.
func codexLiveFetch() Provider {
	p := Provider{ID: "codex", Name: "Codex", Source: "cli", CapturedAt: now()}
	bin := codexBinary()
	if bin == "" {
		p.failWith("not-installed", "cli-missing", "codex")
		return p
	}
	res, err := codexAskRateLimits(bin)
	if err != nil {
		// Never err.Error() from exec: those quote the full path being run,
		// which carries the local username onto every watcher's screen.
		switch err {
		case errCodexTimeout:
			p.failWith("unreachable", "cli-timeout", "codex app-server")
		case errCodexRefused:
			p.failWith("cli-failed", "cli-exit", "codex app-server")
		case errCodexProtocol:
			p.failWith("cli-failed", "cli-shape", "codex app-server")
		default:
			p.failWith("cli-failed", "cli-failed", "codex app-server")
		}
		return p
	}

	// The first ref is the account's own bucket by construction, and it is the
	// only one the plan and the credit balance may come from: every other
	// bucket is a window on some other meter and carries nulls for both.
	for i, ref := range codexOrderedBuckets(res) {
		p.Limits = append(p.Limits, codexBucketLimits(ref, i == 0)...)
		if i > 0 {
			continue
		}
		if plan := asStr(ref.b.PlanType, 40); plan != "" {
			p.PlanType = sp(plan)
		}
		if c := ref.b.Credits; c != nil {
			p.Credits = &Credits{
				HasCredits: asBool(c.HasCredits),
				Unlimited:  asBool(c.Unlimited),
				Balance:    anyToString(c.Balance),
			}
		}
	}
	if len(p.Limits) == 0 {
		// An answer with no usable window is not a reading. Saying ok here
		// would leave the page showing its previous gauge, freshly stamped
		// live — worse than showing nothing, and worse than the log.
		p.failWith("no-readings", "no-window")
		return p
	}
	p.OK = true
	// Asked and answered in the same breath, so the reading is as of now. The
	// cache floor keeps CapturedAt moving while this stands still, which is
	// what makes a served-from-cache number visibly age on the card.
	p.RecordedAt = fp(p.CapturedAt)
	return p
}

// Prefer the live reading; fall back to the session log.
//
// The fallback is deliberately silent about WHY it fell back, and that is not
// the same as hiding it. What the reader needs is which leg produced the number
// in front of them, and that is on the card already: `source` prints as "via
// CLI" or "local log", and a log reading that has stopped moving wears its own
// age next to it. A machine with no Codex installed would otherwise gain a
// permanent red error for a feature it never had.
//
// "Live wins" is a claim about FRESHNESS, so it has to be checked rather than
// assumed. The cache keeps serving its last success for a while after refreshes
// start failing (provhttp.go), which is right — a stale real number beats an
// error — but it means an `ok` reading here can be minutes old while a session
// written since then sits unread on disk. So a live answer that is no longer
// fresh is compared against the log rather than trusted on its rung alone, and
// whichever was recorded later is the one that goes out.
func collectCodex() Provider {
	live := codexLiveCache.collect(codexLiveFetch)
	if !live.OK {
		return collectCodexLog()
	}
	if live.RecordedAt == nil || live.CapturedAt-*live.RecordedAt < provMinInterval {
		return live
	}
	logged := collectCodexLog()
	if logged.OK && logged.RecordedAt != nil && *logged.RecordedAt > *live.RecordedAt {
		return logged
	}
	return live
}
