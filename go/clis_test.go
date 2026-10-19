package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A machine of the test's own: a home and two system bin directories under
// one temporary root, and none of the overrides from the environment the
// tests happen to run in.
func cliMachine(t *testing.T) (home, sys string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the layouts under test are symlinks")
	}
	root := t.TempDir()
	home = filepath.Join(root, "home")
	sys = filepath.Join(root, "sys")
	old := cliSystemDirs
	cliSystemDirs = []string{filepath.Join(sys, "usr", "local", "bin"), filepath.Join(sys, "opt", "homebrew", "bin")}
	t.Cleanup(func() { cliSystemDirs = old })
	for _, k := range []string{"APPDATA", "VOLTA_HOME", codexBinEnv, ampBinEnv, agyBinEnv, kiroBinEnv, cliOffEnv} {
		t.Setenv(k, "")
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, sys
}

func cliFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func cliLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func cliPkg(t *testing.T, dir, name, version string) {
	t.Helper()
	cliFile(t, filepath.Join(dir, "package.json"), `{"name":"`+name+`","version":"`+version+`","bin":{}}`, 0o644)
}

// A program that leaves a mark if anything ever runs it.
func cliProgram(t *testing.T, path, mark string) {
	t.Helper()
	cliFile(t, path, "#!/bin/sh\ntouch '"+mark+"'\n", 0o755)
}

func cliOf(list []CLI, id string) []CLI {
	var out []CLI
	for _, c := range list {
		if c.ID == id {
			out = append(out, c)
		}
	}
	return out
}

func TestCLIsReadEachLayoutsOwnRecord(t *testing.T) {
	home, sys := cliMachine(t)
	mark := filepath.Join(t.TempDir(), "ran")

	// Codex from npm under nvm: the link into node_modules, and the Node.
	nvm := filepath.Join(home, ".nvm", "versions", "node", "v22.19.0")
	codexPkg := filepath.Join(nvm, "lib", "node_modules", "@openai", "codex")
	cliPkg(t, codexPkg, "@openai/codex", "0.153.3")
	cliProgram(t, filepath.Join(codexPkg, "bin", "codex.js"), mark)
	cliLink(t, filepath.Join(codexPkg, "bin", "codex.js"), filepath.Join(nvm, "bin", "codex"))

	// Claude Code from its own installer, and a second copy from npm in a
	// system prefix — both said, the home's first.
	ver := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.302")
	cliProgram(t, ver, mark)
	cliLink(t, ver, filepath.Join(home, ".local", "bin", "claude"))
	claudePkg := filepath.Join(sys, "usr", "local", "lib", "node_modules", "@anthropic-ai", "claude-code")
	cliPkg(t, claudePkg, "@anthropic-ai/claude-code", "2.1.290")
	cliProgram(t, filepath.Join(claudePkg, "bin", "claude.exe"), mark)
	cliLink(t, filepath.Join(claudePkg, "bin", "claude.exe"), filepath.Join(sys, "usr", "local", "bin", "claude"))

	// Amp's own installer: a bare binary behind the ~/.local/bin link, met
	// twice (the link and its own place) and said once, with no version.
	cliProgram(t, filepath.Join(home, ".amp", "bin", "amp"), mark)
	cliLink(t, filepath.Join(home, ".amp", "bin", "amp"), filepath.Join(home, ".local", "bin", "amp"))

	// Grok Build's installer: the release is in the file's name.
	cliProgram(t, filepath.Join(home, ".grok", "bin", "grok-1.0.62"), mark)
	cliLink(t, filepath.Join(home, ".grok", "bin", "grok-1.0.62"), filepath.Join(home, ".local", "bin", "grok"))

	// Kimi from uv: the venv's dist-info.
	venv := filepath.Join(home, ".local", "share", "uv", "tools", "kimi-cli")
	cliFile(t, filepath.Join(venv, "pyvenv.cfg"), "home = /usr/bin\n", 0o644)
	cliProgram(t, filepath.Join(venv, "bin", "kimi"), mark)
	if err := os.MkdirAll(filepath.Join(venv, "lib", "python3.13", "site-packages", "kimi_cli-1.52.0.dist-info"), 0o755); err != nil {
		t.Fatal(err)
	}
	cliLink(t, filepath.Join(venv, "bin", "kimi"), filepath.Join(home, ".local", "bin", "kimi"))

	// Kilo from npm-global, under both of its command names: one install.
	kiloPkg := filepath.Join(home, ".npm-global", "lib", "node_modules", "@kilocode", "cli")
	cliPkg(t, kiloPkg, "@kilocode/cli", "7.8.12")
	cliProgram(t, filepath.Join(kiloPkg, "bin", "kilo"), mark)
	cliLink(t, filepath.Join(kiloPkg, "bin", "kilo"), filepath.Join(home, ".npm-global", "bin", "kilo"))
	cliLink(t, filepath.Join(kiloPkg, "bin", "kilo"), filepath.Join(home, ".npm-global", "bin", "kilocode"))

	// Kiro's installer: a bare binary in ~/.local/bin.
	cliProgram(t, filepath.Join(home, ".local", "bin", "kiro-cli"), mark)

	got := scanCLIs(home)
	want := map[string][]CLI{
		"codex":  {{ID: "codex", Version: "0.153.3", Source: "nvm", Node: "22.19.0"}},
		"claude": {{ID: "claude", Version: "2.1.302", Source: "native"}, {ID: "claude", Version: "2.1.290", Source: "npm"}},
		"amp":    {{ID: "amp", Source: "native"}},
		"grok":   {{ID: "grok", Version: "1.0.62", Source: "native"}},
		"kimi":   {{ID: "kimi", Version: "1.52.0", Source: "uv"}},
		"kilo":   {{ID: "kilo", Version: "7.8.12", Source: "npm"}},
		"kiro":   {{ID: "kiro", Source: "native"}},
	}
	for id, w := range want {
		g := cliOf(got, id)
		if len(g) != len(w) {
			t.Errorf("%s: got %+v, want %+v", id, g, w)
			continue
		}
		for i := range w {
			if g[i] != w[i] {
				t.Errorf("%s[%d]: got %+v, want %+v", id, i, g[i], w[i])
			}
		}
	}
	if g := cliOf(got, "copilot"); len(g) != 0 {
		t.Errorf("nothing installed must say nothing: %+v", g)
	}
	// The tools' order, whatever order the places were found in.
	order := map[string]int{}
	for i, tl := range cliTools {
		order[tl.id] = i
	}
	for i := 1; i < len(got); i++ {
		if order[got[i-1].ID] > order[got[i].ID] {
			t.Fatalf("out of the tools' order: %+v", got)
		}
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("a program was run: versions are read, never asked")
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), home) || strings.Contains(string(b), sys) || strings.Contains(string(b), "/") {
		t.Fatalf("a path travelled: %s", b)
	}
}

