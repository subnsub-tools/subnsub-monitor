// signadapters: the release-side half of the helper's adapter feed.
//
//	signadapters keygen <keyfile>            make a release key (0600), print its public half
//	signadapters pubkey <keyfile>            print the public half of an existing key
//	signadapters sign   <keyfile> <table>    print the signature for adapters.json
//	signadapters verify <pub-b64> <table> <sigfile>
//
// The message is the table prefixed with the same domain line the helper
// uses (adapterfeed.go); keep the two in step. The key file holds the seed,
// base64, and nothing else — it is the one secret in this scheme and it
// lives on the release machine only.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

const domain = "subnsub-monitor adapters\n"

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func loadKey(path string) ed25519.PrivateKey {
	raw, err := os.ReadFile(path)
	if err != nil {
		die("cannot read key: " + err.Error())
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		die("key file does not hold a base64 Ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(seed)
}

func main() {
	args := os.Args[1:]
	if len(args) < 2 {
		die("usage: signadapters keygen|pubkey|sign|verify …")
	}
	switch args[0] {
	case "keygen":
		if _, err := os.Stat(args[1]); err == nil {
			die("refusing to overwrite an existing key")
		}
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			die("no entropy")
		}
		if err := os.WriteFile(args[1], []byte(base64.StdEncoding.EncodeToString(seed)+"\n"), 0o600); err != nil {
			die("cannot write key: " + err.Error())
		}
		pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		fmt.Println(base64.StdEncoding.EncodeToString(pub))
	case "pubkey":
		priv := loadKey(args[1])
		fmt.Println(base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
	case "sign":
		if len(args) < 3 {
			die("usage: signadapters sign <keyfile> <table>")
		}
		priv := loadKey(args[1])
		table, err := os.ReadFile(args[2])
		if err != nil {
			die("cannot read table: " + err.Error())
		}
		fmt.Println(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(domain), table...))))
	case "verify":
		if len(args) < 4 {
			die("usage: signadapters verify <pub-b64> <table> <sigfile>")
		}
		pub, err := base64.StdEncoding.DecodeString(args[1])
		if err != nil || len(pub) != ed25519.PublicKeySize {
			die("bad public key")
		}
		table, err := os.ReadFile(args[2])
		if err != nil {
			die("cannot read table: " + err.Error())
		}
		sigRaw, err := os.ReadFile(args[3])
		if err != nil {
			die("cannot read signature: " + err.Error())
		}
		sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigRaw)))
		if err != nil || !ed25519.Verify(ed25519.PublicKey(pub), append([]byte(domain), table...), sig) {
			die("signature does NOT verify")
		}
		fmt.Println("ok")
	default:
		die("unknown command " + args[0])
	}
}
