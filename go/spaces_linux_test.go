//go:build linux

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSpacesPick(t *testing.T) {
	found := []string{"ts-s25", "cni-0a1b2c3d", "wg.exit_2", "bad name", "../etc", "z", "y", "x", "w", strings.Repeat("n", 33)}
	t.Setenv(spacesEnvVar, "")
	if got := fmt.Sprint(spacesPick(found)); got != "[ts-s25 w wg.exit_2 x]" {
		t.Errorf("every safe name, sorted, four at most, no pod namespaces: %s", got)
	}
	t.Setenv(spacesEnvVar, "off")
	if got := spacesPick(found); len(got) != 0 {
		t.Errorf("off: %v", got)
	}
	t.Setenv(spacesEnvVar, " z , ts-s25,absent,bad name")
	if got := fmt.Sprint(spacesPick(found)); got != "[ts-s25 z]" {
		t.Errorf("a list pins the set: %s", got)
	}
}

// Needs a namespace to enter, so it runs only inside the sandbox that
// infra/helper-netns-live.sh builds (an unprivileged user namespace holding
// "vpn" at 10.99.0.2 and this test at 10.99.0.1).
func TestSpaceProbeLive(t *testing.T) {
	if os.Getenv("MON_NETNS_LIVE") != "1" {
		t.Skip("run by infra/helper-netns-live.sh")
	}
	scratchHome(t)
	ln, err := net.Listen("tcp", "10.99.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	seen := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("agent") != agentID() {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, `{"stamp":%q}`, host)
	})
	mux.HandleFunc("/from", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		mu.Lock()
		seen[host]++
		mu.Unlock()
	})
	go http.Serve(ln, mux)
	base := "http://" + ln.Addr().String()

	mainNS := func() uint64 {
		var st syscall.Stat_t
		if err := syscall.Stat("/proc/self/ns/net", &st); err != nil {
			t.Fatal(err)
		}
		return st.Ino
	}
	before := mainNS()
	defer func() {
		// /proc/self is the MAIN thread, and /proc/net is its view: the
		// sampler's interface counters come from there.
		if after := mainNS(); after != before {
			t.Errorf("the main thread was moved into the namespace (%d → %d)", before, after)
		}
	}()
	if found := fmt.Sprint(spacesFound()); found != "[vpn]" {
		t.Fatalf("named namespaces: %s", found)
	}
	sp, status := spaceProbe(base, "tok", "vpn")
	if sp == nil || !sp.OK || sp.Stamp != "10.99.0.2" || sp.RTTMs == nil || status != 200 {
		t.Fatalf("the probe should arrive FROM the namespace: %+v (status %d)", sp, status)
	}
	// And nothing else moved there: every other connection this process
	// makes, on whatever thread, still leaves from its own namespace.
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			if r, err := c.Get(base + "/from"); err == nil {
				r.Body.Close()
			}
		}()
		if i%2 == 0 {
			spaceProbe(base, "tok", "vpn")
		}
	}
	wg.Wait()
	if seen["10.99.0.1"] != 64 || len(seen) != 1 {
		t.Fatalf("a thread leaked into the namespace: %v", seen)
	}

	if sp, _ := spaceProbe(base, "wrong", "vpn"); sp == nil || sp.OK || sp.Error != "refused" || sp.Stamp != "" {
		t.Errorf("a refusal: %+v", sp)
	}
	if sp, _ := spaceProbe("http://10.99.0.1:1", "tok", "vpn"); sp == nil || sp.OK || sp.Error != "unreachable" {
		t.Errorf("nothing listening: %+v", sp)
	}
	// An address that swallows the SYN: reported as the namespace timing out
	// — it WAS entered — and within one address's budget, not the kernel's.
	began := time.Now()
	if sp, _ := spaceProbe("http://10.99.0.77:9", "tok", "vpn"); sp == nil || sp.OK || (sp.Error != "timeout" && sp.Error != "unreachable") {
		t.Errorf("a black hole: %+v", sp)
	}
	if took := time.Since(began); took > spaceDialTimeout+3*time.Second {
		t.Errorf("the dial was not bounded: %v", took)
	}
	if sp, _ := spaceProbe(base, "tok", "absent"); sp != nil {
		t.Errorf("a namespace that cannot be entered is not reported: %+v", sp)
	}
	if _, status := spaceProbe(base+"/nowhere", "tok", "vpn"); status != 404 {
		t.Errorf("an older relay: %d", status)
	}
	if un := spacesPass(base+"/nowhere", "tok"); !un || spacesSnapshot() != nil {
		t.Errorf("a relay without /whoami is unsupported, and nothing is reported: %v %v", un, spacesSnapshot())
	}
	if spacesPass(base, "tok") {
		t.Fatal("this relay has it")
	}
	if got := spacesSnapshot(); len(got) != 1 || got[0].Name != "vpn" || got[0].Stamp != "10.99.0.2" {
		t.Errorf("snapshot: %+v", got)
	}
}