func TestCLIsOnlyTheToolsOwnRecordSpeaks(t *testing.T) {
	home, sys := cliMachine(t)
	mark := filepath.Join(t.TempDir(), "ran")

	// Amp from npm: the command is a binary nested in a second package
	// inside its own. The outer package is the release.
	amp := filepath.Join(home, ".npm-global", "lib", "node_modules", "@sourcegraph", "amp")
	cliPkg(t, amp, "@sourcegraph/amp", "0.0.1792310430-g2972dc")
	inner := filepath.Join(amp, "node_modules", "@ampcode", "cli")
	cliPkg(t, inner, "@ampcode/cli", "9.9.9")
	cliProgram(t, filepath.Join(inner, "bin", "amp.exe"), mark)
	cliLink(t, filepath.Join(inner, "bin", "amp.exe"), filepath.Join(home, ".npm-global", "bin", "amp"))

	// A package.json that names another package says nothing about this one.
	codex := filepath.Join(home, ".bun", "install", "global", "node_modules", "@openai", "codex")
	cliPkg(t, codex, "@openai/codex-fork", "99.0.0")
	cliProgram(t, filepath.Join(codex, "bin", "codex.js"), mark)
	cliLink(t, filepath.Join(codex, "bin", "codex.js"), filepath.Join(home, ".bun", "bin", "codex"))

	// Homebrew: a cask of the tool's own is its release; a shim manager brew
	// installed (mise) is in brew without being one.
	brew := filepath.Join(sys, "opt", "homebrew")
	cask := filepath.Join(brew, "Caskroom", "codex", "0.163.0", "codex-aarch64-apple-darwin")
	cliProgram(t, cask, mark)
	cliLink(t, cask, filepath.Join(brew, "bin", "codex"))
	mise := filepath.Join(brew, "Cellar", "mise", "2026.1.0", "bin", "mise")
	cliProgram(t, mise, mark)
	cliLink(t, mise, filepath.Join(brew, "bin", "claude"))
	// npm's globals inside brew's own node keg are npm's, and read so.
	copilot := filepath.Join(brew, "Cellar", "node", "24.1.0", "lib", "node_modules", "@github", "copilot")
	cliPkg(t, copilot, "@github/copilot", "1.0.98")
	cliProgram(t, filepath.Join(copilot, "npm-loader.js"), mark)
	cliLink(t, filepath.Join(copilot, "npm-loader.js"), filepath.Join(brew, "bin", "copilot"))

	got := scanCLIs(home)
	if g := cliOf(got, "copilot"); len(g) != 1 || g[0] != (CLI{ID: "copilot", Version: "1.0.98", Source: "npm"}) {
		t.Errorf("npm inside brew's node: %+v", g)
	}
	if g := cliOf(got, "amp"); len(g) != 1 || g[0] != (CLI{ID: "amp", Version: "0.0.1792310430-g2972dc", Source: "npm"}) {
		t.Errorf("amp: %+v", g)
	}
	codexGot := cliOf(got, "codex")
	wantCodex := []CLI{{ID: "codex", Version: "0.163.0", Source: "homebrew"}, {ID: "codex", Source: "bun"}}
	if len(codexGot) != 2 || codexGot[0] != wantCodex[0] || codexGot[1] != wantCodex[1] {
		t.Errorf("codex: got %+v, want %+v", codexGot, wantCodex)
	}
	if g := cliOf(got, "claude"); len(g) != 1 || g[0] != (CLI{ID: "claude", Source: "other"}) {
		t.Errorf("a mise shim is not Claude's release: %+v", g)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("a program was run")
	}
}

