package main

// Which agents' command-line tools this machine has, and which release of
// each — read off the disk, never by running them.
//
// The Agent usage card says every account once; a tool is not an account but
// a program on one machine, so its version is said per machine, and the page
// marks the machine whose copy has fallen behind (operator ruling 2026-10-18: see whether cc-switch's
// new release added agent management and updating … do only the first
// step). This is that first
// step and nothing past it: what is installed and which release. Installing,
// upgrading or removing a tool is not here, and neither is anything that
// would need to: no command runs, no PATH is consulted, nothing is asked of
// the network.
//
// WHY NOT `--version`. It is the obvious question and the wrong one for a
// program that runs unattended on somebody's machine every few minutes: a
// vendor CLI started with any argument at all may check for updates, refresh
// a token, or install itself over itself (agy did exactly that under a quota
// tool's probe, 2026-10-07). Every install worth reporting already writes its
// version down where it put the program:
//
//   - npm, nvm, bun, Homebrew's node, Termux: the command is a link into
//     node_modules/<package>/, whose package.json says the version — and the
//     package's NAME is checked, so a neighbour cannot speak for a command
//     that is not its own;
//   - npm on Windows, and Claude Code's old local install: <dir>/<name> (a
//     .cmd or a shell script) beside <dir>/node_modules/<package>;
//   - Volta: a shim; the package sits in Volta's image of it;
//   - Homebrew: …/Cellar/<formula>/<version>/ or …/Caskroom/<cask>/<version>/,
//     the formula or cask checked against the tool's own (a mise or Volta shim
//     brew installed is not that tool's release);
//   - Claude Code's own installer: ~/.local/share/claude/versions/<version>;
//   - Grok Build's own installer: ~/.grok/bin/grok-<version>;
//   - uv and pipx: the tool's venv holds <dist>-<version>.dist-info.
//
// A program that keeps no version beside it — Amp's, Kiro's and Antigravity's
// own installers drop a bare binary, and each keeps itself current — is
// reported as installed with no version, which the page says as such rather
// than guessing.
//
// WHERE TO LOOK is the CLI collectors' rule (clirun.go): a fixed list of
// places, never PATH, plus the MON_*_BIN override where a collector has one,
// so the version shown includes the program the helper itself reads quota
// through. Each place found is one install, the same install reached twice is
// one, and two different ones are both said: two copies of one tool on a
// machine is exactly what its operator wants to know before upgrading one.
//
// WHAT TRAVELS: the tool, the version, how it was installed (a word from a
// closed set) and, for nvm, which Node it sits under. Never a path — a path
// names the account it lives in. MON_CLIS=off says nothing at all.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// One install of an agent's command-line tool.
type CLI struct {
	// The tool, by the provider id the page already knows it under.
	ID string `json:"id"`
	// The release, as the install wrote it down. Empty where it keeps none
	// on disk.
	Version string `json:"v,omitempty"`
	// How it was installed: native (the vendor's own installer, or a program
	// standing on its own), npm, nvm, bun, volta, homebrew, uv, pipx, pip,
	// or other (inside some other manager this file does not read).
	Source string `json:"src"`
	// The Node an nvm install sits under — which of a machine's two copies
	// is which, when the operator switched Node and installed again.
	Node string `json:"node,omitempty"`
}

type cliTool struct {
	id    string
	names []string // the commands it installs
	npm   string   // its npm package, where it ships as one
	brew  []string // its Homebrew formula or cask tokens
	pypi  string   // its Python distribution, where it ships as one
	env   string   // the collector's MON_*_BIN override, where there is one
	// The vendor's own install places beyond the shared list.
	extra func(home string) []string
	// The version its own installer writes into the program's path, or "".
	native func(real, home string) string
}

