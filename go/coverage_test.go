package main

// The coverage endpoint (functions/api/monitor-coverage.js) tells the
// dashboard which providers this helper ships, and it says so from its own
// hardcoded list — the endpoint cannot import Go. This test is the tie: the
// SUPPORTED array there and the collector registry in types.go must be the
// same list, or the dashboard's coverage card drifts from the binary the
// moment either side adds a provider without the other.
//
// Skipped silently outside the monorepo (the public mirror ships helper/go
// without the site), so `go test` stays green for anyone who cloned just the
// helper — the check matters in the tree where both files live, which is the
// tree releases are cut from.

import (
	"os"
	"regexp"
	"testing"
)

func TestCoverageEndpointMatchesCollectorRegistry(t *testing.T) {
	src, err := os.ReadFile("../../functions/api/monitor-coverage.js")
	if err != nil {
		t.Skip("coverage endpoint not in this tree (public mirror)")
	}
	m := regexp.MustCompile(`(?s)const SUPPORTED = \[(.*?)\];`).FindSubmatch(src)
	if m == nil {
		t.Fatal("could not find SUPPORTED in monitor-coverage.js")
	}
	declared := map[string]bool{}
	for _, id := range regexp.MustCompile(`'([a-z0-9]+)'`).FindAllStringSubmatch(string(m[1]), -1) {
		declared[id[1]] = true
	}

	// The registry's ids, NOT collectAll()'s output — running the collectors
	// here would make `go test` place real authenticated calls to four
	// vendors from whatever machine runs it.
	shipped := map[string]bool{}
	for _, c := range collectors {
		shipped[c.id] = true
	}

	for id := range shipped {
		if !declared[id] {
			t.Errorf("collector %q ships but monitor-coverage.js does not declare it", id)
		}
	}
	for id := range declared {
		if !shipped[id] {
			t.Errorf("monitor-coverage.js declares %q but no collector ships it", id)
		}
	}
	if len(shipped) == 0 || len(declared) == 0 {
		t.Fatal("one of the lists parsed empty — the test itself has rotted")
	}
}
