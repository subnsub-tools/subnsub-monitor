package main

// Sampling rate and REPORTING rate are two different numbers, and until now
// this helper had only one. A push every thirty seconds meant one system
// reading every thirty seconds — and because cpu_percent is a difference
// against the previous reading, that number was a thirty-second AVERAGE
// delivered thirty seconds apart. A compile that ran hot for five seconds
// arrived as a two-degree bump in a flat line, on a page that repainted twice
// a minute. The dashboard looked frozen because, at the resolution it was
// given, it was.
//
// So: sample every second into a ring, and let the push carry the whole ring
// alongside the snapshot. The relay bills pushes, not samples, so thirty times
// the resolution costs nothing there — the frame grows by about a kilobyte and
// the write count, the request count and the cadence are all exactly what they
// were. What changes is that the page receives a LINE where it used to receive
// a dot, and can draw the last thirty seconds instead of asserting them.
//
// ★ THE SAMPLER OWNS THE DELTAS. cpu_percent, the two network rates and the
// retransmit rate are all differences against package-level "previous reading"
// state (see cpuDelta, netDelta, tcpDelta). Two callers reading at different
// cadences do not each get the interval they think they asked for — each one
// gets whatever gap happened to separate it from the OTHER caller's read, so a
// push landing 200ms after a sample would report a 200ms average as its
// thirty-second one. Once this sampler is running it is therefore the only
// thing in the process that calls collectSystem(), and collectAll takes the
// newest sample instead of measuring again. The modes that do not run it
// (one-shot, serve) keep measuring for themselves, which is correct: a single
// snapshot has no series to carry.

import (
	"math"
	"sync"
	"time"
)

const (
	// One second: the finest resolution the page can actually draw on a strip
	// a few hundred pixels wide, and cheap enough to be uninteresting —
	// collectSystem is a handful of /proc reads and a statfs (measured at well
	// under a millisecond per call on a 4-core box with 460 /proc entries).
	samplerStep = 1.0
	// How many slots a frame may carry. clampInterval allows a push interval up
	// to 60s, and a late push has to find its samples still here, so this is
	// that plus a fifth of slack. A machine that was asleep, or backing off
	// after a failed push, drops the oldest samples rather than sending a
	// history nobody can use.
	samplerSlots = 72
)

type sysSample struct {
	at  float64
	sys System
}

// Package state rather than a value threaded through collectAll: the push loop
// and the sampler are two goroutines that have to agree on which readings have
// already been sent, and the alternative is a parameter on every path that
// reaches collectAll — including the ones (one-shot, serve) that must keep
// working with no sampler at all.
var sampler struct {
	sync.Mutex
	on   bool
	buf  []sysSample // oldest first, at most samplerSlots
	sent float64     // `at` of the newest sample a push has already carried
}

// Begin sampling in the background. Idempotent, and safe to call before the
// push loop starts — which is the only place it IS called from, because a
// series is meaningless to a process that collects once and exits.
func startSysSampler() {
	sampler.Lock()
	if sampler.on {
		sampler.Unlock()
		return
	}
	sampler.on = true
	sampler.Unlock()

	// The first sample is taken synchronously, before this returns. If the
	// first push were to arrive with the ring still empty it would fall back to
	// measuring for itself — and that one call is enough to take the baseline
	// this sampler is about to build its first delta against.
	recordSysSample(collectSystem())

	go func() {
		t := time.NewTicker(time.Duration(samplerStep * float64(time.Second)))
		defer t.Stop()
		for range t.C {
			recordSysSample(collectSystem())
		}
	}()
}

func recordSysSample(sys System) {
	sampler.Lock()
	defer sampler.Unlock()
	sampler.buf = append(sampler.buf, sysSample{at: now(), sys: sys})
	if n := len(sampler.buf); n > samplerSlots {
		// Copy down rather than reslice: sampler.buf[n-samplerSlots:] would keep
		// the whole backing array alive and let it grow without bound, one
		// sample per second, forever.
		sampler.buf = append(sampler.buf[:0], sampler.buf[n-samplerSlots:]...)
	}
}

