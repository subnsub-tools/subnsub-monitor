package main

// The console — commands typed on the dashboard, run on this machine.
//
// This is the one part of the helper that DOES something to the box rather
// than reading it, so the whole design is about who decides that it may.
//
// OFF UNLESS THE OPERATOR TURNED IT ON, AT THE MACHINE. Not a checkbox on a
// web page, not a flag the relay can set, not a default anyone can be talked
// into: a file in this install's own config directory, written by someone who
// had a shell here already. `consoleEnabled` is the only thing consulted, it
// fails closed on every error, and a helper with the file absent never asks
// the relay whether there is anything to run — so there is no channel to
// attack, not merely no permission to use one. Every install in the field
// today is in exactly that state and stays there until its operator says
// otherwise.
//
// The transport is the same outbound-only shape as the push, for the same
// reason: nothing listens here. The helper ASKS whether a command is waiting;
// the relay answers. A machine behind NAT with no inbound port and no public
// address works unchanged, and unplugging the console is deleting a file.
//
// What runs is `/bin/sh -c <line>`, as the user the helper runs as, in that
// user's home directory. It is not a shell session: each command is its own
// process with its own environment, so `cd` does not persist and neither does
// anything else. That is a real limitation and it is deliberate — a persistent
// PTY is a much larger thing to get right, and the question this tab exists to
// answer ("what is that box doing?") is answered by one-shot commands.
//
// Every command is written to stderr before it runs. On a normal install that
// is the systemd journal or the launchd log, which means the machine keeps its
// own record of what was done to it that does not pass through us.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// Turning it on for one run without touching the filesystem — a container,
	// a `--dry-run`, a test. Same precedence rule the agent id follows: the
	// environment wins over the file.
	consoleEnvVar = "MON_CONSOLE"
	// The file whose EXISTENCE is the switch. Content is ignored: a switch
	// whose meaning depends on whether it says "0" or "false" or "off" is a
	// switch someone will get wrong in the direction that leaves it on.
	consoleFileName = "console"

	// How long one command may run. Long enough for a package index refresh,
	// short enough that a typo cannot pin a helper forever.
	consoleTimeout = 30 * time.Second
	// After the deadline kills the process, how long to wait for its output
	// pipes to close before giving up on them. Without a bound, one
	// backgrounded grandchild holding the pipe open would hang this goroutine
	// for as long as it lives.
	consoleWaitDelay = 2 * time.Second
	// Output relayed back. The relay caps it again at the same size; this one
	// is what stops a `yes` from being sent at all.
	consoleMaxOutput = 16 * 1024
	// And the ceiling on the SERIALISED report, which is the number the relay
	// actually enforces. JSON escaping expands the output — every newline
	// costs two bytes instead of one — so a payload sized only by
	// consoleMaxOutput can still be refused. Kept under the relay's own
	// MAX_RESULT_BODY (24 KiB) with room for the rest of the document.
	consoleMaxBody = 20 * 1024

	// The relay holds a poll for ~25s when a console is open, so the client
	// deadline has to clear that with room for a slow link.
	consolePollTimeout = 45 * time.Second
	// What it costs when nobody is watching: one small request this often,
	// answered immediately. It is also the worst case for how long a console
	// someone just opened waits before this machine notices — after which the
	// relay holds each poll and commands land in the time a keystroke takes.
	//
	// Ten seconds is the trade: long enough that an idle fleet is not chatty,
	// short enough that opening the console does not feel broken. The one
	// number this cannot be is "until the next push" — a 30-second wait before
	// the first command is the kind of first impression a feature does not
	// recover from. It is also how quickly `console on`/`off` reaches a helper
	// that is already running.
	consoleIdleWait = 10 * time.Second
	// Backoff ceiling when the relay is unreachable. The push loop is the
	// thing that reports a machine's health; this one going quiet must never
	// become the reason it stops.
	consoleErrorWait = 60 * time.Second
)

// The live relay token, which rotates under the console loop's feet.
//
// `connect` trades the token for a fresh one before it expires (see renew.go),
// and the console runs alongside that loop rather than inside it. Handing it a
// copy of the string would work until the first renewal and then fail closed
// forever — a console that silently stopped answering some hours after
// install, which is the kind of bug that gets diagnosed as "the feature does
// not work".
type tokenBox struct {
	mu sync.RWMutex
	v  string
}

func (b *tokenBox) get() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.v
}

func (b *tokenBox) set(s string) {
	b.mu.Lock()
	b.v = s
	b.mu.Unlock()
}

func consoleFile() string {
	dir := configDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, consoleFileName)
}

