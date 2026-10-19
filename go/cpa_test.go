package main

// The CPA rung end to end, against a stand-in for CLI Proxy API: the listing,
// the proxied vendor calls, the precedence, and every way the proxy itself can
// be the problem. The vendor answers are the payloads adapter_test.go pins.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetCPACache() {
	cpaCache.Lock()
	cpaCache.creds, cpaCache.listedAt, cpaCache.per = nil, 0, nil
	cpaCache.failAt, cpaCache.failWhy = 0, ""
	cpaCache.Unlock()
}

type fakeCPA struct {
	key     string
	calls   []map[string]any
	listing string
}

func (f *fakeCPA) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.key {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v0/management/auth-files":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, f.listing)
		case "/v0/management/api-call":
			var call map[string]any
			body, _ := io.ReadAll(r.Body)
			if json.Unmarshal(body, &call) != nil {
				w.WriteHeader(400)
				return
			}
			f.calls = append(f.calls, call)
			hdr, _ := call["header"].(map[string]any)
			if hdr["Authorization"] != "Bearer $TOKEN$" {
				t.Errorf("the placeholder must reach CPA unsubstituted, got %v", hdr["Authorization"])
			}
			target, _ := call["url"].(string)
			respond := func(status int, vendorBody any) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"status_code": status, "header": map[string]any{}, "body": vendorBody})
			}
			switch {
			case strings.Contains(target, "/api/oauth/usage"):
				if hdr["anthropic-beta"] != "oauth-2025-04-20" {
					t.Errorf("the proxy channel must carry the beta header, got %v", hdr["anthropic-beta"])
				}
				if call["auth_index"] == "2" {
					respond(401, `{"error":"expired"}`)
					return
				}
				respond(200, map[string]any{"limits": []any{
					map[string]any{"kind": "session", "percent": 22, "is_active": true},
					map[string]any{"kind": "weekly_all", "percent": 54},
				}})
			case strings.HasSuffix(target, "/api/oauth/profile"):
				respond(200, map[string]any{"account": map[string]any{"has_claude_max": true}})
			case strings.HasSuffix(target, "/wham/usage"):
				if hdr["Chatgpt-Account-Id"] != "acct-1" {
					t.Errorf("codex call lacks the account id header: %v", hdr)
				}
				// As text — the other shape CPA hands back.
				respond(200, codexUsageFixture)
			default:
				respond(404, "")
			}
		default:
			w.WriteHeader(404)
		}
	}
}

const codexJWT = "h.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiYWNjdC0xIn19.s"

func TestCollectCPAEndToEnd(t *testing.T) {
	f := &fakeCPA{key: "secret", listing: `{"files":[
	  {"name":"claude-a@example.com.json","provider":"claude","auth_index":"1","email":"a@example.com"},
	  {"name":"claude-b@example.com.json","provider":"claude","auth_index":"2","email":"b@example.com"},
	  {"name":"codex-c.json","type":"codex","auth_index":3,"email":"c@example.com","id_token":"` + codexJWT + `"},
	  {"name":"claude-off.json","provider":"claude","auth_index":"4","disabled":true},
	  {"name":"gemini.json","provider":"gemini","auth_index":"5"}
	]}`}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	t.Setenv(cpaURLEnv, srv.URL)
	t.Setenv(cpaKeyEnv, "secret")
	resetCPACache()

	res := collectCPA()
	if !res.configured || res.status != nil {
		t.Fatalf("configured, no status tile: %+v", res)
	}
	ids := []string{}
	for _, p := range res.providers {
		ids = append(ids, p.ID)
	}
	if got := joinIDs(ids); got != "claude,claude@2,codex" {
		t.Fatalf("ids: %s", got)
	}
	a := res.providers[0]
	if !a.OK || a.Source != "cpa" || a.Account != "a@example.com" || len(a.Limits) != 2 || a.PlanType == nil || *a.PlanType != "Max" {
		t.Errorf("first claude: %+v", a)
	}
	b := res.providers[1]
	if b.OK || b.Error != "token-expired" || b.DetailCode != "http-401" || b.Account != "b@example.com" {
		t.Errorf("second claude should report the vendor's 401: %+v", b)
	}
	c := res.providers[2]
	if !c.OK || c.ID != "codex" || len(c.Limits) != 2 || c.PlanType == nil || *c.PlanType != "Pro" || c.Account != "c@example.com" {
		t.Errorf("codex: %+v", c)
	}

	// Folded into the natives: CPA's claude rows stand where claude stood,
	// codex likewise, copilot (no CPA adapter) stays native. The gemini file
	// CPA lists has no adapter and no native collector, so it shows nowhere.
	native := []Provider{{ID: "codex", OK: true, Source: "cli"}, {ID: "claude", OK: true, Source: "api"}, {ID: "copilot", OK: true, Source: "api"}}
	merged := mergeCPA(native, res)
	ids = ids[:0]
	for _, p := range merged {
		ids = append(ids, p.ID+"/"+p.Source)
	}
	if got := joinIDs(ids); got != "codex/cpa,claude/cpa,claude@2/cpa,copilot/api" {
		t.Errorf("merged: %s", got)
	}

	// The second pass inside the floor serves the cache: no new calls.
	n := len(f.calls)
	collectCPA()
	if len(f.calls) != n {
		t.Errorf("a second collect inside the floor made %d more calls", len(f.calls)-n)
	}
}