func TestCLIsShimsBesideTheirPackage(t *testing.T) {
	home, _ := cliMachine(t)
	mark := filepath.Join(t.TempDir(), "ran")

	// npm on Windows: <prefix>\copilot.cmd beside <prefix>\node_modules.
	appdata := filepath.Join(t.TempDir(), "AppData", "Roaming")
	t.Setenv("APPDATA", appdata)
	cliFile(t, filepath.Join(appdata, "npm", "copilot.cmd"), "@echo off\r\n", 0o755)
	cliPkg(t, filepath.Join(appdata, "npm", "node_modules", "@github", "copilot"), "@github/copilot", "1.0.98")

	// Claude Code's old local install: a script beside its node_modules.
	local := filepath.Join(home, ".claude", "local")
	cliProgram(t, filepath.Join(local, "claude"), mark)
	cliPkg(t, filepath.Join(local, "node_modules", "@anthropic-ai", "claude-code"), "@anthropic-ai/claude-code", "1.0.128")

	// Volta: a shim in Volta's bin, the package in its image.
	cliProgram(t, filepath.Join(home, ".volta", "bin", "volta-shim"), mark)
	cliLink(t, filepath.Join(home, ".volta", "bin", "volta-shim"), filepath.Join(home, ".volta", "bin", "kilo"))
	cliPkg(t, filepath.Join(home, ".volta", "tools", "image", "packages", "@kilocode", "cli", "lib", "node_modules", "@kilocode", "cli"), "@kilocode/cli", "7.8.0")

	got := scanCLIs(home)
	if g := cliOf(got, "copilot"); len(g) != 1 || g[0] != (CLI{ID: "copilot", Version: "1.0.98", Source: "npm"}) {
		t.Errorf("copilot: %+v", g)
	}
	if g := cliOf(got, "claude"); len(g) != 1 || g[0] != (CLI{ID: "claude", Version: "1.0.128", Source: "npm"}) {
		t.Errorf("claude: %+v", g)
	}
	if g := cliOf(got, "kilo"); len(g) != 1 || g[0] != (CLI{ID: "kilo", Version: "7.8.0", Source: "volta"}) {
		t.Errorf("kilo: %+v", g)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("a program was run")
	}
}

func TestCLIsBoundedAndNewestNodeFirst(t *testing.T) {
	home, _ := cliMachine(t)
	mark := filepath.Join(t.TempDir(), "ran")
	// Six Node versions, each with its own Codex; "v22.9.0" must not pass
	// for newer than "v22.19.0".
	for i, n := range []string{"18.20.0", "20.11.1", "22.9.0", "22.19.0", "24.1.0", "16.0.0"} {
		nvm := filepath.Join(home, ".nvm", "versions", "node", "v"+n)
		pkg := filepath.Join(nvm, "lib", "node_modules", "@openai", "codex")
		cliPkg(t, pkg, "@openai/codex", "0.1."+strconv.Itoa(i))
		cliProgram(t, filepath.Join(pkg, "bin", "codex.js"), mark)
		cliLink(t, filepath.Join(pkg, "bin", "codex.js"), filepath.Join(nvm, "bin", "codex"))
	}
	g := cliOf(scanCLIs(home), "codex")
	if len(g) != cliPerTool {
		t.Fatalf("installs of one tool are bounded at %d: %+v", cliPerTool, g)
	}
	for i, n := range []string{"24.1.0", "22.19.0", "22.9.0", "20.11.1"} {
		if g[i].Node != n {
			t.Fatalf("newest Node first: %+v", g)
		}
	}
	// The operator's override is a place of its own, and the first.
	t.Setenv(codexBinEnv, filepath.Join(home, ".nvm", "versions", "node", "v16.0.0", "bin", "codex"))
	if g := cliOf(scanCLIs(home), "codex"); len(g) != cliPerTool || g[0].Node != "16.0.0" {
		t.Fatalf("the override comes first: %+v", g)
	}
}

