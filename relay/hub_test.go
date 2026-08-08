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

// A caller that has already hung up.
func closed() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// A caller that is still there. A nil channel never becomes ready, which is
// exactly "this request has not been cancelled".
func live() <-chan struct{} { return nil }

// A caller that hangs up shortly. For the polls that are expected to find
// nothing: with a console open the hold is 25 seconds by design, and a test
// that waits it out is a test nobody runs.
func hangsUp(d time.Duration) <-chan struct{} {
	c := make(chan struct{})
	go func() { time.Sleep(d); close(c) }()
	return c
}

func TestPushThenCommandRoundTrip(t *testing.T) {
	h := newHub("")
	if st, _ := h.push(pushBody("machineone", true, false)); st != pushOK {
		t.Fatalf("push status %v", st)
	}
	if e := h.enqueueExec("machineone", "cmd1", "uname -a", "", 0); e != enqOK {
		t.Fatalf("enqueue: %v", e)
	}
	ans := h.commands("machineone", live())
	if !ans.Open || len(ans.Commands) != 1 || ans.Commands[0].Cmd != "uname -a" {
		t.Fatalf("poll answer = %+v", ans)
	}
	if ans.Commands[0].Kind != "sh" {
		t.Fatalf("kind = %q", ans.Commands[0].Kind)
	}
	// Delivered once. A second poll must not hand the same command out again.
	// Queuing a command also opened the console, so this one holds — it is
	// cut short rather than waited out.
	ans2 := h.commands("machineone", hangsUp(150*time.Millisecond))
	if len(ans2.Commands) != 0 {
		t.Fatalf("command delivered twice: %+v", ans2.Commands)
	}
}

func TestConsoleGateFollowsTheMachine(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", false, false))
	if e := h.enqueueExec("machineone", "c1", "echo hi", "", 0); e != enqNoConsole {
		t.Fatalf("a machine with the console off accepted a command: %v", e)
	}
	if e := h.enqueueUpdate("machineone", "u1", "", 0); e != enqNoUpdate {
		t.Fatalf("a machine with updates off accepted one: %v", e)
	}
	if e := h.enqueueExec("nosuchmachine", "c1", "echo hi", "", 0); e != enqNoMachine {
		t.Fatalf("unknown machine: %v", e)
	}
	// And it follows the LATEST push, not the first one.
	h.push(pushBody("machineone", true, true))
	if e := h.enqueueExec("machineone", "c2", "echo hi", "", 0); e != enqOK {
		t.Fatalf("console on was not honoured: %v", e)
	}
	if e := h.enqueueUpdate("machineone", "u2", "", 0); e != enqOK {
		t.Fatalf("update on was not honoured: %v", e)
	}
}

func TestOneUpdateAtATime(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", false, true))
	if e := h.enqueueUpdate("machineone", "u1", "", 0); e != enqOK {
		t.Fatal(e)
	}
	if e := h.enqueueUpdate("machineone", "u2", "", 0); e != enqBusy {
		t.Fatalf("second update while queued: %v", e)
	}
	h.commands("machineone", live()) // moves u1 into issued
	if e := h.enqueueUpdate("machineone", "u3", "", 0); e != enqBusy {
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

	h.enqueueExec("machineone", "realcmd", "echo hi", "", 0)
	drain(ch) // the queued frame
	h.commands("machineone", live())
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
	h.enqueueExec("machineone", "old", "echo hi", "", 0)
	// Age it past the shell-command TTL without sleeping for 90 seconds.
	h.mu.Lock()
	h.machines["machineone"].queue[0].at = nowMS() - cmdTTL - 1000
	h.mu.Unlock()
	ans := h.commands("machineone", hangsUp(150*time.Millisecond))
	if len(ans.Commands) != 0 {
		t.Fatalf("a stale command was delivered: %+v", ans.Commands)
	}
}

func TestPollDoesNotHoldWhenNobodyIsWatching(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", true, false))
	start := time.Now()
	ans := h.commands("machineone", live())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("held %v with no console open", d)
	}
	if ans.Open || ans.Next != pollCold {
		t.Fatalf("answer = %+v, want a cold cadence", ans)
	}

	// With a console open it holds — until the caller goes away, which has to
	// end the hold rather than run out the 25 seconds.
	h.setTerm("machineone", true)
	stop := make(chan struct{})
	go func() { time.Sleep(200 * time.Millisecond); close(stop) }()
	start = time.Now()
	h.commands("machineone", stop)
	d := time.Since(start)
	if d < 100*time.Millisecond {
		t.Fatalf("returned in %v — it did not hold at all", d)
	}
	if d > 3*time.Second {
		t.Fatalf("held %v after the caller hung up", d)
	}
	// And the slot it took is back.
	h.mu.Lock()
	polls := h.polls
	h.mu.Unlock()
	if polls != 0 {
		t.Fatalf("poll slots still held: %d", polls)
	}
}

