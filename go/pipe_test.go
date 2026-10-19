package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subnsubmonitor/transport"

	"tailscale.com/types/key"
)

func TestTransferSwitchIsOnUnlessFiledOff(t *testing.T) {
	home := scratchHome(t)
	os.Unsetenv(transferEnvVar)
	if !transferEnabled() {
		t.Fatal("transfer is off on a fresh install; it is meant to be on")
	}
	if err := setTransfer(false); err != nil {
		t.Fatal(err)
	}
	if transferEnabled() {
		t.Fatal("transfer is on after setTransfer(false)")
	}
	fi, err := os.Stat(filepath.Join(home, ".config", "subnsub-monitor", transferOffFile))
	if err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("switch file: %v %v", err, fi)
	}
	if err := setTransfer(true); err != nil {
		t.Fatal(err)
	}
	if !transferEnabled() {
		t.Fatal("transfer is off after setTransfer(true)")
	}
	// The environment wins over the file, both ways.
	t.Setenv(transferEnvVar, "0")
	if transferEnabled() {
		t.Fatal("MON_TRANSFER=0 did not override")
	}
	setTransfer(false)
	t.Setenv(transferEnvVar, "on")
	if !transferEnabled() {
		t.Fatal("MON_TRANSFER=on did not override the file")
	}
}

