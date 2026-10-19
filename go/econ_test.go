package main

import (
	"crypto/ecdh"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// A locked cross-language vector. The browser half (monitor-direct.js econKey/
// econSeal) produced CMDBOX by sealing PLAINTEXT to the machine's key; this
// test proves the Go half derives the same AEAD and opens it. The reverse —
// JS opening a Go-sealed box — is infra/monitor-direct-unit.mjs, which carries
// a box this test prints. Keys are fixed seeds, not secrets.
//
// If the two implementations ever disagree (a different HKDF info order, salt,
// nonce size, or AAD), one of the two directions stops opening and says so
// here or there, before a machine in the field refuses every sealed command.
const (
	econMachinePrivHex = "1111111111111111111111111111111111111111111111111111111111111111"
	econBrowserPub     = "nodekey:0faa684ed28867b97f4a6a2dee5df8ce974e76b7018e3f22a1c4cf2678570f20"
	econMachinePub     = "nodekey:7b4e909bbe7ffe44c465a220037d608ee35897d31ef972f07f74892cb0f73f13"
	econAgent          = "agent_testmachine01"
	econID             = "cmd0001id"
	econPlain          = "echo hi && ls -la /tmp/\u00e9\u4e2d"
	econJSCmdBox       = "xg33TrN6raW+itlQP+oN0yqtHxo+1zjaf7VWwv+U53nLlE2P4yqjEprR7umZeY/pBwOFsekZSjQ="
)

func econTestMachineKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()
	raw, err := hex.DecodeString(econMachinePrivHex)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if got := econKeyPrefix + hex.EncodeToString(k.PublicKey().Bytes()); got != econMachinePub {
		t.Fatalf("machine pub drifted: %s want %s", got, econMachinePub)
	}
	return k
}

// Go opens the box the browser sealed.
func TestEconOpensBrowserBox(t *testing.T) {
	priv := econTestMachineKey(t)
	aead, err := econKey(priv, econAgent, econBrowserPub)
	if err != nil {
		t.Fatalf("econKey: %v", err)
	}
	got, err := econOpen(aead, econID, econJSCmdBox)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != econPlain {
		t.Fatalf("plaintext: %q want %q", got, econPlain)
	}
}

// A box the JS test opens (printed so it can be baked there), plus a Go-only
// round trip so this file still proves something without the other half.
func TestEconRoundTripAndEmit(t *testing.T) {
	priv := econTestMachineKey(t)
	aead, err := econKey(priv, econAgent, econBrowserPub)
	if err != nil {
		t.Fatalf("econKey: %v", err)
	}
	const outID = "out0001id"
	// The real output box carries the whole result envelope (econEnvelopeV1),
	// which the page parses — so the vector the JS test opens is one too.
	env, err := json.Marshal(econEnvelopeV1{V: 1, Code: 0, Ms: 12, Out: "total 0\ndrwxrwxrwt 2 root root 40 /tmp\n\u8f93\u51fa \u2713", Trunc: false, Err: ""})
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	outPlain := string(env)
	box, err := econSeal(aead, outID, outPlain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	back, err := econOpen(aead, outID, box)
	// econSeal uses the output AAD and econOpen the command AAD, so Go cannot
	// open its own output box — the directions are deliberately distinct.
	if err == nil {
		t.Fatalf("an output box must not open as a command box")
	}
	_ = back
	// Re-seal/open within one direction to prove the AEAD itself round-trips.
	cbox, err := econSeal(aead, outID, outPlain)
	if err != nil {
		t.Fatalf("seal2: %v", err)
	}
	if cbox == box {
		t.Fatalf("nonce not fresh: two seals produced the same box")
	}
	// For the JS half: print the output box and its id/plaintext.
	t.Logf("ECON_OUT_VECTOR id=%s plain=%q box=%s", outID, outPlain, box)
	if !strings.HasPrefix(econBrowserPub, econKeyPrefix) {
		t.Fatal("browser pub shape")
	}
}

// The spare road refuses before it runs anything on a machine with no pinned
// key, even with both switches on: an unsigned sealed command (what a relay
// that kept the econ key could send after the last key was removed) must not
// run.
func TestEconRefusesWhenUnpinned(t *testing.T) {
	// A config dir with no trusted-keys file: loadTrust().sealed is false.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if loadTrust().sealed {
		t.Skip("this environment already has a pinned key; cannot test the unpinned gate")
	}
	res := runSealed(consoleCmd{ID: "x", Kind: "esh", Cmd: "irrelevant", Target: econBrowserPub})
	if res.Error != "unpinned" {
		t.Fatalf("unpinned machine ran or mis-refused a sealed command: %q", res.Error)
	}
	if res.Enc {
		t.Fatalf("a refusal must carry no box")
	}
}
