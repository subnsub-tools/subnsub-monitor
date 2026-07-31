package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	machTok = "machine-token-for-tests-0000"
	opTok   = "operator-token-for-tests-0000"
)

func testServer(t *testing.T) (*httptest.Server, *Hub) {
	t.Helper()
	hub := newHub("")
	srv := httptest.NewServer(routes(hub, newAuth(machTok, opTok)))
	t.Cleanup(srv.Close)
	return srv, hub
}

func do(t *testing.T, srv *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if rd != nil {
		req, err = http.NewRequest(method, srv.URL+path, rd)
	} else {
		req, err = http.NewRequest(method, srv.URL+path, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestEveryEndpointRefusesWithoutTheRightToken(t *testing.T) {
	srv, _ := testServer(t)
	push := `{"agent_id":"machineone","providers":[{"id":"codex","ok":true,"limits":[{"used_percent":1}]}]}`
	for _, tc := range []struct{ name, method, path, token, body string }{
		{"push, no token", "POST", "/push", "", push},
		{"push, wrong token", "POST", "/push", "wrong-token-entirely-0000000", push},
		{"push, operator token", "POST", "/push", opTok, push},
		{"commands, no token", "GET", "/commands?agent=machineone", "", ""},
		{"commands, operator token", "GET", "/commands?agent=machineone", opTok, ""},
		{"result, no token", "POST", "/result", "", `{"agent":"machineone","id":"x"}`},
		{"stream, no token", "GET", "/api/stream", "", ""},
		{"stream, machine token", "GET", "/api/stream", machTok, ""},
		{"op, no token", "POST", "/api/op", "", `{"t":"name","agent":"machineone","name":"x"}`},
		{"op, machine token", "POST", "/api/op", machTok, `{"t":"name","agent":"machineone","name":"x"}`},
	} {
		resp := do(t, srv, tc.method, tc.path, tc.token, tc.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", tc.name, resp.StatusCode)
		}
	}
}

func TestSeparateTokensKeepTheTwoSidesApart(t *testing.T) {
	srv, _ := testServer(t)
	push := `{"agent_id":"machineone","exec":true,"providers":[{"id":"codex","ok":true,"limits":[{"used_percent":1}]}]}`
	if r := do(t, srv, "POST", "/push", machTok, push); r.StatusCode != 200 {
		t.Fatalf("push with the machine token: %d", r.StatusCode)
	}
	// A machine that leaks its token leaks pushing — not the console.
	r := do(t, srv, "POST", "/api/op", machTok, `{"t":"exec","agent":"machineone","id":"cmd1","cmd":"id"}`)
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("the machine token drove the console: %d", r.StatusCode)
	}
	r = do(t, srv, "POST", "/api/op", opTok, `{"t":"exec","agent":"machineone","id":"cmd1","cmd":"id"}`)
	if r.StatusCode != 200 {
		t.Fatalf("the operator token was refused: %d", r.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	if out["ok"] != true {
		t.Fatalf("op answer = %v", out)
	}
}

func TestOneTokenModeWatchesAndPushes(t *testing.T) {
	hub := newHub("")
	// What `-op-token` unset produces: the push token also watches.
	srv := httptest.NewServer(routes(hub, newAuth(machTok, machTok)))
	t.Cleanup(srv.Close)
	push := `{"agent_id":"machineone","providers":[{"id":"codex","ok":true,"limits":[{"used_percent":1}]}]}`
	if r := do(t, srv, "POST", "/push", machTok, push); r.StatusCode != 200 {
		t.Fatalf("push: %d", r.StatusCode)
	}
	if r := do(t, srv, "POST", "/api/op", machTok, `{"t":"name","agent":"machineone","name":"box"}`); r.StatusCode != 200 {
		t.Fatalf("op: %d", r.StatusCode)
	}
}

func TestOversizeBodiesAreRefused(t *testing.T) {
	srv, _ := testServer(t)
	big := `{"pad":"` + strings.Repeat("x", maxPushBody) + `"}`
	if r := do(t, srv, "POST", "/push", machTok, big); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize push: %d, want 413", r.StatusCode)
	}
	huge := `{"pad":"` + strings.Repeat("x", maxResultBody) + `"}`
	if r := do(t, srv, "POST", "/result", machTok, huge); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize result: %d, want 413", r.StatusCode)
	}
}

func TestMethodsAreNotInterchangeable(t *testing.T) {
	srv, _ := testServer(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/push"}, {"POST", "/commands"}, {"GET", "/result"},
		{"POST", "/api/stream"}, {"GET", "/api/op"},
	} {
		r := do(t, srv, tc.method, tc.path, machTok, "")
		if r.StatusCode != http.StatusMethodNotAllowed && r.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: %d", tc.method, tc.path, r.StatusCode)
		}
	}
}

func TestCommandsNeedsAWellFormedAgent(t *testing.T) {
	srv, _ := testServer(t)
	for _, q := range []string{"", "?agent=", "?agent=ab", "?agent=has%20space", "?agent=" + strings.Repeat("z", 40)} {
		r := do(t, srv, "GET", "/commands"+q, machTok, "")
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("agent %q: %d, want 400", q, r.StatusCode)
		}
	}
}

func TestOpRefusesMalformedActions(t *testing.T) {
	srv, hub := testServer(t)
	hub.push(pushBody("machineone", true, true))
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"not json", `{`, http.StatusBadRequest},
		{"unknown verb", `{"t":"rm-rf","agent":"machineone"}`, http.StatusBadRequest},
		{"bad agent", `{"t":"name","agent":"../etc","name":"x"}`, http.StatusBadRequest},
		{"exec with no id", `{"t":"exec","agent":"machineone","cmd":"id"}`, http.StatusBadRequest},
		{"exec with a hostile id", `{"t":"exec","agent":"machineone","id":"../../x","cmd":"id"}`, http.StatusBadRequest},
	} {
		r := do(t, srv, "POST", "/api/op", opTok, tc.body)
		if r.StatusCode != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, r.StatusCode, tc.want)
		}
	}
	// A refusal the machine owns comes back as a named error rather than a
	// status: the dashboard prints it beside the prompt.
	hub.push(pushBody("machinetwo", false, false))
	r := do(t, srv, "POST", "/api/op", opTok, `{"t":"exec","agent":"machinetwo","id":"cmd1","cmd":"id"}`)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(r.Body).Decode(&out)
	if out["error"] != string(enqNoConsole) {
		t.Fatalf("answer = %v, want a no-console error", out)
	}
}

func TestDashboardIsSelfContained(t *testing.T) {
	srv, _ := testServer(t)
	// The page is public — it is a token prompt until someone types one.
	r := do(t, srv, "GET", "/", "", "")
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	// Nothing that would fetch from another host. The SVG namespace URI is
	// an identifier rather than a request, and the install one-liner in the
	// empty state is text for a human to copy — neither is a load.
	for _, bad := range []string{
		`src="http`, `href="http`, `@import`, `url(http`, `url("http`, `fetch('http`, `fetch("http`,
		"<script src", `<link rel="stylesheet"`, "cdn.", "googleapis", "unpkg",
	} {
		if strings.Contains(page, bad) {
			t.Errorf("the page reaches for %q", bad)
		}
	}
	if !strings.Contains(page, "<title>") {
		t.Error("no title")
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	srv, _ := testServer(t)
	for _, p := range []string{"/nope", "/../etc/passwd", "/api", "/dashboard.html"} {
		if r := do(t, srv, "GET", p, "", ""); r.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, r.StatusCode)
		}
	}
}