// May this machine be asked to run commands?
//
// Read every time rather than cached, so `subnsub-monitor console off` takes
// effect on a helper that is already running — within one idle poll, without a
// restart. Turning it OFF is the direction that has to be fast, and a cached
// answer would make the fast direction the slow one.
func consoleEnabled() bool {
	if v := strings.TrimSpace(os.Getenv(consoleEnvVar)); v != "" {
		return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
	}
	path := consoleFile()
	if path == "" {
		return false
	}
	// Any error at all — permissions, a directory where the file should be, a
	// filesystem that went away — is a no. There is no error here that means
	// "probably fine".
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Turn it on or off, from the command line on the machine itself.
func setConsole(on bool) error {
	dir := configDir()
	if dir == "" {
		return os.ErrNotExist
	}
	path := filepath.Join(dir, consoleFileName)
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 0600: the file's presence is a security decision, and one another local
	// user should not be able to make on this operator's behalf.
	return os.WriteFile(path, []byte("on\n"), 0o600)
}

// Whether the installed systemd unit's sandbox matches the console setting.
//
// The unit is written once, at install time, and its filesystem confinement
// depends on whether the console was on THEN — see install.sh. So flipping the
// switch afterwards leaves the two disagreeing, and the disagreement is silent
// in the direction that matters: `console on` succeeds, the dashboard offers a
// prompt, and every command that writes anything fails with a permission error
// that names nothing to do with any of this.
//
// Returns the advice to print, or "" when there is nothing to say — no unit, a
// unit that already agrees, or a platform where none of this applies.
func consoleSandboxNote(on bool) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "subnsub-monitor.service")
	b, err := os.ReadFile(unit)
	if err != nil {
		return "" // not a systemd install; launchd imposes none of this
	}
	strict := strings.Contains(string(b), "ProtectSystem=strict")
	if on && strict {
		return "note: this machine's service is sandboxed read-only, so commands can look\n" +
			"      but not write. Reinstall with --console to lift that:\n" +
			"        curl -fsSL https://tools.subnsub.com/monitor/install.sh | sh -s -- <TOKEN> --console"
	}
	if !on && !strict {
		return "note: the service is still running under the relaxed sandbox the console\n" +
			"      needed. Reinstall without --console to put the strict one back."
	}
	return ""
}

type consoleCmd struct {
	ID  string `json:"id"`
	Cmd string `json:"cmd"`
}

type consolePoll struct {
	Open     bool         `json:"open"`
	Commands []consoleCmd `json:"commands"`
}

// Ask the relay for work, run it, report back. Runs forever, alongside the
// push loop, and never lets its own failures reach that loop.
func consoleLoop(base string, tok *tokenBox) {
	pollURL := strings.TrimRight(base, "/") + "/commands"
	resultURL := strings.TrimRight(base, "/") + "/result"
	client := &http.Client{
		Timeout: consolePollTimeout,
		// Same refusal as the push client, for the same reason: the token is a
		// bearer secret and Go's default client would carry it across an
		// https→http hop on the same host.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	warned := false
	fails := 0

	for {
		if !consoleEnabled() {
			// Not enabled: no request is made at all. This is the state every
			// existing install is in, and it has to cost nothing.
			time.Sleep(consoleIdleWait)
			continue
		}
		if !warned {
			// Said once, on the machine, in its own log. Someone reading a
			// journal to find out why a box did something should not have to
			// already know this feature exists.
			warnf("console is ENABLED: this machine will run commands sent from the dashboard")
			warned = true
		}

		id := agentID()
		token := tok.get()
		if id == "" || token == "" {
			time.Sleep(consoleIdleWait)
			continue
		}

		poll, err := consoleAsk(client, pollURL, id, token)
		if err != nil {
			fails++
			wait := time.Duration(fails) * 5 * time.Second
			if wait > consoleErrorWait {
				wait = consoleErrorWait
			}
			time.Sleep(wait)
			continue
		}
		fails = 0
		if !poll.Open {
			// Nobody is looking. The relay answered immediately, so this sleep
			// is the whole idle cost.
			time.Sleep(consoleIdleWait)
			continue
		}
		for _, c := range poll.Commands {
			if c.ID == "" || c.Cmd == "" {
				continue
			}
			// Re-read the switch between the poll and the command, and again
			// between commands. The poll can be held for 25 seconds and a
			// batch can take a minute to work through, so `console off` typed
			// during either would otherwise be obeyed only after everything
			// already in flight had run — which is the opposite of what
			// somebody typing it in a hurry means by it.
			if !consoleEnabled() {
				consoleReport(client, resultURL, id, tok,
					consoleResult{ID: c.ID, Code: -1, Error: "console-off"})
				continue
			}
			consoleReport(client, resultURL, id, tok, runConsoleCommand(c.ID, c.Cmd))
		}
		// Straight back round with no sleep: a console that is open should feel
		// like a terminal, and the relay's own hold is what paces this loop.
	}
}

func consoleAsk(client *http.Client, pollURL, id, token string) (consolePoll, error) {
	var out consolePoll
	req, err := http.NewRequest("GET", pollURL+"?agent="+url.QueryEscape(id), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "subnsub-monitor")
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return out, errStatus(resp.StatusCode)
	}
	// Bounded: the relay is ours, but "the endpoint we trust" is exactly the
	// assumption an attacker who owns DNS gets to break.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	return out, nil
}

type statusError int

func (e statusError) Error() string { return "http " + itoa(int(e)) }
func errStatus(code int) error      { return statusError(code) }

