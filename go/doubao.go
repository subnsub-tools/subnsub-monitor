package main

// Doubao: quota via arkcli, the vendor's own CLI. Amp's cost rung, and the
// only one of the CLI collectors whose output is structured:
//
//	$ arkcli usage plan --format json
//	{"viewer":{"auth_method":"sso"},
//	 "items":[{"product":"coding-plan","subscribed":true,
//	   "periods":[{"label":"5-hour","percent":42.5,"reset_at":"2026-08-01T12:00:00Z"}],
//	   "updated_at":1754000000}]}
//
// No credential opened or held. CodexBar's Doubao provider also implements a
// Volcengine AK/SK signed path; that is the manual-key world this helper
// stays out of, so only the CLI path is here. Fields decode loose (codex.go's
// lesson: a surprise type in a field we don't read must not sink the row).

import (
	"encoding/json"
	"strings"
	"time"
)

const arkcliBinEnv = "MON_ARKCLI_BIN"

var doubaoCache provCache

func collectDoubao() Provider {
	return doubaoCache.collect(fetchDoubao)
}

// The products arkcli reports and the id prefix each maps to, so an
// agent-plan window does not collide with a coding-plan one.
var doubaoProducts = map[string]string{
	"coding-plan":      "",
	"agent-plan":       "agent_",
	"coding-plan-team": "coding_team_",
	"agent-plan-team":  "agent_team_",
}

// A period label to a window key and its minutes. The label vocabulary is
// arkcli's; anything unrecognised keeps its own label and carries no window.
func doubaoWindow(label string) (string, *float64) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "session", "5-hour", "five_hour", "5h":
		return "session", fp(300)
	case "weekly", "week":
		return "weekly", fp(10080)
	case "monthly", "month":
		return "monthly", fp(43200)
	}
	return "", nil
}

func doubaoReset(v any) *float64 {
	switch x := v.(type) {
	case string:
		if e := isoToEpoch(x); e > 0 {
			return fp(e)
		}
	case float64:
		if x <= 0 {
			return nil
		}
		if x >= 1e11 {
			x /= 1000 // milliseconds
		}
		if finite(x) {
			return fp(x)
		}
	}
	return nil
}

func fetchDoubao() Provider {
	p := Provider{ID: "doubao", Name: "Doubao", Source: "cli", CapturedAt: now()}
	fail := func(err, detail string) Provider {
		p.Error, p.Detail = err, detail
		return p
	}

	bin := vendorBinary(arkcliBinEnv, vendorCandidates("arkcli"))
	if bin == "" {
		return fail("not-installed", "找不到可用的 arkcli 命令")
	}
	out, exited, err := runVendorCLI(bin, []string{"usage", "plan", "--format", "json"},
		[]string{"VOLC", "VOLCENGINE_", "DOUBAO_", "ARK_"}, 15*time.Second)
	if err != nil {
		if err == errAmpTimeout {
			return fail("unreachable", "arkcli 超时")
		}
		if exited && arkcliSignedOut(out) {
			return fail("not-signed-in", "arkcli 未登录；跑一次 arkcli login。")
		}
		return fail("cli-failed", "arkcli 执行失败")
	}

	var doc struct {
		Viewer *struct {
			AuthMethod string `json:"auth_method"`
		} `json:"viewer"`
		Items []struct {
			Product    string `json:"product"`
			Subscribed *bool  `json:"subscribed"`
			Error      string `json:"error"`
			Periods    []struct {
				Label   string `json:"label"`
				Percent any    `json:"percent"`
				ResetAt any    `json:"reset_at"`
			} `json:"periods"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &doc) != nil {
		return fail("api-error", "arkcli 返回的不是预期 JSON")
	}
	if doc.Viewer != nil && strings.EqualFold(strings.TrimSpace(doc.Viewer.AuthMethod), "none") {
		return fail("not-signed-in", "arkcli 未登录；跑一次 arkcli login。")
	}

	for _, it := range doc.Items {
		prefix, known := doubaoProducts[it.Product]
		if !known || (it.Subscribed != nil && !*it.Subscribed) {
			continue
		}
		for _, w := range it.Periods {
			pct := asNum(w.Percent)
			if pct == nil {
				continue
			}
			v := *pct
			if v < 0 {
				v = 0
			}
			if v > 100 {
				v = 100
			}
			key, mins := doubaoWindow(w.Label)
			if key == "" {
				key = tame(w.Label, 24)
				if key == "" {
					key = "window"
				}
			}
			l := Limit{Key: prefix + key, UsedPercent: round2(v), ResetsAt: doubaoReset(w.ResetAt)}
			if mins != nil {
				l.WindowMinutes = mins
				l.WindowLabel = windowLabel(*mins)
			}
			p.Limits = append(p.Limits, l)
		}
	}

	if len(p.Limits) == 0 {
		if exited {
			return fail("cli-failed", "arkcli 以非零状态退出")
		}
		return fail("no-readings", "arkcli 没有返回可用的额度窗口")
	}

	p.OK = true
	p.RecordedAt = fp(now())
	if doc.Viewer != nil {
		if plan := tame(doc.Viewer.AuthMethod, 32); plan != "" {
			p.PlanType = sp(plan)
		}
	}
	return p
}

func arkcliSignedOut(s string) bool {
	low := strings.ToLower(s)
	for _, phrase := range []string{"not logged in", "not authenticated",
		"authentication required", "login required", "please login", "please log in"} {
		if strings.Contains(low, phrase) {
			return true
		}
	}
	return false
}
