package main

// Which coding agents' transcripts this machine can read, and the small amount
// each one needs that the other ones do not.
//
// sessions.go used to hold this list, back when it was two entries that agreed
// about everything: a transcript was a file of one JSON message per line, its
// id was the file's stem, and the working directory was written inside the
// file. Every tool added since disagrees with at least one of those. Grok names
// every transcript chat_history.jsonl and puts the session id on the directory
// above it. Cursor and Grok both record the working directory in the PATH
// rather than in the file. Kiro writes it into a sidecar. Amp does not write
// lines at all — a thread is one pretty-printed JSON document.
//
// So a source is now the tool's name, its tree, and up to four small hooks for
// exactly those disagreements. Everything else — the os.Root confinement, the
// hard-link refusal, the redaction, the budget, the ordering — stays one
// implementation shared by all of them, which is the point of keeping the
// per-tool part this small.
//
// What is NOT here: a tool whose conversations live on a server (Amp's own
// sync, Kimi, Kilo — this program reads their quota
// over HTTP and they keep no local transcript), and a tool that stores them in
// a format this cannot honestly parse (Antigravity writes protobuf; opencode
// splits one conversation across a directory of one-file-per-message). A
// missing reader is better than one that quietly returns the wrong text.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// The ceiling on a whole-document transcript. A document has to be held in
// memory to be parsed at all — that is what makes it a document rather than a
// stream — so the size that stops being reasonable is much smaller than the
// 8 MB a single LINE is allowed. Amp's largest thread on the machine this was
// written against is 1.2 MB; a file past this is reported as trouble, which
// marks the answer partial rather than dropping it silently.
const sessMaxDocBytes = 6 << 20

// One agent tool whose transcripts this machine searches. The root is the tree
// every read is confined to; the hooks below are each nil for a tool that does
// not need them, which is the common case.
type sessSource struct {
	agent string
	root  string

	// Whether a path under root is one of this tool's transcripts. The
	// argument is the path RELATIVE to root, not the base name, because half
	// these tools say what a file is by where it sits rather than by its name.
	match func(rel string) bool

	// The id worth showing for a transcript, when it is not the file's own
	// stem. nil means the stem, which is what sessIDFromName does.
	id func(rel string) string

	// The working directory the session ran in, when the tool records it
	// beside the transcript rather than inside it — in the path, or in a
	// sidecar file. Any read goes through the same confined root as the
	// transcript itself. nil means the file will say, or nobody will.
	cwd func(root *os.Root, rel string) string

	// Set when the file is ONE json document rather than one message per line.
	// ok=false for anything that is not this tool's document.
	doc func(b []byte) (sessDocument, bool)
}

// A whole-document transcript, pulled apart.
type sessDocument struct {
	// The messages in order, each still raw so the readers can parse them
	// exactly as they parse a line.
	Msgs []json.RawMessage
	// The working directory the document names, or "".
	Cwd string
	// What makes this the SAME conversation from one read to the next.
	//
	// Load-bearing for the archive, which resumes a partly-uploaded transcript
	// and has to tell "this grew" from "this is a different thing at the same
	// path". For a line-shaped file that is a fingerprint of its opening
	// bytes; a document has no such stable opening — Amp puts a revision
	// counter in the second line of its own, so the head changes on every
	// single write. The document's own id does not.
	Ident string
}

// The tools searched, in the order their hits should tie-break. Local
// transcript trees only — a tool that is not installed simply has no
// directory, and that is a normal answer, not an error.
func sessSources() []sessSource {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []sessSource{
		{
			agent: "claude",
			root:  filepath.Join(home, ".claude", "projects"),
			match: sessClaudeMatch,
		},
		{
			agent: "codex",
			root:  codexSessionsDir(),
			match: sessCodexMatch,
		},
		{
			// <project-slug>/agent-transcripts/<uuid>/<uuid>.jsonl, and the
			// same content-item shape Claude writes, so nothing but the path
			// needs saying. Any depth under agent-transcripts counts, because
			// a subagent's own transcript sits one level further down and is
			// as much a conversation as the one that spawned it — Claude keeps
			// both in one directory and both have always been searched.
			agent: "cursor",
			root:  filepath.Join(home, ".cursor", "projects"),
			match: func(rel string) bool {
				p := sessPathParts(rel)
				return len(p) >= 4 && p[1] == "agent-transcripts" &&
					strings.HasSuffix(p[len(p)-1], ".jsonl")
			},
			cwd: sessCursorCwd(home),
		},
		{
			// <percent-encoded working directory>/<session uuid>/chat_history.jsonl.
			// The sibling prompt_history.jsonl is the same prompts a second
			// time with nothing else, so it is deliberately not matched: it
			// would double every hit a user's own words produce.
			agent: "grok",
			root:  filepath.Join(home, ".grok", "sessions"),
			match: func(rel string) bool {
				p := sessPathParts(rel)
				return len(p) == 3 && p[2] == "chat_history.jsonl"
			},
			id: func(rel string) string {
				p := sessPathParts(rel)
				if len(p) >= 2 {
					return p[len(p)-2]
				}
				return ""
			},
			cwd: func(_ *os.Root, rel string) string {
				p := sessPathParts(rel)
				if len(p) == 0 {
					return ""
				}
				return sessUnescapePath(p[0])
			},
		},
		{
			// One transcript per session, with everything else about it —
			// including the working directory — in a .json of the same stem.
			agent: "kiro",
			root:  filepath.Join(home, ".kiro", "sessions", "cli"),
			match: sessSuffixMatch(".jsonl"),
			cwd:   sessKiroCwd,
		},
		{
			// A thread is one pretty-printed JSON document, so this is the one
			// source with a `doc` hook. XDG first because that is what Amp
			// itself honours.
			agent: "amp",
			root:  filepath.Join(sessDataHome(home), "amp", "threads"),
			match: func(rel string) bool {
				n := filepath.Base(rel)
				return strings.HasPrefix(n, "T-") && strings.HasSuffix(n, ".json")
			},
			doc: sessAmpDoc,
		},
	}
}

