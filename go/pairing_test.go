package main

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subnsubmonitor/transport"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// A token the way the page makes one (monitor-direct.js pairMint).
func testPairToken(t *testing.T) string {
	t.Helper()
	pk := tailcat.NewPrivateKey()
	pk.Public.Region = []*tailcfg.DERPRegion{{
		RegionID: 304, Nodes: []*tailcfg.DERPNode{{HostName: "derp.example.org", IPv4: "203.0.113.9"}},
	}}
	text, err := pk.Private.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(string(text), "privkey:") + "." + string(pk.Public.Addr())
}

func writePairing(t *testing.T, dir, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, pairingFileName)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestReadPairing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conf")
	now := time.Now()
	if _, err := readPairing(dir, now); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no file: %v, want not-exist", err)
	}
	tok := testPairToken(t)
	browser := key.NewNode().Public()
	writePairing(t, dir, tok+"\n"+browser.String()+"\n", 0o600)
	pf, err := readPairing(dir, now)
	if err != nil {
		t.Fatalf("a file the installer wrote: %v", err)
	}
	if pf.peer != browser || pf.sig != tok+"\n"+browser.String() {
		t.Fatalf("read %+v", pf)
	}
	if d := pf.until.Sub(now); d < pairingTTL-time.Minute || d > pairingTTL+time.Minute {
		t.Fatalf("deadline %v from now, want a week", d)
	}

	// Readable by others: refused, as the helper's other secrets are.
	writePairing(t, dir, tok+"\n"+browser.String()+"\n", 0o644)
	if _, err := readPairing(dir, now); err == nil {
		t.Fatal("a world-readable pairing file was read")
	}

	// A week old: over, and said so with the contents to remove.
	writePairing(t, dir, tok+"\n"+browser.String()+"\n", 0o600)
	old := now.Add(-pairingTTL - time.Hour)
	if err := os.Chtimes(filepath.Join(dir, pairingFileName), old, old); err != nil {
		t.Fatal(err)
	}
	pf, err = readPairing(dir, now)
	if !errors.Is(err, errPairingExpired) || pf.sig == "" {
		t.Fatalf("an old pairing: %v %+v", err, pf)
	}

	for name, body := range map[string]string{
		"no browser key":  tok + "\n",
		"bad browser key": tok + "\nnodekey:xyz\n",
		"bad token":       "deadbeef.tcAAAA\n" + browser.String() + "\n",
	} {
		writePairing(t, dir, body, 0o600)
		if _, err := readPairing(dir, now); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

func TestRemovePairingOnlyTheOneItRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conf")
	browser := key.NewNode().Public().String()
	body := testPairToken(t) + "\n" + browser + "\n"
	writePairing(t, dir, body, 0o600)
	first, err := readPairing(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The line run again meanwhile, the same contents renamed into place as
	// the installer does it: another file, and not the read one's to remove.
	tmp := filepath.Join(dir, pairingFileName+".new")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, pairingFileName)); err != nil {
		t.Fatal(err)
	}
	if err := removePairing(dir, first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, pairingFileName)); err != nil {
		t.Fatal("the newer pairing was removed for the older one's end")
	}
	second, err := readPairing(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := removePairing(dir, second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, pairingFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the finished pairing is still on disk")
	}
	if err := removePairing(dir, second); err != nil {
		t.Fatalf("removing what is gone: %v", err)
	}
}

// The machine's own node, saying hello to an allowed browser after the
// pairing was written, is what ends that browser's pairing.
func TestHelloEndsThePairing(t *testing.T) {
	scratchHome(t)
	browser := key.NewNode()
	if err := allowClient(browser.Public().String()); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Minute)
	if peerHeardSince(browser.Public(), before) {
		t.Fatal("heard before any hello")
	}
	h := newDirectHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/hello", nil)
	req = req.WithContext(transport.WithPeer(req.Context(), browser.Public()))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hello: %d", rec.Code)
	}
	if !peerHeardSince(browser.Public(), before) {
		t.Fatal("a hello to an allowed browser did not end its pairing")
	}
	// A pairing written after that hello — the line run again on a machine
	// the browser already watches — is not done by it.
	if peerHeardSince(browser.Public(), time.Now().Add(time.Minute)) {
		t.Fatal("an earlier hello ended a later pairing")
	}
	if !stillAllowed(browser.Public()) {
		t.Fatal("an allowed browser reads as struck")
	}
	if err := disallowClient(browser.Public().String()); err != nil {
		t.Fatal(err)
	}
	if stillAllowed(browser.Public()) {
		t.Fatal("a struck browser still reads as allowed")
	}
}

func TestPairingStatusLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conf")
	if s := pairingStatusLine(dir, time.Now()); s != "" {
		t.Fatalf("no pairing: %q", s)
	}
	writePairing(t, dir, testPairToken(t)+"\n"+key.NewNode().Public().String()+"\n", 0o600)
	if s := pairingStatusLine(dir, time.Now()); !strings.HasPrefix(s, "pairing: waiting") {
		t.Fatalf("a pairing on disk: %q", s)
	}
}
