//go:build linux || darwin || freebsd

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"subnsubmonitor/transport"

	"tailscale.com/types/key"
)

func ptyWaitFor(t *testing.T, s *ptySess, want string) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		got := string(s.ring)
		wake := s.notify
		ended := s.ended
		s.mu.Unlock()
		if strings.Contains(got, want) {
			return got
		}
		if ended {
			t.Fatalf("session ended before %q appeared: %q", want, got)
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("no %q in the output: %q", want, got)
		}
	}
}

// A session is a shell that keeps its state between lines — the thing the
// console is not — sized as asked, and gone when ended.
func TestPtySessionKeepsAShell(t *testing.T) {
	t.Setenv(consoleEnvVar, "1")
	hub := newPtyHub()
	peer := key.NewNode().Public()
	tok, why := hub.open(peer, nil, "100x30")
	if why != "" {
		t.Fatalf("open refused: %s", why)
	}
	if !ptyTokenRe.MatchString(tok) {
		t.Fatalf("token shape: %q", tok)
	}
	hub.mu.Lock()
	s := hub.sess[tok]
	hub.mu.Unlock()
	if s == nil {
		t.Fatal("no session behind the token")
	}
	// Two lines: the second sees what the first set, and the terminal's size.
	s.tty.Write([]byte("MON_PTY_T=kept\n"))
	s.tty.Write([]byte("echo got-$MON_PTY_T-$(stty size | tr ' ' x)\n"))
	ptyWaitFor(t, s, "got-kept-30x100")
	ptyResizeTTY(s.tty, 90, 20)
	s.tty.Write([]byte("echo now-$(stty size | tr ' ' x)\n"))
	ptyWaitFor(t, s, "now-20x90")
	hub.end(s)
	hub.mu.Lock()
	n := len(hub.sess)
	hub.mu.Unlock()
	if n != 0 {
		t.Errorf("an ended session must leave the table: %d", n)
	}
	hub.end(s) // twice is once
}

func TestPtyOpenGates(t *testing.T) {
	peer := key.NewNode().Public()
	t.Setenv(consoleEnvVar, "0")
	if _, why := newPtyHub().open(peer, nil, "80x24"); why != "console-off" {
		t.Errorf("console off must refuse: %q", why)
	}
	t.Setenv(consoleEnvVar, "1")
	for _, bad := range []string{"", "80", "80x", "1x1", "9999x24", "80x24x1", "axb"} {
		if _, why := newPtyHub().open(peer, nil, bad); why != "bad-target" {
			t.Errorf("size %q must be refused: %q", bad, why)
		}
	}
	hub := newPtyHub()
	defer hub.endAll()
	for i := 0; i < ptyMaxSessions; i++ {
		if _, why := hub.open(peer, nil, "80x24"); why != "" {
			t.Fatalf("session %d refused: %s", i, why)
		}
	}
	if _, why := hub.open(peer, nil, "80x24"); why != "busy" {
		t.Errorf("past the ceiling must be busy: %q", why)
	}
}

// A session whose page has gone quiet, or whose opener left the allowlist,
// is ended by the sweep.
func TestPtySweep(t *testing.T) {
	t.Setenv(consoleEnvVar, "1")
	hub := newPtyHub()
	defer hub.endAll()
	peer := key.NewNode().Public()
	tok, why := hub.open(peer, nil, "80x24")
	if why != "" {
		t.Fatalf("open refused: %s", why)
	}
	hub.mu.Lock()
	s := hub.sess[tok]
	hub.mu.Unlock()
	count := func() int { hub.mu.Lock(); defer hub.mu.Unlock(); return len(hub.sess) }
	// The sweep's other rule — nothing pinned — is the machine's state in a
	// test, so only the per-session rules are read here.
	quiet := func(at time.Time) bool { s.mu.Lock(); defer s.mu.Unlock(); return at.Sub(s.seen) > ptyQuietMax }
	if quiet(time.Now()) {
		t.Fatal("a session just opened is not quiet")
	}
	if !quiet(time.Now().Add(ptyQuietMax + time.Second)) {
		t.Error("past the quiet limit a session must read as quiet")
	}
	hub.sweep(time.Now().Add(ptyQuietMax + time.Second))
	if count() != 0 {
		t.Errorf("the sweep must end a quiet session: %d left", count())
	}
}