// The tools, in the helper's collector order (types.go), each by the id its
// quota reading carries. A tool with no npm, brew, pypi or native rule is
// still found and said — installed, version not kept.
var cliTools = []cliTool{
	{id: "codex", names: []string{"codex"}, npm: "@openai/codex", brew: []string{"codex"}, env: codexBinEnv},
	{id: "antigravity", names: []string{"agy"}, env: agyBinEnv},
	{id: "amp", names: []string{"amp"}, npm: "@sourcegraph/amp", env: ampBinEnv,
		extra: func(home string) []string { return []string{filepath.Join(home, ".amp", "bin", "amp")} }},
	{id: "claude", names: []string{"claude"}, npm: "@anthropic-ai/claude-code",
		brew: []string{"claude-code", "claude-code@latest"},
		// The local install `claude migrate-installer` made: a script beside
		// its own node_modules.
		extra:  func(home string) []string { return []string{filepath.Join(home, ".claude", "local", "claude")} },
		native: claudeNativeVersion},
	{id: "copilot", names: []string{"copilot"}, npm: "@github/copilot", brew: []string{"copilot-cli"}},
	{id: "kimi", names: []string{"kimi"}, pypi: "kimi-cli"},
	{id: "grok", names: []string{"grok"}, npm: "@xai-official/grok",
		extra:  func(home string) []string { return []string{filepath.Join(home, ".grok", "bin", "grok")} },
		native: grokNativeVersion},
	{id: "kilo", names: []string{"kilo", "kilocode"}, npm: "@kilocode/cli"},
	{id: "kiro", names: []string{"kiro-cli"}, env: kiroBinEnv},
}

const (
	cliOffEnv = "MON_CLIS"
	// How long one look at the disk stands. A tool is upgraded by hand a few
	// times a week; a push is every thirty seconds.
	cliEvery = 5 * time.Minute
	// Bounds the relay holds too (clean.js CLI_PER_TOOL, CLI_MAX).
	cliPerTool = 4
	cliMax     = 24
	// A package.json is a few kilobytes; one that is not is not read.
	cliPkgMax = 256 << 10
)

