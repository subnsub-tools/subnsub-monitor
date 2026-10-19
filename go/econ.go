package main

// The console's spare road — a shell line through the relay's mailbox, sealed
// end to end.
//
// Since 2026-10-06 a shell line rides the tunnel alone (console.go), and that
// stays the road for every machine that has one. Some machine has none: a
// sandbox that can only dial out through a proxy keeps a tunnel up for a few
// seconds and no longer, while its ordinary pushes go
// through fine. For that machine this file keeps the mailbox usable without
// giving the relay anything to read:
//
//   - the dashboard seals the line to THIS machine's key and signs the sealed
//     bytes with the key this machine has pinned (signing.go). The relay
//     carries ciphertext and a signature it cannot produce, nothing else;
//   - this machine opens it, runs it as any console line, and seals the
//     output back to the browser that signed. The relay broadcasts
//     ciphertext; only that browser can read it.
//
// One switch at the machine: `console on`, the same grant every console line
// needs. A machine that allows remote commands offers this road too, and the
// DASHBOARD decides per machine whether to take it (its own switch, off until
// pressed) — so the person at the panel can open it without a second visit
// to the machine. Until 2026.10.11.5 there was a second switch here, `console
// relay on`; it is gone, and the subcommand only reports.
//
// The keys: this machine keeps an X25519 pair of its own for this (econ-key,
// made on first use, never the tunnel's), and publishes the public half in
// every push (`econ`, types.go). The browser's half is the X25519 key it
// already carries for tailcat (nodekey:…); it travels in the command's
// `target`, under the browser's signature, so a relay cannot substitute one.
// Key: HKDF-SHA256 over the X25519 shared secret, bound to the machine id and
// both public keys. Box: AES-256-GCM, a fresh 96-bit nonce in front of every
// ciphertext, the command id as associated data — a sealed line answers to
// one id and one direction and nothing else.
//
// What the relay can still do is what it could always do to the mailbox:
// drop a command, delay it, or refuse to carry it. It cannot WRITE one — a
// command is signed by a pinned browser key it does not hold (verifyCommand,
// and runSealed refuses an unpinned machine outright) — and it cannot forge
// the result, which is sealed whole (code, output, truncation) and opened
// against the id this page sealed to, not a field the relay chose.
//
// The one thing it shares with the existing tunnel road:
// the relay is what ADVERTISES where this machine is
// reached — the tunnel's address there, this machine's econ key here. An
// honest relay (ours) carries ciphertext it cannot read. A relay that
// actively SUBSTITUTED this key could have a browser seal a command to a key
// it holds and read it — but the real machine would then fail to open it and
// the operator would see the command fail, so a substitution cannot run
// anything and cannot stay hidden. It is the same trust the direct road
// already places in the address the relay advertises; the strong claim is
// against a passive relay, which is the deployment.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// The switch, as console's: the file's presence.
	// The machine's own X25519 private key, 32 bytes hex, mode 0600.
	econKeyFile = "econ-key"
	// The text form both ends use for a public key — tailcat's own.
	econKeyPrefix = "nodekey:"

	econSalt    = "subnsub-monitor econ v1"
	econAADCmd  = "c"
	econAADOut  = "o"
	econNonceSz = 12
	// Output sealed back: capped BEFORE sealing so the base64 ciphertext stays
	// inside the relay's cap on a result's `out` (16 KiB of characters) with
	// the tag and nonce. Smaller than consoleMaxOutput, which the tunnel keeps.
	econMaxOutput = 11 * 1024
)

var econMu sync.Mutex

// consoleRelayEnabled: whether this machine takes sealed lines through the
// relay's mailbox — the console's own switch, read every time like
// consoleEnabled so off is the fast direction. The dashboard's per-machine
// switch is the other half, and it lives there.
func consoleRelayEnabled() bool {
	return consoleEnabled()
}

// econPrivate: the machine's key, made the first time it is asked for.
func econPrivate() (*ecdh.PrivateKey, error) {
	econMu.Lock()
	defer econMu.Unlock()
	dir := configDir()
	if dir == "" {
		return nil, os.ErrNotExist
	}
	path := filepath.Join(dir, econKeyFile)
	if b, err := os.ReadFile(path); err == nil {
		raw, derr := hex.DecodeString(strings.TrimSpace(string(b)))
		if derr == nil && len(raw) == 32 {
			if k, kerr := ecdh.X25519().NewPrivateKey(raw); kerr == nil {
				return k, nil
			}
		}
		// A file that is there and not a key is not quietly replaced: the
		// browser may already hold the public half that was published from
		// it. Said, and refused.
		return nil, errors.New("econ-key is not a key")
	}
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(hex.EncodeToString(k.Bytes())+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return k, nil
}

// econPublic is what the push carries: this machine's key in text, or ""
// when the spare road is not open here — the switch off, the console off, or
// no dashboard key pinned (an unsigned sealed line would be a line from
// anybody; signing.go refuses nothing on a machine with nothing pinned, so
// the road is not offered there at all).
func econPublic() string {
	if !consoleRelayEnabled() || !consoleEnabled() || !loadTrust().sealed {
		return ""
	}
	k, err := econPrivate()
	if err != nil {
		return ""
	}
	return econKeyPrefix + hex.EncodeToString(k.PublicKey().Bytes())
}

func econParsePublic(s string) (*ecdh.PublicKey, bool) {
	if !strings.HasPrefix(s, econKeyPrefix) {
		return nil, false
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(s, econKeyPrefix))
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	k, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, false
	}
	return k, true
}

