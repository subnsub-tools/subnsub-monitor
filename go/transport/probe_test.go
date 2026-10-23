package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// ProbeRelay dials the region as the tunnel's own DERP client does: a pinned
// certificate passes, a wrong pin or an untrusted certificate does not, a
// relay that is not there does not, a node that does not answer gives way
// to the next one, and a region with nothing to dial says so.
func TestProbeRelay(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	sum := sha256.Sum256(srv.Certificate().Raw)
	pin := "sha256-raw:" + hex.EncodeToString(sum[:])
	node := func(name, cert string) *tailcfg.DERPNode {
		return &tailcfg.DERPNode{Name: name, RegionID: 900, HostName: host, IPv4: host, IPv6: "none", DERPPort: p, CertName: cert}
	}
	probe := func(nodes ...*tailcfg.DERPNode) error {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		tc := &Tailcat{reg: &tailcfg.DERPRegion{RegionID: 900, RegionCode: "t", Nodes: nodes}}
		return tc.ProbeRelay(ctx)
	}
	if err := probe(node("900a", pin)); err != nil {
		t.Fatalf("the pinned relay did not answer: %v", err)
	}
	if err := probe(node("900a", "sha256-raw:"+hex.EncodeToString(make([]byte, 32)))); err == nil {
		t.Fatalf("a wrong pin passed")
	}
	if err := probe(node("900a", "")); err == nil {
		t.Fatalf("an untrusted certificate passed")
	}
	stun := node("900s", pin)
	stun.STUNOnly = true
	if err := probe(stun, node("900a", pin)); err != nil {
		t.Fatalf("a STUN-only node was not skipped: %v", err)
	}
	if err := probe(stun); err == nil {
		t.Fatalf("a region with only a STUN node passed")
	}
	// A first node that never answers (TEST-NET-1) does not take the time
	// the next one needs.
	dark := &tailcfg.DERPNode{Name: "900d", RegionID: 900, HostName: "192.0.2.1", IPv4: "192.0.2.1", IPv6: "none", DERPPort: 443}
	if err := probe(dark, node("900a", pin)); err != nil {
		t.Fatalf("a dark first node starved the next: %v", err)
	}
	if err := (&Tailcat{}).ProbeRelay(context.Background()); err == nil {
		t.Fatalf("a carrier with no region passed")
	}
	srv.Close()
	if err := probe(node("900a", pin)); err == nil {
		t.Fatalf("a relay that is gone passed")
	}
}

// PeerRx answers nothing for a carrier that is not running.
func TestPeerRxWithoutACarrier(t *testing.T) {
	var none *Tailcat
	if _, ok := none.PeerRx(key.NewNode().Public()); ok {
		t.Fatalf("a nil carrier knows a peer")
	}
	if _, ok := (&Tailcat{}).PeerRx(key.NewNode().Public()); ok {
		t.Fatalf("a carrier with no server knows a peer")
	}
}