// The newest sample, plus every sample since the last push, as one frame's
// worth of system readings. Returns (nil, nil) when no sampler is running, so
// the caller can measure for itself.
//
// Samples are marked as sent HERE rather than after a successful push. A push
// that fails has nothing to retry with — the relay never saw the frame, and the
// next one is thirty seconds away — so holding the samples back would only
// widen the next frame with points too old to draw beside its own.
func sampledSystem() (*System, *SysSeries) {
	sampler.Lock()
	defer sampler.Unlock()
	if !sampler.on || len(sampler.buf) == 0 {
		return nil, nil
	}
	newest := sampler.buf[len(sampler.buf)-1]
	fresh := make([]sysSample, 0, len(sampler.buf))
	for _, sm := range sampler.buf {
		if sm.at > sampler.sent {
			fresh = append(fresh, sm)
		}
	}
	sampler.sent = newest.at
	// A copy: the caller hangs a series off it, and the ring's own sample must
	// stay exactly as it was measured.
	sys := newest.sys
	return &sys, buildSysSeries(fresh, newest.at)
}

// Lay the samples out on a fixed grid ending at the newest one.
//
// A grid rather than a timestamp per sample, because it halves the frame: the
// page needs to know WHEN each point was taken, and "one second apart, ending
// here" says that in two numbers instead of thirty. Slots with no sample are
// null — a machine that slept, or a ticker the scheduler starved, leaves a
// visible hole rather than a line drawn straight across the gap.
func buildSysSeries(fresh []sysSample, end float64) *SysSeries {
	// One point is not a series: the snapshot already carries that reading, and
	// a one-slot series would only be a second copy of it.
	if len(fresh) < 2 {
		return nil
	}
	n := int(math.Round((end-fresh[0].at)/samplerStep)) + 1
	if n < 2 {
		return nil
	}
	if n > samplerSlots {
		// The oldest samples fall off the front — the machine slept, or a push
		// was late enough that the ring wrapped. Sending the gap as hundreds of
		// nulls would be honest and useless.
		n = samplerSlots
	}
	s := &SysSeries{At: end, Step: samplerStep,
		CPU: make([]*float64, n), Mem: make([]*float64, n),
		Rx: make([]*float64, n), Tx: make([]*float64, n)}
	for _, sm := range fresh {
		i := n - 1 - int(math.Round((end-sm.at)/samplerStep))
		if i < 0 || i >= n {
			continue
		}
		// Rounded for the wire, and the widths are the reason: at full
		// precision "278829.74" is nine characters and a frame carries a
		// hundred and twenty of these. A tenth of a percent is finer than a
		// 26-pixel-tall strip can draw, and a byte-per-second rate quoted to
		// the centibyte was never a real measurement. Same rounding the relay
		// already applies to its history buckets.
		s.CPU[i] = round1p(sm.sys.CPUPercent)
		s.Mem[i] = round1p(sm.sys.MemUsedPercent)
		s.Rx[i] = round0p(sm.sys.NetRxBps)
		s.Tx[i] = round0p(sm.sys.NetTxBps)
	}
	// A platform that cannot read a metric sends no array for it rather than a
	// row of nulls — that is a quarter of the frame on a Mac, which reports
	// neither CPU nor memory, and it keeps the relay from validating thirty
	// nulls to decide they are still nulls.
	s.CPU = dropIfAllNil(s.CPU)
	s.Mem = dropIfAllNil(s.Mem)
	s.Rx = dropIfAllNil(s.Rx)
	s.Tx = dropIfAllNil(s.Tx)
	if s.CPU == nil && s.Mem == nil && s.Rx == nil && s.Tx == nil {
		return nil
	}
	return s
}

// Nil in, nil out — a slot with no sample stays a hole rather than becoming a
// zero, which on a CPU line is the difference between "not measured" and
// "perfectly idle".
func round1p(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return fp(math.Round(*v*10) / 10)
}

func round0p(v *float64) *float64 {
	if v == nil {
		return nil
	}
	return fp(math.Round(*v))
}

func dropIfAllNil(v []*float64) []*float64 {
	for _, p := range v {
		if p != nil {
			return v
		}
	}
	return nil
}
