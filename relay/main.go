package main

// subnsub-monitor-relay — the relay, for people who would rather run it
// themselves.
//
// The helper's `connect` takes any URL, and the installer takes MON_RELAY;
// this program is the other end of that promise. Point machines here instead
// of at the hosted relay and every reading, every console line, and every
// name stays on hardware you chose. Nothing in this process phones anywhere:
// it accepts pushes, remembers the latest one per machine, and serves a
// dashboard that watches them.
//
// The helper-facing half speaks exactly the hosted relay's protocol —
// POST /push, GET /commands, POST /result — so the shipped helper works
// unmodified, old versions included. The dashboard half is this program's
// own: an event stream and a small command endpoint, because a single Go
// binary has no Durable Objects and needs none.
//
// One token, presented as a Bearer header, covers pushing and watching —
// the same "whoever holds it can push readings and read them" contract the
// helper's own `token` command describes. Splitting the two is one flag away
// (-op-token) and worth it on a fleet: a machine that leaks its token then
// leaks pushing, not watching and not the console.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const relayUsage = `subnsub-monitor-relay — self-hosted relay + dashboard for subnsub-monitor

  subnsub-monitor-relay -token TOKEN [options]

options:
  -token TOKEN      the bearer secret machines push with (or MON_RELAY_TOKEN)
  -op-token TOKEN   a separate secret for the dashboard (or MON_RELAY_OP_TOKEN);
                    unset, the push token also watches
  -listen ADDR      address to bind (default 127.0.0.1:8788)
  -state PATH       file to persist names and last readings across restarts
  -dist DIR         also serve the helper's installer and binaries from DIR at
                    /dl/ — build with go/build.sh, then go/stamp.sh writes the
                    matching installer beside them — so machines download from
                    this relay and nowhere else

Mint a token with 'subnsub-monitor token' (any 24-128 chars of A-Za-z0-9_- do).
Then, on each machine to watch:

  curl -fsSL https://tools.subnsub.com/monitor/install.sh \
    | MON_RELAY=https://relay.example.org sh -s -- TOKEN

(The assignment rides on sh, not curl: a VAR=value prefix reaches only the
first command of a pipeline, and it is the installer that reads MON_RELAY.)

or, by hand:  subnsub-monitor connect https://relay.example.org TOKEN

The default bind is loopback on purpose. Put a TLS reverse proxy (caddy,
nginx) in front and point -listen at it, or bind a public address explicitly
once TLS is someone's job — a bearer token on plain HTTP across a network is
readable by that network.
`

// Same alphabet and bounds the hosted relay enforces, so a token that works
// here works there and the other way round.
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{24,128}$`)

// Request body ceilings, the hosted relay's numbers — and they have to STAY
// the hosted relay's, because a helper that pushes here must behave identically
// against both. This one drifted: the hosted MAX_BODY went to 16 KiB when the
// collector count grew, and 8 KiB here meant a fully-populated machine got a
// 413 and simply stopped appearing on its own dashboard, with the reason
// visible only in a relay log nobody was reading. A reading now carries a
// per-second sample series as well (four lanes, up to 72 slots), which is
// another kilobyte or two on top of fourteen providers.
//
// AND THEN IT DRIFTED AGAIN, the same way, which is the argument for a test
// rather than a comment: the interface list (2026-08-06) can add 8 KiB at its
// own caps — sixteen interfaces, eight addresses each, an IPv6 literal is 45
// characters — and the hosted relay went to 32 KiB for exactly that reason
// while this one stayed at 16. A crowded container host was one valid report
// away from a 413 here and fine there. Matched again, and
// TestAFullFrameFitsTheBodyCeiling now builds its fixture with every list at
// its cap so the next round cannot repeat this quietly.
const (
	maxPushBody   = 32 * 1024
	maxResultBody = 24 * 1024
	maxOpBody     = 8 * 1024
)

type auth struct {
	// SHA-256 of the accepted tokens, compared in constant time. Hashing
	// first means the comparison cost does not depend on where two strings
	// diverge, and the plaintext is not sitting in a long-lived struct.
	machine [32]byte
	op      [32]byte
}

func newAuth(machineTok, opTok string) auth {
	return auth{machine: sha256.Sum256([]byte(machineTok)), op: sha256.Sum256([]byte(opTok))}
}