func TestCLISnapshotHeldAndSwitchedOff(t *testing.T) {
	home, _ := cliMachine(t)
	t.Setenv("HOME", home)
	cliMem.Lock()
	cliMem.at, cliMem.list = time.Time{}, nil
	cliMem.Unlock()
	t.Cleanup(func() {
		cliMem.Lock()
		cliMem.at, cliMem.list = time.Time{}, nil
		cliMem.Unlock()
	})
	if got := cliSnapshot(); len(got) != 0 {
		t.Fatalf("an empty machine says nothing: %+v", got)
	}
	cliProgram(t, filepath.Join(home, ".local", "bin", "kiro-cli"), filepath.Join(t.TempDir(), "ran"))
	if got := cliSnapshot(); len(got) != 0 {
		t.Fatalf("one look stands for %v: %+v", cliEvery, got)
	}
	cliMem.Lock()
	cliMem.at = time.Now().Add(-cliEvery - time.Second)
	cliMem.Unlock()
	if got := cliSnapshot(); len(got) != 1 || got[0].ID != "kiro" {
		t.Fatalf("an old look is taken again: %+v", got)
	}
	t.Setenv(cliOffEnv, "off")
	if got := cliSnapshot(); got != nil {
		t.Fatalf("MON_CLIS=off says nothing: %+v", got)
	}
}

func TestCLIVersionShape(t *testing.T) {
	for in, want := range map[string]string{
		"2.1.302": "2.1.302", "v0.163.0": "0.163.0", " 1.0.62 ": "1.0.62",
		"0.0.1792310430-g2972dc": "0.0.1792310430-g2972dc", "1.2.3-beta.1+build": "1.2.3-beta.1+build",
		"": "", "latest": "", "1 2": "", "1.0/../x": "", "1.0.0\n": "1.0.0", "1.0.0;rm": "",
		strings.Repeat("9", 41): "",
	} {
		if got := cliVersion(in); got != want {
			t.Errorf("cliVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// The relay rebuilds the list field by field (monitor-worker/src/clean.js);
// a tool or a source word it does not know is dropped on arrival, so the two
// vocabularies, the version shape and the bounds are one protocol. Skipped
// outside the monorepo, as coverage_test.go is: the public mirror ships
// helper/go alone, and a lockstep check with nothing to step with is not a
// failure of the helper.
func TestCLIVocabularyIsTheRelays(t *testing.T) {
	b, err := os.ReadFile("../../monitor-worker/src/clean.js")
	if err != nil {
		t.Skip("relay cleaner not in this tree (public mirror)")
	}
	relay := string(b)
	set := func(name string) map[string]bool {
		m := regexp.MustCompile(`const ` + name + ` = new Set\(\[([^\]]*)\]\);`).FindStringSubmatch(relay)
		if m == nil {
			t.Fatalf("clean.js has no %s", name)
		}
		out := map[string]bool{}
		for _, s := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(m[1], -1) {
			out[s[1]] = true
		}
		return out
	}
	ids := set("CLI_IDS")
	for _, tl := range cliTools {
		if !ids[tl.id] {
			t.Errorf("relay drops tool %q", tl.id)
		}
		delete(ids, tl.id)
	}
	for id := range ids {
		t.Errorf("relay admits tool %q the helper never sends", id)
	}
	srcs := set("CLI_SOURCES")
	for _, s := range []string{"native", "npm", "nvm", "bun", "volta", "homebrew", "uv", "pipx", "pip", "other"} {
		if !srcs[s] {
			t.Errorf("relay drops source %q", s)
		}
		delete(srcs, s)
	}
	for s := range srcs {
		t.Errorf("relay admits source %q the helper never sends", s)
	}
	if !strings.Contains(relay, "const CLI_VERSION_RE = /"+cliVersionRE.String()+"/;") {
		t.Errorf("relay's CLI_VERSION_RE is not %s", cliVersionRE)
	}
	if !strings.Contains(relay, "const CLI_NODE_RE = /"+strings.ReplaceAll(cliNodeRE.String(), "[0-9]", `\d`)+"/;") {
		t.Errorf("relay's CLI_NODE_RE is not %s", cliNodeRE)
	}
	for name, n := range map[string]int{"CLI_PER_TOOL": cliPerTool, "CLI_MAX": cliMax} {
		if !strings.Contains(relay, "const "+name+" = "+strconv.Itoa(n)+";") {
			t.Errorf("relay's %s is not %d", name, n)
		}
	}
}