// The streams as the page uses them: output down a GET, keys up a POST whose
// body stays open, the size by a POST of its own — and all of it for the
// peer that opened the session and nobody else.
func TestPtyStreamsOverTheHandler(t *testing.T) {
	scratchHome(t)
	t.Setenv(consoleEnvVar, "1")
	owner, other := key.NewNode().Public(), key.NewNode().Public()
	for _, k := range []key.NodePublic{owner, other} {
		if err := allowClient(k.String()); err != nil {
			t.Fatal(err)
		}
	}
	h := newDirectHandler()
	// The process's table, with the signing-key rule off: this test opens
	// its session by hand, signed by nobody.
	saved := h.ptys.trusted
	h.ptys.trusted = nil
	defer func() { h.ptys.endAll(); h.ptys.trusted = saved }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := owner
		if r.Header.Get("X-Test-Peer") == "other" {
			k = other
		}
		h.ServeHTTP(w, r.WithContext(transport.WithPeer(r.Context(), k)))
	}))
	defer srv.Close()
	tok, why := h.ptys.open(owner, nil, "80x24")
	if why != "" {
		t.Fatalf("open refused: %s", why)
	}
	// Another allowed browser's tunnel, the right token: no session.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/pty/out?s="+tok, nil)
	req.Header.Set("X-Test-Peer", "other")
	if resp, err := srv.Client().Do(req); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another peer must find no session: %v %v", resp, err)
	}
	// A read is a GET and a write a POST.
	if resp, _ := srv.Client().Post(srv.URL+"/v1/pty/out?s="+tok, "", nil); resp == nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to the output stream must be refused: %v", resp)
	}

	out, err := srv.Client().Get(srv.URL + "/v1/pty/out?s=" + tok)
	if err != nil || out.StatusCode != http.StatusOK {
		t.Fatalf("output stream: %v %v", out, err)
	}
	defer out.Body.Close()
	got := make(chan string, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := out.Body.Read(buf)
			if n > 0 {
				got <- string(buf[:n])
			}
			if err != nil {
				close(got)
				return
			}
		}
	}()
	seen := ""
	waitFor := func(want string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for !strings.Contains(seen, want) {
			select {
			case s, ok := <-got:
				if !ok {
					t.Fatalf("stream ended before %q: %q", want, seen)
				}
				seen += s
			case <-deadline:
				t.Fatalf("no %q on the stream: %q", want, seen)
			}
		}
	}

	pr, pw := io.Pipe()
	inDone := make(chan int, 1)
	go func() {
		resp, err := srv.Client().Post(srv.URL+"/v1/pty/in?s="+tok, "application/octet-stream", pr)
		if err != nil {
			inDone <- 0
			return
		}
		resp.Body.Close()
		inDone <- resp.StatusCode
	}()
	pw.Write([]byte("echo typed-$((40+2))\n"))
	waitFor("typed-42")

	if resp, err := srv.Client().Post(srv.URL+"/v1/pty/resize?s="+tok+"&c=120&r=40", "", nil); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("resize: %v %v", resp, err)
	}
	if resp, _ := srv.Client().Post(srv.URL+"/v1/pty/resize?s="+tok+"&c=0&r=40", "", nil); resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a size out of bounds must be refused: %v", resp)
	}
	pw.Write([]byte("echo sized-$(stty size | tr ' ' x)\n"))
	waitFor("sized-40x120")

	// The shell exits: the output stream ends cleanly, the input stream is
	// told the session is gone, and the token names nothing.
	pw.Write([]byte("exit\n"))
	deadline := time.After(10 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-got:
			open = ok
		case <-deadline:
			t.Fatal("the output stream did not end with the shell")
		}
	}
	select {
	case code := <-inDone:
		if code != http.StatusGone {
			t.Errorf("the input stream must end as gone: %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Error("the input stream did not end with the shell")
	}
	pw.Close()
	if resp, _ := srv.Client().Post(srv.URL+"/v1/pty/resize?s="+tok+"&c=80&r=24", "", nil); resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("an ended session's token must name nothing: %v", resp)
	}
}

// Ending a session takes what the shell started with it: a foreground job
// in a process group of its own, deaf to the hangup.
func TestPtyEndKillsTheForegroundJob(t *testing.T) {
	t.Setenv(consoleEnvVar, "1")
	hub := newPtyHub()
	tok, why := hub.open(key.NewNode().Public(), nil, "80x24")
	if why != "" {
		t.Fatalf("open refused: %s", why)
	}
	hub.mu.Lock()
	s := hub.sess[tok]
	hub.mu.Unlock()
	s.tty.Write([]byte("sh -c 'trap \"\" HUP; echo job-$$-$((1+1))up; exec sleep 300'\n"))
	out := ptyWaitFor(t, s, "-2up")
	i := strings.LastIndex(out, "job-")
	var pid int
	if _, err := fmt.Sscanf(out[i:], "job-%d-2up", &pid); err != nil || pid <= 1 {
		t.Fatalf("no pid in %q", out)
	}
	hub.end(s)
	deadline := time.Now().Add(8 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the foreground job %d outlived its session", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