func TestCollectCPAWhenTheProxyIsTheProblem(t *testing.T) {
	f := &fakeCPA{key: "secret", listing: `{"files":[]}`}
	srv := httptest.NewServer(f.handler(t))
	t.Setenv(cpaURLEnv, srv.URL)

	t.Setenv(cpaKeyEnv, "wrong")
	resetCPACache()
	res := collectCPA()
	if res.status == nil || res.status.ID != "cpa" || res.status.DetailCode != "cpa-key" || res.status.Error != "api-error" {
		t.Errorf("wrong key: %+v", res.status)
	}

	t.Setenv(cpaKeyEnv, "secret")
	resetCPACache()
	res = collectCPA()
	if res.status != nil || len(res.providers) != 0 || !res.configured {
		t.Errorf("empty listing is not an error: %+v", res)
	}

	srv.Close()
	resetCPACache()
	res = collectCPA()
	if res.status == nil || res.status.DetailCode != "cpa-down" || res.status.Error != "unreachable" {
		t.Errorf("proxy down: %+v", res.status)
	}

	t.Setenv(cpaURLEnv, "http://10.1.1.1:8317")
	resetCPACache()
	res = collectCPA()
	if res.status == nil || res.status.DetailCode != "cpa-url" {
		t.Errorf("plain http off loopback: %+v", res.status)
	}

	// Nothing of that reaches the board: the natives are exactly as they were.
	native := []Provider{{ID: "claude", OK: true, Source: "api"}}
	merged := mergeCPA(native, res)
	if len(merged) != 1 || merged[0].Source != "api" {
		t.Errorf("merge with a broken proxy must change nothing: %+v", merged)
	}
}

func TestCPAAntigravityNeedsAProject(t *testing.T) {
	f := &fakeCPA{key: "secret", listing: `{"files":[
	  {"name":"ag-1.json","provider":"antigravity","auth_index":"1","email":"x@example.com"},
	  {"name":"ag-2.json","provider":"antigravity","auth_index":"2","email":"y@example.com","attributes":{"gemini_virtual_project":"proj-9"}}
	]}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/api-call" {
			body, _ := io.ReadAll(r.Body)
			var call map[string]any
			json.Unmarshal(body, &call)
			f.calls = append(f.calls, call)
			w.Header().Set("Content-Type", "application/json")
			// The plan's own call (v4): no project needed, and asked only
			// once the quota call has gone through.
			if target, _ := call["url"].(string); strings.HasSuffix(target, ":loadCodeAssist") {
				if call["data"] != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
					t.Errorf("assist body: %v", call["data"])
				}
				json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{
					"currentTier": map[string]any{"id": "free-tier"}, "paidTier": map[string]any{"id": "g1-pro-tier"}}})
				return
			}
			if call["data"] != `{"project":"proj-9"}` {
				t.Errorf("body: %v", call["data"])
			}
			json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": map[string]any{"groups": []any{
				map[string]any{"displayName": "Gemini", "buckets": []any{
					map[string]any{"bucketId": "g-5h", "window": "five_hour", "remainingFraction": 0.75}}}}}})
			return
		}
		f.handler(t)(w, r)
	}))
	defer srv.Close()
	t.Setenv(cpaURLEnv, srv.URL)
	t.Setenv(cpaKeyEnv, "secret")
	resetCPACache()
	res := collectCPA()
	if len(res.providers) != 2 {
		t.Fatalf("got %+v", res.providers)
	}
	if res.providers[0].OK || res.providers[0].DetailCode != "cpa-cred-field" || res.providers[0].DetailArg != "project_id" {
		t.Errorf("no project id: %+v", res.providers[0])
	}
	p := res.providers[1]
	if !p.OK || p.ID != "antigravity@2" || len(p.Limits) != 1 || p.Limits[0].UsedPercent != 25 || *p.Limits[0].Scope != "Gemini" {
		t.Errorf("with project id: %+v", p)
	}
	if len(f.calls) != 2 {
		t.Errorf("only the credential with a project should be asked (its quota, then its plan), got %d calls", len(f.calls))
	}
	if p.PlanType == nil || *p.PlanType != "Pro" {
		t.Errorf("plan from the paid tier: %v", p.PlanType)
	}
}

// The installer writes MON_CPA_URL / MON_CPA_KEY into the private config file,
// and installedEnv reads that file by an allow-list of keys — a key missing
// from the list is a setting that installs perfectly and never takes effect
// (found on the first real machine, 2026-09-09: the file carried both lines
// and the helper reported no CPA at all).
func TestCPASettingsComeFromTheInstalledFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(cpaURLEnv, "")
	t.Setenv(cpaKeyEnv, "")
	os.Unsetenv(cpaURLEnv)
	os.Unsetenv(cpaKeyEnv)
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "SUBNSUB_MONITOR_TOKEN=tok\nMON_CPA_URL=http://127.0.0.1:8317/ \nMON_CPA_KEY= s3cret \n"
	if err := os.WriteFile(filepath.Join(dir, tokenEnvFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, code := loadCPAConfig()
	if code != "" || c.base == nil {
		t.Fatalf("settings not read from the file: %+v %q", c, code)
	}
	if c.base.String() != "http://127.0.0.1:8317" {
		t.Errorf("url: %s", c.base)
	}
	if c.key != "s3cret" {
		t.Errorf("key: %q (loadCPAConfig trims; the file reader must not drop it)", c.key)
	}
}
