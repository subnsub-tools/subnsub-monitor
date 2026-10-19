//go:build linux

package main

import (
	"sort"
	"testing"
)

// The second road has to arrive where the first one does. On ordinary Linux
// both are open, so every interface the fallback lists must be one the
// stdlib lists, with the same up flag and the same addresses; the fallback is
// allowed to miss only the interfaces that carry no address at all.
func TestInterfacesNoBindMatchesStdlib(t *testing.T) {
	got, ok := interfacesNoBind()
	if !ok {
		t.Skip("netlink RTM_GETADDR not available here")
	}
	want, ok := stdlibInterfaces()
	if !ok {
		t.Skip("net.Interfaces unavailable here")
	}
	byName := map[string]ifaceInfo{}
	for _, w := range want {
		byName[w.name] = w
	}
	seen := map[string]bool{}
	for _, g := range got {
		w, found := byName[g.name]
		if !found {
			t.Errorf("fallback lists %q, stdlib does not", g.name)
			continue
		}
		if seen[g.name] {
			t.Errorf("fallback lists %q twice", g.name)
		}
		seen[g.name] = true
		if g.up != w.up || g.loopback != w.loopback {
			t.Errorf("%s: fallback up=%v loopback=%v, stdlib up=%v loopback=%v", g.name, g.up, g.loopback, w.up, w.loopback)
		}
		gs, ws := ipStrings(g), ipStrings(w)
		if len(gs) != len(ws) {
			t.Errorf("%s: fallback addresses %v, stdlib %v", g.name, gs, ws)
			continue
		}
		for i := range gs {
			if gs[i] != ws[i] {
				t.Errorf("%s: fallback addresses %v, stdlib %v", g.name, gs, ws)
				break
			}
		}
	}
	for _, w := range want {
		if !seen[w.name] && len(w.ips) > 0 {
			t.Errorf("stdlib lists %q with addresses, fallback missed it", w.name)
		}
	}
}

func ipStrings(i ifaceInfo) []string {
	out := make([]string, 0, len(i.ips))
	for _, ip := range i.ips {
		out = append(out, ip.String())
	}
	sort.Strings(out)
	return out
}
