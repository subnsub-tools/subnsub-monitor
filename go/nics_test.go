package main

import (
	"net"
	"testing"
)

// The list drops the addresses that are true of every machine and describe
// none of them. A regression here reaches the wire as a card wearing a page of
// link-local noise.
func TestNICAddressFilter(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"192.168.1.20", true},
		{"2001:db8::1", true},
		{"10.0.0.5", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"169.254.3.4", false},
		{"fe80::1", false},
		{"0.0.0.0", false},
		{"::", false},
	}
	for _, c := range cases {
		if got := nicAddrWorthSending(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("%s: worth sending %v, want %v", c.ip, got, c.want)
		}
	}
}

// Whatever this machine happens to have, the SHAPE it travels in is the
// contract: named rows, no duplicates, bounded both ways, loopback gone.
func TestFillNICsShape(t *testing.T) {
	var s System
	fillNICs(&s)
	if len(s.NICs) > maxNICs {
		t.Fatalf("%d interfaces, cap is %d", len(s.NICs), maxNICs)
	}
	seen := map[string]bool{}
	for _, n := range s.NICs {
		if n.Name == "" {
			t.Error("an interface row with no name")
		}
		if seen[n.Name] {
			t.Errorf("interface %q listed twice", n.Name)
		}
		seen[n.Name] = true
		if len(n.IPs) > maxNICAddrs {
			t.Errorf("%s: %d addresses, cap is %d", n.Name, len(n.IPs), maxNICAddrs)
		}
		for _, ip := range n.IPs {
			p := net.ParseIP(ip)
			if p == nil {
				t.Errorf("%s: %q is not an address", n.Name, ip)
			} else if !nicAddrWorthSending(p) {
				t.Errorf("%s: %q should have been filtered", n.Name, ip)
			}
		}
		if !n.Up && len(n.IPs) == 0 {
			t.Errorf("%s: a down interface with no address earns no row", n.Name)
		}
	}
}

// Counters attach by name, and only to interfaces the address walk already
// admitted: a name present in the kernel's counter table but not in its
// interface table must not invent a row, and a negative counter is not a
// reading.
func TestNICCountersMatchByName(t *testing.T) {
	s := System{NICs: []NIC{{Name: "eth0"}, {Name: "wg0"}}}
	nicCounters(&s,
		map[string]float64{"eth0": 100, "ghost": 7},
		map[string]float64{"eth0": 200, "wg0": -1})
	if len(s.NICs) != 2 {
		t.Fatalf("the counter table changed the row count: %d", len(s.NICs))
	}
	if s.NICs[0].RxTotal == nil || *s.NICs[0].RxTotal != 100 {
		t.Error("eth0 lost its receive counter")
	}
	if s.NICs[0].TxTotal == nil || *s.NICs[0].TxTotal != 200 {
		t.Error("eth0 lost its transmit counter")
	}
	if s.NICs[1].RxTotal != nil {
		t.Error("wg0 was given a counter it has none of")
	}
	if s.NICs[1].TxTotal != nil {
		t.Error("a negative counter was admitted as a reading")
	}
}

// The cap has to spend itself on the interfaces worth having. A container host
// full of bridges must not push the machine's real NIC off the list just
// because "br-" sorts before "eth".
func TestNICCapPrefersUsefulInterfaces(t *testing.T) {
	out := make([]NIC, 0, 20)
	for i := 0; i < 18; i++ {
		out = append(out, NIC{Name: "br-" + string(rune('a'+i)), Up: true})
	}
	out = append(out, NIC{Name: "eth0", IPs: []string{"10.0.0.5"}, Up: true})
	out = append(out, NIC{Name: "wg0", IPs: []string{"10.9.0.2"}, Up: false})
	kept := capNICs(out)
	if len(kept) != maxNICs {
		t.Fatalf("kept %d, want %d", len(kept), maxNICs)
	}
	seen := map[string]bool{}
	for _, n := range kept {
		seen[n.Name] = true
	}
	if !seen["eth0"] {
		t.Error("the addressed interface was capped away")
	}
	if !seen["wg0"] {
		t.Error("a down interface WITH an address still beats an addressless one")
	}
	for i := 1; i < len(kept); i++ {
		if kept[i-1].Name > kept[i].Name {
			t.Fatalf("display order is not by name: %q before %q", kept[i-1].Name, kept[i].Name)
		}
	}
}