// econKey: the AEAD both ends derive. priv is THIS machine's econ key, agent
// its id, peer the BROWSER's key in text (the command's target).
func econKey(priv *ecdh.PrivateKey, agent, peer string) (cipher.AEAD, error) {
	pub, ok := econParsePublic(peer)
	if !ok {
		return nil, errors.New("bad peer key")
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, err
	}
	mine := econKeyPrefix + hex.EncodeToString(priv.PublicKey().Bytes())
	// Canonical order, the SAME on both ends regardless of who is deriving:
	// the machine id, then the MACHINE's econ key, then the BROWSER's. Here
	// `mine` is the machine's and `peer` the browser's; the page builds the
	// same string with the names the other way round (monitor-direct.js
	// econKey). Reversing them would make the two sides derive different keys
	// and every box fail to open — caught by the shared vector in the test.
	info := agent + "\n" + mine + "\n" + peer
	k, err := hkdf.Key(sha256.New, shared, []byte(econSalt), info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func econAAD(dir, id string) []byte { return []byte(dir + "\n" + id) }

// econOpen: a sealed command line (base64 of nonce||ciphertext) to its text.
func econOpen(aead cipher.AEAD, id, sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < econNonceSz+aead.Overhead() {
		return "", errors.New("bad box")
	}
	out, err := aead.Open(nil, raw[:econNonceSz], raw[econNonceSz:], econAAD(econAADCmd, id))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// econSeal: output back to the browser, base64 of nonce||ciphertext.
func econSeal(aead cipher.AEAD, id string, text string) (string, error) {
	nonce := make([]byte, econNonceSz)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := aead.Seal(nil, nonce, []byte(text), econAAD(econAADOut, id))
	return base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

// econEnvelopeV1 is what a sealed OUTPUT box carries: the WHOLE result, so a
// relay cannot lie about the exit code, the truncation, or substitute output
// for a command that failed. The page authenticates these by opening the box;
// the outer consoleResult fields carry nothing it trusts for an esh.
type econEnvelopeV1 struct {
	V     int    `json:"v"`
	Code  int    `json:"code"`
	Ms    int64  `json:"ms"`
	Out   string `json:"out"`
	Trunc bool   `json:"trunc,omitempty"`
	Err   string `json:"err,omitempty"`
}

// runSealed answers an `esh`: the sealed line in c.Cmd, the browser's key in
// c.Target, both already under the signature verifyCommand checked. A refusal
// before the command runs is a plain code the page already knows and carries
// NO box; the result of a command that ran — its code, output, truncation and
// any timeout — is sealed whole so the relay can neither read it nor forge it.
func runSealed(c consoleCmd) consoleResult {
	res := consoleResult{ID: c.ID, Kind: "esh", Code: -1}
	// Pinning is required here, not only at publication: verifyCommand waves
	// an unsigned command through on a machine with nothing pinned, which is
	// right for the relay's account-gated mailbox and wrong for a road whose
	// only proof of WHO is the signature. Without this a relay that kept the
	// econ key from before the last key was removed could run unsigned
	// commands. Same gate direct.go sets.
	if !loadTrust().sealed {
		res.Error = "unpinned"
		return res
	}
	if !consoleEnabled() {
		res.Error = "console-off"
		return res
	}
	if !consoleRelayEnabled() {
		res.Error = "econ-off"
		return res
	}
	priv, err := econPrivate()
	if err != nil {
		res.Error = "econ-key"
		return res
	}
	aead, err := econKey(priv, agentID(), c.Target)
	if err != nil {
		res.Error = "econ-key"
		return res
	}
	line, err := econOpen(aead, c.ID, c.Cmd)
	if err != nil {
		warnf("console: a sealed command (id %q) did not open — not for this machine, or not from the key it names", c.ID)
		res.Error = "econ-open"
		return res
	}
	if line == "" || len(line) > 2000 {
		res.Error = "bad-request"
		return res
	}
	warnf("console: a sealed command arrived through the relay; running it as a console line")
	ran := runConsoleCommand(c.ID, line)
	out := ran.Out
	trunc := ran.Truncated
	if len(out) > econMaxOutput {
		// On a rune boundary, as capWriter keeps one.
		cut := econMaxOutput
		for cut > 0 && cut < len(out) && (out[cut]&0xC0) == 0x80 {
			cut--
		}
		out = out[:cut]
		trunc = true
	}
	env, err := json.Marshal(econEnvelopeV1{V: 1, Code: ran.Code, Ms: ran.Ms, Out: out, Trunc: trunc, Err: ran.Error})
	if err != nil {
		res.Error = "econ-key"
		return res
	}
	sealed, err := econSeal(aead, c.ID, string(env))
	if err != nil {
		res.Error = "econ-key"
		return res
	}
	// The outer fields carry nothing the page trusts for an esh: the sealed
	// envelope is the result. Code 0 here only marks "a box is present".
	res.Code = 0
	res.Error = ""
	res.Out = sealed
	res.Enc = true
	return res
}
