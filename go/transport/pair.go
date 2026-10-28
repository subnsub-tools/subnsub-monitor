package transport

// The pairing node: a second, short-lived tailcat node whose identity the
// watching browser minted and put in the install line (--pair), so that the
// browser knows where it waits before the machine exists. The browser dials
// it with the key the same line allowed on the machine's own node, asks once
// (GET /v1/pair) and is told the machine's real address; from then on it
// dials that, like any machine. The machine's own node key, pre-shared key
// and region stay where they always were — made here, kept here — so a line
// in a shell history, a clipboard or a chat log is worth something only until
// the pairing is over, and is worth nothing about the machine's own identity
// at any time.
//
// What the pairing node is NOT: a second way in. It admits exactly one key,
// serves exactly one frame, holds no handler of the machine's, and the caller
// shuts it as soon as that browser has reached the machine's own node (or the
// pairing ran out).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// PairIdentity is a pairing node's identity, decoded from an install line's
// --pair token.
type PairIdentity struct {
	Key    key.NodePrivate
	PSK    tailcat.PresharedKey
	Region *tailcfg.DERPRegion
	Addr   string
}

// The token's longest acceptable form: 64 hex characters, a dot and an
// address with one region embedded. The page's addresses run to a few
// hundred characters; 2048 is the page's own ceiling for an address.
const maxPairToken = 64 + 1 + 2 + 2048

// ParsePairToken decodes what an install line carries after --pair:
//
//	<64 hex: the pairing node's private key>.<its tailcat address>
//
// The address already names the node's public key, its disco key, the
// WireGuard pre-shared key and one DERP region with its relays; the private
// key is the one thing it cannot say. The two halves are checked against each
// other — a private key whose public half is not the address's is a line that
// was edited, and a node started on it would wait under a key nobody dials.
func ParsePairToken(s string) (PairIdentity, error) {
	var zero PairIdentity
	s = strings.TrimSpace(s)
	if len(s) > maxPairToken {
		return zero, errors.New("pairing token too long")
	}
	hexKey, addr, ok := strings.Cut(s, ".")
	if !ok {
		return zero, errors.New("pairing token has no address")
	}
	if len(hexKey) != 64 || strings.Trim(hexKey, "0123456789abcdef") != "" {
		return zero, errors.New("pairing token's key is not 64 lowercase hex characters")
	}
	var k key.NodePrivate
	if err := k.UnmarshalText([]byte("privkey:" + hexKey)); err != nil {
		return zero, fmt.Errorf("pairing token's key: %w", err)
	}
	if k.IsZero() {
		return zero, errors.New("pairing token's key is all zero")
	}
	if !strings.HasPrefix(addr, "tc") || len(addr) < 18 ||
		strings.Trim(addr[2:], "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
		return zero, errors.New("pairing token's address is not a tailcat address")
	}
	ci, err := tailcat.ParseAddr(tailcat.Addr(addr))
	if err != nil {
		return zero, fmt.Errorf("pairing token's address: %w", err)
	}
	if ci.ServerPublic.NodePublic != k.Public() {
		return zero, errors.New("pairing token's key does not match its address")
	}
	if !ci.ServerDiscoPublic.Equal(tailcat.DiscoPublicForNode(k)) {
		return zero, errors.New("pairing token's address carries the wrong path-discovery key")
	}
	if ci.PresharedKey.IsZero() {
		return zero, errors.New("pairing token's address carries no pre-shared key")
	}
	if len(ci.Region) != 1 || ci.Region[0] == nil || len(ci.Region[0].Nodes) == 0 {
		return zero, errors.New("pairing token's address names no relay")
	}
	for _, n := range ci.Region[0].Nodes {
		if n == nil || n.HostName == "" {
			return zero, errors.New("pairing token's address names a relay without a host name")
		}
	}
	return PairIdentity{Key: k, PSK: ci.PresharedKey, Region: ci.Region[0], Addr: addr}, nil
}

