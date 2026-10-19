package main

// Transfer pipe — the Transfer tool's fourth leg, carried by the direct
// tunnel.
//
// WHAT IT IS. Two browsers that could not reach each other directly (the
// Transfer tool's WebRTC attempt failed) meet at this machine instead: one of
// them already watches this machine through the tunnel, and it INVITES the
// other for one session. Each side then opens a send stream and a receive
// stream inside its own tunnel, and this machine couples them — bytes the
// first side sends come out of the second side's receive stream and vice
// versa. A pipe, nothing else: no storage, no parsing, no third party.
//
// WHAT THE MACHINE SEES. Ciphertext. The frames that cross are end-to-end encrypted
// between the two browsers with keys they agree on INSIDE the pipe, so this
// machine — like the signalling relay the same frames otherwise ride —
// forwards what it cannot read.
//
// WHY THIS IS NOT THE GENERAL-PURPOSE FORWARDING THE PLAN FORBIDS. A guest is
// admitted for one session, to the pipe routes only, and its bytes go to
// exactly one place: the inviting browser, which is already on this
// machine's allowlist. Neither side can name an address, a port, or another
// machine; there is no way to reach anything through this but the other
// party of the invite. The one thing an allowed watcher gains is the ability
// to route its OWN transfer through a machine it already watches.
//
// THE SWITCH IS A FILE, inverted: `no-transfer` in the config directory turns
// it off (`subnsub-monitor transfer off`); absent, it is on — this road
// exists so that a machine somebody bothered to install the helper on can
// carry their files without a second setup step. MON_TRANSFER=0|1 wins over
// the file, as every other switch's environment does. Read on every invite,
// never cached: off is the direction that has to be fast.
//
// BOUNDS. Four sessions at once; an invite both sides have not attached
// within two minutes is forgotten; a session nobody has moved a byte through
// for five minutes ends (the page sends an empty keepalive frame well inside
// that while a channel is merely idle); a sender that goes silent for two
// minutes is cut (the deadline tracks silence, not duration, so an honestly
// slow transfer is never cut for being slow); nothing lives past twelve
// hours. Memory is one 64 KiB buffer per stream — io.Pipe hands bytes across
// synchronously, so a reader that stops reading stops the writer rather than
// filling anything.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"subnsubmonitor/transport"

	"tailscale.com/types/key"
)

const (
	// The file whose EXISTENCE turns the transfer pipe OFF. Content is ignored.
	transferOffFile = "no-transfer"
	// One run's override: MON_TRANSFER=0 (off) or =1 (on), environment wins.
	transferEnvVar = "MON_TRANSFER"

	pipeMaxSessions = 4
	pipeInviteTTL   = 2 * time.Minute
	pipeIdle        = 5 * time.Minute
	pipeMaxLife     = 12 * time.Hour
	pipeReadWait    = 2 * time.Minute
	pipeWriteWait   = 30 * time.Second
	pipeChunk       = 64 << 10
	// A session this idle may be evicted to make room for a new invite when
	// the table is full — a browser that vanished without saying goodbye must
	// not hold a slot until the idle sweep gets to it.
	pipeEvictIdle = time.Minute
	// Invites per minute, all hosts together. Every invite adds a guest key
	// to the carrier's WireGuard allowlist, which upstream cannot shrink
	// (transport/tailcat.go AddAllowed), so the rate at which an allowed
	// watcher can grow it is bounded here: a transfer needs one.
	pipeInviteRate = 12
	// And the total: once this many guests have been admitted to one
	// carrier, the carrier is rebuilt at the next quiet moment (direct.go
	// serveDirect) — the address survives a rebuild, the accumulated keys
	// do not.
	pipeGuestRebuild = 256
)

var pipeSIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,32}$`)

// guestAgentID is what a guest's frames carry in place of this machine's
// id: a fixed word in the id's alphabet, so the page's frame gate accepts
// the envelope and the guest learns nothing.
const guestAgentID = "transfer"

var (
	errPipeEnded = errors.New("pipe: session ended")
	errPipeGone  = errors.New("pipe: peer went away")
	errPipeIdle  = errors.New("pipe: idle")
)

// ── the switch ─────────────────────────────────────────────────────────

func transferEnabled() bool {
	if v, ok := os.LookupEnv(transferEnvVar); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "on", "true", "yes":
			return true
		}
		return false
	}
	dir := configDir()
	if dir == "" {
		return false
	}
	fi, err := os.Lstat(filepath.Join(dir, transferOffFile))
	return !(err == nil && fi.Mode().IsRegular())
}

func setTransfer(on bool) error {
	dir := configDir()
	if dir == "" {
		return errString("no config directory")
	}
	path := filepath.Join(dir, transferOffFile)
	if on {
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

func runTransferCLI(args []string) int {
	if len(args) > 0 {
		on := args[0] == "on" || args[0] == "1" || args[0] == "true" || args[0] == "yes"
		off := args[0] == "off" || args[0] == "0" || args[0] == "false" || args[0] == "no"
		if !on && !off {
			warnf("say 'transfer on' or 'transfer off'")
			return 2
		}
		if err := setTransfer(on); err != nil {
			warnf("could not change the transfer setting: %v", err)
			return 1
		}
	}
	if transferEnabled() {
		fmt.Printf("on   (%s)\n", agentID())
		fmt.Println("a browser allowed through the direct tunnel may route one of its own file")
		fmt.Println("transfers through this machine; the bytes are encrypted between the two browsers")
	} else {
		fmt.Printf("off  (%s)\n", agentID())
	}
	if len(args) > 0 {
		fmt.Println("takes effect on the next invite; no restart needed")
	}
	return 0
}

// ── sessions ───────────────────────────────────────────────────────────

// pipeDir is one direction of a session: written by one side's send stream,
// read by the other side's receive stream.
type pipeDir struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	sendOn bool // the writer's stream is attached
	recvOn bool // the reader's stream is attached
}

func newPipeDir() *pipeDir {
	r, w := io.Pipe()
	return &pipeDir{r: r, w: w}
}

func (d *pipeDir) close(err error) {
	d.w.CloseWithError(err)
	d.r.CloseWithError(err)
}

type pipeSession struct {
	sid     string
	host    key.NodePublic // the allowed browser that invited
	guest   key.NodePublic // the browser it invited, admitted for this session only
	created time.Time
	last    time.Time // last byte moved, or last stream attached
	h2g     *pipeDir  // host sends, guest receives
	g2h     *pipeDir  // guest sends, host receives
	ended   bool
	// done is closed when the session ends, for a send stream blocked in a
	// body read that no byte will ever wake.
	done chan struct{}
}

func (s *pipeSession) attachedAll() bool {
	return s.h2g.sendOn && s.h2g.recvOn && s.g2h.sendOn && s.g2h.recvOn
}

// pipeHub holds the sessions of one carrier. It is created with the direct
// handler and dies with it; a carrier restart forgets every session, which
// is right — the tunnels the sessions rode are gone too.
type pipeHub struct {
	mu       sync.Mutex
	sessions map[string]*pipeSession
	invites  []time.Time      // the last minute's invites, for the rate cap
	admitted int              // guests admitted to the carrier since it started
	now      func() time.Time // a test's clock
	// hostOK says whether a session's host is STILL allowed — the file's
	// current answer. A host struck from the file takes its sessions down
	// at the next sweep, as a struck watcher loses its event stream.
	hostOK func(key.NodePublic) bool
}

func newPipeHub() *pipeHub {
	return &pipeHub{sessions: map[string]*pipeSession{}, now: time.Now}
}

// admittedGuests is how many guest keys this carrier has been told about.
func (p *pipeHub) admittedGuests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.admitted
}

// guests are the keys admitted for live sessions — what the carrier's gate
// adds to the allowlist file. A session that ended takes its guest off this
// list, and the guest's next request is refused at the gate.
func (p *pipeHub) guests() []key.NodePublic {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []key.NodePublic
	for _, s := range p.sessions {
		if !s.ended {
			out = append(out, s.guest)
		}
	}
	return out
}

func (p *pipeHub) isGuest(k key.NodePublic) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		if !s.ended && s.guest == k {
			return true
		}
	}
	return false
}

// invite opens a session for host and guest under sid. The answer is an
// error code in the tunnel's vocabulary, or "" with the session and the
// invite's TTL.
func (p *pipeHub) invite(host key.NodePublic, sid string, guest key.NodePublic) (*pipeSession, time.Duration, string) {
	if !pipeSIDRe.MatchString(sid) || guest.IsZero() || guest == host {
		return nil, 0, "bad-frame"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	keep := p.invites[:0]
	for _, t := range p.invites {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	p.invites = keep
	if len(p.invites) >= pipeInviteRate {
		return nil, 0, "busy"
	}
	if old := p.sessions[sid]; old != nil {
		// The same host inviting again under the same id is a retry (its
		// first answer was lost, or the guest changed): the old session is
		// replaced. Anybody else's id is not theirs to take.
		if old.host != host {
			return nil, 0, "busy"
		}
		p.endLocked(old, errPipeEnded)
	}
	live := 0
	var idlest *pipeSession
	for _, s := range p.sessions {
		if s.ended {
			continue
		}
		live++
		if idlest == nil || s.last.Before(idlest.last) {
			idlest = s
		}
	}
	if live >= pipeMaxSessions {
		if idlest == nil || now.Sub(idlest.last) < pipeEvictIdle {
			return nil, 0, "busy"
		}
		p.endLocked(idlest, errPipeIdle)
	}
	p.invites = append(p.invites, now)
	p.admitted++
	s := &pipeSession{
		sid: sid, host: host, guest: guest, created: now, last: now,
		h2g: newPipeDir(), g2h: newPipeDir(), done: make(chan struct{}),
	}
	p.sessions[sid] = s
	return s, pipeInviteTTL, ""
}

// attach claims one of a session's four streams for peer: its send stream
// (send=true) or its receive stream. The direction handed back is the one
// the caller writes to (send) or reads from (recv).
func (p *pipeHub) attach(sid string, peer key.NodePublic, send bool) (*pipeSession, *pipeDir, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.sessions[sid]
	if s == nil || s.ended {
		return nil, nil, "no-session"
	}
	var d *pipeDir
	var on *bool
	switch {
	case peer == s.host && send:
		d, on = s.h2g, &s.h2g.sendOn
	case peer == s.host:
		d, on = s.g2h, &s.g2h.recvOn
	case peer == s.guest && send:
		d, on = s.g2h, &s.g2h.sendOn
	case peer == s.guest:
		d, on = s.h2g, &s.h2g.recvOn
	default:
		return nil, nil, "not-allowed"
	}
	if *on {
		return nil, nil, "busy"
	}
	*on = true
	s.last = p.now()
	return s, d, ""
}

func (p *pipeHub) touch(s *pipeSession) {
	p.mu.Lock()
	s.last = p.now()
	p.mu.Unlock()
}

// end closes both directions of s with err — every stream of the session
// sees it on its next read or write — and forgets the session. By session,
// not by id: a stream of a session that an invite has since REPLACED under
// the same id must not take the replacement down with it.
func (p *pipeHub) end(s *pipeSession, err error) {
	if s == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions[s.sid] == s {
		p.endLocked(s, err)
	}
}

func (p *pipeHub) endLocked(s *pipeSession, err error) {
	if s.ended {
		return
	}
	s.ended = true
	if err == nil {
		err = errPipeEnded
	}
	s.h2g.close(err)
	s.g2h.close(err)
	close(s.done)
	delete(p.sessions, s.sid)
}

func (p *pipeHub) endAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		p.endLocked(s, errPipeEnded)
	}
}

// sweep is the janitor: invites nobody completed, sessions nobody used,
// sessions that have simply lived long enough, and sessions whose host has
// been struck from the allowlist since.
func (p *pipeHub) sweep(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		switch {
		case p.hostOK != nil && !p.hostOK(s.host):
			p.endLocked(s, errPipeGone)
		case !s.attachedAll() && now.Sub(s.created) > pipeInviteTTL:
			p.endLocked(s, errPipeGone)
		case now.Sub(s.last) > pipeIdle:
			p.endLocked(s, errPipeIdle)
		case now.Sub(s.created) > pipeMaxLife:
			p.endLocked(s, errPipeEnded)
		}
	}
}

func (p *pipeHub) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.sessions {
		if !s.ended {
			n++
		}
	}
	return n
}

// ── the three routes ───────────────────────────────────────────────────

// pipeInvite is POST /v1/pipe: an allowed browser names a session and the
// guest's key. Guests cannot reach this route at all (ServeHTTP), so the
// peer here is always a key from the allowlist file.
func (h *directHandler) pipeInvite(w http.ResponseWriter, r *http.Request) {
	peer, ok := transport.PeerFromContext(r.Context())
	if !ok {
		h.refuse(w, http.StatusForbidden, "not-allowed")
		return
	}
	if !transferEnabled() {
		h.refuse(w, http.StatusForbidden, "transfer-off")
		return
	}
	limit := transport.Limits[transport.TypePipe]
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil || len(body) > limit {
		h.refuse(w, http.StatusRequestEntityTooLarge, "too-large")
		return
	}
	f, err := transport.Decode(body)
	if err != nil || f.Type != transport.TypePipe {
		h.refuse(w, http.StatusBadRequest, "bad-frame")
		return
	}
	var req transport.Pipe
	if len(f.Body) == 0 || json.Unmarshal(f.Body, &req) != nil || req.Op != "invite" {
		h.refuse(w, http.StatusBadRequest, "bad-frame")
		return
	}
	guest, ok := transport.ParseNodeKey(req.Guest)
	if !ok {
		h.refuse(w, http.StatusBadRequest, "bad-frame")
		return
	}
	if h.admit == nil {
		h.refuse(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	sess, ttl, code := h.pipes.invite(peer, req.SID, guest)
	if code != "" {
		status := http.StatusBadRequest
		if code == "busy" {
			status = http.StatusServiceUnavailable
		}
		h.refuse(w, status, code)
		return
	}
	// Admitted at the WireGuard layer now, so the guest's very first
	// handshake is answered; the gate consults guests() from here on.
	h.admit(guest)
	out, err := transport.New(transport.TypePipe, agentID(), f.ID, transport.Pipe{SID: req.SID, OK: true, TTLSec: int(ttl / time.Second)})
	var b []byte
	if err == nil {
		b, err = transport.Encode(out)
	}
	if err != nil {
		h.pipes.end(sess, errPipeEnded)
		h.refuse(w, http.StatusInternalServerError, "unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// pipeSend is POST /v1/pipe/send?sid=…: the caller's outgoing bytes, read for
// as long as they keep coming and handed to the other side's receive stream.
// The request ends when the caller ends its body (a clean goodbye) or goes
// silent past pipeReadWait; either way the session is over.
func (h *directHandler) pipeSend(w http.ResponseWriter, r *http.Request) {
	peer, ok := transport.PeerFromContext(r.Context())
	if !ok {
		h.refuse(w, http.StatusForbidden, "not-allowed")
		return
	}
	s, d, code := h.pipes.attach(r.URL.Query().Get("sid"), peer, true)
	if code != "" {
		h.refuseFor(w, r, pipeStatus(code), code)
		return
	}
	rc := http.NewResponseController(w)
	// Full duplex, and no keep-alive: when the session ends under a body
	// that is still open, the answer must go out NOW. Without both, the
	// server would first try to read the rest of the body — a stream the
	// browser has not ended — and hold the answer until the silence deadline.
	rc.EnableFullDuplex()
	w.Header().Set("Connection", "close")
	// The body is read on its own goroutine so that a session ended by the
	// OTHER side (its streams closed) can end this request too, rather than
	// leaving it blocked in a read until the silence deadline.
	result := make(chan error, 1)
	go func() {
		buf := make([]byte, pipeChunk)
		for {
			// Silence, not duration: the deadline moves with every read.
			rc.SetReadDeadline(time.Now().Add(pipeReadWait))
			n, rerr := r.Body.Read(buf)
			if n > 0 {
				if _, werr := d.w.Write(buf[:n]); werr != nil {
					result <- werr
					return
				}
				h.pipes.touch(s)
			}
			if rerr != nil {
				if rerr == io.EOF {
					result <- nil
				} else {
					result <- rerr
				}
				return
			}
		}
	}()
	var failed error
	select {
	case failed = <-result:
	case <-s.done:
		failed = errPipeEnded
		// The reader goroutine is still parked in a body read; the server
		// will wait for that read before it can finish this request, so
		// make it return now rather than at the silence deadline.
		rc.SetReadDeadline(time.Now())
	}
	if failed != nil {
		h.pipes.end(s, failed)
		h.refuseFor(w, r, http.StatusGone, "pipe-closed")
		return
	}
	h.pipes.end(s, errPipeEnded)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// pipeRecv is GET /v1/pipe/recv?sid=…: the other side's bytes, streamed as
// they arrive, flushed per chunk, until the session ends.
func (h *directHandler) pipeRecv(w http.ResponseWriter, r *http.Request) {
	peer, ok := transport.PeerFromContext(r.Context())
	if !ok {
		h.refuse(w, http.StatusForbidden, "not-allowed")
		return
	}
	s, d, code := h.pipes.attach(r.URL.Query().Get("sid"), peer, false)
	if code != "" {
		h.refuseFor(w, r, pipeStatus(code), code)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	rc.Flush()
	// A reader blocked in d.r.Read learns that its browser went away only
	// through the request context; closing the read half wakes it.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-r.Context().Done():
			d.r.CloseWithError(errPipeGone)
		case <-stop:
		}
	}()
	buf := make([]byte, pipeChunk)
	for {
		n, rerr := d.r.Read(buf)
		if n > 0 {
			rc.SetWriteDeadline(time.Now().Add(pipeWriteWait))
			if _, werr := w.Write(buf[:n]); werr != nil {
				h.pipes.end(s, werr)
				return
			}
			if err := rc.Flush(); err != nil {
				h.pipes.end(s, err)
				return
			}
			h.pipes.touch(s)
		}
		if rerr != nil {
			h.pipes.end(s, rerr)
			return
		}
	}
}

func pipeStatus(code string) int {
	switch code {
	case "no-session":
		return http.StatusNotFound
	case "not-allowed":
		return http.StatusForbidden
	case "busy":
		return http.StatusConflict
	}
	return http.StatusBadRequest
}
