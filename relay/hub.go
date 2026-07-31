package main

// The room, for people who run their own relay: last reading per machine, a
// command mailbox per machine, and a fan-out to however many dashboards are
// watching. One process, one room — the hosted relay's job of keeping many
// accounts apart does not exist here, which is most of why this file is small
// enough to audit in a sitting.
//
// Timings and limits are the hosted relay's, on purpose. The helper was tuned
// against those numbers — its push floor sits under this offline threshold,
// its poll deadline clears this hold — and a relay that "improved" them would
// be a relay the same helper behaves strangely against.

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Where this program says what it could not do. Stderr, which is the journal
// under systemd and the log under Docker — the operator's own record, not
// something that has to reach a browser to be seen.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "monitor-relay: "+format+"\n", args...)
}

const (
	// Seconds without a push before the page paints a machine as quiet. The
	// helper's maximum push interval is 60s; the margin is what keeps a slow
	// but healthy machine from flickering.
	offlineAfter = 90

	// How long "the console is open" lasts without a heartbeat, in ms. The
	// page re-arms it while the panel is on screen; a closed laptop stops
	// re-arming and the helper drops back to the cold poll on its own.
	termTTL = 120 * 1000

	// Command lifetimes, in ms. Short for shell lines — a command typed at a
	// console that nothing collected within 90 seconds is stale, and running
	// it later would surprise the person who typed it. Long for updates: a
	// download plus a restart is minutes, not seconds.
	cmdTTL       = 90 * 1000
	issuedTTL    = 120 * 1000
	updCmdTTL    = 10 * 60 * 1000
	updIssuedTTL = 15 * 60 * 1000

	// The long-poll: hold this long, look this often. What the helper is told
	// to wait between polls when someone is watching, and when nobody is.
	pollHold = 25 * time.Second
	pollStep = 400 * time.Millisecond
	pollWarm = 10
	pollCold = 60

	// Ceilings. Machines is a memory bound, not a plan: past it, new ids are
	// refused and said so, because silently dropping the 65th machine is how
	// an operator spends an evening blaming the helper.
	maxMachines = 64
	queueMax    = 8
	maxWatchers = 8
	maxPolls    = 16

	maxCmdRunes = 2000
)

type pending struct {
	ID   string `json:"id"`
	Kind string `json:"kind,omitempty"`
	Cmd  string `json:"cmd"`
	at   int64  // ms; enqueue time in queue, issue time in issued
}

type machine struct {
	SeenAt  int64    `json:"seen_at"` // ms
	Reading *reading `json:"reading"`

	queue     []*pending
	issued    map[string]*pending
	termUntil int64 // ms
}

type frame = map[string]any

type Hub struct {
	mu       sync.Mutex
	machines map[string]*machine
	names    map[string]string
	watchers map[chan []byte]bool
	polls    int

	statePath string
	dirty     bool
}

func newHub(statePath string) *Hub {
	h := &Hub{
		machines:  map[string]*machine{},
		names:     map[string]string{},
		watchers:  map[chan []byte]bool{},
		statePath: statePath,
	}
	h.loadState()
	return h
}

func nowMS() int64 { return time.Now().UnixMilli() }

// ── broadcast ──────────────────────────────────────────────────────────────

// Send one frame to every watcher. Called with the lock held; the send is
// non-blocking because a watcher that stopped reading must not be able to
// hold the push path hostage — it loses frames and then loses the stream,
// which SSE reconnection turns back into a fresh hello.
func (h *Hub) broadcast(f frame) {
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	for ch := range h.watchers {
		select {
		case ch <- b:
		default:
			// Full buffer = a reader that went away without hanging up. Close
			// from this side; the stream handler sees the closed channel and
			// returns. Deleting here keeps the next broadcast from trying.
			delete(h.watchers, ch)
			close(ch)
		}
	}
}

func (h *Hub) roster() []frame {
	out := make([]frame, 0, len(h.machines))
	for id, m := range h.machines {
		out = append(out, frame{"id": id, "seen_at": m.SeenAt})
	}
	return out
}

// ── push ───────────────────────────────────────────────────────────────────

type pushStatus int

const (
	pushOK pushStatus = iota
	pushBad
	pushFull
)