// newRelayHub builds the hub the way the binary does — state, plus the one
// derived fact hello tells the page: whether the dashboard secret differs
// from the machine one. Split out of main so tests exercise the same
// derivation the binary ships, not a hand-set field: this bit decides
// whether a dashboard may prefill its own token into an install command,
// and a test that sets it by hand would keep passing with the derivation
// deleted or inverted. Callers pass opTok already resolved (empty means
// "same as machine", and main resolves that before calling).
func newRelayHub(statePath, machineTok, opTok string) *Hub {
	h := newHub(statePath)
	h.tokenSplit = opTok != machineTok
	return h
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}

func (a auth) allow(r *http.Request, want [32]byte) bool {
	t := bearer(r)
	if !tokenRe.MatchString(t) {
		return false
	}
	got := sha256.Sum256([]byte(t))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func main() {
	var (
		tokenFlag = flag.String("token", "", "")
		opFlag    = flag.String("op-token", "", "")
		listen    = flag.String("listen", "127.0.0.1:8788", "")
		statePath = flag.String("state", "", "")
		distDir   = flag.String("dist", "", "")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, relayUsage) }
	flag.Parse()

	machineTok := *tokenFlag
	if machineTok == "" {
		machineTok = os.Getenv("MON_RELAY_TOKEN")
	}
	opTok := *opFlag
	if opTok == "" {
		opTok = os.Getenv("MON_RELAY_OP_TOKEN")
	}
	if machineTok == "" {
		fmt.Fprint(os.Stderr, relayUsage)
		fmt.Fprintln(os.Stderr, "error: a -token is required; this relay does not run open")
		os.Exit(2)
	}
	if !tokenRe.MatchString(machineTok) {
		fmt.Fprintln(os.Stderr, "error: token must be 24-128 characters of A-Za-z0-9_-")
		os.Exit(2)
	}
	if opTok == "" {
		opTok = machineTok
	} else if !tokenRe.MatchString(opTok) {
		fmt.Fprintln(os.Stderr, "error: op-token must be 24-128 characters of A-Za-z0-9_-")
		os.Exit(2)
	}

	var distRoot *os.Root
	if *distDir != "" {
		// Opened once, now, loudly — and this handle is the directory /dl
		// serves for the life of the process. Re-resolving the path per
		// request would follow whatever the path's components have become
		// since; a symlink swapped into it after startup would quietly point
		// every later download at a different directory than the one that
		// was validated here. (os.Root is documented safe for concurrent
		// use.) A mistyped directory failing as silent 404s would read as
		// "the download link is broken" on some other machine, hours later.
		r, err := os.OpenRoot(*distDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: -dist %s is not an openable directory: %v\n", *distDir, err)
			os.Exit(2)
		}
		distRoot = r
	}

	hub := newRelayHub(*statePath, machineTok, opTok)
	az := newAuth(machineTok, opTok)
	mux := routes(hub, az, distRoot)

	// Timeouts shaped like the helper's own listener: header and body reads
	// bounded, no global write deadline — /api/stream and a held /commands
	// poll are long-lived writes by design, and each handler bounds itself.
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go hub.maintain()
	// Flush on the way out, so a restart does not cost the names and the
	// roster. SIGINT for a terminal, SIGTERM for a service manager.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		hub.saveState()
		os.Exit(0)
	}()

	fmt.Fprintf(os.Stderr, "subnsub-monitor-relay listening on http://%s\n", *listen)
	if strings.HasPrefix(*listen, "127.0.0.1") || strings.HasPrefix(*listen, "localhost") {
		fmt.Fprintf(os.Stderr, "loopback only — put a TLS proxy in front for machines elsewhere\n")
	}
	if *distDir != "" {
		fmt.Fprintf(os.Stderr, "serving helper downloads from %s at /dl/\n", *distDir)
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "listener stopped: %v\n", err)
		hub.saveState()
		os.Exit(1)
	}
}

// The exact names /dl may serve — the whitelist IS the access model, and a
// name either matches an entry here or is not served, which is what makes the
// route safe to leave unauthenticated: nothing an operator drops in the
// directory by accident can reach the network, and a traversal has no name to
// arrive at. The binary names are a protocol, not a convention — the helper's
// self-update and the installer both spell them from GOOS-GOARCH — so this
// list changes only when the published target list does.
var distFiles = map[string]string{
	"install.sh":                    "text/plain; charset=utf-8",
	"SHA256SUMS":                    "text/plain; charset=utf-8",
	"VERSION":                       "text/plain; charset=utf-8",
	"subnsub-monitor-linux-amd64":   "application/octet-stream",
	"subnsub-monitor-linux-arm":     "application/octet-stream",
	"subnsub-monitor-linux-arm64":   "application/octet-stream",
	"subnsub-monitor-darwin-amd64":  "application/octet-stream",
	"subnsub-monitor-darwin-arm64":  "application/octet-stream",
	"subnsub-monitor-freebsd-amd64": "application/octet-stream",
	"subnsub-monitor-freebsd-arm64": "application/octet-stream",
}

