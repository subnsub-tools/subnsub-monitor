package transport

import (
	"context"
	"errors"
	"sync"
)

// Fake is an in-memory carrier for tests: two ends joined by bounded channels,
// so a test can drive the helper's frame logic without a network, and a
// recorder can assert exactly which frames crossed in which order.
//
// Bounded on purpose. The queue depth is the backpressure model every real
// carrier has to honour — a Send into a full queue blocks until the peer
// reads or the context ends — and a test that fills it is a test that has
// found a reader which stopped reading.
type Fake struct {
	mu      sync.Mutex
	started bool
	closed  bool
	id      Identity
	in      chan Frame // frames for this end to Receive
	out     chan Frame // frames this end Sent; the peer's in
	sent    []Frame    // everything Send accepted, in order
	// done is closed by Close so a Send blocked on backpressure returns
	// ErrClosed instead of panicking on a channel closed under it; sending
	// counts the Sends in flight so Close can wait for them before it
	// closes out (review 2026-08-31: send-on-closed-channel).
	done    chan struct{}
	sending sync.WaitGroup
}

// NewFakePair returns two joined ends with the given queue depth per
// direction. depth < 1 is treated as 1: an unbuffered pair would make every
// Send wait for a concurrent Receive, which is a stricter model than any
// carrier actually offers.
func NewFakePair(depth int) (*Fake, *Fake) {
	if depth < 1 {
		depth = 1
	}
	ab := make(chan Frame, depth)
	ba := make(chan Frame, depth)
	return &Fake{in: ba, out: ab, done: make(chan struct{})}, &Fake{in: ab, out: ba, done: make(chan struct{})}
}

// ErrNotStarted is returned by Send and Receive before Start.
var ErrNotStarted = errors.New("transport: not started")

// ErrClosed is returned by Send after Close.
var ErrClosed = errors.New("transport: closed")

func (f *Fake) Start(ctx context.Context, id Identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrClosed
	}
	f.started = true
	f.id = id
	return nil
}

// Send stamps the frame with this end's identity (a carrier never lets a
// frame claim to be from somebody else) and queues it, blocking under
// backpressure until the peer drains or ctx ends.
func (f *Fake) Send(ctx context.Context, fr Frame) error {
	f.mu.Lock()
	if !f.started {
		f.mu.Unlock()
		return ErrNotStarted
	}
	if f.closed {
		f.mu.Unlock()
		return ErrClosed
	}
	fr.AgentID = f.id.AgentID
	f.sending.Add(1)
	f.mu.Unlock()
	defer f.sending.Done()
	if _, err := Encode(fr); err != nil {
		return err
	}
	select {
	case f.out <- fr:
	case <-f.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	f.mu.Lock()
	f.sent = append(f.sent, fr)
	f.mu.Unlock()
	return nil
}

func (f *Fake) Receive(ctx context.Context) (<-chan Frame, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started {
		return nil, ErrNotStarted
	}
	return f.in, nil
}

// Close closes this end's outgoing channel, which the peer's Receive sees as
// end-of-stream. Idempotent; a second Close is a no-op rather than a panic.
func (f *Fake) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	close(f.done)
	f.mu.Unlock()
	// Every Send that passed the closed check above is counted; they all
	// leave through done now, and only then is out safe to close.
	f.sending.Wait()
	close(f.out)
	return nil
}

// Sent returns a copy of every frame this end has successfully sent, in
// order — the recorder half of "recorder/fake transport" in the plan.
func (f *Fake) Sent() []Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Frame, len(f.sent))
	copy(out, f.sent)
	return out
}
