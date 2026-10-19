//go:build linux

package main

import (
	"strconv"
	"strings"
	"time"
)

// systemBootUnix: when this system booted, unix seconds, or 0 when it cannot
// be read. /proc/uptime first; an Android app, refused that file, has the
// boot clock itself (android_linux.go bootSeconds). outages.go asks it at
// start, to know a restart of the helper from a restart of the machine.
func systemBootUnix() int64 {
	up := 0.0
	if raw, ok := readSmall("/proc/uptime"); ok {
		if f := strings.Fields(raw); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				up = v
			}
		}
	}
	if up <= 0 {
		if v, ok := bootSeconds(); ok {
			up = v
		}
	}
	if up <= 0 || !finite(up) {
		return 0
	}
	return time.Now().Unix() - int64(up)
}
