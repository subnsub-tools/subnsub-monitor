package main

// Narrowing a sessions query: which project, which window of time. Every
// sessions verb applies the same filter in the same order — the test that
// needs no file opened first, the one that needs the transcript's head second
// — so `search` and `recent` cannot drift into disagreeing about what a
// filter means.
//
// What a filter deliberately is NOT: a second search. `--project` matches the
// label a listing already shows, and `--since`/`--until` bound the session's
// LAST ACTIVITY rather than each message's own clock. A conversation that ran
// for a month is one session, and "touched since Monday" is the question
// somebody looking for recent work is actually asking. The archive narrows on
// the same column for the same reason, so a query means one thing whichever
// source answers it.

import (
	"fmt"
	"strings"
	"time"
)

// The longest project name a filter will carry. A flag value is caller-shaped
// and this one ends up inside a fold-scan of every candidate label; there is
// no answer a longer needle could give that a shorter one could not.
const sessFilterProjectMax = 128

type sessFilter struct {
	// Case-folded and matched as a SUBSTRING of the project label, using
	// sessIndexFold — the same folding the search terms get, so a filter and a
	// term never disagree about what "matches" means.
	project string
	// Zero means "no bound on this side"; a filter with both zero is off.
	since time.Time
	until time.Time
	// One session and no other, by the id a listing showed. Matched as a
	// PREFIX, because the id a reader has in front of them is usually the
	// eight characters on the chip rather than the whole uuid. The cheapest
	// filter here by a distance: it is decided from the file's name, so a
	// transcript it rules out is never opened at all.
	session string
	// Who has to have said it: "user", "assistant" or "tool", or "" for
	// anybody. The one narrowing here that is about a MESSAGE rather than a
	// session — "what did I ask about this" and "what did the model claim
	// about it" are different questions, and every other filter here answers
	// both of them at once. Listing verbs ignore it: every session has a user
	// message in it, so narrowing a LIST by role narrows nothing.
	role string
}

// The half that needs nothing opened. A transcript whose mtime falls outside
// the window cannot be in the answer, and skipping it HERE is the whole
// performance argument for filters: the file is never read, so a query over a
// week of work costs a week of work rather than the whole history.
func (fl sessFilter) wantsTime(mtime time.Time) bool {
	if !fl.since.IsZero() && mtime.Before(fl.since) {
		return false
	}
	if !fl.until.IsZero() && mtime.After(fl.until) {
		return false
	}
	return true
}

// The half that needs the transcript's head, where the working directory is
// recorded. An empty label means the transcript never said where it ran, and
// an unlabelled session is NOT a match for a named project — the other answer
// would drop every unlabelled session into every project's results, which is
// the one outcome that makes a filter worse than no filter.
func (fl sessFilter) wantsProject(project string) bool {
	if fl.project == "" {
		return true
	}
	return sessIndexFold(project, fl.project) >= 0
}

// The half that needs nothing but the file's NAME — cheaper even than the
// mtime test, and applied beside it for the same reason.
func (fl sessFilter) wantsSession(id string) bool {
	return fl.session == "" || strings.HasPrefix(id, fl.session)
}

// The half that needs the message. An UNOWNED line — one the readers could not
// attribute, which sessRole returns "" for — is not a match for a named role:
// the alternative files every system note and every unrecognised shape under
// whichever role was asked for, which is the one answer that makes the filter
// worse than not having it.
func (fl sessFilter) wantsRole(role string) bool {
	return fl.role == "" || fl.role == role
}

// The roles a transcript can actually name. Checked rather than passed
// through, so `role:usr` is a typo somebody is told about instead of a filter
// that silently matches nothing.
func sessValidRole(r string) bool {
	switch r {
	case "user", "assistant", "tool":
		return true
	}
	return false
}

// A moment, written the way a person types one: relative forms counting back
// from `now` (30m, 24h, 7d, 4w), a plain date, or a full RFC3339 stamp. The
// second return says the text named a DAY rather than an instant, which is what
// lets `--until` cover the day somebody wrote instead of stopping at its first
// midnight — see sessFilterFlag.
func sessParseWhen(s string, now time.Time) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, fmt.Errorf("empty")
	}
	if n, unit, ok := sessSplitRel(s); ok {
		var d time.Duration
		switch unit {
		case 'm':
			d = time.Duration(n) * time.Minute
		case 'h':
			d = time.Duration(n) * time.Hour
		case 'd':
			d = time.Duration(n) * 24 * time.Hour
		case 'w':
			d = time.Duration(n) * 7 * 24 * time.Hour
		}
		return now.Add(-d), false, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("not a time")
}

