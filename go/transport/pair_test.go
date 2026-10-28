package transport

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// A pairing identity the way the page mints one: a node key, its derived
// disco key, a pre-shared key and one region embedded with its relay.
func testPairKey(t *testing.T) (*tailcat.PrivateKey, string) {
	t.Helper()
	pk := tailcat.NewPrivateKey()
	pk.Public.Region = []*tailcfg.DERPRegion{{
		RegionID: 303, RegionCode: "fra",
		Nodes: []*tailcfg.DERPNode{{Name: "303a", RegionID: 303, HostName: "derp.example.net", IPv4: "203.0.113.7", IPv6: "2001:db8::7"}},
	}}
	text, err := pk.Private.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	return pk, strings.TrimPrefix(string(text), "privkey:")
}

func TestParsePairTokenRoundTrip(t *testing.T) {
	pk, hexKey := testPairKey(t)
	addr := string(pk.Public.Addr())
	id, err := ParsePairToken(hexKey + "." + addr)
	if err != nil {
		t.Fatalf("a token made like the page's: %v", err)
	}
	if !id.Key.Equal(pk.Private) || !id.PSK.Equal(pk.Public.PresharedKey) || id.Addr != addr {
		t.Fatalf("decoded identity differs: %+v", id)
	}
	if id.Region == nil || len(id.Region.Nodes) != 1 || id.Region.Nodes[0].HostName != "derp.example.net" || id.Region.Nodes[0].IPv4 != "203.0.113.7" {
		t.Fatalf("region: %+v", id.Region)
	}
	// Surrounding whitespace is a pasted line, not a different token.
	if _, err := ParsePairToken("  " + hexKey + "." + addr + "\n"); err != nil {
		t.Fatalf("trimmed token refused: %v", err)
	}
}

