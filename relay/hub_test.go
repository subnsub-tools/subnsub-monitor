package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func pushBody(agent string, exec, upd bool) []byte {
	return []byte(`{"agent_id":"` + agent + `","exec":` + b(exec) + `,"upd":` + b(upd) +
		`,"providers":[{"id":"codex","ok":true,"limits":[{"used_percent":10}]}]}`)
}

func b(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func closed() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

func TestPushThenCommandRoundTrip(t *testing.T) {
	h := newHub("")
	if st := h.push(pushBody("machineone", true, false)); st != pushOK {
		t.Fatalf("push status %v", st)
	}
	if e := h.enqueueExec("machineone", "cmd1", "uname -a"); e != enqOK {
		t.Fatalf("enqueue: %v", e)
	}
	ans := h.commands("machineone", closed())
	if !ans.Open || len(ans.Commands) != 1 || ans.Commands[0].Cmd != "uname -a" {
		t.Fatalf("poll answer = %+v", ans)
	}
	if ans.Commands[0].Kind != "sh" {
		t.Fatalf("kind = %q", ans.Commands[0].Kind)
	}
	// Delivered once. A second poll must not hand the same command out again.
	ans2 := h.commands("machineone", closed())
	if len(ans2.Commands) != 0 {
		t.Fatalf("command delivered twice: %+v", ans2.Commands)
	}
}

func TestConsoleGateFollowsTheMachine(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", false, false))
	if e := h.enqueueExec("machineone", "c1", "echo hi"); e != enqNoConsole {
		t.Fatalf("a machine with the console off accepted a command: %v", e)
	}
	if e := h.enqueueUpdate("machineone", "u1"); e != enqNoUpdate {
		t.Fatalf("a machine with updates off accepted one: %v", e)
	}
	if e := h.enqueueExec("nosuchmachine", "c1", "echo hi"); e != enqNoMachine {
		t.Fatalf("unknown machine: %v", e)
	}
	// And it follows the LATEST push, not the first one.
	h.push(pushBody("machineone", true, true))
	if e := h.enqueueExec("machineone", "c2", "echo hi"); e != enqOK {
		t.Fatalf("console on was not honoured: %v", e)
	}
	if e := h.enqueueUpdate("machineone", "u2"); e != enqOK {
		t.Fatalf("update on was not honoured: %v", e)
	}
}

func TestOneUpdateAtATime(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", false, true))
	if e := h.enqueueUpdate("machineone", "u1"); e != enqOK {
		t.Fatal(e)
	}
	if e := h.enqueueUpdate("machineone", "u2"); e != enqBusy {
		t.Fatalf("second update while queued: %v", e)
	}
	h.commands("machineone", closed()) // moves u1 into issued
	if e := h.enqueueUpdate("machineone", "u3"); e != enqBusy {
		t.Fatalf("second update while issued: %v", e)
	}
}

func TestResultMustAnswerAnIssuedCommand(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", true, false))
	ch, _, _ := h.attach()
	drain(ch) // the push frame that predates this watcher, if any

	// Never issued: nothing reaches the page.
	h.result([]byte(`{"agent":"machineone","id":"forged","code":0,"out":"pwned"}`))
	if f := recv(ch, 100*time.Millisecond); f != nil {
		t.Fatalf("a forged result was broadcast: %v", f)
	}

	h.enqueueExec("machineone", "realcmd", "echo hi")
	drain(ch) // the queued frame
	h.commands("machineone", closed())
	h.result([]byte(`{"agent":"machineone","id":"realcmd","code":0,"out":"hi"}`))
	f := recv(ch, time.Second)
	if f == nil || f["type"] != "result" || f["out"] != "hi" {
		t.Fatalf("real result not broadcast: %v", f)
	}
	// Consumed on use: a replay of the same document is not a second result.
	h.result([]byte(`{"agent":"machineone","id":"realcmd","code":0,"out":"again"}`))
	if f := recv(ch, 100*time.Millisecond); f != nil {
		t.Fatalf("a replayed result was broadcast: %v", f)
	}
}

func TestExpiredCommandsAreNotDelivered(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", true, false))
	h.enqueueExec("machineone", "old", "echo hi")
	// Age it past the shell-command TTL without sleeping for 90 seconds.
	h.mu.Lock()
	h.machines["machineone"].queue[0].at = nowMS() - cmdTTL - 1000
	h.mu.Unlock()
	ans := h.commands("machineone", closed())
	if len(ans.Commands) != 0 {
		t.Fatalf("a stale command was delivered: %+v", ans.Commands)
	}
}

func TestPollDoesNotHoldWhenNobodyIsWatching(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", true, false))
	start := time.Now()
	ans := h.commands("machineone", closed())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("held %v with no console open", d)
	}
	if ans.Open || ans.Next != pollCold {
		t.Fatalf("answer = %+v, want a cold cadence", ans)
	}
	h.setTerm("machineone", true)
	start = time.Now()
	ans = h.commands("machineone", closed()) // cancelled context returns at once
	if !ans.Open || ans.Next != pollWarm {
		t.Fatalf("answer = %+v, want warm", ans)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a cancelled poll held %v", d)
	}
}