// Every route this relay answers, in one place so the tests drive the same
// mux the binary serves rather than a reconstruction of it. dist is nil when
// -dist was not given, and the /dl route simply does not exist.
func routes(hub *Hub, az auth, dist *os.Root) *http.ServeMux {
	mux := http.NewServeMux()

	// ── the helper's three legs, wire-compatible with the hosted relay ─────

	mux.HandleFunc("/push", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if !az.allow(r, az.machine) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		body, ok := readBounded(w, r, maxPushBody)
		if !ok {
			return
		}
		st, poll := hub.push(body)
		switch st {
		case pushOK:
			// One bit rides back on the request the helper already made:
			// whether /commands has (or may soon have) something for it. Read
			// only by helpers that take instructions; everyone older ignores
			// the body exactly as they ignored "ok".
			writeJSON(w, map[string]any{"ok": true, "poll": poll})
		case pushFull:
			// Said plainly, and once per push: a silent cap reads as a broken
			// helper on the machine that hit it.
			http.Error(w, "machine limit reached on this relay", http.StatusForbidden)
		default:
			http.Error(w, "bad reading", http.StatusBadRequest)
		}
	})

	mux.HandleFunc("/commands", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if !az.allow(r, az.machine) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		agent := r.URL.Query().Get("agent")
		if cleanAgentID(agent) == "" {
			http.Error(w, "bad agent", http.StatusBadRequest)
			return
		}
		ans := hub.commands(agent, r.Context().Done())
		writeJSON(w, ans)
	})

	mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if !az.allow(r, az.machine) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		body, ok := readBounded(w, r, maxResultBody)
		if !ok {
			return
		}
		// Accepted whether or not it matched an issued command — the command
		// already ran on the machine, and a status code cannot change that.
		// What a stray or replayed id cannot do is reach the page.
		hub.result(body)
		io.WriteString(w, "ok")
	})

	// ── the dashboard's two, this relay's own ──────────────────────────────

	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if !az.allow(r, az.op) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		serveStream(hub, w, r)
	})

	mux.HandleFunc("/api/op", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if !az.allow(r, az.op) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		body, ok := readBounded(w, r, maxOpBody)
		if !ok {
			return
		}
		serveOp(hub, w, body)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveDashboard(w, r)
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "monitor-relay ok")
	})

	// ── optional: hand out the helper itself ───────────────────────────────
	//
	// With -dist, the install command this relay's dashboard generates points
	// machines HERE for the installer and the binaries, so a deployment built
	// from source touches no host but its own. Unauthenticated on purpose:
	// the installer is fetched by `curl | sh` with no way to carry a header,
	// and everything nameable here is public release material — the same
	// bytes as the repository this program came from. Integrity does not
	// hang on this route either way: install.sh carries the expected SHA-256
	// of every binary inline, which is also why the directory must hold an
	// installer stamped for the same source it was built from.
	if dist != nil {
		mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			name := strings.TrimPrefix(r.URL.Path, "/dl/")
			ct, ok := distFiles[name]
			if !ok {
				http.NotFound(w, r)
				return
			}
			// Lstat, never Stat: a SYMLINK wearing a listed name must read
			// as the symlink it is — followed, it would lend this route's
			// unauthenticated reach to whatever it points at — and a FIFO
			// must be refused without being opened, because opening one
			// blocks until a writer appears. Anything but a plain file is a
			// misconfiguration, answered like an absent one.
			st, err := dist.Lstat(name)
			if err != nil || !st.Mode().IsRegular() {
				http.NotFound(w, r)
				return
			}
			// distOpenExtra is O_NOFOLLOW|O_NONBLOCK where the platform has
			// them (dist_unix.go): the open itself then refuses a symlink
			// raced in since the Lstat, and cannot block on a raced-in FIFO
			// — a nonblocking open of one succeeds and is caught by the
			// fstat below. Elsewhere the flags are zero and the SameFile
			// check is the (weaker) guard for the same race. Either way the
			// Root bounds resolution to the directory; what no userspace
			// check can see is a bind mount or a hard link wearing a listed
			// name, which is why the directory itself must be trusted.
			f, err := dist.OpenFile(name, os.O_RDONLY|distOpenExtra, 0)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			fst, err := f.Stat()
			if err != nil || !fst.Mode().IsRegular() || !os.SameFile(st, fst) {
				http.NotFound(w, r)
				return
			}
			// This server has no global write timeout — /api/stream and the
			// held poll are long-lived writes by design — so an
			// unauthenticated multi-megabyte download must bound itself: a
			// reader drawing one byte a minute would otherwise hold the
			// goroutine and two descriptors for as long as it pleased.
			// Generous, because the slow it exists to stop is adversarial
			// slow, not rural-link slow.
			rc := http.NewResponseController(w)
			rc.SetWriteDeadline(time.Now().Add(10 * time.Minute))
			w.Header().Set("Content-Type", ct)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "no-store")
			// The marker the dashboard's probe looks for: a reverse proxy
			// that answers every path with a friendly 200 must not be read
			// as "this relay serves binaries".
			w.Header().Set("X-Mon-Dist", "1")
			http.ServeContent(w, r, name, fst.ModTime(), f)
		})
	}

	return mux
}

