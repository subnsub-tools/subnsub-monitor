package transport

// An end-to-end pass over a real DERP: start the carrier, dial it with a
// tailcat client holding an allowed key, fetch through the gate, and confirm
// a key that is NOT allowed is refused at the application layer even after
// it was admitted at the WireGuard layer — the revocation path the plan
// depends on.
//
// Opt-in (MON_E2E=1): it needs the network and the public tailcat.dev DERP,
// and `go test ./...` must stay hermetic. Run it by hand before a release:
//
//	MON_E2E=1 go test ./transport/ -run E2E -v

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func TestE2ETunnelGateAndRevocation(t *testing.T) {
	if os.Getenv("MON_E2E") == "" {
		t.Skip("set MON_E2E=1 to run against a real DERP")
	}
	good := key.NewNode()
	other := key.NewNode()
	// The allowlist the gate consults; mutable so the test can revoke.
	allowed := []key.NodePublic{good.Public(), other.Public()}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k, ok := PeerFromContext(r.Context())
		if !ok {
			http.Error(w, "no peer", 500)
			return
		}
		w.Write([]byte("hello " + k.String()))
	})
	// The library's own chatter, for proof the carrier is UDP-first:
	// magicsock says "disabled udp4 per TS_DEBUG_ALWAYS_USE_DERP" when it
	// is pinned to DERP, and "endpoints changed: …" the moment a bound UDP
	// socket finds an endpoint (a STUN answer, an interface address) —
	// the list a direct path is built from.
	var logMu sync.Mutex
	var logLines []string
	logf := func(format string, args ...any) {
		logMu.Lock()
		logLines = append(logLines, fmt.Sprintf(format, args...))
		logMu.Unlock()
	}
	tc, err := StartTailcat(TailcatConfig{
		StateDir: t.TempDir() + "/state",
		Port:     7770,
		Allowed:  func() []key.NodePublic { return allowed },
		Handler:  h,
		Logf:     logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	t.Logf("address %s region %d by name %v", tc.ConnBlob()[:12]+"…", tc.Region(), tc.ByName())

	dial := func(priv key.NodePrivate) (*http.Client, *tailcat.Client) {
		cl := &tailcat.Client{Server: tailcat.ConnBlob(tc.ConnBlob()), Key: priv, Logf: logger.Discard}
		return &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return cl.DialTCPPort(ctx, 7770)
				},
			},
		}, cl
	}
	get := func(c *http.Client) (int, string) {
		resp, err := c.Get("http://x:7770/v1/hello")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, string(b)
	}

	hc, cl := dial(good)
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if _, err := cl.Ping(ctx); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("allowed client never registered")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if st, body := get(hc); st != 200 || !strings.Contains(body, good.Public().String()) {
		t.Fatalf("allowed peer: %d %q", st, body)
	}

	// UDP first: a native peer leaves DERP for a direct path. Both ends
	// share this host, so each one's interface addresses reach the other.
	dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dcancel()
	for {
		// Each ping gets its own short bound: when every probe is lost,
		// magicsock never answers and only the context ends the wait.
		pctx, pcancel := context.WithTimeout(dctx, 5*time.Second)
		r, err := cl.DiscoPing(pctx)
		pcancel()
		if err == nil && r.Endpoint != "" {
			t.Logf("direct path: %s", r.Endpoint)
			break
		}
		if dctx.Err() != nil {
			t.Fatal("a native peer never left DERP for a direct UDP path")
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Revoke `other` at the file level: it was admitted at Start, so the
	// WireGuard handshake still succeeds — and the gate must still refuse.
	hc2, cl2 := dial(other)
	defer cl2.Close()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	for {
		if _, err := cl2.Ping(ctx2); err == nil {
			break
		}
		if ctx2.Err() != nil {
			t.Fatal("second client never registered")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if st, _ := get(hc2); st != 200 {
		t.Fatalf("second peer before revocation: %d", st)
	}
	allowed = []key.NodePublic{good.Public()}
	if st, body := get(hc2); st != 403 || !strings.Contains(body, "not-allowed") {
		t.Fatalf("revoked peer: %d %q", st, body)
	}
	if st, _ := get(hc); st != 200 {
		t.Fatalf("revoking one peer broke another: %d", st)
	}

	// And a key never on the list never gets a tunnel at all.
	stranger := key.NewNode()
	_, cl3 := dial(stranger)
	defer cl3.Close()
	ctx3, cancel3 := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel3()
	if _, err := cl3.Ping(ctx3); err == nil {
		t.Fatal("a stranger's meow was answered")
	}

	// Two peers handshaked and were served, so the carrier has had every
	// occasion to scan for endpoints: it must have bound UDP and found at
	// least one (every test host has an interface address).
	logMu.Lock()
	defer logMu.Unlock()
	found := false
	for _, l := range logLines {
		if strings.Contains(l, "per TS_DEBUG_ALWAYS_USE_DERP") {
			t.Errorf("the carrier is pinned to DERP: %q", l)
		}
		if strings.Contains(l, "endpoints changed:") {
			found = true
		}
	}
	if !found {
		t.Error("magicsock never found a UDP endpoint: the carrier has no direct path to offer")
	}
}