func TestParsePairTokenRefusals(t *testing.T) {
	pk, hexKey := testPairKey(t)
	addr := string(pk.Public.Addr())
	_, otherHex := testPairKey(t)

	noPSK := *pk
	noPSK.Public.PresharedKey = tailcat.PresharedKey{}
	wrongDisco := *pk
	wrongDisco.Public.ServerDiscoPublic = tailcat.DiscoPublic{DiscoPublic: key.NewDisco().Public()}
	byID := *pk
	byID.Public.Region = nil
	byID.Public.RegionID = 303
	noHost := *pk
	noHost.Public.Region = []*tailcfg.DERPRegion{{RegionID: 1, Nodes: []*tailcfg.DERPNode{{IPv4: "203.0.113.7"}}}}

	for name, tok := range map[string]string{
		"no dot":             hexKey + addr,
		"short key":          hexKey[:62] + "." + addr,
		"uppercase key":      strings.ToUpper(hexKey) + "." + addr,
		"all-zero key":       strings.Repeat("0", 64) + "." + addr,
		"another key":        otherHex + "." + addr,
		"not an address":     hexKey + ".xx" + addr[2:],
		"bad characters":     hexKey + "." + addr + "!",
		"no pre-shared key":  hexKey + "." + string(noPSK.Public.Addr()),
		"wrong disco key":    hexKey + "." + string(wrongDisco.Public.Addr()),
		"region by id only":  hexKey + "." + string(byID.Public.Addr()),
		"relay with no name": hexKey + "." + string(noHost.Public.Addr()),
		"too long":           hexKey + "." + addr + strings.Repeat("A", maxPairToken),
	} {
		if _, err := ParsePairToken(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The one frame, to the one key: the handler alone, without a tunnel, the
// way the gate's own test drives it (the remote address is what the tunnel
// derives from the peer's key).
func TestPairHandler(t *testing.T) {
	peer := key.NewNode().Public()
	stranger := key.NewNode().Public()
	served := 0
	addr := "tc" + strings.Repeat("A", 40)
	cfg := PairConfig{
		Peer:     peer,
		Port:     7770,
		Answer:   func() (string, string) { return "abcd1234", addr },
		OnServed: func() { served++ },
	}
	h := pairHandler(cfg)
	ask := func(from key.NodePublic, method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = net.JoinHostPort(addrForKey(from).String(), "41641")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := ask(peer, http.MethodGet, "/v1/pair")
	if w.Code != http.StatusOK {
		t.Fatalf("the paired browser: status %d", w.Code)
	}
	f, err := Decode(w.Body.Bytes())
	if err != nil || f.Type != TypePair || f.AgentID != "abcd1234" {
		t.Fatalf("frame: %+v %v", f, err)
	}
	var body Pair
	if err := json.Unmarshal(f.Body, &body); err != nil || body.Addr != addr {
		t.Fatalf("body: %+v %v", body, err)
	}
	if served != 1 {
		t.Fatalf("OnServed told %d times, want 1", served)
	}

	if w := ask(stranger, http.MethodGet, "/v1/pair"); w.Code != http.StatusForbidden {
		t.Errorf("another key: status %d, want 403", w.Code)
	}
	if w := ask(peer, http.MethodGet, "/v1/hello"); w.Code != http.StatusNotFound {
		t.Errorf("any other path: status %d, want 404", w.Code)
	}
	if w := ask(peer, http.MethodPost, "/v1/pair"); w.Code != http.StatusNotFound {
		t.Errorf("a POST: status %d, want 404", w.Code)
	}
	cfg.Answer = func() (string, string) { return "abcd1234", "" }
	h = pairHandler(cfg)
	if w := ask(peer, http.MethodGet, "/v1/pair"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("no address yet: status %d, want 503", w.Code)
	}
	// Struck from the machine's allowlist: refused here too.
	cfg.Answer = func() (string, string) { return "abcd1234", addr }
	cfg.Allowed = func(k key.NodePublic) bool { return k != peer }
	h = pairHandler(cfg)
	if w := ask(peer, http.MethodGet, "/v1/pair"); w.Code != http.StatusForbidden {
		t.Errorf("a struck browser: status %d, want 403", w.Code)
	}
	if served != 1 {
		t.Errorf("OnServed told for a refusal")
	}
}

func TestStartPairingRefusesAnIncompleteConfig(t *testing.T) {
	pk, hexKey := testPairKey(t)
	id, err := ParsePairToken(hexKey + "." + string(pk.Public.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	answer := func() (string, string) { return "", "" }
	for name, cfg := range map[string]PairConfig{
		"no identity": {Peer: key.NewNode().Public(), Port: 7770, Answer: answer},
		"no peer":     {Identity: id, Port: 7770, Answer: answer},
		"no port":     {Identity: id, Peer: key.NewNode().Public(), Answer: answer},
		"no answer":   {Identity: id, Peer: key.NewNode().Public(), Port: 7770},
	} {
		if p, err := StartPairing(cfg); err == nil {
			p.Close()
			t.Errorf("%s: started", name)
		}
	}
}

// An identity the page made (monitor-direct.js pairAddress, fixed inputs —
// infra/monitor-direct-unit.mjs pins the same string): decoded by tailcat's
// own ParseAddr, its public and disco keys derived here from the private key,
// so the page's X25519, HMAC and CBOR agree with Go's byte for byte.
const pageToken = "404bbf64efcc69dc6df2a8fc0c908d246cba2290402ed93b05e4235bd3ea7762.tcpGFwWCBYdWcQEv2PtEMTC7am7f3Mi7yZnEPHjX7ygo39afX6AmFrWCASBywQy3Hgbk3UUOX_BJasiOiQnBZANcDpp22mhNENCmFxWCChoaGhoaGhoaGhoaGhoaGhsrKysrKysrKysrKysrKysmFygaFhToKjYTRvMTg1LjE3OC4yMDIuMTk3YTZxMmEwMDpkZDgwOjIwOjoyMDdhaG50YzMwM2EuaXBuLmRldqVhaHBkZXJwLmV4YW1wbGUubmV0YXMgYWQZIPthdHBkZXJwLmV4YW1wbGUubmV0YXj1"

func TestParsePairTokenFromThePage(t *testing.T) {
	id, err := ParsePairToken(pageToken)
	if err != nil {
		t.Fatalf("the page's token: %v", err)
	}
	var psk tailcat.PresharedKey
	for i := range psk {
		psk[i] = 0xa1
		if i >= 16 {
			psk[i] = 0xb2
		}
	}
	if !id.PSK.Equal(psk) {
		t.Errorf("pre-shared key differs")
	}
	if got := tailcat.DiscoPublicForNode(id.Key).DiscoPublic.String(); got != "discokey:12072c10cb71e06e4dd450e5ff0496ac88e8909c164035c0e9a76da684d10d0a" {
		t.Errorf("disco key %s, not the page's", got)
	}
	ns := id.Region.Nodes
	if len(ns) != 2 {
		t.Fatalf("relays: %+v", ns)
	}
	if ns[0].HostName != "tc303a.ipn.dev" || ns[0].IPv4 != "185.178.202.197" || ns[0].IPv6 != "2a00:dd80:20::207" || ns[0].DERPPort != 0 {
		t.Errorf("first relay: %+v", ns[0])
	}
	if ns[1].HostName != "derp.example.net" || ns[1].STUNPort != -1 || ns[1].DERPPort != 8443 || ns[1].CertName != "derp.example.net" || !ns[1].InsecureForTests {
		t.Errorf("second relay: %+v", ns[1])
	}
}
