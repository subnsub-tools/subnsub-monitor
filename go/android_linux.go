//go:build linux

package main

// Android, which is Linux with most of the doors shut.
//
// The kernel is Linux and this package's Linux collectors compile for it as
// they stand (the `linux` build tag is true under GOOS=android), but the
// process runs as an untrusted application, and Android's SELinux policy
// takes away — with no permission an app can ask for — the very files the
// Linux collectors are built on: /proc/stat, /proc/loadavg, /proc/uptime,
// /proc/vmstat and /proc/diskstats since 8.0, all of /proc/net since 10, the
// netlink RTM_GETLINK that net.Interfaces() needs since 11, and every other
// process's /proc entry since 7 (hidepid=2). Each collector already names
// what it could not read; what this file adds is the handful of substitutes
// that ARE reachable (a syscall where a /proc file is refused, the data
// partition where "/" is a read-only system image), the two runtime fixes
// any Go binary needs on Android at all (name resolution with no
// /etc/resolv.conf, TLS roots in directories Go does not look in), and the
// detection the rest relies on.
//
// What runs here is decided at RUN time, not build time: the static
// GOOS=linux arm64 binary is what people have been running under Termux and
// rooted shells, so `runtime.GOOS == "android"` alone would miss most of
// them. /dev/__properties__ is the bionic property store — present on every
// Android since 5.0, stat-able by everything because every app's libc reads
// properties, never present on a Linux distribution — which is the same test
// tailscale's androidbin uses.
//
// The blank import below is most of the fix. tailscale.com is already a
// dependency (the direct-mode tunnel), and its androidbin feature installs a
// resolver that speaks to Android's own DNS daemon over /dev/socket/dnsproxyd
// — the phone's real DNS, Private DNS included — points Go's root-CA loader
// at the system store in GOOS=linux builds, and gives the tunnel's network
// monitor an interface list where netlink refuses one. It is inert on
// ordinary Linux (one stat of /etc/resolv.conf) and does not exist on other
// platforms, which is why this file carries the build tag and android_other.go
// carries the stubs.

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	_ "tailscale.com/feature/androidbin"
)

var androidOnce struct {
	sync.Once
	on bool
}

// onAndroid reports whether this process runs on Android — as a GOOS=android
// build, or as a Linux build under an Android kernel.
func onAndroid() bool {
	androidOnce.Do(func() {
		if runtime.GOOS == "android" {
			androidOnce.on = true
			return
		}
		_, err := os.Stat("/dev/__properties__")
		androidOnce.on = err == nil
	})
	return androidOnce.on
}

// androidPrefix is Termux's install prefix when this process runs inside
// Termux, else "". Termux exports PREFIX to every shell it starts and the
// installer's keeper loop inherits it; the check for a shell under it keeps a
// stray PREFIX on some other system from steering anything.
func androidPrefix() string {
	if !onAndroid() {
		return ""
	}
	p := strings.TrimSpace(os.Getenv("PREFIX"))
	if p == "" || !strings.HasPrefix(p, "/") {
		return ""
	}
	if fi, err := os.Stat(p + "/bin/sh"); err != nil || fi.IsDir() {
		return ""
	}
	return p
}

// androidInit runs first thing in main, before any request is made. A no-op
// anywhere but Android.
func androidInit() {
	if !onAndroid() {
		return
	}
	androidCerts()
	androidDNS()
}

const (
	// Where Android keeps its root store. Since Android 14 the authoritative
	// copy is the Conscrypt APEX module's, updated through Play; the system
	// partition's copy is still shipped, and is read second so a device
	// that predates the module finds its roots there.
	androidApexCACerts = "/apex/com.android.conscrypt/cacerts"
	androidSysCACerts  = "/system/etc/security/cacerts"
)

