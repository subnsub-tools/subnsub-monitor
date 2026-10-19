//go:build darwin

package main

// macOS keeps in the login keychain what the other platforms keep in a file.
//
// Claude Code stores its OAuth document there — a generic password under the
// service "Claude Code-credentials", the same JSON as ~/.claude/.credentials.json
// — and writes the file only where the keychain is unavailable. So on a Mac
// the credential rung found no file and reported "not signed in" for a user
// who was. This reads the item, and only ever hands back its bytes for the
// same four-scalar decode claude.go has always done; the MCP tokens in the
// same document are no more read from here than from the file.
//
// Through the system's own `security` tool, under the discipline amp.go set
// for running another program: a fixed absolute path (never PATH), null
// stdin, a deadline, a bounded buffer, and an environment that carries none
// of this helper's own secrets (keychainEnv, local.go) — `security` needs
// HOME to find the login keychain and nothing else. No prompt: the item's
// access list admits
// `security` because that is the tool Claude Code wrote it with. `-w` prints
// the secret alone, as text when it is UTF-8 (the JSON these tools store) and
// as hex otherwise, which json.Unmarshal then refuses as it should.

import (
	"bytes"
	"context"
	"time"
)

const (
	keychainTool    = "/usr/bin/security"
	keychainMax     = 256 * 1024
	keychainTimeout = 5 * time.Second
)

func keychainPassword(service string) ([]byte, bool) {
	if service == "" || !usableBinary(keychainTool) {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()
	cmd := toolCommand(ctx, keychainTool, "find-generic-password", "-s", service, "-w")
	cmd.Stdin = nil
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = keychainEnv()
	var out bytes.Buffer
	lw := &limitedWriter{w: &out, n: keychainMax}
	cmd.Stdout, cmd.Stderr = lw, nil
	if err := cmd.Run(); err != nil || lw.over {
		return nil, false
	}
	raw := bytes.TrimSpace(out.Bytes())
	if len(raw) == 0 {
		return nil, false
	}
	return raw, true
}
