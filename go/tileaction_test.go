package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"subnsubmonitor/transport"
)

func TestTileActionConfigValidation(t *testing.T) {
	bad := []string{
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"set","exec":"true","bogus":1}]}]}`, // unknown field
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"Set","exec":"true"}]}]}`,           // id shape
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"set"}]}]}`,                         // no command
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"set","exec":""}]}]}`,               // empty command
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"set","exec":[]}]}]}`,               // empty argv
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true"},{"id":"s","exec":"true"}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true","label":"a label that is far too long to fit"}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true","timeout":0.5}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true","timeout":61}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true","tone":"loud"}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true","input":{"min":5,"max":1}}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true","input":{"max":1}}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true","input":{"min":0,"max":1,"step":-1}}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true","input":{"min":0,"max":1e8,"step":1}}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"s","exec":"true","icon":"Robot"}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":[{"id":"a","exec":"true"},{"id":"b","exec":"true"},{"id":"c","exec":"true"},{"id":"d","exec":"true"},{"id":"e","exec":"true"}]}]}`,
		`{"tiles":[{"id":"a","value":1,"actions":["set"]}]}`, // ids are for printed tiles
		`{"tiles":[{"id":"q","type":"quota","value":40,"actions":[{"id":"s","exec":"true"}]}]}`,
	}
	for _, raw := range bad {
		if _, _, err := parseCustomConfig([]byte(raw)); err == nil {
			t.Errorf("accepted: %s", raw)
		}
	}
	cfg, _, err := parseCustomConfig([]byte(`{"tiles":[{"id":"w","name":"Weight","value":6,
		"actions":[{"id":"set","label":"Set","exec":["/bin/echo","x"],"input":{"min":0,"max":20}},
		           {"id":"reset","exec":"echo reset","confirm":true,"tone":"bad"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tile := tileOf(&cfg.Tiles[0], "w", time.Time{}, now())
	if len(tile.Actions) != 2 || tile.Actions[0].Label != "Set" || tile.Actions[0].Input == nil ||
		tile.Actions[0].Input.Step != 1 || tile.Actions[0].Input.Max != 20 ||
		tile.Actions[1].Label != "reset" || !tile.Actions[1].Confirm || tile.Actions[1].Tone != "bad" || tile.Actions[1].Input != nil {
		t.Fatalf("wire: %+v", tile.Actions)
	}
	b, _ := json.Marshal(tile)
	if strings.Contains(string(b), "exec") || strings.Contains(string(b), "echo") {
		t.Fatalf("the command reached the wire: %s", b)
	}
}

func TestTileActionPrinted(t *testing.T) {
	cfg, _, err := parseCustomConfig([]byte(`{"tiles":[{"id":"ws","exec":"true","interval":15,"type":"number",
		"actions":[{"id":"set","exec":"true","input":{"min":0,"max":20}},{"id":"reset","exec":"true"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	spec := cfg.Tiles[0]
	list, err := parseExecOutput(spec, `[{"id":"w_jp","value":3,"actions":["set"]},{"id":"w_m","value":6},{"id":"none","value":1,"actions":[]}]`)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(acts []actionSpec) string {
		var s []string
		for _, a := range acts {
			s = append(s, a.ID)
		}
		return strings.Join(s, ",")
	}
	if ids(list[0].acts) != "set" || ids(list[1].acts) != "set,reset" || ids(list[2].acts) != "" {
		t.Fatalf("printed: %q %q %q", ids(list[0].acts), ids(list[1].acts), ids(list[2].acts))
	}
	one, err := parseExecOutput(spec, `{"value":4,"actions":["reset"]}`)
	if err != nil || ids(one[0].acts) != "reset" {
		t.Fatalf("object: %v %+v", err, one)
	}
	for _, out := range []string{
		`[{"id":"x","value":1,"actions":["nope"]}]`,
		`[{"id":"x","value":1,"actions":[{"id":"evil","exec":"rm -rf ~"}]}]`,
		`[{"id":"x","value":1,"actions":["set","set"]}]`,
		`{"value":1,"actions":[{"id":"evil","exec":"rm -rf ~"}]}`,
	} {
		if _, err := parseExecOutput(spec, out); err == nil {
			t.Errorf("accepted printed actions: %s", out)
		}
	}
}

func TestTileActionValue(t *testing.T) {
	in := &actionInput{Min: fp(0), Max: fp(20), Step: 1}
	half := &actionInput{Min: fp(-1), Max: fp(1), Step: 0.5}
	for _, c := range []struct {
		in       *actionInput
		raw, out string
		ok       bool
	}{
		{in, "6", "6", true},
		{in, " 20 ", "20", true},
		{in, "0", "0", true},
		{in, "6.0", "6", true},
		{in, "21", "", false},
		{in, "-1", "", false},
		{in, "6.5", "", false},
		{in, "", "", false},
		{in, "6; rm -rf ~", "", false},
		{in, "NaN", "", false},
		{in, "1e400", "", false},
		{half, "-0.5", "-0.5", true},
		{&actionInput{Min: fp(0), Max: fp(999999999), Step: 1e9}, "999999999", "", false}, // the grid point past max
		{half, "0.25", "", false},
		{nil, "", "", true},
		{nil, "1", "", false},
	} {
		got, err := actionValue(c.in, c.raw)
		if (err == nil) != c.ok || got != c.out {
			t.Errorf("actionValue(%q) = %q, %v; want %q ok=%v", c.raw, got, err, c.out, c.ok)
		}
	}
}

func TestTileActionRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	dir := customHome(t)
	out := filepath.Join(t.TempDir(), "pressed")
	custom.Lock()
	custom.on, custom.sources, custom.runs, custom.checked = false, nil, nil, false
	custom.Unlock()
	cfg := `{"tiles":[
	  {"id":"ws","exec":"printf '[{\"id\":\"w_m\",\"value\":6},{\"id\":\"w_jp\",\"value\":3,\"actions\":[\"set\"]}]'","interval":30,"type":"number",
	   "actions":[
	     {"id":"set","exec":["/bin/sh","-c","printf '%s %s %s %s' \"$SUBNSUB_SOURCE\" \"$SUBNSUB_TILE\" \"$SUBNSUB_ACTION\" \"$SUBNSUB_VALUE\" > ` + out + `; echo done $SUBNSUB_VALUE"],"input":{"min":0,"max":20}},
	     {"id":"fail","exec":"echo working; echo nope >&2; exit 3"}]},
	  {"id":"plain","value":1,"actions":[{"id":"poke","exec":"echo poked"}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "rw.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	custom.Lock()
	custom.on = true
	custom.sem = make(chan struct{}, execWorkers)
	custom.runs = map[string]*execRun{}
	custom.Unlock()
	defer func() {
		custom.Lock()
		custom.on, custom.sources, custom.runs = false, nil, nil
		custom.Unlock()
	}()
	customReload(true)
	customRunDue()
	deadline := time.Now().Add(5 * time.Second)
	for {
		custom.Lock()
		r := custom.runs["rw/ws"]
		done := r != nil && r.lastOK != nil && !r.running
		custom.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	tiles, _ := collectCustom()
	if len(tiles) != 3 || len(tiles[0].Actions) != 2 || len(tiles[1].Actions) != 1 || len(tiles[2].Actions) != 1 {
		t.Fatalf("tiles: %+v", tiles)
	}

	press := func(target, value string) consoleResult {
		return runTileAction(consoleCmd{ID: "c1", Kind: "act", Target: target, Cmd: value})
	}
	// Nothing pinned: no press at all.
	if tileActionsOn() {
		t.Fatal("actions on without a pinned key")
	}
	if r := press("w_m/set", "8"); r.Error != "unpinned" {
		t.Fatalf("unpinned: %+v", r)
	}
	if err := os.WriteFile(trustedKeysPath(), []byte("not-a-key-but-pins-the-machine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !tileActionsOn() {
		t.Fatal("actions off with a pinned key and a declaration")
	}
	// The tunnel names presses wherever a key is pinned: its hello is read
	// once, and an action declared after it must not wait for a reconnect.
	if caps := directCaps(); !strings.Contains(strings.Join(caps, ","), transport.CapAct) {
		t.Fatalf("caps without act: %v", caps)
	}
	if r := press("w_m/set", "8"); r.Error != "" || r.Code != 0 || r.Out != "done 8" || r.Kind != "act" {
		t.Fatalf("set: %+v", r)
	}
	if b, _ := os.ReadFile(out); string(b) != "rw w_m set 8" {
		t.Fatalf("environment: %q", b)
	}
	for _, c := range []struct{ target, value, why string }{
		{"w_m/set", "21", "bad-value"},
		{"w_m/set", "8 && touch /tmp/x", "bad-value"},
		{"w_m/set", "", "bad-value"},
		{"w_jp/fail", "", "no-action"}, // narrowed to set by the command
		{"w_jp/nope", "", "no-action"},
		{"ghost/set", "1", "no-action"},
		{"w_m", "1", "bad-target"},
		{"w m/set", "1", "bad-target"},
		{"plain/poke", "1", "bad-value"},
	} {
		if r := press(c.target, c.value); r.Error != c.why {
			t.Errorf("press %s %q: %+v, want %s", c.target, c.value, r, c.why)
		}
	}
	if r := press("w_m/fail", ""); r.Error != "action-failed" || r.Code != 3 || r.Out != "nope" {
		t.Fatalf("fail: %+v", r)
	}
	if r := press("plain/poke", ""); r.Error != "" || r.Out != "poked" {
		t.Fatalf("plain: %+v", r)
	}
	// runCommand routes the kind.
	if r, swap := runCommand(consoleCmd{ID: "c2", Kind: "act", Target: "plain/poke"}); r.Error != "" || swap {
		t.Fatalf("runCommand: %+v", r)
	}
	if mailboxRefusal(consoleCmd{Kind: "act"}) != "" {
		t.Fatal("act refused on the mailbox")
	}
	// A portable copy is not granted presses.
	if why := verifyCloneCommand(consoleCmd{Kind: "act"}, "agent", activeClone{}, time.Now()); why != "clone-not-allowed" {
		t.Fatalf("clone: %q", why)
	}
}

func TestTileActionPushRefused(t *testing.T) {
	customHome(t)
	resetPushed()
	defer resetPushed()
	got := pushApply([]byte(`{"id":"x","value":1,"actions":[{"id":"s","exec":"true"}]}`))
	if !strings.Contains(got, "may not carry actions") {
		t.Fatalf("push: %s", got)
	}
}

func TestTileVia(t *testing.T) {
	cfg, _, err := parseCustomConfig([]byte(`{"tiles":[{"id":"pool","exec":"true","interval":30,"type":"number"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	spec := cfg.Tiles[0]
	list, err := parseExecOutput(spec, `[{"id":"rec_weight","name":"weight","type":"text","value":"6","via":{"machine":"node-a","tile":"w_jp"}},{"id":"n","value":1}]`)
	if err != nil {
		t.Fatal(err)
	}
	tile := tileOf(&list[0], "pool", time.Time{}, now())
	if tile.Via == nil || tile.Via.Machine != "node-a" || tile.Via.Tile != "w_jp" || len(tile.Actions) != 0 {
		t.Fatalf("via on the wire: %+v", tile)
	}
	if other := tileOf(&list[1], "pool", time.Time{}, now()); other.Via != nil {
		t.Fatalf("via leaked to a sibling: %+v", other)
	}
	b, _ := json.Marshal(tile)
	if !strings.Contains(string(b), `"via":{"machine":"node-a","tile":"w_jp"}`) {
		t.Fatalf("wire: %s", b)
	}
	for _, out := range []string{
		`[{"id":"x","value":1,"via":{"machine":"","tile":"w_jp"}}]`,
		`[{"id":"x","value":1,"via":{"machine":"node-a","tile":"w jp"}}]`,
		`[{"id":"x","value":1,"via":{"machine":"node-a","tile":"w_jp","exec":"rm -rf ~"}}]`,
	} {
		if _, err := parseExecOutput(spec, out); err == nil {
			t.Errorf("accepted: %s", out)
		}
	}
	// Its own actions win: a tile never offers both.
	own, _, err := parseCustomConfig([]byte(`{"tiles":[{"id":"w","value":6,"via":{"machine":"m","tile":"t"},
		"actions":[{"id":"set","exec":"true","input":{"min":0,"max":20}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if w := tileOf(&own.Tiles[0], "w", time.Time{}, now()); w.Via != nil || len(w.Actions) != 1 {
		t.Fatalf("own actions should win: %+v", w)
	}
}