// readBounded reads a request body up to max, answering 413 past it. The
// Content-Length fast path refuses an honest oversize before reading it; the
// LimitReader catches the chunked liar.
func readBounded(w http.ResponseWriter, r *http.Request, max int64) ([]byte, bool) {
	if r.ContentLength > max {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return nil, false
	}
	if int64(len(body)) > max {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// serveStream is the dashboard's live feed: hello, then every frame the hub
// broadcasts, with a comment ping often enough that idle proxies keep the
// connection. Same per-write deadline discipline as the helper's own /events.
func serveStream(hub *Hub, w http.ResponseWriter, r *http.Request) {
	ch, hello, ok := hub.attach()
	if !ok {
		http.Error(w, "too many watchers", http.StatusServiceUnavailable)
		return
	}
	defer hub.detach(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	send := func(b []byte) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send(hello) {
		return
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case b, open := <-ch:
			if !open || !send(b) {
				return
			}
		case <-ping.C:
			if err := rc.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
				return
			}
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// serveOp is the dashboard's command surface — the hosted relay's WebSocket
// control frames, reshaped as one POST per action so a plain fetch drives it.
func serveOp(hub *Hub, w http.ResponseWriter, body []byte) {
	var op struct {
		T     string `json:"t"`
		Agent string `json:"agent"`
		ID    string `json:"id"`
		Cmd   string `json:"cmd"`
		Sig   string `json:"sig"`
		At    int64  `json:"at"`
		Name  string `json:"name"`
		Open  bool   `json:"open"`
	}
	if err := json.Unmarshal(body, &op); err != nil {
		http.Error(w, "bad op", http.StatusBadRequest)
		return
	}
	if cleanAgentID(op.Agent) == "" && op.Agent != legacyAgent {
		http.Error(w, "bad agent", http.StatusBadRequest)
		return
	}
	switch op.T {
	case "exec", "update":
		if op.ID == "" || len(op.ID) > 40 || !agentIDRe.MatchString(op.ID) {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		// Shape only. This relay cannot check a signature — it holds no key,
		// which is the point — but it can refuse to store something that is
		// not one: these two ride in memory and back out to a helper.
		sig, issuedAt := "", int64(0)
		if sigRe.MatchString(op.Sig) && op.At >= int64(epochMin) && op.At <= int64(epochMax) {
			sig, issuedAt = op.Sig, op.At
		}
		var e enqueueErr
		if op.T == "exec" {
			e = hub.enqueueExec(op.Agent, op.ID, op.Cmd, sig, issuedAt)
		} else {
			e = hub.enqueueUpdate(op.Agent, op.ID, sig, issuedAt)
		}
		if e != enqOK {
			writeJSON(w, map[string]string{"error": string(e)})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case "name":
		if !hub.setName(op.Agent, op.Name) {
			writeJSON(w, map[string]string{"error": string(enqNoMachine)})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case "term":
		hub.setTerm(op.Agent, op.Open)
		writeJSON(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "unknown op", http.StatusBadRequest)
	}
}