func TestACancelledPollDoesNotSpendACommand(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", true, false))
	h.setTerm("machineone", true)
	if e := h.enqueueExec("machineone", "cmd1", "echo hi", "", 0); e != enqOK {
		t.Fatal(e)
	}
	// The helper hung up before this poll was answered. Taking the command
	// here would move it to `issued`, where it waits out its TTL and reaches
	// nobody — the operator's prompt just never answers.
	if ans := h.commands("machineone", closed()); len(ans.Commands) != 0 {
		t.Fatalf("a hung-up poll took a command: %+v", ans.Commands)
	}
	h.mu.Lock()
	queued, issued := len(h.machines["machineone"].queue), len(h.machines["machineone"].issued)
	h.mu.Unlock()
	if queued != 1 || issued != 0 {
		t.Fatalf("queued=%d issued=%d, want the command still waiting", queued, issued)
	}
	// The next real poll gets it.
	ans := h.commands("machineone", live())
	if len(ans.Commands) != 1 || ans.Commands[0].ID != "cmd1" {
		t.Fatalf("the command did not survive for a live caller: %+v", ans)
	}
}

func TestTheCapEvictsAQuietMachineButNotALiveOne(t *testing.T) {
	h := newHub("")
	for i := 0; i < maxMachines; i++ {
		if st, _ := h.push(pushBody("machine"+itoaTest(i)+"x", false, false)); st != pushOK {
			t.Fatalf("push %d: %v", i, st)
		}
	}
	// Every slot is live, so the cap holds and says so.
	if st, _ := h.push(pushBody("newcomerone", false, false)); st != pushFull {
		t.Fatalf("evicted a live machine: %v", st)
	}
	// Age one past the offline threshold; now the newcomer takes its place.
	h.mu.Lock()
	h.machines["machine7x"].SeenAt = nowMS() - (offlineAfter+60)*1000
	h.mu.Unlock()
	h.setName("machine7x", "old box")
	if st, _ := h.push(pushBody("newcomerone", false, false)); st != pushOK {
		t.Fatalf("a quiet slot was not reclaimed: %v", st)
	}
	if h.machines["machine7x"] != nil {
		t.Fatal("the quiet machine kept its slot")
	}
	if _, ok := h.names["machine7x"]; ok {
		t.Fatal("its name was left behind")
	}
	if len(h.machines) != maxMachines {
		t.Fatalf("machines = %d, want %d", len(h.machines), maxMachines)
	}
}

func TestAFailedSaveStaysDirtySoItIsRetried(t *testing.T) {
	dir := t.TempDir()
	// A directory where the state file should be: every write fails, and the
	// question is whether the relay gives up quietly.
	path := dir + "/state.json"
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	h := newHub(path)
	h.push(pushBody("machineone", false, false))
	h.saveState()
	h.mu.Lock()
	dirty := h.dirty
	h.mu.Unlock()
	if !dirty {
		t.Fatal("a failed save cleared the dirty flag, so it would never be retried")
	}
	// Clear the obstruction; the retry now succeeds.
	os.Remove(path + ".tmp")
	h.saveState()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the retry did not write the file: %v", err)
	}
}

func TestTheStateDirectoryIsCreated(t *testing.T) {
	dir := t.TempDir()
	// The shape the README's systemd example uses: a directory nothing else
	// creates.
	path := dir + "/monitor-relay/state.json"
	h := newHub(path)
	h.push(pushBody("machineone", false, false))
	h.saveState()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state was not written into a directory that had to be made: %v", err)
	}
}

