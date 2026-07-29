package main

// Two ways to get readings out: a local listener for the demo page, and the
// outbound push that makes watching a remote machine possible.

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Pages allowed to read from the local listener. A wide-open localhost port
// would let ANY page you happen to visit read your plan and quota.
var allowedOrigins = map[string]bool{
	"http://localhost:8124":     true,
	"http://127.0.0.1:8124":     true,
	"http://localhost:8125":     true,
	"http://127.0.0.1:8125":     true,
	"https://www.200000.live":   true,
	"https://200000.live":       true,
	"https://tools.subnsub.com": true,
	"https://tool.subnsub.com":  true,
}

// Concurrent /events streams, and why /events is stricter than /quota: a
// request with no Origin cannot READ the response cross-origin, but it can
// still hold a stream open, and enough of them starve the real page. /quota
// stays open to curl for debugging — it answers and hangs up.
const maxStreams = 8

var streams struct {
	sync.Mutex
	n int
}

func serve(port int) {
	mux := http.NewServeMux()

	mux.HandleFunc("/quota", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !allowedOrigins[origin] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		cors(w, origin)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(dump(collectAll()))
	})

	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !allowedOrigins[origin] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		streams.Lock()
		if streams.n >= maxStreams {
			streams.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		streams.n++
		streams.Unlock()
		defer func() { streams.Lock(); streams.n--; streams.Unlock() }()

		cors(w, origin)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)

		/* A client that stops reading would otherwise block Fprintf forever,
		   holding a stream slot for good. A blanket WriteTimeout on the server
		   cannot express this — it would also kill healthy long-lived streams —
		   so the deadline is refreshed per write instead. */
		rc := http.NewResponseController(w)
		for {
			if err := rc.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", dump(collectAll())); err != nil {
				return // client went away or stopped reading
			}
			if err := rc.Flush(); err != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	})

	// 127.0.0.1, never 0.0.0.0 — this must not be reachable from the network.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	warnf("subnsub-monitor serving on http://%s  (/quota, /events)", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// No WriteTimeout: /events is a long-lived stream and a global write
		// deadline would cut it off. That handler sets its own per-write
		// deadline instead.
		IdleTimeout: 60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		warnf("listener stopped: %v", err)
	}
}

func cors(w http.ResponseWriter, origin string) {
	if allowedOrigins[origin] {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
}

// Bounds on the push interval. The ceiling is the relay's own offline
// threshold minus a margin: push slower and a healthy helper reads as silent
// between pushes. Zero would make every sleep — including the backoff — a
// no-op and turn the retry loop into a hot loop against the relay.
const (
	minInterval = 5.0
	maxInterval = 60.0
)

// Dial out and push readings, forever.
//
// This is the shape that matters: the machine you want to watch is usually one
// you cannot reach — behind NAT, behind a cloud firewall, on a laptop that
// moves. Nothing here listens; it only makes outbound HTTPS, which is the one
// thing that works everywhere.
func connect(base, token string, every float64) {
	every = clampInterval(every)
	url := strings.TrimRight(base, "/") + "/push"
	client := &http.Client{
		Timeout: 15 * time.Second,
		// Refuse redirects here too. The relay token is a bearer secret — it
		// can push readings and read them — and Go's default client keeps
		// Authorization across a redirect to the same domain or a subdomain,
		// judging by host alone without checking the scheme. So an https→http
		// hop on the same host would put the token on the wire in clear. The
		// Claude collector already refuses; this path is no different.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	fails := 0

	for {
		req, err := http.NewRequest("POST", url, bytes.NewReader(dump(collectAll())))
		if err != nil {
			warnf("cannot build request")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		// Bearer, not a query parameter: a token in the URL ends up in the
		// relay's request logs and every proxy in between.
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", "subnsub-monitor")

		wait := every
		resp, err := client.Do(req)
		switch {
		case err != nil:
			fails++
			warnf("push failed")
			wait = every * math.Min(math.Pow(2, float64(fails)), 10)
		case resp.StatusCode == 200:
			resp.Body.Close()
			if fails > 0 {
				warnf("reconnected")
			}
			fails = 0
		case resp.StatusCode == 429:
			// The relay is pacing the ROOM, not rejecting us: several machines
			// share one, and two that started together collide.
			//
			// Backing off by whole multiples of a fixed cadence is exactly the
			// wrong answer here — the phase between two helpers never changes,
			// so the pair that collided once collides on every subsequent
			// round, forever. A short RANDOM wait is what breaks the lock:
			// after it, the two are no longer in step.
			resp.Body.Close()
			warnf("relay is pacing us; retrying shortly")
			wait = 2 + 3*jitter()
		default:
			// 4xx means this client is wrong (bad token, room not enabled) and
			// retrying at speed won't fix it. Back off rather than hammer.
			code := resp.StatusCode
			resp.Body.Close()
			fails++
			warnf("push rejected: %d", code)
			wait = every * math.Min(math.Pow(2, float64(fails)), 10)
		}
		// Spread the steady-state cadence too, by a few percent. Machines
		// installed in one sitting otherwise stay in lockstep for as long as
		// they run, and the collision that costs one of them a push is the same
		// collision every time.
		if wait == every {
			wait = every * (0.95 + 0.1*jitter())
		}
		time.Sleep(time.Duration(wait * float64(time.Second)))
	}
}

// A float in [0,1), from the system CSPRNG.
//
// crypto/rand rather than math/rand because this binary already imports it for
// the token and the machine id, and pulling in a second generator to seed for
// scheduling noise buys nothing. A failure falls back to no jitter, which is
// the pre-jitter behaviour rather than a hang.
func jitter() float64 {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return float64(uint16(b[0])<<8|uint16(b[1])) / 65536.0
}

// A usable number of seconds, whatever was passed in.
//
// math.Min/Max propagate NaN rather than rejecting it, and a NaN or negative
// duration makes time.Sleep return immediately — turning the push loop into a
// hot loop against the relay. Anything that is not a finite positive number
// falls back to the default.
func clampInterval(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 30
	}
	if v < minInterval {
		return minInterval
	}
	if v > maxInterval {
		return maxInterval
	}
	return v
}
