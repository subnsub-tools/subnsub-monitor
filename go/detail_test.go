package main

// The failure-detail vocabulary, checked in both directions.
//
// A detail code is a wire value with four copies of itself: the phrase in
// detail.go, the table in monitor.js, and the English and Chinese strings in
// translations.js. Nothing at runtime notices when one of them is missing —
// the page just silently falls back a rung, which is exactly the class of bug
// this whole mechanism exists to end. So it is checked here.
//
// The file scan is deliberate rather than a registry of codes: a registry
// would only prove the registry agrees with itself, while what can actually
// rot is a call site passing a code nobody ever wrote a sentence for.
//
// The site-side half is skipped outside the monorepo (the public mirror ships
// helper/go without the panel), like coverage_test.go.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// `fail("not-signed-in", "creds-missing", …)` and `p.failWith("no-sessions",
// "no-home")` — the collectors' two spellings of the same call.
var detailCallRE = regexp.MustCompile(`fail(?:With)?\("[a-z-]*", "([a-z][a-z0-9-]*)"`)

// gemini's token refresh answers with a (token, error, code, arg) tuple rather
// than calling fail() itself, since its caller decides what to do with it.
var detailTupleRE = regexp.MustCompile(`return "", "[a-z-]+", "([a-z][a-z0-9-]*)"`)

func codesUsedInPackage(t *testing.T) map[string]bool {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil || len(names) == 0 {
		t.Fatalf("no sources to scan: %v", err)
	}
	used := map[string]bool{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range []*regexp.Regexp{detailCallRE, detailTupleRE} {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				used[m[1]] = true
			}
		}
	}
	if len(used) == 0 {
		t.Fatal("scanned every collector and found no detail codes — the test has rotted")
	}
	return used
}

func TestEveryDetailCodeHasAPhrase(t *testing.T) {
	used := codesUsedInPackage(t)
	for code := range used {
		if _, ok := detailPhrases[code]; !ok {
			t.Errorf("collectors send detail code %q with no phrase in detail.go", code)
		}
	}
	for code := range detailPhrases {
		if !used[code] {
			t.Errorf("detail.go carries a phrase for %q that nothing sends", code)
		}
	}
}

func TestDetailTextSplicesAndRefusesUnknownCodes(t *testing.T) {
	if got := detailText("cli-missing", "amp"); got != "no usable amp command was found" {
		t.Errorf("argument not spliced: %q", got)
	}
	if got := detailText("no-such-code", "amp"); got != "" {
		t.Errorf("unknown code must render nothing, got %q", got)
	}
	// A control character in a vendor's error string must not ride out to
	// somebody else's screen, and the cut is by rune: half a character is not
	// a character. 200 wide characters is well past the limit.
	long := strings.Repeat("东", 200)
	if got := detailArgText(long); len([]rune(got)) != detailArgMax {
		t.Errorf("argument cut to %d runes, want %d", len([]rune(got)), detailArgMax)
	}
	if got := detailArgText("dial\x00tcp\nrefused"); strings.ContainsAny(got, "\x00\n") {
		t.Errorf("control characters survived: %q", got)
	}
}

func TestFailWithFillsBothForms(t *testing.T) {
	var p Provider
	p.failWith("not-installed", "cli-missing", "auggie")
	if p.Error != "not-installed" || p.DetailCode != "cli-missing" || p.DetailArg != "auggie" {
		t.Fatalf("failWith left a field unset: %+v", p)
	}
	if p.Detail != "no usable auggie command was found" {
		t.Errorf("sentence and code disagree: %q", p.Detail)
	}
	// No argument is a normal case, and must not leave a hole behind.
	p.failWith("api-error", "http-shape")
	if p.DetailArg != "" || !strings.HasPrefix(p.Detail, "the endpoint answered in a shape") {
		t.Errorf("argument-free code rendered %q / %q", p.DetailArg, p.Detail)
	}
}

// ── the site side ─────────────────────────────────────────────────────────