// A relative stamp split into its count and its unit, or ok=false for anything
// else. The count goes through parsePositive, which refuses more than four
// digits — and that bound is load-bearing rather than cosmetic: 9999 weeks is
// the largest of these that still fits a time.Duration, and a wider field would
// let `99999w` wrap the nanosecond count into a time in the past's past.
func sessSplitRel(s string) (int, byte, bool) {
	if len(s) < 2 {
		return 0, 0, false
	}
	unit := s[len(s)-1]
	if unit >= 'A' && unit <= 'Z' {
		unit += 'a' - 'A'
	}
	switch unit {
	case 'm', 'h', 'd', 'w':
	default:
		return 0, 0, false
	}
	n, err := parsePositive(s[:len(s)-1])
	if err != nil || n == 0 {
		return 0, 0, false
	}
	return n, unit, true
}

// The filter flags, parsed once for every verb that takes them. Returns
// handled=false when the argument is not one of these, so each verb's own loop
// carries on with its own flags and its own unknown-flag refusal.
//
// `i` is the caller's loop index and is advanced when a flag takes its value as
// the next argument — the same shape `--limit` already uses, so `--since 7d`
// and `--since=7d` are one code path with one meaning.
func sessFilterFlag(fl *sessFilter, rest []string, i *int, now time.Time) (bool, error) {
	a := rest[*i]
	name, val := a, ""
	inline := false
	if eq := strings.IndexByte(a, '='); eq > 0 {
		name, val, inline = a[:eq], a[eq+1:], true
	}
	switch name {
	case "--project", "--since", "--until", "--role", "--session":
	default:
		return false, nil
	}
	if !inline {
		if *i+1 >= len(rest) {
			return true, fmt.Errorf("%s needs a value", name)
		}
		*i++
		val = rest[*i]
	}
	switch name {
	case "--project":
		p := strings.TrimSpace(val)
		if p == "" {
			return true, fmt.Errorf("--project needs a name")
		}
		fl.project = sessLowerASCII(sessCapRunes(p, sessFilterProjectMax))
	case "--session":
		id := strings.TrimSpace(val)
		if id == "" || !sessValidID(id) {
			return true, fmt.Errorf("--session needs a session id, not %q", val)
		}
		fl.session = id
	case "--role":
		r := sessLowerASCII(strings.TrimSpace(val))
		if !sessValidRole(r) {
			return true, fmt.Errorf("--role takes user, assistant or tool, not %q", val)
		}
		fl.role = r
	default:
		t, dayOnly, err := sessParseWhen(val, now)
		if err != nil {
			return true, fmt.Errorf("%s needs a time like 7d, 24h or 2026-08-01, not %q", name, val)
		}
		if name == "--since" {
			fl.since = t
		} else {
			// A bare date names a DAY, and `--until 2026-08-01` means "up to and
			// including the first of August" — not "up to its first midnight",
			// which is what the raw parse gives and which would drop all but the
			// first instant of the day somebody asked for.
			if dayOnly {
				t = t.Add(24*time.Hour - time.Nanosecond)
			}
			fl.until = t
		}
	}
	return true, nil
}

// How a filter reads back in the one-line summary each verb prints, so a
// terminal answer says what it was narrowed to rather than leaving the reader
// to wonder why their session is missing.
func (fl sessFilter) describe() string {
	parts := make([]string, 0, 4)
	if fl.project != "" {
		parts = append(parts, "project "+fl.project)
	}
	if fl.session != "" {
		parts = append(parts, "session "+fl.session)
	}
	if fl.role != "" {
		parts = append(parts, "role "+fl.role)
	}
	if !fl.since.IsZero() {
		parts = append(parts, "since "+fl.since.UTC().Format(time.RFC3339))
	}
	if !fl.until.IsZero() {
		parts = append(parts, "until "+fl.until.UTC().Format(time.RFC3339))
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, ", ") + "]"
}
