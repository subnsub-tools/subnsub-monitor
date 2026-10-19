package main

// The adapter feed: what is accepted (a newer table, signed by the release
// key) and everything that is not.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func testAdapterKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signTable(priv ed25519.PrivateKey, raw []byte) []byte {
	sig := ed25519.Sign(priv, append([]byte(adapterFeedDomain), raw...))
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n")
}

// The embedded table with its version moved, so a "newer" table is real.
func tableAt(t *testing.T, version string) []byte {
	t.Helper()
	raw := string(builtinAdaptersJSON)
	cur, err := parseAdapters(builtinAdaptersJSON)
	if err != nil {
		t.Fatal(err)
	}
	from := `"version": ` + strconv.Itoa(cur.Version) + `,`
	if !strings.Contains(raw, from) {
		t.Fatal("fixture cannot find the embedded table's version line")
	}
	return []byte(strings.Replace(raw, from, `"version": `+version+`,`, 1))
}

func resetAdapters() {
	adapterState.Lock()
	adapterState.set = nil
	adapterState.Unlock()
}

func TestAdapterSignature(t *testing.T) {
	pub, priv := testAdapterKey(t)
	other, _ := testAdapterKey(t)
	raw := tableAt(t, "2")
	sig := signTable(priv, raw)
	if !adapterSigned(raw, sig, []ed25519.PublicKey{pub}) {
		t.Fatal("a good signature was refused")
	}
	if adapterSigned(raw, sig, []ed25519.PublicKey{other}) {
		t.Error("another key's signature was accepted")
	}
	if adapterSigned(append(raw, ' '), sig, []ed25519.PublicKey{pub}) {
		t.Error("a tampered table was accepted")
	}
	if adapterSigned(raw, []byte("not base64!"), []ed25519.PublicKey{pub}) {
		t.Error("garbage was accepted as a signature")
	}
	// The same key over the bare bytes — no domain line — is a signature
	// over some other document.
	bare := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)))
	if adapterSigned(raw, bare, []ed25519.PublicKey{pub}) {
		t.Error("a signature without the domain line was accepted")
	}
	if adapterSigned(raw, sig, nil) {
		t.Error("no keys must accept nothing")
	}
}

func TestAdapterRefreshInstallsOnlyNewer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetAdapters()
	t.Cleanup(resetAdapters)
	pub, priv := testAdapterKey(t)
	keys := []ed25519.PublicKey{pub}

	served := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := served[strings.TrimPrefix(r.URL.Path, "/monitor/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	base := srv.URL + "/monitor"

	// Nothing published: an error, and the embedded table stays.
	if _, err := adapterRefreshOnce(context.Background(), base, keys); err == nil {
		t.Fatal("a missing table should be an error")
	}
	embedded := adaptersVersion()
	if embedded < 1 {
		t.Fatalf("embedded version: %d", embedded)
	}

	// The same version, signed: nothing to do.
	served["adapters.json"] = tableAt(t, strconv.Itoa(embedded))
	served["adapters.sig"] = signTable(priv, served["adapters.json"])
	if installed, err := adapterRefreshOnce(context.Background(), base, keys); err != nil || installed {
		t.Fatalf("same version: installed=%v err=%v", installed, err)
	}

	// Newer, wrongly signed: refused.
	served["adapters.json"] = tableAt(t, "30")
	if _, err := adapterRefreshOnce(context.Background(), base, keys); err == nil {
		t.Fatal("a table with a stale signature was accepted")
	}
	if adaptersVersion() != embedded {
		t.Fatal("a refused table changed the table in force")
	}

	// Newer, signed: in force, and kept for the next start.
	served["adapters.sig"] = signTable(priv, served["adapters.json"])
	installed, err := adapterRefreshOnce(context.Background(), base, keys)
	if err != nil || !installed || adaptersVersion() != 30 {
		t.Fatalf("newer signed: installed=%v err=%v version=%d", installed, err, adaptersVersion())
	}
	kept, err := os.ReadFile(filepath.Join(configDir(), adapterFeedFile))
	if err != nil || string(kept) != string(served["adapters.json"]) {
		t.Fatalf("the table was not kept: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(configDir(), adapterFeedFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("kept table mode %o, want 0600", fi.Mode().Perm())
	}

	// An older table published later (a replay) is refused by version.
	served["adapters.json"] = tableAt(t, "20")
	served["adapters.sig"] = signTable(priv, served["adapters.json"])
	if installed, _ := adapterRefreshOnce(context.Background(), base, keys); installed || adaptersVersion() != 30 {
		t.Error("a replayed older table was installed")
	}

	// A newer table naming a host off the list is refused whole.
	bad := strings.Replace(string(tableAt(t, "40")), "https://api.kimi.com/coding/v1/usages", "https://evil.example/usages", 1)
	served["adapters.json"] = []byte(bad)
	served["adapters.sig"] = signTable(priv, served["adapters.json"])
	if _, err := adapterRefreshOnce(context.Background(), base, keys); err == nil || adaptersVersion() != 30 {
		t.Error("a signed table off the allow-list was accepted")
	}
}

func TestPersistedAdaptersLoadAtStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetAdapters()
	t.Cleanup(resetAdapters)
	pub, priv := testAdapterKey(t)

	// A kept table from an earlier run, under the key this build trusts.
	raw := tableAt(t, "50")
	if err := persistAdapters(raw, signTable(priv, raw)); err != nil {
		t.Fatal(err)
	}
	saved := adapterFeedKeys
	adapterFeedKeys = []ed25519.PublicKey{pub}
	adapterPersistOnce = sync.Once{}
	t.Cleanup(func() { adapterFeedKeys = saved; adapterPersistOnce = sync.Once{} })
	loadPersistedAdapters()
	if adaptersVersion() != 50 {
		t.Fatalf("kept table not loaded: version %d", adaptersVersion())
	}

	// Tampered on disk: ignored, the embedded table stands.
	resetAdapters()
	adapterPersistOnce = sync.Once{}
	os.WriteFile(filepath.Join(configDir(), adapterFeedFile), tableAt(t, "60"), 0o600)
	loadPersistedAdapters()
	if adaptersVersion() >= 50 {
		t.Fatalf("a tampered kept table was loaded: version %d", adaptersVersion())
	}
}

func TestBuiltBinaryCarriesAKey(t *testing.T) {
	if adapterFeedPubKey == "ADAPTER_FEED_PUBKEY" {
		t.Skip("release key not stamped in this tree")
	}
	if len(adapterFeedKeys) != 1 {
		t.Fatal("the stamped release key does not parse")
	}
}
