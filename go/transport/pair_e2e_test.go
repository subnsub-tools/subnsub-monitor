package transport

// The pairing node over a real DERP: an identity made the page's way (a
// region embedded in the address, its relay found in the public map), the
// node started from the token, the browser's key dialling the address and
// asking for the machine's — and a key the line did not name never getting a
// tunnel at all.
//
// Opt-in like the carrier's own end-to-end pass (MON_E2E=1): it needs the
// network and the public tailcat.dev DERP.
//
//	MON_E2E=1 go test ./transport/ -run E2EPairing -v

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func TestE2EPairing(t *testing.T) {
	if os.Getenv("MON_E2E") == "" {
		t.Skip("set MON_E2E=1 to run against a real DERP")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dm, err := tailcat.FetchDERPMap(ctx, tailcat.ExpandForServer)
	if err != nil {
		t.Fatal(err)
	}
	var reg *tailcfg.DERPRegion
	for _, id := range slices.Sorted(maps.Keys(dm.Regions)) {
		if r := dm.Regions[id]; r != nil && len(r.Nodes) > 0 {
			reg = r.Clone()
			break
		}
	}
	if reg == nil {
		t.Fatal("the public map has no region")
	}
	reg.Nodes = reg.Nodes[:min(2, len(reg.Nodes))]
	pk := tailcat.NewPrivateKey()
	pk.Public.Region = []*tailcfg.DERPRegion{reg}
	text, _ := pk.Private.MarshalText()
	id, err := ParsePairToken(strings.TrimPrefix(string(text), "privkey:") + "." + string(pk.Public.Addr()))
	if err != nil {
		t.Fatal(err)
	}

	browser, stranger := key.NewNode(), key.NewNode()
	const machine = "tcTHEMACHINESOWNADDRESSxx"
	served := make(chan struct{}, 4)
	p, err := StartPairing(PairConfig{
		Identity: id,
		Peer:     browser.Public(),
		Port:     7770,
		Answer:   func() (string, string) { return "e2eagent", machine },
		OnServed: func() { served <- struct{}{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	dial := func(priv key.NodePrivate) (*http.Client, *tailcat.Client) {
		cl := &tailcat.Client{Server: tailcat.Addr(id.Addr), Key: priv, Logf: logger.Discard}
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
	hc, cl := dial(browser)
	defer cl.Close()
	pctx, pcancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer pcancel()
	for {
		if _, err := cl.Ping(pctx); err == nil {
			break
		}
		if pctx.Err() != nil {
			t.Fatal("the browser's key never reached the pairing node")
		}
		time.Sleep(300 * time.Millisecond)
	}
	resp, err := hc.Get("http://x:7770/v1/pair")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pair: %d %s", resp.StatusCode, b)
	}
	f, err := Decode(b)
	if err != nil || f.Type != TypePair || f.AgentID != "e2eagent" {
		t.Fatalf("frame: %+v %v", f, err)
	}
	var body Pair
	if json.Unmarshal(f.Body, &body) != nil || body.Addr != machine {
		t.Fatalf("body %s", f.Body)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("OnServed was not told")
	}

	_, cl2 := dial(stranger)
	defer cl2.Close()
	sctx, scancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer scancel()
	if _, err := cl2.Ping(sctx); err == nil {
		t.Fatal("a key the line did not name reached the pairing node")
	}
}