// Go's crypto/x509 reads SSL_CERT_DIR (colon-separated) and SSL_CERT_FILE
// lazily, on the first handshake, so setting them here is enough. A
// GOOS=android build already looks in the system partition's directory, and
// a GOOS=linux build is pointed at it by androidbin; neither knows the APEX
// one, which on Android 14+ is where an updated root lands. A person's own
// SSL_CERT_* is left alone — except the one value androidbin's init set a
// moment ago, which is ours to widen.
func androidCerts() {
	if os.Getenv("SSL_CERT_FILE") != "" {
		return
	}
	if d := os.Getenv("SSL_CERT_DIR"); d != "" && d != androidSysCACerts {
		return
	}
	var dirs []string
	for _, d := range []string{androidApexCACerts, androidSysCACerts} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) > 0 {
		os.Setenv("SSL_CERT_DIR", strings.Join(dirs, ":"))
	}
	// Termux's ca-certificates package, when installed: one PEM bundle,
	// read IN ADDITION to the directories above (Go reads both), so a stale
	// bundle cannot hide a root the system store has.
	if p := androidPrefix(); p != "" {
		if fi, err := os.Stat(p + "/etc/tls/cert.pem"); err == nil && fi.Mode().IsRegular() {
			os.Setenv("SSL_CERT_FILE", p+"/etc/tls/cert.pem")
		}
	}
}

// Comma-separated DNS servers for the case androiddns cannot serve — see
// androidDNS.
const androidDNSEnv = "MON_DNS"

// Where names get resolved, for `selftest`.
var androidDNSSource = "Go default"

// Name resolution when androiddns could not install itself: the daemon's
// socket refused (a policy tighter than AOSP's), or a build without the
// feature. Go's resolver, finding no /etc/resolv.conf, would send every query
// to localhost and fail. Servers come from MON_DNS (host or host:port, comma
// separated), else from Termux's own $PREFIX/etc/resolv.conf when its
// resolv-conf package wrote one. With neither, nothing is invented: a public
// resolver chosen here would send every name this machine looks up to a third
// party the person never picked, so the helper says what is wrong and what to
// set instead.
func androidDNS() {
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		androidDNSSource = "/etc/resolv.conf"
		return // a real resolver configuration: Go reads it
	}
	if net.DefaultResolver.Dial != nil {
		androidDNSSource = "system resolver daemon (dnsproxyd)"
		return // androiddns is in place
	}
	servers := dnsServersFromList(os.Getenv(androidDNSEnv))
	source := androidDNSEnv
	if len(servers) == 0 {
		if p := androidPrefix(); p != "" {
			if raw, ok := readSmall(p + "/etc/resolv.conf"); ok {
				servers = dnsServersFromResolvConf(raw)
				source = p + "/etc/resolv.conf"
			}
		}
	}
	if len(servers) == 0 {
		androidDNSSource = "none"
		warnf("android: no way to resolve names (no /etc/resolv.conf, and the system resolver daemon did not answer); set %s=<ip>[:port][,<ip>…] or install Termux's resolv-conf package", androidDNSEnv)
		return
	}
	androidDNSSource = source
	var turn uint32
	net.DefaultResolver.PreferGo = true
	net.DefaultResolver.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		// Go picked localhost for want of a resolv.conf; its address is
		// ignored and ours are taken in turn, one per attempt.
		i := atomic.AddUint32(&turn, 1) - 1
		var d net.Dialer
		return d.DialContext(ctx, network, servers[int(i%uint32(len(servers)))])
	}
}