func TestMachineCapIsRefusedNotSilent(t *testing.T) {
	h := newHub("")
	for i := 0; i < maxMachines; i++ {
		id := "machine" + strings.Repeat("x", i%20) + itoaTest(i)
		if st := h.push(pushBody(id, false, false)); st != pushOK {
			t.Fatalf("push %d refused: %v", i, st)
		}
	}
	if st := h.push(pushBody("onemoremachine", false, false)); st != pushFull {
		t.Fatalf("past the cap: %v, want pushFull", st)
	}
	// An existing machine still pushes fine at the cap.
	if st := h.push(pushBody("machine0", false, false)); st != pushOK {
		t.Fatalf("existing machine refused at the cap: %v", st)
	}
}

func TestStateRoundTripsThroughTheGate(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	h := newHub(path)
	h.push(pushBody("machineone", true, false))
	h.setName("machineone", "build box")
	h.saveState()

	h2 := newHub(path)
	m := h2.machines["machineone"]
	if m == nil || m.Reading == nil {
		t.Fatal("the reading did not survive a restart")
	}
	if h2.names["machineone"] != "build box" {
		t.Fatalf("names = %v", h2.names)
	}
	// The mailbox deliberately does not survive: a command outliving the
	// process that took it is a stale execution nobody asked for twice.
	if len(m.queue) != 0 || len(m.issued) != 0 || m.termUntil != 0 {
		t.Fatalf("mailbox survived: %+v", m)
	}
}

func TestCorruptStateStartsEmptyRatherThanRefusingToStart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	if err := writeFileTest(path, "{not json"); err != nil {
		t.Fatal(err)
	}
	h := newHub(path)
	if len(h.machines) != 0 {
		t.Fatal("machines from a corrupt file")
	}
	// And a state file that names a machine it has no business naming.
	if err := writeFileTest(path, `{"machines":{"bad id":{"seen_at":1,"reading":{"ok":true}}}}`); err != nil {
		t.Fatal(err)
	}
	h = newHub(path)
	if len(h.machines) != 0 {
		t.Fatalf("a malformed record was loaded: %+v", h.machines)
	}
}

func TestWatcherThatStoppedReadingIsDropped(t *testing.T) {
	h := newHub("")
	ch, _, ok := h.attach()
	if !ok {
		t.Fatal("attach refused")
	}
	// Fill the buffer without reading, then push more than it can hold.
	for i := 0; i < 200; i++ {
		h.push(pushBody("machineone", false, false))
	}
	h.mu.Lock()
	n := len(h.watchers)
	h.mu.Unlock()
	if n != 0 {
		t.Fatal("a watcher that stopped reading was kept")
	}
	if _, open := <-ch; open {
		// draining is fine; what matters is that the channel eventually closes
		for range ch {
		}
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func recv(ch chan []byte, wait time.Duration) map[string]any {
	select {
	case b, ok := <-ch:
		if !ok {
			return nil
		}
		var f map[string]any
		if json.Unmarshal(b, &f) != nil {
			return nil
		}
		return f
	case <-time.After(wait):
		return nil
	}
}

func drain(ch chan []byte) {
	for {
		select {
		case <-ch:
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

func itoaTest(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func writeFileTest(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
