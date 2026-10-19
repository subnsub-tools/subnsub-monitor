//go:build linux

package main

// The interface list for a process that may not ask netlink the usual way.
//
// net.Interfaces() binds a NETLINK_ROUTE socket and sends RTM_GETLINK, and
// Android's policy for apps refuses both (RTM_GETLINK since 11, in the name
// of hiding MAC addresses; bind on netlink sockets alongside it), so the call
// fails outright and the card shows no interfaces at all. What bionic's own
// getifaddrs does for such apps is the road taken here: an UNBOUND socket,
// an RTM_GETADDR dump (nlmsg_read, which apps keep), and ioctl(SIOCGIFNAME)
// to turn each address's interface index into a name, SIOCGIFFLAGS for
// whether it is up. No MAC is asked for or wanted.
//
// What it cannot see is an interface with no address at all. fillNICs drops
// those when they are down anyway, and an up interface with no address is a
// slot rather than a connection. Everything else — names, addresses, the up
// flag — comes out as the stdlib's does, which the test checks on ordinary
// Linux, where both roads are open.

import (
	"encoding/binary"
	"net"
	"syscall"
	"unsafe"
)

func init() { interfacesFallback = interfacesNoBind }

const (
	ifNameSize = 16 // IFNAMSIZ
	ifReqSize  = 40 // sizeof(struct ifreq) on every Linux ABI Go builds for
)

// interfacesNoBind lists the interfaces that carry an address, or false when
// even this road is shut.
func interfacesNoBind() ([]ifaceInfo, bool) {
	byIndex, order, ok := netlinkAddrs()
	if !ok || len(order) == 0 {
		return nil, false
	}
	sock, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	defer syscall.Close(sock)
	out := make([]ifaceInfo, 0, len(order))
	for _, idx := range order {
		name, ok := ifNameByIndex(sock, idx)
		if !ok {
			continue
		}
		flags, ok := ifFlagsByName(sock, name)
		if !ok {
			continue
		}
		out = append(out, ifaceInfo{
			name:     name,
			up:       flags&syscall.IFF_UP != 0,
			loopback: flags&syscall.IFF_LOOPBACK != 0,
			ips:      byIndex[idx],
		})
	}
	return out, len(out) > 0
}

// netlinkAddrs dumps RTM_GETADDR on an unbound NETLINK_ROUTE socket:
// addresses by interface index, and the indices in first-seen order so the
// caller's list is stable across two reads.
func netlinkAddrs() (map[int][]net.IP, []int, bool) {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return nil, nil, false
	}
	defer syscall.Close(fd)
	// nlmsghdr, then rtgenmsg: one byte of family (AF_UNSPEC, already zero).
	// The same 17 bytes the stdlib sends; the kernel aligns them itself.
	req := make([]byte, syscall.NLMSG_HDRLEN+syscall.SizeofRtGenmsg)
	binary.NativeEndian.PutUint32(req[0:4], uint32(len(req)))
	binary.NativeEndian.PutUint16(req[4:6], syscall.RTM_GETADDR)
	binary.NativeEndian.PutUint16(req[6:8], syscall.NLM_F_DUMP|syscall.NLM_F_REQUEST)
	binary.NativeEndian.PutUint32(req[8:12], 1) // seq; pid 0 lets the kernel fill it in
	if err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return nil, nil, false
	}
	byIndex := map[int][]net.IP{}
	var order []int
	rb := make([]byte, 64*1024)
	// A dump arrives in as many messages as it takes, ending in NLMSG_DONE;
	// the count bounds a kernel that never says so.
	for reads := 0; reads < 256; reads++ {
		n, _, err := syscall.Recvfrom(fd, rb, 0)
		if err != nil || n < syscall.NLMSG_HDRLEN {
			return nil, nil, false
		}
		msgs, err := syscall.ParseNetlinkMessage(rb[:n])
		if err != nil {
			return nil, nil, false
		}
		for i := range msgs {
			m := &msgs[i]
			switch m.Header.Type {
			case syscall.NLMSG_DONE:
				return byIndex, order, true
			case syscall.NLMSG_ERROR:
				return nil, nil, false
			case syscall.RTM_NEWADDR:
				if len(m.Data) < syscall.SizeofIfAddrmsg {
					continue
				}
				ifam := (*syscall.IfAddrmsg)(unsafe.Pointer(&m.Data[0]))
				attrs, err := syscall.ParseNetlinkRouteAttr(m)
				if err != nil {
					continue
				}
				ip := addrFromAttrs(ifam, attrs)
				if ip == nil {
					continue
				}
				idx := int(ifam.Index)
				if _, seen := byIndex[idx]; !seen {
					order = append(order, idx)
				}
				byIndex[idx] = append(byIndex[idx], ip)
			}
		}
	}
	return nil, nil, false
}

// The address an RTM_NEWADDR message carries. A point-to-point interface
// puts the peer in IFA_ADDRESS and its own end in IFA_LOCAL; the stdlib takes
// IFA_LOCAL when it is present, and so does this.
func addrFromAttrs(ifam *syscall.IfAddrmsg, attrs []syscall.NetlinkRouteAttr) net.IP {
	var addr, local net.IP
	for _, a := range attrs {
		switch a.Attr.Type {
		case syscall.IFA_ADDRESS:
			addr = ipFromBytes(ifam.Family, a.Value)
		case syscall.IFA_LOCAL:
			local = ipFromBytes(ifam.Family, a.Value)
		}
	}
	if local != nil {
		return local
	}
	return addr
}

func ipFromBytes(family uint8, b []byte) net.IP {
	switch family {
	case syscall.AF_INET:
		if len(b) == 4 {
			return net.IPv4(b[0], b[1], b[2], b[3])
		}
	case syscall.AF_INET6:
		if len(b) == 16 {
			ip := make(net.IP, 16)
			copy(ip, b)
			return ip
		}
	}
	return nil
}

// ioctl(SIOCGIFNAME): the name for an interface index. struct ifreq is the
// 16-byte name followed by a union that begins at byte 16 — the index going
// in, and for SIOCGIFFLAGS below the flags coming out.
func ifNameByIndex(sock int, index int) (string, bool) {
	var ifr [ifReqSize]byte
	binary.NativeEndian.PutUint32(ifr[ifNameSize:ifNameSize+4], uint32(index))
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(sock), syscall.SIOCGIFNAME, uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		return "", false
	}
	name := cString(ifr[:ifNameSize])
	return name, name != ""
}

func ifFlagsByName(sock int, name string) (uint16, bool) {
	var ifr [ifReqSize]byte
	copy(ifr[:ifNameSize-1], name)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(sock), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr[0]))); errno != 0 {
		return 0, false
	}
	return binary.NativeEndian.Uint16(ifr[ifNameSize : ifNameSize+2]), true
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
