package main

// Shared plumbing for the credential-holding collectors (Claude was the first;
// Gemini, Copilot, Droid and Kimi joined it). Two pieces live here, and both
// exist because the fourth copy of either would have drifted from the third:
//
// THE CACHE FLOOR. Every one of these is a real request to someone else's
// service, and the push loop's 30s would be 2,880 calls a day per machine per
// provider. The rules are amp.go's, which found them the hard way:
//   - the floor is on ATTEMPTS, not successes, so an outage does not turn the
//     floor off exactly when hammering helps least;
//   - inside the floor a stale-but-real reading is served over a fresh error,
//     up to a staleness bound past which the error is the truth;
//   - CapturedAt says when we looked, RecordedAt when it was fetched, so a
//     cached reading visibly ages on the page.
//
// THE REQUEST SHAPE. Same non-negotiables as claude.go, because these carry
// credentials: redirects refused outright (Go's client forwards Authorization
// on same-host hops — a redirect anywhere is a token handed to whoever named
// the Location), bounded reads, and NO error text derived from the transport —
// an http error can quote the URL and the Authorization header, and these
// failures are PUBLISHED.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	provMinInterval = 120.0 // seconds between real calls, per provider
	provStaleMax    = 600.0 // how long a cached success outlives failures
	provTimeout     = 10 * time.Second
	provMaxBody     = 512 * 1024
)

type provCache struct {
	sync.Mutex
	last        *Provider // last SUCCESSFUL reading
	fetchedAt   float64
	fail        *Provider // last failure, served while the floor holds
	attemptedAt float64
}

// The serve/floor/fetch dance every cached collector does, in one place.
// fetch runs with the lock held — same as every existing collector, and
// deliberate: two pushes racing would otherwise both call the vendor.
func (c *provCache) collect(fetch func() Provider) Provider {
	c.Lock()
	defer c.Unlock()

	t := now()
	serve := func(p *Provider) Provider {
		out := *p
		out.CapturedAt = t // when we looked; RecordedAt still says when fetched
		return out
	}

	if c.last != nil && t-c.fetchedAt < provMinInterval {
		return serve(c.last)
	}
	if c.attemptedAt > 0 && t-c.attemptedAt < provMinInterval {
		if c.last != nil && t-c.fetchedAt < provStaleMax {
			return serve(c.last)
		}
		if c.fail != nil {
			return serve(c.fail)
		}
	}

	c.attemptedAt = t
	p := fetch()
	if p.OK {
		c.last, c.fetchedAt, c.fail = &p, t, nil
		return p
	}
	c.fail = &p
	if c.last != nil && t-c.fetchedAt < provStaleMax {
		return serve(c.last)
	}
	c.last = nil
	return p
}

var errProvRedirect = errors.New("redirect refused")

// One authenticated request, returning status and a bounded body. The error is
// a bare category — never the transport's text, for the reason at the top.
func provRequest(method, url string, header map[string]string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, errors.New("bad request")
	}
	req.Header.Set("User-Agent", "subnsub-monitor")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Timeout: provTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errProvRedirect
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, errors.New("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, nil, errProvRedirect
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, provMaxBody+1))
	if err != nil {
		return resp.StatusCode, nil, errors.New("read failed")
	}
	if int64(len(data)) > provMaxBody {
		// A quota endpoint answering in megabytes is not the endpoint we think
		// it is. Refuse the whole reading rather than parse a prefix.
		return resp.StatusCode, nil, errors.New("response too large")
	}
	return resp.StatusCode, data, nil
}