func TestMachineCapIsRefusedNotSilent(t *testing.T) {
	h := newHub("")
	for i := 0; i < maxMachines; i++ {
		id := "machine" + strings.Repeat("x", i%20) + itoaTest(i)
		if st, _ := h.push(pushBody(id, false, false)); st != pushOK {
			t.Fatalf("push %d refused: %v", i, st)
		}
	}
	if st, _ := h.push(pushBody("onemoremachine", false, false)); st != pushFull {
		t.Fatalf("past the cap: %v, want pushFull", st)
	}
	// An existing machine still pushes fine at the cap.
	if st, _ := h.push(pushBody("machine0", false, false)); st != pushOK {
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

func TestSilentMachinesAreForgotten(t *testing.T) {
	h := newHub("")
	h.push(pushBody("machineone", false, false))
	h.push(pushBody("machinetwo", false, false))
	if !h.setName("machineone", "old box") {
		t.Fatal("naming a live machine was refused")
	}
	// Age one of them past the TTL.
	h.mu.Lock()
	h.machines["machineone"].SeenAt = nowMS() - machineTTL.Milliseconds() - 1000
	h.mu.Unlock()

	if n := h.sweep(); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	if h.machines["machineone"] != nil {
		t.Fatal("the silent machine kept its slot")
	}
	if _, ok := h.names["machineone"]; ok {
		t.Fatal("its name outlived it, which is the map that would grow forever")
	}
	if h.machines["machinetwo"] == nil {
		t.Fatal("a live machine was swept")
	}
	// And the freed slot is usable.
	if st, _ := h.push(pushBody("machineone", false, false)); st != pushOK {
		t.Fatalf("the returning machine was refused: %v", st)
	}
}

func TestNamingAnUnknownMachineIsRefused(t *testing.T) {
	h := newHub("")
	if h.setName("nosuchmachine", "ghost") {
		t.Fatal("named a machine that has never pushed")
	}
	if len(h.names) != 0 {
		t.Fatalf("names = %v", h.names)
	}
}

func TestExpiredRecordsDoNotComeBackFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/state.json"
	h := newHub(path)
	h.push(pushBody("machineone", false, false))
	h.setName("machineone", "old box")
	h.mu.Lock()
	h.machines["machineone"].SeenAt = nowMS() - machineTTL.Milliseconds() - 1000
	h.mu.Unlock()
	h.saveState()

	h2 := newHub(path)
	if len(h2.machines) != 0 {
		t.Fatalf("a machine past the TTL was restored: %+v", h2.machines)
	}
	if len(h2.names) != 0 {
		t.Fatalf("an orphan name was restored: %v", h2.names)
	}
}

// A signature has to survive the mailbox unchanged, because a machine with a
// pinned key verifies over exactly the bytes the browser signed. This relay
// carries it; it cannot make one, and that is the whole property.
func TestSignatureSurvivesTheMailbox(t *testing.T) {
	h := newHub("")
	if st, _ := h.push(pushBody("machineone", true, false)); st != pushOK {
		t.Fatalf("push status %v", st)
	}
	const sig = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	const at = int64(1785969600)

	if e := h.enqueueExec("machineone", "c1", "uptime", sig, at); e != enqOK {
		t.Fatalf("enqueue: %v", e)
	}
	ans := h.commands("machineone", live())
	if !ans.Open || len(ans.Commands) != 1 {
		t.Fatalf("poll answer = %+v", ans)
	}
	p := ans.Commands[0]
	if p.Sig != sig || p.IssuedAt != at {
		t.Fatalf("mailbox altered the signature: %q %d", p.Sig, p.IssuedAt)
	}
	// The delivery clock is ours and the signing clock is the browser's; a
	// relay that overwrote one with the other would make every command look
	// fresh to a machine that should have refused it.
	if p.IssuedAt != at {
		t.Errorf("the signing timestamp was replaced: %d", p.IssuedAt)
	}
	if p.Cmd != "uptime" {
		t.Errorf("command text changed in transit: %q", p.Cmd)
	}
}

// An unsigned command still queues: a machine that pinned nothing must keep
// working, and this relay is not the place that decides.
func TestUnsignedCommandsStillQueue(t *testing.T) {
	h := newHub("")
	if st, _ := h.push(pushBody("machineone", true, false)); st != pushOK {
		t.Fatalf("push status %v", st)
	}
	if e := h.enqueueExec("machineone", "c1", "uptime", "", 0); e != enqOK {
		t.Fatalf("an unsigned command was refused by the relay: %v", e)
	}
}

// The push answer's second value: a machine with nothing waiting is told not
// to poll, one with a queued command or an open console is summoned. This is
// the bit the helper reads instead of polling on a timer; the hosted relay
// answers it identically, which is the whole point of it being here.
func TestPushSaysWhetherPollingIsWorthIt(t *testing.T) {
	h := newHub("")
	if _, poll := h.push(pushBody("machineone", true, false)); poll {
		t.Fatal("an idle machine must not be summoned")
	}
	if e := h.enqueueExec("machineone", "cmd1", "uname -a", "", 0); e != enqOK {
		t.Fatalf("enqueue: %v", e)
	}
	if _, poll := h.push(pushBody("machineone", true, false)); !poll {
		t.Fatal("a queued command must summon the machine")
	}
	// Collected: the queue is empty again, but queuing also opened the
	// console, and an open console is a reason to keep coming on its own.
	if ans := h.commands("machineone", live()); len(ans.Commands) != 1 {
		t.Fatalf("collect: %+v", ans)
	}
	if _, poll := h.push(pushBody("machineone", true, false)); !poll {
		t.Fatal("an open console must keep the machine coming")
	}
}