var panelEntryRE = regexp.MustCompile(
	`'([a-z][a-z0-9-]*)':\s*\['(mon\.d[A-Za-z0-9]+)',\s*(?:"([^"]*)"|'([^']*)')\]`)

func TestPanelTranslatesEveryDetailCode(t *testing.T) {
	panel, err := os.ReadFile("../../monitor.js")
	if err != nil {
		t.Skip("panel not in this tree (public mirror)")
	}
	block := regexp.MustCompile(`(?s)const FAIL_DETAIL = \{(.*?)\n  \};`).FindSubmatch(panel)
	if block == nil {
		t.Fatal("could not find FAIL_DETAIL in monitor.js")
	}
	keys := map[string]string{}     // detail code -> i18n key
	fallback := map[string]string{} // detail code -> English written inline
	for _, m := range panelEntryRE.FindAllStringSubmatch(string(block[1]), -1) {
		text := m[3]
		if text == "" {
			text = m[4]
		}
		keys[m[1]], fallback[m[1]] = m[2], text
	}
	if len(keys) == 0 {
		t.Fatal("FAIL_DETAIL parsed empty — the test has rotted")
	}

	for code, phrase := range detailPhrases {
		key, ok := keys[code]
		if !ok {
			t.Errorf("monitor.js has no entry for detail code %q", code)
			continue
		}
		if fallback[code] != phrase {
			t.Errorf("English for %q differs:\n  helper: %s\n  panel:  %s", code, phrase, fallback[code])
		}
		delete(keys, code)
		_ = key
	}
	for code := range keys {
		t.Errorf("monitor.js translates %q, which no helper sends", code)
	}
}

func TestBothLocalesCarryEveryDetailPhrase(t *testing.T) {
	src, err := os.ReadFile("../../translations.js")
	if err != nil {
		t.Skip("translations not in this tree (public mirror)")
	}
	panel, err := os.ReadFile("../../monitor.js")
	if err != nil {
		t.Skip("panel not in this tree (public mirror)")
	}
	block := regexp.MustCompile(`(?s)const FAIL_DETAIL = \{(.*?)\n  \};`).FindSubmatch(panel)
	if block == nil {
		t.Fatal("could not find FAIL_DETAIL in monitor.js")
	}

	// PACKS.en is the file's first pack; every other locale is a makePack()
	// clone of it, so a key missing from the Chinese one falls back to English
	// silently — which is the failure this asserts against.
	text := string(src)
	split := strings.Index(text, "PACKS['zh-CN'] = makePack(")
	if split < 0 {
		t.Fatal("could not find the zh-CN pack")
	}
	en, zh := text[:split], text[split:]

	for _, m := range panelEntryRE.FindAllStringSubmatch(string(block[1]), -1) {
		code, key := m[1], m[2]
		phrase, ok := detailPhrases[code]
		if !ok {
			continue // reported by TestPanelTranslatesEveryDetailCode
		}
		enVal := localeValue(en, key)
		if enVal == "" {
			t.Errorf("translations.js (en) has no %q", key)
		} else if enVal != phrase {
			t.Errorf("English for %q differs:\n  helper:       %s\n  translations: %s", code, phrase, enVal)
		}
		zhVal := localeValue(zh, key)
		if zhVal == "" {
			t.Errorf("translations.js (zh-CN) has no %q — it would show English", key)
			continue
		}
		// An argument dropped in translation is a sentence with a hole in it:
		// "the endpoint answered 429" becomes "the endpoint answered".
		if strings.Contains(phrase, "{a}") != strings.Contains(zhVal, "{a}") {
			t.Errorf("zh-CN %q loses or invents the {a} argument: %s", key, zhVal)
		}
	}
}

// One key's value out of a pack. Both quote styles, because an English string
// containing an apostrophe is written with double quotes.
func localeValue(pack, key string) string {
	re := regexp.MustCompile(`'` + regexp.QuoteMeta(key) + `':\s*(?:"([^"]*)"|'([^']*)')`)
	m := re.FindStringSubmatch(pack)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}
