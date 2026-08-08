package main

import (
	"strings"
	"testing"
	"time"
)

// The mode must follow the LATEST response: a text body is what every relay
// said before the field existed, a JSON body without the field is a relay
// that answers in JSON but does not speak hints, and both mean "poll on the
// timer". Only the field itself flips the mode — and a rollback flips it
// back.
func TestHintModeFollowsTheLatestResponse(t *testing.T) {
	b := newHintBox()
	if b.signalMode() {
		t.Fatal("a box that has seen nothing must poll on the timer")
	}
	b.absorb([]byte("ok"))
	if b.signalMode() {
		t.Fatal("a text body is not a hint")
	}
	b.absorb([]byte(`{"ok":true}`))
	if b.signalMode() {
		t.Fatal("JSON without the field is not a hint")
	}
	b.absorb([]byte(`{"ok":true,"poll":false}`))
	if !b.signalMode() {
		t.Fatal("the field itself is what flips the mode")
	}
	b.absorb([]byte("ok"))
	if b.signalMode() {
		t.Fatal("a relay rolled back must take its helpers back with it")
	}
}

// A body bigger than the bound is not the protocol, however its first bytes
// parse. Truncating and believing it would let one oversized response pick
// the mode.
func TestHintOversizeBodyIsNotTheProtocol(t *testing.T) {
	b := newHintBox()
	big := `{"ok":true,"poll":false` + strings.Repeat(" ", hintMaxBody) + `}`
	b.absorb([]byte(big))
	if b.signalMode() {
		t.Fatal("an oversized body entered signal mode")
	}
}

// `poll: true` has to end a dormant wait already in progress, and one that
// arrives while the loop is busy must end the NEXT wait instead of being
// lost — the buffered token is that memory.
func TestHintTrueWakesAWaiterNowOrNext(t *testing.T) {
	b := newHintBox()

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		b.dormantAwait(5 * time.Second)
		done <- time.Since(start)
	}()
	time.Sleep(50 * time.Millisecond)
	b.absorb([]byte(`{"ok":true,"poll":true}`))
	select {
	case d := <-done:
		if d >= 5*time.Second {
			t.Fatalf("waited the whole timer despite a hint: %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hint did not wake the waiter")
	}

	// Nobody waiting: the token sits, and the next wait returns at once.
	b.absorb([]byte(`{"ok":true,"poll":true}`))
	start := time.Now()
	b.dormantAwait(5 * time.Second)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("a banked hint should end the next wait at once, took %v", d)
	}
}

// The relay stopping to speak hints must ALSO end a dormant wait: the nap
// was only ever safe while the relay was answering for the queue, and a
// shell command lives ninety seconds while the nap lasts three hundred.
// Same for a push that failed outright — that is wakeNow's other caller.
func TestLosingTheHintWakesTheWaiter(t *testing.T) {
	b := newHintBox()
	b.absorb([]byte(`{"ok":true,"poll":false}`)) // signal mode, nothing banked
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		b.dormantAwait(5 * time.Second)
		done <- time.Since(start)
	}()
	time.Sleep(50 * time.Millisecond)
	b.absorb([]byte("ok")) // the relay rolled back mid-nap
	select {
	case d := <-done:
		if d >= 5*time.Second {
			t.Fatalf("slept through the rollback: %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rollback did not wake the waiter")
	}
	if b.signalMode() {
		t.Fatal("mode did not follow the rollback")
	}
}

// `poll: false` must not wake anybody — it is the common case, and if it
// woke the loop every push would trigger a poll, which is the exact traffic
// this exists to remove.
func TestHintFalseDoesNotWake(t *testing.T) {
	b := newHintBox()
	b.absorb([]byte(`{"ok":true,"poll":false}`))
	start := time.Now()
	b.dormantAwait(80 * time.Millisecond)
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("poll:false ended a wait early (%v)", d)
	}
}

// The dormancy decision: only a hint-speaking relay's COLD answer earns the
// safety net. A legacy relay keeps the plain timer whatever it says, and a
// warm answer keeps the ten-second cadence even when hints are spoken — the
// interactive path must not change.
func TestHintDormantOnlyOnTheColdHintingCase(t *testing.T) {
	if hintDormant(false, 60) {
		t.Fatal("legacy relay, cold: must keep the plain timer")
	}
	if hintDormant(true, 10) {
		t.Fatal("hinting relay, warm: must keep the warm cadence")
	}
	if !hintDormant(true, 60) {
		t.Fatal("hinting relay, cold: must take the safety net")
	}
	if hintDormant(true, 0) {
		t.Fatal("a relay that sent no next is not saying cold")
	}
}