type consoleResult struct {
	Agent     string `json:"agent"`
	ID        string `json:"id"`
	Code      int    `json:"code"`
	Ms        int64  `json:"ms"`
	Out       string `json:"out"`
	Truncated bool   `json:"truncated"`
	Error     string `json:"error,omitempty"`
}

func consoleReport(client *http.Client, resultURL, id string, tok *tokenBox, r consoleResult) {
	r.Agent = id

	// Size the ENCODED body, not the raw output.
	//
	// consoleMaxOutput bounds what the command printed; JSON then escapes it,
	// and a newline costs two bytes rather than one — 16 KiB of `ls -1` output
	// serialises to about 32 KiB, sails past the relay's limit, and comes back
	// 413. The result of that is the worst outcome this feature has: the
	// command RAN, and the operator is told nothing about what it did. So the
	// output is trimmed until the document fits, and the trim is marked.
	body := dump(r)
	for len(body) > consoleMaxBody && len(r.Out) > 0 {
		// Halve, then step back to a rune boundary — cutting mid-sequence
		// would put a replacement glyph in somebody's terminal.
		cut := len(r.Out) / 2
		for cut > 0 && !utf8.RuneStart(r.Out[cut]) {
			cut--
		}
		r.Out = r.Out[:cut]
		r.Truncated = true
		body = dump(r)
	}

	req, err := http.NewRequest("POST", resultURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.get())
	req.Header.Set("User-Agent", "subnsub-monitor")
	resp, err := client.Do(req)
	if err != nil {
		warnf("console: could not report the result of a command that ran")
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	// Said out loud rather than swallowed. A non-2xx here means a command ran
	// on this machine and its output reached nobody — the one failure in this
	// whole path that leaves the operator with a wrong picture rather than no
	// picture, so it belongs in the machine's own log even though there is
	// nothing useful to retry.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		warnf("console: the relay refused a result (%d); the command still ran", resp.StatusCode)
	}
}

// A writer that stops at a cap and remembers that it did.
//
// Not a truncation after the fact: a command that prints a gigabyte must not
// put a gigabyte in this process's memory first, so the limit is enforced as
// the bytes arrive.
type capWriter struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) <= room {
			w.buf.Write(p)
		} else {
			w.buf.Write(p[:room])
			w.over = true
		}
	} else if len(p) > 0 {
		w.over = true
	}
	// Always claim the whole write. Returning a short count makes io.Copy
	// report ErrShortWrite and, more to the point, makes the child's write()
	// fail — which turns "we stopped listening" into "your command crashed".
	return len(p), nil
}

func runConsoleCommand(id, line string) consoleResult {
	res := consoleResult{ID: id, Code: -1}

	// The audit line, written BEFORE the command runs, so a command that takes
	// the machine down is still in the log that survives it.
	warnf("console: running %s", strings.ReplaceAll(line, "\n", " "))

	ctx, cancel := context.WithTimeout(context.Background(), consoleTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", line)
	// The user's own home. Not the working directory the service manager
	// happened to start us in, which is `/` under systemd and would make every
	// relative path in a typed command mean something surprising.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cmd.Dir = home
	}
	// Own process group, so the deadline can take the whole tree down rather
	// than the shell only — `sh -c 'sleep 999'` leaves the sleep behind
	// otherwise, holding the output pipe open.
	cmd.SysProcAttr = consoleProcAttr()
	cmd.Cancel = func() error { return consoleKill(cmd) }
	// The backstop for output pipes a killed process left open anyway.
	cmd.WaitDelay = consoleWaitDelay

	out := &capWriter{max: consoleMaxOutput}
	cmd.Stdout = out
	cmd.Stderr = out

	started := time.Now()
	err := cmd.Run()
	res.Ms = time.Since(started).Milliseconds()
	res.Out = out.buf.String()
	res.Truncated = out.over

	// Take the process group down UNCONDITIONALLY, including after a command
	// that succeeded. The deadline only fires while the foreground shell is
	// still running, and `sleep 999 &` makes the shell exit immediately — so
	// without this line the timeout bounds the command and not the tree it
	// leaves behind, and a console session could accumulate stray processes on
	// a machine nobody is watching.
	//
	// This does mean a deliberately backgrounded process does not survive its
	// command. That is the honest reading of what this console is: each
	// command is its own shell and nothing carries over, so a daemon started
	// here would have outlived the only thing that knew about it. Anyone who
	// wants a process to persist has systemd, tmux, and every other tool
	// designed for it — none of which need this one to leak.
	consoleKill(cmd)

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.Error = "timeout"
		res.Code = -1
	case err == nil:
		res.Code = 0
	default:
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			res.Code = ee.ExitCode()
		case cmd.ProcessState != nil:
			// WaitDelay elapsed: the command itself finished and its status is
			// real, we simply stopped waiting for output pipes something it
			// spawned was still holding open. Reporting this as "spawn" — the
			// could-not-start case — would have been exactly backwards.
			res.Code = cmd.ProcessState.ExitCode()
			res.Error = "detached"
		default:
			// Could not start it at all — no /bin/sh, no fork, no permission.
			// Distinct from "it ran and failed", which has an exit code.
			res.Error = "spawn"
			res.Code = -1
		}
	}
	return res
}