func (h *Hub) push(body []byte) pushStatus {
	id, rd, ok := cleanReading(body)
	if !ok {
		return pushBad
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	m := h.machines[id]
	if m == nil {
		if len(h.machines) >= maxMachines {
			return pushFull
		}
		m = &machine{issued: map[string]*pending{}}
		h.machines[id] = m
	}
	m.SeenAt = nowMS()
	m.Reading = rd
	h.dirty = true

	h.broadcast(frame{"type": "reading", "agent_id": id, "seen_at": m.SeenAt,
		"reading": rd, "roster": h.roster()})
	return pushOK
}

// ── the command mailbox ────────────────────────────────────────────────────

func ttlFor(kind string, issued bool) int64 {
	if kind == "update" {
		if issued {
			return updIssuedTTL
		}
		return updCmdTTL
	}
	if issued {
		return issuedTTL
	}
	return cmdTTL
}

// Drop what expired. Called with the lock held, before every decision that
// reads the queue — a mailbox is only trustworthy at the moment it was swept.
func (m *machine) reap(now int64) {
	kept := m.queue[:0]
	for _, p := range m.queue {
		if now-p.at <= ttlFor(p.Kind, false) {
			kept = append(kept, p)
		}
	}
	m.queue = kept
	for id, p := range m.issued {
		if now-p.at > ttlFor(p.Kind, true) {
			delete(m.issued, id)
		}
	}
}

type enqueueErr string

const (
	enqOK        enqueueErr = ""
	enqNoMachine enqueueErr = "no-machine"
	enqNoConsole enqueueErr = "no-console"
	enqNoUpdate  enqueueErr = "no-update"
	enqTooLong   enqueueErr = "too-long"
	enqBusy      enqueueErr = "busy"
	enqFullQ     enqueueErr = "queue-full"
)

// enqueueExec queues one shell line, and counts as opening the console: the
// person typing is obviously looking, so the helper is told to stay close
// without a separate term frame having to race this one.
func (h *Hub) enqueueExec(agent, id, cmd string) enqueueErr {
	if len([]rune(cmd)) > maxCmdRunes {
		return enqTooLong
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := nowMS()
	m := h.machines[agent]
	if m == nil {
		return enqNoMachine
	}
	// The machine's own last word decides. reading.exec is the helper
	// reporting the switch on its disk; a machine that never said yes is not
	// asked. The helper checks again before running — this gate exists so the
	// dashboard can say "that machine does not take commands" immediately.
	if m.Reading == nil || !m.Reading.Exec {
		return enqNoConsole
	}
	m.reap(now)
	if len(m.queue) >= queueMax {
		return enqFullQ
	}
	m.queue = append(m.queue, &pending{ID: id, Kind: "sh", Cmd: cmd, at: now})
	m.termUntil = now + termTTL
	h.broadcast(frame{"type": "queued", "agent_id": agent, "id": id,
		"ahead": len(m.queue) - 1})
	return enqOK
}

// enqueueUpdate queues the update button. No command text travels — the
// helper downloads from the release bucket and verifies the published
// checksum, so the worst a relay (this one included) can do is press the
// button. One at a time: two updates in flight are two processes racing to
// replace one file.
func (h *Hub) enqueueUpdate(agent, id string) enqueueErr {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := nowMS()
	m := h.machines[agent]
	if m == nil {
		return enqNoMachine
	}
	if m.Reading == nil || !m.Reading.Upd {
		return enqNoUpdate
	}
	m.reap(now)
	for _, p := range m.queue {
		if p.Kind == "update" {
			return enqBusy
		}
	}
	for _, p := range m.issued {
		if p.Kind == "update" {
			return enqBusy
		}
	}
	if len(m.queue) >= queueMax {
		return enqFullQ
	}
	m.queue = append(m.queue, &pending{ID: id, Kind: "update", at: now})
	h.broadcast(frame{"type": "queued", "agent_id": agent, "id": id, "ahead": 0})
	return enqOK
}

func (h *Hub) setTerm(agent string, open bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.machines[agent]
	if m == nil {
		return
	}
	if open {
		m.termUntil = nowMS() + termTTL
	} else {
		m.termUntil = 0
	}
}

func (h *Hub) setName(agent, name string) {
	name = displayText(name, maxNameRunes)
	h.mu.Lock()
	defer h.mu.Unlock()
	if name == "" {
		delete(h.names, agent)
	} else {
		h.names[agent] = name
	}
	h.dirty = true
	h.broadcast(frame{"type": "names", "names": h.names})
}

type pollAnswer struct {
	Open     bool       `json:"open"`
	Commands []*pending `json:"commands"`
	Next     int        `json:"next,omitempty"`
}

// takeOne moves the oldest queued command into issued and returns it, if the
// mailbox has one. Lock held by the caller.
func (m *machine) takeOne(now int64) *pending {
	if len(m.queue) == 0 {
		return nil
	}
	p := m.queue[0]
	m.queue = m.queue[1:]
	p.at = now
	m.issued[p.ID] = p
	return p
}

// commands answers one helper poll, holding while a console is open and a
// command might arrive. `stop` is the request's own cancellation — a helper
// that hung up mid-hold should release its slot now, not at the deadline.
func (h *Hub) commands(agent string, stop <-chan struct{}) pollAnswer {
	h.mu.Lock()
	if h.polls >= maxPolls {
		// Enough helpers are already parked here. Answer immediately rather
		// than hold; the caller retries on its own cadence and nothing is
		// lost but latency.
		defer h.mu.Unlock()
		m := h.machines[agent]
		if m == nil {
			return pollAnswer{Commands: []*pending{}, Next: pollCold}
		}
		now := nowMS()
		m.reap(now)
		if p := m.takeOne(now); p != nil {
			return pollAnswer{Open: true, Commands: []*pending{p}, Next: pollWarm}
		}
		open := m.termUntil > now
		next := pollCold
		if open {
			next = pollWarm
		}
		return pollAnswer{Open: open, Commands: []*pending{}, Next: next}
	}
	h.polls++
	h.mu.Unlock()
	defer func() { h.mu.Lock(); h.polls--; h.mu.Unlock() }()

	deadline := time.Now().Add(pollHold)
	for {
		h.mu.Lock()
		m := h.machines[agent]
		if m == nil {
			h.mu.Unlock()
			// Unknown machine: a helper whose first poll beat its first push.
			// Cold, not an error — the push is seconds away.
			return pollAnswer{Commands: []*pending{}, Next: pollCold}
		}
		now := nowMS()
		m.reap(now)
		if p := m.takeOne(now); p != nil {
			h.mu.Unlock()
			// One per answer, exactly like the hosted relay: the helper runs
			// them serially anyway, and delivering a batch would just move
			// queue-expiry decisions onto the machine.
			return pollAnswer{Open: true, Commands: []*pending{p}, Next: pollWarm}
		}
		open := m.termUntil > now
		h.mu.Unlock()

		if !open {
			return pollAnswer{Open: false, Commands: []*pending{}, Next: pollCold}
		}
		if time.Now().After(deadline) {
			return pollAnswer{Open: true, Commands: []*pending{}, Next: pollWarm}
		}
		select {
		case <-stop:
			return pollAnswer{Open: true, Commands: []*pending{}, Next: pollWarm}
		case <-time.After(pollStep):
		}
	}
}

// result accepts what a machine says a command did. Only answers to a command
// this relay actually issued — and each id is consumed on use, so a result
// can neither be invented nor replayed into the page.
func (h *Hub) result(body []byte) {
	var raw struct {
		Agent     string   `json:"agent"`
		ID        string   `json:"id"`
		Code      *float64 `json:"code"`
		Ms        *float64 `json:"ms"`
		Out       string   `json:"out"`
		Truncated bool     `json:"truncated"`
		Error     string   `json:"error"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return
	}
	agent := cleanAgentID(raw.Agent)
	if agent == "" || raw.ID == "" || len(raw.ID) > 40 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.machines[agent]
	if m == nil {
		return
	}
	now := nowMS()
	m.reap(now)
	p, ok := m.issued[raw.ID]
	if !ok {
		return
	}
	delete(m.issued, raw.ID)

	out := raw.Out
	if len(out) > 16*1024 {
		out = out[:16*1024]
		raw.Truncated = true
	}
	f := frame{"type": "result", "agent_id": agent, "id": raw.ID,
		"out": out, "truncated": raw.Truncated, "kind": p.Kind}
	if raw.Code != nil {
		f["code"] = *raw.Code
	}
	if raw.Ms != nil {
		f["ms"] = *raw.Ms
	}
	if e := displayText(raw.Error, 40); e != "" {
		f["error"] = e
	}
	h.broadcast(f)
}

// ── watchers ───────────────────────────────────────────────────────────────

// attach registers a dashboard and hands back its frame channel plus the
// hello that describes everything currently known. Hello is built under the
// same lock that registers the channel, so no reading can fall between them.
func (h *Hub) attach() (chan []byte, []byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.watchers) >= maxWatchers {
		return nil, nil, false
	}
	machines := make([]frame, 0, len(h.machines))
	for id, m := range h.machines {
		machines = append(machines, frame{"id": id, "seen_at": m.SeenAt, "reading": m.Reading})
	}
	hello, err := json.Marshal(frame{"type": "hello",
		"offline_after": offlineAfter, "term_ttl": termTTL,
		"names": h.names, "machines": machines})
	if err != nil {
		return nil, nil, false
	}
	ch := make(chan []byte, 32)
	h.watchers[ch] = true
	return ch, hello, true
}

func (h *Hub) detach(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.watchers[ch] {
		delete(h.watchers, ch)
		close(ch)
	}
}

// ── persistence ────────────────────────────────────────────────────────────

// What survives a restart: the roster, the names, the last readings. Not the
// mailbox — a command outliving the relay process that took it is exactly the
// stale-execution surprise the TTLs exist to prevent.
type stateFile struct {
	Names    map[string]string   `json:"names"`
	Machines map[string]*machine `json:"machines"`
}

func (h *Hub) loadState() {
	if h.statePath == "" {
		return
	}
	b, err := os.ReadFile(h.statePath)
	if err != nil {
		// A first run has no file yet and that is not news. Anything else —
		// a permission problem, a directory in the way — is: it means this
		// process will also fail to WRITE, and the operator should hear it now
		// rather than discover it after the restart that lost everything.
		if !os.IsNotExist(err) {
			warnf("could not read the state file: %v", err)
		}
		return
	}
	var st stateFile
	// A corrupt state file starts empty rather than refusing to start: the
	// readings in it are minutes from being replaced by live pushes anyway.
	if err := json.Unmarshal(b, &st); err != nil {
		warnf("the state file is not readable JSON; starting empty")
		return
	}
	for id, name := range st.Names {
		if cleanAgentID(id) == "" && id != legacyAgent {
			continue
		}
		h.names[id] = displayText(name, maxNameRunes)
	}
	now := nowMS()
	n := 0
	for id, m := range st.Machines {
		if m == nil || m.Reading == nil || (cleanAgentID(id) == "" && id != legacyAgent) {
			continue
		}
		if n >= maxMachines {
			break
		}
		// Round-trip the stored reading through the same gate live pushes
		// pass. The state file is ours, but "ours" is a filesystem statement,
		// not an integrity one — and this is cheap. The id is folded back in
		// so the rebuild answers for the whole record, not just the body.
		raw, err := json.Marshal(m.Reading)
		if err != nil {
			continue
		}
		var doc map[string]json.RawMessage
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		if id != legacyAgent {
			idJSON, err := json.Marshal(id)
			if err != nil {
				continue
			}
			doc["agent_id"] = idJSON
		}
		wire, err := json.Marshal(doc)
		if err != nil {
			continue
		}
		gotID, rd, ok := cleanReading(wire)
		if !ok || gotID != id {
			continue
		}
		seen := m.SeenAt
		if seen < 0 || seen > now {
			seen = now
		}
		h.machines[id] = &machine{SeenAt: seen, Reading: rd,
			issued: map[string]*pending{}}
		n++
	}
}

func (h *Hub) saveState() {
	if h.statePath == "" {
		return
	}
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return
	}
	h.dirty = false
	b, err := json.MarshalIndent(stateFile{Names: h.names, Machines: h.machines}, "", " ")
	h.mu.Unlock()
	if err != nil {
		warnf("could not encode the state file: %v", err)
		return
	}
	// Write-then-rename, so a crash mid-write leaves the previous state
	// rather than half a JSON document. 0600: readings describe machines.
	//
	// Every failure below is SAID. A relay that was asked to persist and
	// silently does not is one whose operator finds out by losing the names
	// they typed, and the same permission problem will still be there at
	// shutdown — when there is no run left to notice it in.
	tmp := h.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		warnf("could not write the state file: %v", err)
		return
	}
	if err := os.Rename(tmp, h.statePath); err != nil {
		warnf("could not replace the state file: %v", err)
		os.Remove(tmp)
	}
}

// saveLoop flushes dirty state once a minute, and on demand at shutdown via
// the final call in main.
func (h *Hub) saveLoop() {
	for {
		time.Sleep(time.Minute)
		h.saveState()
	}
}
