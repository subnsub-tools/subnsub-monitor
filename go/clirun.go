package main

// Shared plumbing for the CLI-rung collectors that came after Amp. Amp wrote
// the rules (amp.go's header tells the whole story); this file is those rules
// with the Amp-specific names factored out, so Kiro, Auggie and arkcli do not
// each re-learn them:
//
//   - the binary comes from a fixed list of absolute paths or an explicit
//     override, never from PATH;
//   - the child's environment is rebuilt from the same allowlist Amp uses —
//     this helper's own relay token must never reach a third party's address
//     space — plus the vendor's own prefixes;
//   - stdin is the null device, output is bounded, the deadline is real
//     (WaitDelay breaks the grandchild-holds-the-pipe hang), and an
//     over-budget output refuses the whole reading rather than parsing a
//     prefix.
//
// amp.go itself deliberately does NOT route through here: it shipped first,
// its behaviour is pinned by tests and fleets, and rewiring a working
// collector to prove a refactor is backwards.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Resolve a vendor CLI to a runnable absolute path. Same contract as
// ampBinary: the override refuses rather than falls through, and PATH is
// never consulted.
func vendorBinary(envOverride string, candidates []string) string {
	if v := strings.TrimSpace(os.Getenv(envOverride)); v != "" {
		if filepath.IsAbs(v) && usableBinary(v) {
			return v
		}
		return ""
	}
	for _, c := range candidates {
		if c != "" && usableBinary(c) {
			return c
		}
	}
	return ""
}

// The usual home-relative candidates for a CLI named `name`, plus the fixed
// prefixes. Callers append vendor-specific extras.
func vendorCandidates(name string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(home, ".local", "bin", name))
	}
	out = append(out, "/usr/local/bin/"+name, "/opt/homebrew/bin/"+name)
	if appdata := os.Getenv("APPDATA"); filepath.IsAbs(appdata) {
		out = append(out,
			filepath.Join(appdata, "npm", name+".cmd"),
			filepath.Join(appdata, "npm", name+".exe"))
	}
	return out
}

// Run one vendor CLI to completion. Returns its text output, whether it
// exited non-zero, and an error only when there is nothing usable to parse.
// `envPrefixes` names the vendor's own variable families to pass through, on
// top of the shared allowlist (ampEnvKeys — the list is not Amp-specific,
// only its name is).
func runVendorCLI(bin string, args []string, envPrefixes []string, deadline time.Duration) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	cmd := toolCommand(ctx, bin, args...)
	cmd.Stdin = nil
	env := []string{"NO_COLOR=1"}
	for _, k := range ampEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		k := kv[:i]
		if strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
			continue
		}
		for _, p := range envPrefixes {
			if strings.HasPrefix(k, p) {
				env = append(env, kv)
				break
			}
		}
	}
	cmd.Env = env
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	lo := &limitedWriter{w: &stdout, n: ampMaxOut}
	le := &limitedWriter{w: &stderr, n: ampMaxOut}
	cmd.Stdout, cmd.Stderr = lo, le

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", false, errAmpTimeout
	}
	if lo.over || le.over {
		return "", false, &ampErr{"output too large"}
	}
	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = stderr.String()
	}
	if strings.TrimSpace(out) == "" {
		if err != nil {
			return "", true, err
		}
		return "", false, &ampErr{"empty output"}
	}
	return out, err != nil, nil
}