// A version as an install writes one: digits first, then the characters
// semver, Homebrew and npm's timestamped builds use. The relay holds the same
// shape (CLI_VERSION_RE).
var cliVersionRE = regexp.MustCompile(`^[0-9][0-9A-Za-z.+_-]{0,39}$`)
var cliNodeRE = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$`)

// The shared system places, outside any home. A variable so a test can lay
// out a machine of its own; never consulted on Windows, where a path that
// starts with a slash is relative to whatever drive the service started on.
var cliSystemDirs = []string{"/usr/local/bin", "/opt/homebrew/bin", "/home/linuxbrew/.linuxbrew/bin", "/usr/bin"}

var cliMem struct {
	sync.Mutex
	at   time.Time
	list []CLI
}

// What this machine's readings carry: the last look at the disk, taken again
// once it is cliEvery old.
func cliSnapshot() []CLI {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(cliOffEnv)), "off") {
		return nil
	}
	cliMem.Lock()
	defer cliMem.Unlock()
	if !cliMem.at.IsZero() && time.Since(cliMem.at) < cliEvery {
		return cliMem.list
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	cliMem.list = scanCLIs(home)
	cliMem.at = time.Now()
	return cliMem.list
}

// Every install of every tool this machine has, in the tools' order and,
// within a tool, in the order of its places.
func scanCLIs(home string) []CLI {
	var out []CLI
	for _, t := range cliTools {
		seen := map[string]bool{}
		n := 0
		for _, place := range cliPlaces(t, home) {
			if n >= cliPerTool || len(out) >= cliMax {
				break
			}
			c, key, ok := cliInspect(t, place, home)
			if !ok || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c)
			n++
		}
	}
	return out
}

// The places a tool's command can be, most usual first: the collectors' list
// (vendorCandidates), the node managers', the tool's own, and the operator's
// override where its collector has one.
func cliPlaces(t cliTool, home string) []string {
	var out []string
	if t.env != "" {
		if v := strings.TrimSpace(os.Getenv(t.env)); v != "" && filepath.IsAbs(v) {
			out = append(out, v)
		}
	}
	for _, name := range t.names {
		if home != "" {
			out = append(out, filepath.Join(home, ".local", "bin", name))
			if runtime.GOOS == "windows" {
				out = append(out, filepath.Join(home, ".local", "bin", name+".exe"))
			}
		}
		if p := androidPrefix(); p != "" {
			out = append(out, p+"/bin/"+name)
		}
		if runtime.GOOS != "windows" {
			for _, d := range cliSystemDirs {
				out = append(out, filepath.Join(d, name))
			}
		}
		// The same absolute-path guard vendorCandidates keeps: an unset
		// APPDATA must not become a relative `npm/<name>.cmd`.
		if appdata := os.Getenv("APPDATA"); filepath.IsAbs(appdata) {
			out = append(out, filepath.Join(appdata, "npm", name+".cmd"))
		}
		if vh := voltaHome(home); vh != "" {
			out = append(out, filepath.Join(vh, "bin", name))
			if runtime.GOOS == "windows" {
				out = append(out, filepath.Join(vh, "bin", name+".exe"))
			}
		}
		if home != "" {
			out = append(out,
				filepath.Join(home, ".bun", "bin", name),
				filepath.Join(home, ".npm-global", "bin", name))
			out = append(out, nvmPlaces(home, name)...)
		}
	}
	if t.extra != nil && home != "" {
		out = append(out, t.extra(home)...)
	}
	return out
}

// nvm's copies of one command, the newest Node first — compared as numbers,
// for the reason codexNodeVersionCandidates spells out ("v22.9.0" sorts after
// "v22.19.0" as text).
func nvmPlaces(home, name string) []string {
	matches, err := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin", name))
	if err != nil || len(matches) == 0 {
		return nil
	}
	key := func(p string) [3]int {
		out := [3]int{-1, -1, -1}
		v := strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(p))), "v")
		for i, f := range strings.SplitN(v, ".", 3) {
			if n, err := strconv.Atoi(f); err == nil {
				out[i] = n
			}
		}
		return out
	}
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := key(matches[i]), key(matches[j])
		for k := 0; k < 3; k++ {
			if a[k] != b[k] {
				return a[k] > b[k]
			}
		}
		return matches[i] > matches[j]
	})
	return matches
}

// What one place says about the tool installed there, and the key that
// tells two places leading to one install apart. ok is false where nothing
// is there.
func cliInspect(t cliTool, place, home string) (CLI, string, bool) {
	st, err := os.Stat(place)
	if err != nil || !st.Mode().IsRegular() {
		return CLI{}, "", false
	}
	real, err := filepath.EvalSymlinks(place)
	if err != nil {
		real = place
	}
	c := CLI{ID: t.id}

	// Homebrew first: a formula can carry its own node_modules, and the
	// release that counts is the formula's. A keg that is not the tool's
	// own — npm's globals inside brew's node, a mise shim — is read by the
	// rules below, and said as `other` when none of them can.
	ver, inBrew := brewRelease(real, t.brew)
	if ver != "" {
		c.Version, c.Source = ver, "homebrew"
		return c, real, true
	}
	vh := voltaHome(home)
	if t.npm != "" {
		if dir := npmPackageDir(real, t.npm); dir != "" {
			c.Version = packageVersion(dir, t.npm)
			c.Source, c.Node = npmSource(dir)
			return c, dir, true
		}
		// <dir>/<name> beside <dir>/node_modules/<package>: npm's .cmd on
		// Windows, Claude Code's local install everywhere.
		dir := filepath.Join(filepath.Dir(real), "node_modules", filepath.FromSlash(t.npm))
		if v := packageVersion(dir, t.npm); v != "" {
			c.Version, c.Source = v, "npm"
			return c, dir, true
		}
		if vh != "" && voltaShim(place, real, vh) {
			dir := voltaPackageDir(vh, t.npm)
			if v := packageVersion(dir, t.npm); v != "" {
				c.Version = v
			}
			c.Source = "volta"
			return c, dir, true
		}
	}
	if t.native != nil {
		if v := t.native(real, home); v != "" {
			c.Version, c.Source = v, "native"
			return c, real, true
		}
	}
	if t.pypi != "" {
		if venv, ok := pythonVenv(real); ok {
			c.Version = distVersion(venv, t.pypi)
			s := filepath.ToSlash(venv)
			switch {
			case strings.Contains(s, "/uv/tools/"):
				c.Source = "uv"
			case strings.Contains(s, "/pipx/venvs/"):
				c.Source = "pipx"
			default:
				c.Source = "pip"
			}
			return c, venv, true
		}
	}
	// Inside something another manager owns — another package's
	// node_modules, Volta's tree, a keg brew installed for something else:
	// found, not read.
	if s := filepath.ToSlash(real); inBrew || strings.Contains(s, "/node_modules/") || inVolta(s) {
		c.Source = "other"
		return c, real, true
	}
	c.Source = "native"
	return c, real, true
}

// The package directory a command lives in, when it is the tool's own
// package: the outermost node_modules/<package>/ on its path (Amp's command
// is a binary nested in a second package inside its own), or "".
func npmPackageDir(real, pkg string) string {
	s := filepath.ToSlash(real)
	i := strings.Index(s, "/node_modules/"+pkg+"/")
	if i < 0 {
		return ""
	}
	return filepath.FromSlash(s[:i+len("/node_modules/")+len(pkg)])
}

// A package directory's version, when its package.json names the package
// asked about; "" for anything else.
func packageVersion(dir, pkg string) string {
	// A regular file and nothing else: opening a FIFO planted as a
	// package.json would wait for a writer for ever, holding up every
	// reading behind this one.
	path := filepath.Join(dir, "package.json")
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() || st.Size() > cliPkgMax {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, cliPkgMax+1))
	if err != nil || len(b) > cliPkgMax {
		return ""
	}
	var m struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &m) != nil || m.Name != pkg {
		return ""
	}
	return cliVersion(m.Version)
}

// Which manager put a package where it is, and the Node an nvm one sits
// under. An nvm directory whose name is no Node version is still nvm's.
func npmSource(dir string) (string, string) {
	s := filepath.ToSlash(dir)
	const nvm = "/.nvm/versions/node/v"
	if i := strings.Index(s, nvm); i >= 0 {
		rest := s[i+len(nvm):]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			rest = rest[:j]
		}
		if !cliNodeRE.MatchString(rest) {
			rest = ""
		}
		return "nvm", rest
	}
	if strings.Contains(s, "/.bun/install/global/node_modules/") {
		return "bun", ""
	}
	if inVolta(s) {
		return "volta", ""
	}
	return "npm", ""
}

// Volta's own directory: VOLTA_HOME where the operator set one, else
// ~/.volta — or %LOCALAPPDATA%\Volta on Windows, where its installer puts it.
func voltaHome(home string) string {
	if v := os.Getenv("VOLTA_HOME"); filepath.IsAbs(v) {
		return v
	}
	if runtime.GOOS == "windows" {
		if l := os.Getenv("LOCALAPPDATA"); filepath.IsAbs(l) {
			return filepath.Join(l, "Volta")
		}
		return ""
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".volta")
}

// Whether a slash-form path is inside Volta's tree.
func inVolta(s string) bool {
	return strings.Contains(s, "/.volta/") || strings.Contains(s, "/Volta/tools/") || strings.Contains(s, "/Volta/bin/")
}

// Whether a command is one of Volta's shims: in Volta's own bin, or a link
// to its shim program.
func voltaShim(place, real, vh string) bool {
	if filepath.Dir(place) == filepath.Join(vh, "bin") {
		return true
	}
	b := filepath.Base(real)
	return b == "volta-shim" || b == "volta-shim.exe"
}

// Where Volta keeps a package it installed: its image, laid out like a
// global prefix of its own.
func voltaPackageDir(vh, pkg string) string {
	img := filepath.Join(vh, "tools", "image", "packages", filepath.FromSlash(pkg))
	if runtime.GOOS == "windows" {
		return filepath.Join(img, "node_modules", filepath.FromSlash(pkg))
	}
	return filepath.Join(img, "lib", "node_modules", filepath.FromSlash(pkg))
}

// A command inside Homebrew's Cellar or Caskroom: in is true for any of
// them, and the version is the keg's when the formula or cask is one of the
// tool's own — a shim manager brew installed (mise, Volta) is in brew without
// being the tool's release.
func brewRelease(real string, tokens []string) (string, bool) {
	s := filepath.ToSlash(real)
	for _, marker := range []string{"/Caskroom/", "/Cellar/"} {
		i := strings.Index(s, marker)
		if i < 0 {
			continue
		}
		parts := strings.SplitN(s[i+len(marker):], "/", 3)
		if len(parts) < 2 {
			return "", true
		}
		for _, tok := range tokens {
			if parts[0] == tok {
				// A cask's version can carry a build after a comma.
				v, _, _ := strings.Cut(parts[1], ",")
				return cliVersion(v), true
			}
		}
		return "", true
	}
	return "", false
}

// Claude Code's installer keeps each release as
// ~/.local/share/claude/versions/<version> and links the command to one.
func claudeNativeVersion(real, home string) string {
	if home == "" || !inDir(real, filepath.Join(home, ".local", "share", "claude", "versions")) {
		return ""
	}
	return cliVersion(filepath.Base(real))
}

// Grok Build's installer (0.2.112 on) keeps ~/.grok/bin/grok-<version> and
// links the command to it.
func grokNativeVersion(real, home string) string {
	if home == "" || !inDir(real, filepath.Join(home, ".grok", "bin")) {
		return ""
	}
	b := strings.TrimSuffix(filepath.Base(real), ".exe")
	if !strings.HasPrefix(b, "grok-") {
		return ""
	}
	return cliVersion(strings.TrimPrefix(b, "grok-"))
}

// Whether a resolved file sits directly in dir. The file's path has been
// through EvalSymlinks, so dir goes through it too: a home reached through
// a link (/home/u → /data/u) must still be the same home.
func inDir(real, dir string) bool {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return filepath.Dir(real) == dir
}

// The venv a command stands in — <venv>/bin/<name>, or Scripts on Windows —
// when it is one.
func pythonVenv(real string) (string, bool) {
	bin := filepath.Dir(real)
	if b := filepath.Base(bin); b != "bin" && b != "Scripts" {
		return "", false
	}
	venv := filepath.Dir(bin)
	if st, err := os.Stat(filepath.Join(venv, "pyvenv.cfg")); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return venv, true
}

// The version of a distribution installed in a venv, from its dist-info
// directory's name (PEP 427: the name normalized, then the version).
func distVersion(venv, dist string) string {
	norm := strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(dist))
	var found []string
	for _, pat := range []string{
		filepath.Join(venv, "lib", "python*", "site-packages", norm+"-*.dist-info"),
		filepath.Join(venv, "Lib", "site-packages", norm+"-*.dist-info"),
	} {
		m, _ := filepath.Glob(pat)
		found = append(found, m...)
	}
	sort.Strings(found)
	for _, d := range found {
		v := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(d), norm+"-"), ".dist-info")
		if v = cliVersion(v); v != "" {
			return v
		}
	}
	return ""
}

// A version as it travels, or "" for anything not shaped like one. A leading
// v is the tag's, not the version's.
func cliVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if !cliVersionRE.MatchString(v) {
		return ""
	}
	return v
}
