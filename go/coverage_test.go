package main

// The coverage endpoint (functions/api/monitor-coverage.js) tells the
// dashboard which providers this helper ships alongside separately labelled
// references and candidates. The endpoint cannot import Go, so this test is
// the tie: the SUPPORTED array there and the collector registry here must be
// the same list. Candidate coverage must never become an implementation claim
// merely because a third-party project knows a provider's name.
//
// Skipped silently outside the monorepo (the public mirror ships helper/go
// without the site), so `go test` stays green for anyone who cloned just the
// helper — the check matters in the tree where both files live, which is the
// tree releases are cut from.

import (
	"os"
	"regexp"
	"strings"
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

	text := string(src)
	if strings.Contains(text, "api.github.com") || strings.Contains(text, "raw.githubusercontent.com") || strings.Contains(text, "fetch(") {
		t.Fatal("coverage registry must not fetch a moving upstream source")
	}
	commits := regexp.MustCompile(`commit:\s*'([^']+)'`).FindAllStringSubmatch(text, -1)
	if len(commits) == 0 {
		t.Fatal("coverage registry has no pinned source commits")
	}
	fullCommit := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, m := range commits {
		if !fullCommit.MatchString(m[1]) {
			t.Errorf("coverage source commit %q is not an immutable full SHA", m[1])
		}
	}
	statuses := regexp.MustCompile(`integration_status:\s*'([^']+)'`).FindAllStringSubmatch(text, -1)
	allowedStatus := map[string]bool{
		"reference":    true,
		"candidate":    true,
		"hold":         true,
		"gateway-only": true,
	}
	for _, m := range statuses {
		if !allowedStatus[m[1]] {
			t.Errorf("coverage source has unknown integration status %q", m[1])
		}
	}
	if !strings.Contains(text, "automatic_upstream_updates: false") ||
		!strings.Contains(text, "executes_third_party_tools: false") {
		t.Fatal("coverage registry must explicitly disable automatic updates and third-party execution")
	}
	codexBar := regexp.MustCompile(`(?s)id:\s*'codexbar'.*?commit:\s*'([^']+)'`).FindStringSubmatch(text)
	if codexBar == nil || codexBar[1] != codexBarReferenceCommit {
		t.Errorf("coverage CodexBar commit and helper provenance differ: %v", codexBar)
	}
	relay, err := os.ReadFile("../../monitor-worker/src/clean.js")
	if err != nil {
		t.Fatalf("coverage endpoint exists but relay cleaner does not: %v", err)
	}
	relayRef := regexp.MustCompile(`\['codexbar',\s*'([^']+)'\]`).FindStringSubmatch(string(relay))
	if relayRef == nil || relayRef[1] != codexBarReferenceCommit {
		t.Errorf("relay CodexBar allow-list and helper provenance differ: %v", relayRef)
	}
	relayRolesBlock := regexp.MustCompile(`(?s)const CODEXBAR_ROLE_BY_PROVIDER = new Map\(\[(.*?)\]\);`).FindSubmatch(relay)
	if relayRolesBlock == nil {
		t.Fatal("could not find relay's provider/reference-role allow-list")
	}
	relayRoles := map[string]string{}
	for _, pair := range regexp.MustCompile(`\['([^']+)',\s*'([^']+)'\]`).FindAllStringSubmatch(string(relayRolesBlock[1]), -1) {
		relayRoles[pair[1]] = pair[2]
	}
	for id, role := range codexBarReferenceRoles {
		if relayRoles[id] != role {
			t.Errorf("relay role for %q is %q, helper role is %q", id, relayRoles[id], role)
		}
		delete(relayRoles, id)
	}
	for id, role := range relayRoles {
		t.Errorf("relay allows undocumented CodexBar role %q for %q", role, id)
	}
}

func TestCollectorProvenanceIsPinnedAndTruthful(t *testing.T) {
	shipped := map[string]bool{}
	for _, c := range collectors {
		shipped[c.id] = true
		got := collectorProvenance(c.id, "api")
		if got == nil || got.ID != "subnsub-native" || got.Method != "api" {
			t.Fatalf("collector %q has invalid native provenance: %#v", c.id, got)
		}
		role := codexBarReferenceRoles[c.id]
		if role == "" {
			if len(got.References) != 0 {
				t.Errorf("collector %q claims an undocumented reference", c.id)
			}
			continue
		}
		if len(got.References) != 1 {
			t.Fatalf("collector %q: got %d references, want one", c.id, len(got.References))
		}
		ref := got.References[0]
		if ref.ID != "codexbar" || ref.Commit != codexBarReferenceCommit || ref.Role != role {
			t.Errorf("collector %q has wrong reference provenance: %#v", c.id, ref)
		}
	}

	roles := map[string]bool{
		"protocol-reference": true,
		"parser-reference":   true,
		"behavior-reference": true,
	}
	fullCommit := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if !fullCommit.MatchString(codexBarReferenceCommit) {
		t.Fatalf("CodexBar reference %q is not a full commit", codexBarReferenceCommit)
	}
	for id, role := range codexBarReferenceRoles {
		if !shipped[id] {
			t.Errorf("reference provenance names unshipped collector %q", id)
		}
		if !roles[role] {
			t.Errorf("collector %q has unknown reference role %q", id, role)
		}
	}
}