func sessSuffixMatch(suffix string) func(string) bool {
	return func(rel string) bool { return strings.HasSuffix(rel, suffix) }
}

// The two oldest matchers, named rather than written inline because the tests
// build their own trees and need the REAL rule: an inline copy in the fixture
// went on passing a base name to a matcher that had been changed to take a
// path, and every codex test silently stopped finding its own files.
func sessClaudeMatch(rel string) bool { return strings.HasSuffix(rel, ".jsonl") }

func sessCodexMatch(rel string) bool {
	n := filepath.Base(rel)
	return strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, ".jsonl")
}

// A relative path's segments, on either platform's separator. WalkDir hands
// back the host's, and a matcher that only knew about '/' would match nothing
// at all on Windows.
func sessPathParts(rel string) []string {
	return strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
}

// Percent-decoding, for the one tool that names a directory after the path it
// ran in. Deliberately not net/url: this is decoding a FILENAME, where a plus
// sign is a plus sign rather than a space, and where a malformed escape should
// be left as written rather than turned into an error nobody can act on.
func sessUnescapePath(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := sessHexVal(s[i+1])
			lo, ok2 := sessHexVal(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func sessHexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Cursor's project slug is the absolute path with every separator turned into
// a dash and the leading one dropped, which cannot be inverted: a directory
// genuinely named `a-b` and the path `a/b` produce the same slug. So this
// returns a LABEL rather than a path, and only does the one piece of inverting
// that is sound — stripping the home directory's own slug, which is known
// exactly. `home-alice-web-app` reads back as `web-app`; a project
// outside the home directory keeps its whole slug, which is ugly and true.
func sessCursorCwd(home string) func(*os.Root, string) string {
	slug := strings.TrimPrefix(strings.ReplaceAll(filepath.ToSlash(home), "/", "-"), "-")
	return func(_ *os.Root, rel string) string {
		p := sessPathParts(rel)
		if len(p) == 0 {
			return ""
		}
		name := p[0]
		if slug == "" {
			return name
		}
		if name == slug {
			return filepath.Base(home)
		}
		if rest := strings.TrimPrefix(name, slug+"-"); rest != name && rest != "" {
			return rest
		}
		return name
	}
}

// Kiro keeps the working directory in a sidecar of the same stem. The read
// goes through the transcript's own root, so a sidecar that has been replaced
// by a link out of the tree is refused exactly like a transcript would be.
func sessKiroCwd(root *os.Root, rel string) string {
	if root == nil {
		return ""
	}
	side := strings.TrimSuffix(rel, filepath.Ext(rel)) + ".json"
	if side == rel {
		return ""
	}
	b, ok := sessReadCapped(root, side, 1<<20)
	if !ok {
		return ""
	}
	var meta struct {
		Cwd string `json:"cwd"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return ""
	}
	return meta.Cwd
}

// One Amp thread. The messages are the document's own list, left raw so the
// readers parse them exactly as they parse a transcript line — the content
// items are Claude's shape, so nothing else is needed. The working directory
// is the first workspace the thread opened with, recorded as a file:// URL.
func sessAmpDoc(b []byte) (sessDocument, bool) {
	var doc struct {
		ID       string            `json:"id"`
		Messages []json.RawMessage `json:"messages"`
		Env      struct {
			Initial struct {
				Trees []struct {
					URI string `json:"uri"`
				} `json:"trees"`
			} `json:"initial"`
		} `json:"env"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return sessDocument{}, false
	}
	out := sessDocument{Msgs: doc.Messages, Ident: doc.ID}
	if t := doc.Env.Initial.Trees; len(t) > 0 {
		out.Cwd = sessFileURLPath(t[0].URI)
	}
	return out, true
}

// A file:// URL as a path. Percent-decoded, because a workspace called
// `My Project` arrives as `My%20Project` and would otherwise be a project
// nobody can name in a filter (review 2026-08-19); and with the slash that
// precedes a Windows drive letter dropped, because `/C:/x` is not a path any
// Windows API accepts. Anything that is not a file URL is returned as written
// — the caller only wants a label out of it either way.
func sessFileURLPath(u string) string {
	if !strings.HasPrefix(u, "file://") {
		return u
	}
	p := sessUnescapePath(strings.TrimPrefix(u, "file://"))
	// file://host/path — a UNC name, which keeps both leading slashes.
	if i := strings.IndexByte(p, '/'); i > 0 {
		return "//" + p
	}
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		return p[1:]
	}
	return p
}

// $XDG_DATA_HOME when it is set to an absolute path, ~/.local/share otherwise
// — the same rule the tools that live there apply themselves. A relative value
// is ignored rather than joined onto whatever the process's directory happens
// to be.
func sessDataHome(home string) string {
	if v := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(home, ".local", "share")
}
