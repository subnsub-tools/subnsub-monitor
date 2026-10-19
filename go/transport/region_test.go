package transport

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"tailscale.com/tailcfg"
	"tailscale.com/types/logger"
)

// The public map's shape: one relay per region, IDs 301–304.
func testDERPMap() *tailcfg.DERPMap {
	dm := &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{}}
	for i, ip := range []string{"199.38.181.166", "208.111.39.38", "185.178.202.197", "172.238.7.124"} {
		id := tailcfg.DERPRegionID(301 + i)
		dm.Regions[id] = &tailcfg.DERPRegion{RegionID: id, RegionCode: "r", Nodes: []*tailcfg.DERPNode{{
			Name: "n", RegionID: id, HostName: "tc" + string(rune('1'+i)) + ".example", IPv4: ip, IPv6: "2001:db8::" + string(rune('1'+i)),
		}}}
	}
	return dm
}

func noMeasure(context.Context, *tailcfg.DERPMap) (tailcfg.DERPRegionID, error) { return 0, nil }

// noDNS keeps every literal: dialByName changes nothing when DNS cannot answer.
func noDNS(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }

func TestPickRegionKeepsThePin(t *testing.T) {
	dm := testDERPMap()
	measured := false
	best := func(context.Context, *tailcfg.DERPMap) (tailcfg.DERPRegionID, error) {
		measured = true
		return 301, nil
	}
	r, _, err := pickRegion(context.Background(), dm, 304, best, noDNS, logger.Discard)
	if err != nil || r.RegionID != 304 {
		t.Fatalf("pinned 304, got %v %v", r, err)
	}
	if measured {
		t.Fatal("measured the regions although the pin was good")
	}
	r.Nodes[0].IPv4 = ""
	if dm.Regions[304].Nodes[0].IPv4 == "" {
		t.Fatal("the region returned shares nodes with the map")
	}
}

func TestPickRegionRepicksAGonePin(t *testing.T) {
	best := func(context.Context, *tailcfg.DERPMap) (tailcfg.DERPRegionID, error) { return 302, nil }
	for _, pinned := range []tailcfg.DERPRegionID{0, 1} { // 1: what every machine pinned before
		r, _, err := pickRegion(context.Background(), testDERPMap(), pinned, best, noDNS, logger.Discard)
		if err != nil || r.RegionID != 302 {
			t.Fatalf("pin %d: want the measured 302, got %v %v", pinned, r, err)
		}
	}
}

func TestPickRegionTakesAnyWhenNothingMeasures(t *testing.T) {
	failing := func(context.Context, *tailcfg.DERPMap) (tailcfg.DERPRegionID, error) {
		return 0, errors.New("netcheck: sendto: operation not permitted")
	}
	for _, best := range []func(context.Context, *tailcfg.DERPMap) (tailcfg.DERPRegionID, error){noMeasure, failing} {
		r, _, err := pickRegion(context.Background(), testDERPMap(), 0, best, noDNS, logger.Discard)
		if err != nil || r.RegionID < 301 || r.RegionID > 304 {
			t.Fatalf("want some region of the map, got %v %v", r, err)
		}
	}
	if _, _, err := pickRegion(context.Background(), &tailcfg.DERPMap{}, 0, noMeasure, noDNS, logger.Discard); err == nil {
		t.Fatal("an empty map produced a region")
	}
}

func lookupAnswers(addrs ...string) func(context.Context, string) ([]netip.Addr, error) {
	return func(context.Context, string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

func TestDialByNameOnlyWhenDNSDisagrees(t *testing.T) {
	cases := []struct {
		name   string
		lookup func(context.Context, string) ([]netip.Addr, error)
		byName bool
	}{
		{"DNS agrees with the map", lookupAnswers("172.238.7.124"), false},
		{"DNS agrees among others", lookupAnswers("10.0.0.1", "172.238.7.124"), false},
		{"DNS fails", func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") }, false},
		{"DNS empty", lookupAnswers(), false},
		{"sandbox gateway", lookupAnswers("198.18.20.241"), true},
	}
	for _, c := range cases {
		reg := testDERPMap().Regions[304].Clone()
		got := dialByName(context.Background(), reg, c.lookup, logger.Discard)
		n := reg.Nodes[0]
		if got != c.byName {
			t.Errorf("%s: byName=%v, want %v", c.name, got, c.byName)
		}
		if c.byName && (n.IPv4 != "" || n.IPv6 != "") {
			t.Errorf("%s: literals kept (%q, %q); derphttp would still dial them", c.name, n.IPv4, n.IPv6)
		}
		if !c.byName && (n.IPv4 != "172.238.7.124" || n.IPv6 == "") {
			t.Errorf("%s: literals changed to (%q, %q)", c.name, n.IPv4, n.IPv6)
		}
	}
}

func TestDialByNameKeepsAnExplicitNone(t *testing.T) {
	reg := testDERPMap().Regions[301].Clone()
	reg.Nodes[0].IPv6 = "none"
	if !dialByName(context.Background(), reg, lookupAnswers("198.18.0.9"), logger.Discard) {
		t.Fatal("did not switch to the name")
	}
	if reg.Nodes[0].IPv6 != "none" {
		t.Fatalf("IPv6 %q, want none kept", reg.Nodes[0].IPv6)
	}
}

// Measuring happens over the rewritten map: netcheck must not be handed the
// IP literals a sandbox would ask its owner to approve one by one.
func TestPickRegionMeasuresByName(t *testing.T) {
	dm := testDERPMap()
	best := func(_ context.Context, got *tailcfg.DERPMap) (tailcfg.DERPRegionID, error) {
		for id, r := range got.Regions {
			for _, n := range r.Nodes {
				if n.IPv4 != "" || n.IPv6 != "" {
					t.Errorf("region %d measured with literals (%q, %q)", id, n.IPv4, n.IPv6)
				}
			}
		}
		return 303, nil
	}
	for _, pinned := range []tailcfg.DERPRegionID{0, 1} {
		r, byName, err := pickRegion(context.Background(), dm, pinned, best, lookupAnswers("198.18.0.1"), logger.Discard)
		if err != nil || r.RegionID != 303 || !byName || r.Nodes[0].IPv4 != "" {
			t.Fatalf("pin %d: got %v byName=%v err=%v", pinned, r, byName, err)
		}
	}
	if dm.Regions[303].Nodes[0].IPv4 == "" {
		t.Fatal("the rewrite reached the fetched map")
	}
	// A good pin is not measured and still gets the rewrite.
	r, byName, err := pickRegion(context.Background(), dm, 302, noMeasure, lookupAnswers("198.18.0.1"), logger.Discard)
	if err != nil || r.RegionID != 302 || !byName || r.Nodes[0].IPv4 != "" {
		t.Fatalf("pinned 302: got %v byName=%v err=%v", r, byName, err)
	}
}