// PairConfig is what StartPairing needs.
type PairConfig struct {
	Identity PairIdentity
	// Peer is the one browser admitted: the key the same install line put on
	// the machine's allowlist.
	Peer key.NodePublic
	// Port is the TCP port served inside the tunnel — the machine's own, so
	// the browser's client needs no second constant.
	Port uint16
	// Answer is read at request time: the machine's id and its own address,
	// so a carrier rebuilt meanwhile is what the browser hears of.
	Answer func() (agentID, addr string)
	// OnServed is told each time the answer went out.
	OnServed func()
	// Allowed, when set, is asked on every request whether the browser is
	// still on the machine's allowlist: a key struck there (`disallow`) is
	// refused here too, from its next request (amp review 2026-10-28).
	Allowed func(key.NodePublic) bool
	Logf    logger.Logf
}

// Pairing is a running pairing node. Construct with StartPairing.
type Pairing struct {
	srv   *tailcat.Server
	ln    *memListener
	httpd *http.Server

	mu     sync.Mutex
	closed bool
}

// StartPairing brings the pairing node up in the region its address names.
// It returns once the node is waiting at its relay.
func StartPairing(cfg PairConfig) (*Pairing, error) {
	if cfg.Identity.Key.IsZero() || cfg.Identity.Region == nil {
		return nil, errors.New("transport: pairing needs an identity")
	}
	if cfg.Peer.IsZero() {
		return nil, errors.New("transport: pairing needs the browser's key")
	}
	if cfg.Port == 0 || cfg.Answer == nil {
		return nil, errors.New("transport: pairing needs a port and an answer")
	}
	logf := cfg.Logf
	if logf == nil {
		logf = logger.Discard
	}
	reg := cfg.Identity.Region.Clone()
	// The same rule as the machine's own node: where this machine's DNS puts
	// a relay's name somewhere else, dial the name.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	dialByName(ctx, reg, lookupIPv4, logf)
	cancel()

	ln := newMemListener(cfg.Port)
	port := cfg.Port
	srv := &tailcat.Server{
		Key:            cfg.Identity.Key,
		PresharedKey:   cfg.Identity.PSK,
		Logf:           logf,
		Region:         reg,
		AllowedClients: []key.NodePublic{cfg.Peer},
		ServedTCPPorts: []filter.PortRange{{First: port, Last: port}},
		OnTCP: func(p uint16) func(net.Conn) {
			if p != port {
				return nil
			}
			return ln.deliver
		},
	}
	if err := srv.Start(); err != nil {
		ln.Close()
		srv.Close()
		return nil, fmt.Errorf("tailcat: pairing: %w", err)
	}
	p := &Pairing{srv: srv, ln: ln}
	p.httpd = &http.Server{
		Handler:           pairHandler(cfg),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	go p.httpd.Serve(ln)
	return p, nil
}

// pairHandler serves the one frame, to the one key.
func pairHandler(cfg PairConfig) http.Handler {
	peer := addrForKey(cfg.Peer)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !pairPeerIs(r.RemoteAddr, peer) || (cfg.Allowed != nil && !cfg.Allowed(cfg.Peer)) {
			pairRefuse(w, http.StatusForbidden, "not-allowed")
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/v1/pair" {
			pairRefuse(w, http.StatusNotFound, "unknown-frame")
			return
		}
		id, addr := cfg.Answer()
		if addr == "" {
			// The machine's own node is between carriers: nothing to hand
			// over yet, and the browser asks again.
			pairRefuse(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		f, err := New(TypePair, id, r.Header.Get("X-Request-Id"), Pair{Addr: addr})
		var b []byte
		if err == nil {
			b, err = Encode(f)
		}
		if err != nil {
			pairRefuse(w, http.StatusInternalServerError, "unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
		if cfg.OnServed != nil {
			cfg.OnServed()
		}
	})
}

func pairPeerIs(remote string, want netip.Addr) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a == want
}

func pairRefuse(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if f, err := New(TypeError, "", "", Error{Code: code}); err == nil {
		if b, err := Encode(f); err == nil {
			w.Write(b)
		}
	}
}

// Close shuts the pairing node down. Idempotent.
func (p *Pairing) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p.httpd.Shutdown(ctx)
	p.ln.Close()
	return p.srv.Close()
}
