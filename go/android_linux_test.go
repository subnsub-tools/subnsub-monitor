//go:build linux

package main

import "testing"

// A server is an IP with or without a port, and nothing else: a name here
// would need the very resolver it is meant to configure.
func TestDNSServerAddr(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"1.1.1.1", "1.1.1.1:53"},
		{"1.1.1.1:5353", "1.1.1.1:5353"},
		{"2606:4700:4700::1111", "[2606:4700:4700::1111]:53"},
		{"[2606:4700:4700::1111]", "[2606:4700:4700::1111]:53"},
		{"[2606:4700:4700::1111]:53", "[2606:4700:4700::1111]:53"},
		{"dns.example.com", ""},
		{"dns.example.com:53", ""},
		{"1.1.1.1:0", ""},
		{"1.1.1.1:70000", ""},
		{"", ""},
	} {
		if got := dnsServerAddr(c.in); got != c.want {
			t.Errorf("dnsServerAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	got := dnsServersFromList(" 9.9.9.9 , bogus, 149.112.112.112:53 ")
	if len(got) != 2 || got[0] != "9.9.9.9:53" || got[1] != "149.112.112.112:53" {
		t.Errorf("dnsServersFromList = %v", got)
	}
	got = dnsServersFromResolvConf("# Termux\nnameserver 8.8.8.8\nnameserver not-an-ip\nsearch lan\nnameserver 2001:4860:4860::8888\n")
	if len(got) != 2 || got[0] != "8.8.8.8:53" || got[1] != "[2001:4860:4860::8888]:53" {
		t.Errorf("dnsServersFromResolvConf = %v", got)
	}
}

// Android's mount table is the operating system talking to itself; only
// removable and adopted media describe a disk a person can fill.
func TestAndroidMountWorthListing(t *testing.T) {
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/storage/1234-5678", true},
		{"/mnt/media_rw/1234-5678", true},
		{"/mnt/expand/0a1b2c", true},
		{"/storage/emulated", false},
		{"/storage/emulated/0", false},
		{"/apex/com.android.conscrypt", false},
		{"/system", false},
		{"/vendor", false},
		{"/product", false},
		{"/data", false},
		{"/metadata", false},
	} {
		if got := androidMountWorthListing(c.path); got != c.want {
			t.Errorf("androidMountWorthListing(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// The clock behind /proc/uptime, read without the file.
func TestBootSeconds(t *testing.T) {
	v, ok := bootSeconds()
	if !ok || v <= 0 {
		t.Fatalf("bootSeconds = %v, %v", v, ok)
	}
	if r := unameRelease(); r == "" {
		t.Fatal("unameRelease is empty")
	}
}
