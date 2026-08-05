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
	"path/filepath"
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
                    /dl/ (build them with go/build.sh and copy install.sh in),
                    so machines download from this relay and nowhere else

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
const (
	maxPushBody   = 16 * 1024
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

	if *distDir != "" {
		// Checked now, loudly. A mistyped directory that turned into silent
		// 404s would read as "the download link is broken" on some other
		// machine, hours later, with the cause long scrolled away.
		if st, err := os.Stat(*distDir); err != nil || !st.IsDir() {
			fmt.Fprintf(os.Stderr, "error: -dist %s is not a readable directory\n", *distDir)
			os.Exit(2)
		}
	}

	hub := newHub(*statePath)
	// Whether the dashboard secret differs from the machine one. The page
	// needs to know because its add-machine panel prefills the token the
	// viewer signed in with — which is exactly right when there is one token,
	// and would hand the DASHBOARD secret to a monitored box when there are
	// two. Written once, before anything serves.
	hub.tokenSplit = opTok != machineTok
	az := newAuth(machineTok, opTok)
	mux := routes(hub, az, *distDir)

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
// mux the binary serves rather than a reconstruction of it.
func routes(hub *Hub, az auth, distDir string) *http.ServeMux {
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
		switch hub.push(body) {
		case pushOK:
			io.WriteString(w, "ok")
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
	if distDir != "" {
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
			p := filepath.Join(distDir, name)
			// ServeFile answers a directory with a listing; a directory
			// wearing a whitelisted name is a misconfiguration, not a file.
			if st, err := os.Stat(p); err != nil || st.IsDir() {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFile(w, r, p)
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
		var e enqueueErr
		if op.T == "exec" {
			e = hub.enqueueExec(op.Agent, op.ID, op.Cmd)
		} else {
			e = hub.enqueueUpdate(op.Agent, op.ID)
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
