package main

// The machine's own interfaces and the addresses on them.
//
// Every other probe shows this and, until 2026-08-06, this one deliberately did
// not — see the NICs note in system.go for the line that was held and why the
// operator lifted it. The collector still bounds what it sends, for the same
// reason every other field here is bounded: a wire format with no ceiling is a
// wire format one strange machine can make unusable.
//
// net.Interfaces() rather than a per-platform read, because it is stdlib
// everywhere the helper builds — so a FreeBSD box with no collector of its own
// still reports its addresses instead of nothing.

import (
	"net"
	"sort"
	"strings"
)

const (
	// A machine with more interfaces than this is a container host, and its
	// list is bridge noise long before it reaches here.
	maxNICs = 16
	// Enough for a v4, a global v6, a privacy v6 and a couple of aliases.
	maxNICAddrs = 8
)

// Which addresses are worth a row. Loopback is excluded with its interface;
// what this drops are the ones that are true of every machine and describe
// none of them: link-local (fe80::/10, 169.254/16), which the kernel makes up
// per interface, and the unspecified address.
func nicAddrWorthSending(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return true
}

// Fill s.NICs. Byte counters are left nil here — the platform collector that
// already walks per-interface counters for the whole-box sum attaches them
// afterwards through nicCounters, so no platform reads the table twice.
func fillNICs(s *System) {
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) == 0 {
		return
	}
	out := make([]NIC, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.TrimSpace(iface.Name)
		if name == "" {
			continue
		}
		n := NIC{Name: name, Up: iface.Flags&net.FlagUp != 0}
		// An interface whose addresses cannot be read still earns its row: the
		// name and the up flag are the two things the card can always show.
		if addrs, aerr := iface.Addrs(); aerr == nil {
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok || !nicAddrWorthSending(ipnet.IP) {
					continue
				}
				n.IPs = append(n.IPs, ipnet.IP.String())
				if len(n.IPs) >= maxNICAddrs {
					break
				}
			}
		}
		// A down interface with no address is a slot the kernel keeps, not a
		// connection anybody has — those are the rows that make the list long
		// and say nothing.
		if !n.Up && len(n.IPs) == 0 {
			continue
		}
		out = append(out, n)
	}
	if out = capNICs(out); len(out) > 0 {
		s.NICs = out
	}
}

// Apply the cap by USEFULNESS, not alphabetically, and hand back what survived
// in display order.
//
// A container host carries a dozen `br-<hash>` bridges and every one of them
// sorts ahead of `eth0` — capping a name-sorted list there spends the whole
// allowance on bridge noise and drops the one interface the machine is
// actually reached on, which is the opposite of what the cap exists for. So:
// rank (an interface with addresses beats one without, up beats down), take
// the cap, and only THEN sort by name, so a card's rows do not reshuffle
// between two pushes that measured the same machine.
func capNICs(out []NIC) []NIC {
	if len(out) > maxNICs {
		sort.SliceStable(out, func(i, j int) bool {
			ai, aj := len(out[i].IPs) > 0, len(out[j].IPs) > 0
			if ai != aj {
				return ai
			}
			if out[i].Up != out[j].Up {
				return out[i].Up
			}
			return out[i].Name < out[j].Name
		})
		out = out[:maxNICs]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Attach per-interface totals collected by the platform's own network read.
// Names that are not in the list (loopback, an interface that came up between
// the two reads) are dropped rather than inventing a row for them — the
// address list is the authority on which interfaces exist.
func nicCounters(s *System, rx, tx map[string]float64) {
	for i := range s.NICs {
		if v, ok := rx[s.NICs[i].Name]; ok && finite(v) && v >= 0 {
			s.NICs[i].RxTotal = fp(v)
		}
		if v, ok := tx[s.NICs[i].Name]; ok && finite(v) && v >= 0 {
			s.NICs[i].TxTotal = fp(v)
		}
	}
}
