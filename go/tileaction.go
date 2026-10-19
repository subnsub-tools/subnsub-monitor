package main

// Tile actions: the other direction of a tile of your own.
//
// A tile in metrics.d says what a machine knows; an action on it lets the
// dashboard change it — set a weight, restart a worker, flip a mode — without
// a shell.
//
// What makes it safe to offer is where the command lives. Exactly as with a
// tile's own `exec`, the command is written in metrics.d on THIS machine, by
// whoever runs it, where it can be read; the dashboard never sends one. What
// a press carries is the tile's id, the action's id and — for an action that
// takes one — a number inside the bounds the file declared. So the worst a
// dashboard (or a relay in its path) can do is run a command the operator
// already wrote, with a value the operator already allowed:
//
//	{"id": "weights", "exec": ["/home/me/weights.sh", "show"], "interval": 15,
//	 "actions": [
//	   {"id": "set", "label": "Set", "exec": ["/home/me/weights.sh", "set"],
//	    "input": {"min": 0, "max": 20, "step": 1}},
//	   {"id": "reset", "label": "Reset", "exec": ["/home/me/weights.sh", "reset"],
//	    "confirm": true}]}
//
// The command learns what was pressed from its environment, never from its
// command line or a shell: SUBNSUB_TILE (the tile's id), SUBNSUB_ACTION,
// SUBNSUB_VALUE (the number, canonical, or empty) and SUBNSUB_SOURCE (the
// metrics.d file's stem). A command that prints tiles may narrow, per tile,
// which of its entry's actions that tile offers — `"actions": ["set"]` — but
// may never declare one: a printed tile names ids, nothing else.
//
// Who may press: a browser this machine has PINNED (trusted-keys), on either
// road — the tunnel, or the relay's mailbox as a signed closed command like
// rename. A machine with no pinned key takes no action at all, unsigned or
// not, and a portable copy (clone.go) is not granted them. The press reaches
// the command through runCommand like every other kind, one command at a time
// (cmdGate), and is written to the machine's own log.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxTileActions       = 4
	actionLabelRunes     = 24
	actionDefaultTimeout = 20.0
	actionMaxTimeout     = 60.0
	actionOutMax         = 400
	actionValueMax       = 32
	actionBoundMax       = 1e9
	actionStepsMax       = 1e6
)

var actionIDRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,23}$`)

// actionSpec is one action as the operator wrote it. The fields without a
// json tag are filled by parseActions and never read from a file.
type actionSpec struct {
	ID      string          `json:"id"`
	Label   string          `json:"label,omitempty"`
	Icon    string          `json:"icon,omitempty"`
	Exec    json.RawMessage `json:"exec"`
	Input   *actionInput    `json:"input,omitempty"`
	Confirm bool            `json:"confirm,omitempty"`
	Tone    string          `json:"tone,omitempty"`
	Timeout float64         `json:"timeout,omitempty"`

	shell string
	argv  []string
}

// actionInput: a number between min and max, on the step's grid from min.
type actionInput struct {
	Min  *float64 `json:"min"`
	Max  *float64 `json:"max"`
	Step float64  `json:"step,omitempty"`
}

// TileAction and TileActionInput are the wire: what a dashboard needs to draw
// the control, and not one byte of the command behind it.
type TileAction struct {
	ID      string           `json:"id"`
	Label   string           `json:"label"`
	Icon    string           `json:"icon,omitempty"`
	Input   *TileActionInput `json:"input,omitempty"`
	Confirm bool             `json:"confirm,omitempty"`
	Tone    string           `json:"tone,omitempty"`
}

type TileActionInput struct {
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Step float64 `json:"step"`
}

// TileVia points a tile at the tile that holds its actions, on another
// machine of the same board: a recorder's weight, say, whose setting belongs
// to the service on the machine that owns it. Nothing runs on the machine that
// shows it; the dashboard opens the other tile's actions and the press goes
// to that machine, which judges it against its own metrics.d as always.
type TileVia struct {
	Machine string `json:"machine"`
	Tile    string `json:"tile"`
}

func normaliseVia(v *TileVia) error {
	v.Machine = cleanText(v.Machine, tileNameMax)
	v.Tile = strings.TrimSpace(v.Tile)
	if v.Machine == "" {
		return errors.New("via needs the machine's name")
	}
	if !tileIDRe.MatchString(v.Tile) {
		return errors.New("via.tile is a tile id: letters, digits, - and _ (48 at most)")
	}
	return nil
}

// parseActions reads an entry's declarations.
func parseActions(raw json.RawMessage) ([]actionSpec, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var list []actionSpec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("actions: %v", err)
	}
	if len(list) > maxTileActions {
		return nil, fmt.Errorf("at most %d actions on a tile", maxTileActions)
	}
	seen := map[string]bool{}
	for i := range list {
		a := &list[i]
		a.ID = strings.TrimSpace(a.ID)
		if !actionIDRe.MatchString(a.ID) {
			return nil, fmt.Errorf("action %d: id is a lowercase letter then letters, digits, - and _ (24 at most)", i+1)
		}
		if seen[a.ID] {
			return nil, fmt.Errorf("action %q is declared twice", a.ID)
		}
		seen[a.ID] = true
		a.Label = cleanText(a.Label, 4*actionLabelRunes)
		if utf8.RuneCountInString(a.Label) > actionLabelRunes {
			return nil, fmt.Errorf("action %q: label is at most %d characters", a.ID, actionLabelRunes)
		}
		if a.Label == "" {
			a.Label = a.ID
		}
		a.Icon = strings.TrimSpace(a.Icon)
		if a.Icon != "" && !validIcon(a.Icon) {
			return nil, fmt.Errorf("action %q: icon %q is not a built-in name or emoji:X", a.ID, a.Icon)
		}
		ex := bytes.TrimSpace(a.Exec)
		switch {
		case len(ex) == 0 || bytes.Equal(ex, []byte("null")):
			return nil, fmt.Errorf("action %q needs exec: the command a press runs", a.ID)
		case ex[0] == '"':
			var line string
			if err := json.Unmarshal(ex, &line); err != nil || strings.TrimSpace(line) == "" {
				return nil, fmt.Errorf("action %q: exec must be a command line or an argv array", a.ID)
			}
			a.shell = line
		default:
			var argv []string
			if err := json.Unmarshal(ex, &argv); err != nil || len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
				return nil, fmt.Errorf("action %q: exec must be a command line or an argv array", a.ID)
			}
			a.argv = argv
		}
		switch {
		case a.Timeout == 0:
			a.Timeout = actionDefaultTimeout
		case !finite(a.Timeout) || a.Timeout < 1 || a.Timeout > actionMaxTimeout:
			return nil, fmt.Errorf("action %q: timeout must be 1…%v seconds", a.ID, actionMaxTimeout)
		}
		a.Tone = strings.ToLower(strings.TrimSpace(a.Tone))
		if a.Tone != "" && a.Tone != "bad" {
			return nil, fmt.Errorf("action %q: tone is bad or nothing", a.ID)
		}
		if in := a.Input; in != nil {
			if in.Min == nil || in.Max == nil || !finite(*in.Min) || !finite(*in.Max) {
				return nil, fmt.Errorf("action %q: input needs a finite min and max", a.ID)
			}
			if math.Abs(*in.Min) > actionBoundMax || math.Abs(*in.Max) > actionBoundMax || *in.Max <= *in.Min {
				return nil, fmt.Errorf("action %q: input needs max > min, both within ±%g", a.ID, actionBoundMax)
			}
			if in.Step == 0 {
				in.Step = 1
			}
			if !finite(in.Step) || in.Step <= 0 || (*in.Max-*in.Min)/in.Step > actionStepsMax {
				return nil, fmt.Errorf("action %q: input step must be positive and leave at most %g steps", a.ID, actionStepsMax)
			}
		}
	}
	return list, nil
}

// printedActions: the actions a printed tile offers — every one its entry
// declared when it names none, else exactly the ids it names, in its order.
func printedActions(raw json.RawMessage, entry []actionSpec) ([]actionSpec, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return entry, nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, errors.New("a printed tile's actions are ids of its entry's actions, not new ones")
	}
	out := make([]actionSpec, 0, len(ids))
	for _, id := range ids {
		found := false
		for _, a := range entry {
			if a.ID == id {
				for _, have := range out {
					if have.ID == id {
						return nil, fmt.Errorf("action %q is named twice", id)
					}
				}
				out = append(out, a)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("action %q is not one its entry declares", id)
		}
	}
	return out, nil
}

func wireActions(list []actionSpec) []TileAction {
	if len(list) == 0 {
		return nil
	}
	out := make([]TileAction, 0, len(list))
	for _, a := range list {
		w := TileAction{ID: a.ID, Label: a.Label, Icon: a.Icon, Confirm: a.Confirm, Tone: a.Tone}
		if a.Input != nil {
			w.Input = &TileActionInput{Min: *a.Input.Min, Max: *a.Input.Max, Step: a.Input.Step}
		}
		out = append(out, w)
	}
	return out
}

// actionValue judges what a press carried against what the file allows, and
// returns the value the command will see: canonical, or "" for an action
// that takes none.
func actionValue(in *actionInput, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if in == nil {
		if raw != "" {
			return "", errors.New("this action takes no value")
		}
		return "", nil
	}
	if raw == "" || len(raw) > actionValueMax {
		return "", errors.New("needs a value")
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || !finite(f) {
		return "", errors.New("not a number")
	}
	if f < *in.Min || f > *in.Max {
		return "", errors.New("out of bounds")
	}
	k := (f - *in.Min) / in.Step
	if math.Abs(k-math.Round(k)) > 1e-6 {
		return "", errors.New("not on the step")
	}
	// Snapped onto the grid, then judged again: with a coarse step the
	// nearest grid point can lie past max.
	f = *in.Min + math.Round(k)*in.Step
	if f < *in.Min || f > *in.Max {
		return "", errors.New("out of bounds")
	}
	return strconv.FormatFloat(f, 'f', -1, 64), nil
}

// customActionFor finds the action a press names, judged against the tiles
// this machine is showing NOW — the same first-id-wins order collectCustom
// puts on the wire, so a press can only reach an action the dashboard was
// actually shown. key is the exec run behind the tile ("" for a tile written
// in the file), so the tile can be read again at once.
func customActionFor(tileID, actionID string) (act actionSpec, source, key string, ok bool) {
	custom.Lock()
	defer custom.Unlock()
	if !custom.on {
		return act, "", "", false
	}
	seen := map[string]bool{}
	pick := func(acts []actionSpec) (actionSpec, bool) {
		for _, a := range acts {
			if a.ID == actionID {
				return a, true
			}
		}
		return actionSpec{}, false
	}
	for _, s := range custom.sources {
		for i := range s.cfg.Tiles {
			spec := &s.cfg.Tiles[i]
			if !spec.hasExec() {
				if seen[spec.ID] {
					continue
				}
				seen[spec.ID] = true
				if spec.ID == tileID {
					a, ok := pick(spec.acts)
					return a, s.id, "", ok
				}
				continue
			}
			k := s.id + "/" + spec.ID
			r := custom.runs[k]
			if r == nil {
				continue
			}
			if r.lastOK == nil {
				if seen[spec.ID] {
					continue
				}
				seen[spec.ID] = true
				if spec.ID == tileID {
					a, ok := pick(spec.acts)
					return a, s.id, k, ok
				}
				continue
			}
			for j := range r.lastOK.tiles {
				t := &r.lastOK.tiles[j]
				if seen[t.ID] {
					continue
				}
				seen[t.ID] = true
				if t.ID == tileID {
					a, ok := pick(t.acts)
					return a, s.id, k, ok
				}
			}
		}
	}
	return act, "", "", false
}

// customActionsDeclared: whether any entry in metrics.d declares an action —
// the operator's own consent, read from the files, that this machine takes
// presses at all.
func customActionsDeclared() bool {
	custom.Lock()
	defer custom.Unlock()
	if !custom.on {
		return false
	}
	for _, s := range custom.sources {
		for i := range s.cfg.Tiles {
			if len(s.cfg.Tiles[i].acts) > 0 {
				return true
			}
		}
	}
	return false
}

// tileActionsOn: what the reading's `act` says and what the tunnel names as
// CapAct — something declared, and a key pinned to take the press from.
func tileActionsOn() bool {
	return customActionsDeclared() && loadTrust().sealed
}

// runTileAction is runCommand's `act`: Target is "<tile id>/<action id>",
// Cmd the value. The caller has verified the signature.
func runTileAction(c consoleCmd) consoleResult {
	res := consoleResult{ID: c.ID, Kind: "act", Target: c.Target, Code: -1}
	if !loadTrust().sealed {
		res.Error = "unpinned"
		return res
	}
	tileID, actionID, cut := strings.Cut(c.Target, "/")
	if !cut || !tileIDRe.MatchString(tileID) || !actionIDRe.MatchString(actionID) {
		res.Error = "bad-target"
		return res
	}
	act, source, key, ok := customActionFor(tileID, actionID)
	if !ok {
		res.Error = "no-action"
		return res
	}
	value, err := actionValue(act.Input, c.Cmd)
	if err != nil {
		res.Error = "bad-value"
		res.Out = err.Error()
		return res
	}
	start := time.Now()
	code, out, why := execAction(act, source, tileID, value)
	res.Ms = time.Since(start).Milliseconds()
	res.Code, res.Out, res.Error = code, out, why
	if why == "" && code != 0 {
		res.Error = "action-failed"
	}
	warnf("metrics: dashboard pressed %s/%s on %s (value %q): exit %d %s", tileID, actionID, source, value, code, why)
	// The tile that was pressed is read again at once, and the push woken when
	// that read lands, so the card shows what the press did within seconds
	// rather than at the entry's next interval.
	if key != "" {
		customRerun(key)
	} else {
		wakePush()
	}
	return res
}

// execAction runs one declared command with the press in its environment.
func execAction(a actionSpec, source, tileID, value string) (code int, out, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Timeout*float64(time.Second)))
	defer cancel()
	var cmd *exec.Cmd
	if a.shell != "" {
		cmd = consoleCommand(ctx, a.shell)
		cmd.Cancel = func() error { return consoleKill(cmd) }
	} else {
		cmd = exec.CommandContext(ctx, a.argv[0], a.argv[1:]...)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cmd.Dir = home
	}
	cmd.Env = append(os.Environ(),
		"SUBNSUB_TILE="+tileID, "SUBNSUB_ACTION="+a.ID, "SUBNSUB_VALUE="+value, "SUBNSUB_SOURCE="+source)
	cmd.WaitDelay = execWaitDelay
	stdout := &capWriter{max: 16 << 10}
	stderr := &capWriter{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Start()
	if err == nil {
		if a.shell != "" {
			consoleAdopt(cmd)
		}
		err = cmd.Wait()
	}
	if a.shell != "" {
		consoleKill(cmd)
	}
	o := strings.TrimSpace(stdout.buf.String())
	e := strings.TrimSpace(stderr.buf.String())
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return -1, actionText(o), "timeout"
	case err != nil:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return -1, "", "spawn"
		}
		// A command that refused says why on stderr, or failing that on
		// stdout; the last line is the reason, not the progress before it.
		msg := e
		if msg == "" {
			msg = o
		}
		return ee.ExitCode(), actionText(lastLine(msg)), ""
	}
	return 0, actionText(o), ""
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// actionText: the operator's words back to the dashboard — one paragraph,
// bounded, without control characters.
func actionText(s string) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)), " ")
	if utf8.RuneCountInString(s) > actionOutMax {
		r := []rune(s)
		s = string(r[:actionOutMax-1]) + "…"
	}
	return s
}

// customRerun reads one exec entry again now, and wakes the push loop when
// that read has landed (customRunDue).
func customRerun(key string) {
	custom.Lock()
	r := custom.runs[key]
	if r == nil {
		custom.Unlock()
		wakePush()
		return
	}
	r.next = 0
	r.wake = true
	if r.running {
		r.again = true
	}
	custom.Unlock()
	customRunDue()
}