func dnsServersFromList(v string) []string {
	var out []string
	for _, f := range strings.Split(v, ",") {
		if s := dnsServerAddr(strings.TrimSpace(f)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func dnsServersFromResolvConf(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if s := dnsServerAddr(f[1]); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// dnsServerAddr normalises one server to host:port, or "" for anything that
// is not an IP address — a NAME here would need the resolver it configures.
func dnsServerAddr(s string) string {
	if s == "" {
		return ""
	}
	if host, port, err := net.SplitHostPort(s); err == nil {
		ip := net.ParseIP(host)
		if n, perr := strconv.Atoi(port); ip == nil || perr != nil || n < 1 || n > 65535 {
			return ""
		}
		return net.JoinHostPort(ip.String(), port)
	}
	ip := net.ParseIP(strings.Trim(s, "[]"))
	if ip == nil {
		return ""
	}
	return net.JoinHostPort(ip.String(), "53")
}

// CLOCK_BOOTTIME: the clock /proc/uptime's first field reports.
const clockBoottime = 7

// bootSeconds is /proc/uptime by another road — a clock any process may
// read, where the file is refused to Android apps.
func bootSeconds() (float64, bool) {
	var ts syscall.Timespec
	if _, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockBoottime, uintptr(unsafe.Pointer(&ts)), 0); errno != 0 {
		return 0, false
	}
	v := float64(ts.Sec) + float64(ts.Nsec)/1e9
	if v <= 0 || !finite(v) {
		return 0, false
	}
	return v, true
}

// unameRelease is the kernel release by uname(2), for a process that may
// not read /proc/sys/kernel/osrelease.
func unameRelease() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	b := make([]byte, 0, len(u.Release))
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// Whether /proc shows processes other than this one. Android mounts /proc
// with hidepid=2 for apps (since 7.0) and so do some hardened containers: a
// walk of /proc then counts THIS program's threads as the machine's
// processes, and a top-process table lists nothing but itself. PID 1 exists
// on every Linux system and is the cheapest witness; the mount option does
// not change under a running process, so it is asked once.
var procVis struct {
	sync.Once
	dirs, stats bool
}

func procProbe() {
	procVis.Do(func() {
		_, err := os.Stat("/proc/1")
		procVis.dirs = err == nil
		_, err = os.ReadFile("/proc/1/stat")
		procVis.stats = err == nil
	})
}

// procTreeVisible: the directories under /proc are everybody's, so counting
// them counts the machine's processes.
func procTreeVisible() bool { procProbe(); return procVis.dirs }

// procStatReadable: another process's /proc/<pid>/stat can be read, which
// is what a top-process table is made of.
func procStatReadable() bool { procProbe(); return procVis.stats }

// androidSelftest prints what this process can and cannot reach, for the
// person standing at the phone.
func androidSelftest() {
	fmt.Println("android       : yes (GOOS=" + runtime.GOOS + ")")
	if p := androidPrefix(); p != "" {
		fmt.Println("  termux prefix     :", p)
	}
	fmt.Println("  resolver          :", androidDNSSource)
	fmt.Println("  SSL_CERT_DIR      :", os.Getenv("SSL_CERT_DIR"))
	fmt.Println("  SSL_CERT_FILE     :", os.Getenv("SSL_CERT_FILE"))
	fmt.Println("  /proc shows other processes:", procTreeVisible(), "— their stat readable:", procStatReadable())
	for _, p := range []string{
		"/proc/stat", "/proc/loadavg", "/proc/uptime", "/proc/vmstat", "/proc/diskstats",
		"/proc/meminfo", "/proc/net/dev", "/proc/net/snmp", "/proc/self/mounts",
		"/sys/class/net", "/sys/class/thermal/thermal_zone0/temp", "/sys/class/hwmon",
	} {
		readable := false
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			_, rerr := os.ReadDir(p)
			readable = rerr == nil
		} else {
			_, readable = readSmall(p)
		}
		fmt.Printf("  %-40s readable=%v\n", p, readable)
	}
	if _, ok := stdlibInterfaces(); ok {
		fmt.Println("  interfaces        : netlink (stdlib)")
	} else if list, ok := interfacesNoBind(); ok {
		fmt.Printf("  interfaces        : RTM_GETADDR fallback (%d)\n", len(list))
	} else {
		fmt.Println("  interfaces        : none readable")
	}
	fmt.Println()
}