func TestPipeHubInviteAttachEnd(t *testing.T) {
	hub := newPipeHub()
	now := time.Unix(1_700_000_000, 0)
	hub.now = func() time.Time { return now }
	host, guest, other := key.NewNode().Public(), key.NewNode().Public(), key.NewNode().Public()

	for _, sid := range []string{"", "short", strings.Repeat("a", 33), "has space", "bad/char"} {
		if _, _, code := hub.invite(host, sid, guest); code != "bad-frame" {
			t.Errorf("sid %q accepted (%s)", sid, code)
		}
	}
	if _, _, code := hub.invite(host, "session-01", host); code != "bad-frame" {
		t.Error("a host inviting itself was accepted")
	}
	first, ttl, code := hub.invite(host, "session-01", guest)
	if code != "" || ttl != pipeInviteTTL || first == nil {
		t.Fatalf("invite: %s %v", code, ttl)
	}
	if g := hub.guests(); len(g) != 1 || g[0] != guest || !hub.isGuest(guest) || hub.isGuest(host) {
		t.Fatalf("guests %v", g)
	}
	// Somebody else's session id is not theirs to take; the host's own
	// retry replaces the session.
	if _, _, code := hub.invite(other, "session-01", guest); code != "busy" {
		t.Errorf("another host took the id (%s)", code)
	}
	if _, _, code := hub.invite(host, "session-01", other); code != "" {
		t.Errorf("the host's retry was refused (%s)", code)
	}
	if hub.isGuest(guest) || !hub.isGuest(other) {
		t.Error("the retry did not replace the guest")
	}
	// A stream of the REPLACED session ending must not end the replacement.
	select {
	case <-first.done:
	default:
		t.Fatal("the replaced session is not done")
	}
	hub.end(first, errPipeGone)
	if hub.count() != 1 || !hub.isGuest(other) {
		t.Fatal("ending the replaced session took the replacement with it")
	}
	if hub.admittedGuests() != 2 {
		t.Fatalf("admitted %d guests, want 2", hub.admittedGuests())
	}
	guest = other

	// Four streams, each once, by the right party.
	s, d, code := hub.attach("session-01", host, true)
	if code != "" || d != s.h2g || !s.h2g.sendOn {
		t.Fatalf("host send: %s", code)
	}
	if _, _, code := hub.attach("session-01", host, true); code != "busy" {
		t.Errorf("host send twice: %s", code)
	}
	if _, d2, code := hub.attach("session-01", guest, false); code != "" || d2 != s.h2g || !s.h2g.recvOn {
		t.Fatalf("guest recv: %s", code)
	}
	if _, d3, code := hub.attach("session-01", guest, true); code != "" || d3 != s.g2h {
		t.Fatalf("guest send: %s", code)
	}
	if _, d4, code := hub.attach("session-01", host, false); code != "" || d4 != s.g2h {
		t.Fatalf("host recv: %s", code)
	}
	if !s.attachedAll() {
		t.Fatal("not all attached")
	}
	if _, _, code := hub.attach("session-01", key.NewNode().Public(), true); code != "not-allowed" {
		t.Errorf("a stranger attached (%s)", code)
	}
	if _, _, code := hub.attach("no-such-session", host, true); code != "no-session" {
		t.Errorf("unknown sid: %s", code)
	}

	// Bytes cross, and end() wakes every blocked party.
	go s.h2g.w.Write([]byte("ping"))
	buf := make([]byte, 8)
	n, err := s.h2g.r.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("read %q %v", buf[:n], err)
	}
	readErr := make(chan error, 1)
	go func() { _, err := s.g2h.r.Read(buf); readErr <- err }()
	hub.end(s, nil)
	select {
	case err := <-readErr:
		if err == nil {
			t.Error("a read on an ended session returned no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("end() did not wake the blocked read")
	}
	select {
	case <-s.done:
	default:
		t.Error("done is not closed after end()")
	}
	if hub.isGuest(guest) || hub.count() != 0 {
		t.Error("the ended session still admits its guest")
	}
	if _, _, code := hub.attach("session-01", host, true); code != "no-session" {
		t.Errorf("attach after end: %s", code)
	}
}

func TestPipeHubCapEvictionAndSweep(t *testing.T) {
	hub := newPipeHub()
	now := time.Unix(1_700_000_000, 0)
	hub.now = func() time.Time { return now }
	host := key.NewNode().Public()
	for i := 0; i < pipeMaxSessions; i++ {
		if _, _, code := hub.invite(host, "session-0"+string(rune('a'+i)), key.NewNode().Public()); code != "" {
			t.Fatalf("invite %d: %s", i, code)
		}
		now = now.Add(time.Second)
	}
	// Full, and nothing idle enough to evict.
	if _, _, code := hub.invite(host, "session-full", key.NewNode().Public()); code != "busy" {
		t.Fatalf("fifth invite: %s, want busy", code)
	}
	// Past the eviction idle, the oldest untouched session makes room.
	now = now.Add(pipeEvictIdle)
	if _, _, code := hub.invite(host, "session-full", key.NewNode().Public()); code != "" {
		t.Fatalf("fifth invite after idle: %s", code)
	}
	if hub.count() != pipeMaxSessions {
		t.Fatalf("%d sessions after eviction", hub.count())
	}
	if _, _, code := hub.attach("session-0a", host, true); code != "no-session" {
		t.Errorf("the oldest session survived eviction (%s)", code)
	}
	// The invite rate: every invite grows the carrier's allowlist, so a
	// host gets a dozen a minute and then waits.
	hub = newPipeHub()
	hub.now = func() time.Time { return now }
	for i := 0; i < pipeInviteRate; i++ {
		sid := "rate-" + strings.Repeat("x", 4) + string(rune('a'+i))
		sess, _, code := hub.invite(host, sid, key.NewNode().Public())
		if code != "" {
			t.Fatalf("invite %d under the rate: %s", i, code)
		}
		hub.end(sess, nil) // ended at once: the cap is on invites, not sessions
	}
	if _, _, code := hub.invite(host, "rate-xxxxz", key.NewNode().Public()); code != "busy" {
		t.Fatalf("invite over the rate: %s, want busy", code)
	}
	now = now.Add(time.Minute + time.Second)
	if _, _, code := hub.invite(host, "rate-xxxxz", key.NewNode().Public()); code != "" {
		t.Fatalf("invite a minute later: %s", code)
	}

	// The sweep: an invite nobody completed is forgotten after its TTL.
	hub = newPipeHub()
	hub.now = func() time.Time { return now }
	guest := key.NewNode().Public()
	hub.invite(host, "session-ttl", guest)
	hub.sweep(now.Add(pipeInviteTTL - time.Second))
	if hub.count() != 1 {
		t.Fatal("swept before its TTL")
	}
	hub.sweep(now.Add(pipeInviteTTL + time.Second))
	if hub.count() != 0 || hub.isGuest(guest) {
		t.Fatal("an unattached invite outlived its TTL")
	}
	// A session everybody attached lives while bytes move, and ends idle.
	hub.invite(host, "session-idle", guest)
	s, _, _ := hub.attach("session-idle", host, true)
	hub.attach("session-idle", host, false)
	hub.attach("session-idle", guest, true)
	hub.attach("session-idle", guest, false)
	hub.sweep(now.Add(pipeInviteTTL + time.Minute))
	if hub.count() != 1 {
		t.Fatal("an attached session was swept as an unfinished invite")
	}
	now = now.Add(pipeIdle - time.Second)
	hub.touch(s)
	hub.sweep(now.Add(pipeIdle - time.Second))
	if hub.count() != 1 {
		t.Fatal("swept while bytes were still moving")
	}
	hub.sweep(now.Add(pipeIdle + time.Second))
	if hub.count() != 0 {
		t.Fatal("an idle session was not swept")
	}
	// A host struck from the allowlist loses its sessions at the next sweep.
	allowed := true
	hub.hostOK = func(k key.NodePublic) bool { return allowed && k == host }
	hub.invite(host, "session-revoked", guest)
	hub.sweep(now)
	if hub.count() != 1 {
		t.Fatal("an allowed host's session was swept")
	}
	allowed = false
	hub.sweep(now)
	if hub.count() != 0 || hub.isGuest(guest) {
		t.Fatal("a struck host's session survived the sweep")
	}
}

// The routes, over real HTTP: a host on the allowlist invites a guest, the
// two attach their streams, bytes cross both ways, and a guest can reach
// nothing but the pipe.
func TestPipeRoutesCoupleHostAndGuest(t *testing.T) {
	scratchHome(t)
	os.Unsetenv(transferEnvVar)
	hostK, guestK := key.NewNode(), key.NewNode()
	if err := allowClient(hostK.Public().String()); err != nil {
		t.Fatal(err)
	}
	h := newDirectHandler()
	var admitted []key.NodePublic
	h.admit = func(k key.NodePublic) { admitted = append(admitted, k) }
	// The gate, as a test sees it: the peer named by a header.
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k, ok := transport.ParseNodeKey(r.Header.Get("X-Test-Peer")); ok {
			r = r.WithContext(transport.WithPeer(r.Context(), k))
		}
		h.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(gated)
	defer srv.Close()
	client := srv.Client()
	as := func(k key.NodePrivate, method, path string, body io.Reader) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Test-Peer", k.Public().String())
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	frameOf := func(resp *http.Response) (transport.Frame, map[string]any) {
		t.Helper()
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		f, body, err := directJSON(b)
		if err != nil {
			t.Fatalf("not a frame: %v %q", err, b)
		}
		return f, body
	}
	inviteBody := func(sid, guest string) string {
		f, _ := transport.New(transport.TypePipe, "", "inv-1", transport.Pipe{Op: "invite", SID: sid, Guest: guest})
		b, _ := transport.Encode(f)
		return string(b)
	}

	// The host's hello names transfer; with the switch off it does not, and
	// an invite is refused.
	_, body := frameOf(as(hostK, http.MethodGet, "/v1/hello", nil))
	if caps, _ := body["caps"].([]any); len(caps) != 2 || caps[0] != transport.CapObserve || caps[1] != transport.CapTransfer {
		t.Fatalf("host hello caps %v", body["caps"])
	}
	setTransfer(false)
	_, body = frameOf(as(hostK, http.MethodGet, "/v1/hello", nil))
	if caps, _ := body["caps"].([]any); len(caps) != 1 {
		t.Fatalf("hello names transfer with the switch off: %v", body["caps"])
	}
	resp := as(hostK, http.MethodPost, "/v1/pipe", strings.NewReader(inviteBody("session-off", guestK.Public().String())))
	if f, body := frameOf(resp); resp.StatusCode != 403 || f.Type != transport.TypeError || body["code"] != "transfer-off" {
		t.Fatalf("invite with the switch off: %d %v", resp.StatusCode, body)
	}
	setTransfer(true)

	// Before the invite the guest is nobody: not on the file, not a guest.
	if h.guestOf(httptest.NewRequest("GET", "/", nil).WithContext(transport.WithPeer(t.Context(), guestK.Public()))) != true {
		t.Fatal("an unknown key is not treated as a guest")
	}
	// Garbage invites.
	for _, b := range []string{"{nope", inviteBody("session-01", "nodekey:zz"), inviteBody("x", guestK.Public().String()), inviteBody("session-01", hostK.Public().String())} {
		resp := as(hostK, http.MethodPost, "/v1/pipe", strings.NewReader(b))
		if _, body := frameOf(resp); resp.StatusCode != 400 || body["code"] != "bad-frame" {
			t.Errorf("invite %q: %d %v", b, resp.StatusCode, body)
		}
	}
	// A frame of another type on the invite door.
	hf, _ := transport.New(transport.TypeHello, "", "x", transport.Hello{Caps: []string{"observe"}})
	hb, _ := transport.Encode(hf)
	if resp := as(hostK, http.MethodPost, "/v1/pipe", strings.NewReader(string(hb))); resp.StatusCode != 400 {
		t.Errorf("hello on the invite door: %d", resp.StatusCode)
	}
	if resp := as(hostK, http.MethodGet, "/v1/pipe", nil); resp.StatusCode != 405 {
		t.Errorf("GET on the invite door: %d", resp.StatusCode)
	}

	// The invite.
	resp = as(hostK, http.MethodPost, "/v1/pipe", strings.NewReader(inviteBody("session-01", guestK.Public().String())))
	f, body := frameOf(resp)
	if resp.StatusCode != 200 || f.Type != transport.TypePipe || f.ID != "inv-1" || body["ok"] != true || body["sid"] != "session-01" || body["ttl_sec"] != float64(pipeInviteTTL/time.Second) {
		t.Fatalf("invite answer: %d %+v %v", resp.StatusCode, f, body)
	}
	if len(admitted) != 1 || admitted[0] != guestK.Public() {
		t.Fatalf("the carrier was told %v", admitted)
	}
	if g := h.pipes.guests(); len(g) != 1 || g[0] != guestK.Public() {
		t.Fatalf("gate guests %v", g)
	}

	// The guest: a hello that says transfer and nothing else, and every
	// other door shut.
	f, body = frameOf(as(guestK, http.MethodGet, "/v1/hello", nil))
	if caps, _ := body["caps"].([]any); f.AgentID != "transfer" || len(caps) != 1 || caps[0] != transport.CapTransfer || body["helper_version"] != nil {
		t.Fatalf("guest hello %+v %v", f, body)
	}
	for _, p := range []string{"/v1/snapshot", "/v1/events", "/v1/history"} {
		resp := as(guestK, http.MethodGet, p, nil)
		if f, body := frameOf(resp); resp.StatusCode != 403 || body["code"] != "not-allowed" || f.AgentID != guestAgentID {
			t.Errorf("guest %s: %d %v %+v", p, resp.StatusCode, body, f)
		}
	}
	for _, p := range []string{"/v1/command", "/v1/pipe"} {
		resp := as(guestK, http.MethodPost, p, strings.NewReader(inviteBody("session-02", hostK.Public().String())))
		if _, body := frameOf(resp); resp.StatusCode != 403 || body["code"] != "not-allowed" {
			t.Errorf("guest POST %s: %d %v", p, resp.StatusCode, body)
		}
	}
	// A stranger's sid, and a stranger.
	if resp := as(guestK, http.MethodGet, "/v1/pipe/recv?sid=session-99", nil); resp.StatusCode != 404 {
		t.Errorf("unknown sid: %d", resp.StatusCode)
	} else if f, _ := frameOf(resp); f.AgentID != guestAgentID {
		t.Errorf("a guest's refusal names the machine: %+v", f)
	}
	if resp := as(key.NewNode(), http.MethodGet, "/v1/pipe/recv?sid=session-01", nil); resp.StatusCode != 403 {
		t.Errorf("stranger on the pipe: %d", resp.StatusCode)
	}

	// Streams. The guest listens first; the host's bytes come out there.
	guestRecv := as(guestK, http.MethodGet, "/v1/pipe/recv?sid=session-01", nil)
	if guestRecv.StatusCode != 200 {
		t.Fatalf("guest recv: %d", guestRecv.StatusCode)
	}
	hostPR, hostPW := io.Pipe()
	hostSendDone := make(chan *http.Response, 1)
	go func() { hostSendDone <- as(hostK, http.MethodPost, "/v1/pipe/send?sid=session-01", hostPR) }()
	if _, err := hostPW.Write([]byte("host says hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 15)
	if _, err := io.ReadFull(guestRecv.Body, got); err != nil || string(got) != "host says hello" {
		t.Fatalf("guest read %q %v", got, err)
	}
	// The other way.
	hostRecv := as(hostK, http.MethodGet, "/v1/pipe/recv?sid=session-01", nil)
	if hostRecv.StatusCode != 200 {
		t.Fatalf("host recv: %d", hostRecv.StatusCode)
	}
	guestPR, guestPW := io.Pipe()
	guestSendDone := make(chan *http.Response, 1)
	go func() { guestSendDone <- as(guestK, http.MethodPost, "/v1/pipe/send?sid=session-01", guestPR) }()
	if _, err := guestPW.Write([]byte("guest answers")); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, 13)
	if _, err := io.ReadFull(hostRecv.Body, got); err != nil || string(got) != "guest answers" {
		t.Fatalf("host read %q %v", got, err)
	}
	// A second send stream from the same party is refused: each of the four
	// is claimed once.
	if resp := as(hostK, http.MethodPost, "/v1/pipe/send?sid=session-01", strings.NewReader("x")); resp.StatusCode != 409 {
		t.Errorf("duplicate send stream: %d", resp.StatusCode)
	}
	if h.pipes.count() != 1 {
		t.Fatalf("%d sessions", h.pipes.count())
	}

	// The host says goodbye by ending its body: its send returns, the
	// session ends, and every other stream ends with it — the guest's
	// receive reaches EOF, the guest's send is answered without waiting for
	// more bytes.
	hostPW.Close()
	select {
	case resp := <-hostSendDone:
		if resp.StatusCode != 204 {
			t.Errorf("host send after goodbye: %d", resp.StatusCode)
		}
		resp.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the host's send stream did not end")
	}
	rest, err := io.ReadAll(guestRecv.Body)
	if err != nil || len(rest) != 0 {
		t.Errorf("guest recv after goodbye: %q %v", rest, err)
	}
	guestRecv.Body.Close()
	select {
	case resp := <-guestSendDone:
		if resp.StatusCode != 410 {
			t.Errorf("guest send after the session ended: %d", resp.StatusCode)
		}
		resp.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the guest's send stream did not end with the session")
	}
	guestPW.Close()
	hostRecv.Body.Close()
	if h.pipes.count() != 0 || len(h.pipes.guests()) != 0 {
		t.Fatal("the session outlived the goodbye")
	}
	// The guest is nobody again.
	if resp := as(guestK, http.MethodGet, "/v1/pipe/recv?sid=session-01", nil); resp.StatusCode != 404 {
		t.Errorf("guest after the session: %d", resp.StatusCode)
	}
	var _ = json.Marshal
}
