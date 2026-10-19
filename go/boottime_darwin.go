//go:build darwin

package main

import (
	"context"
	"os/exec"
	"time"
)

// systemBootUnix: kern.boottime, read the way the system collector reads it
// (macstats.go parseMacBoottime), or 0 when it cannot be.
func systemBootUnix() int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, macSysctl, "-n", "kern.boottime").Output()
	if err != nil {
		return 0
	}
	sec, ok := parseMacBoottime(string(out))
	if !ok || sec <= 0 {
		return 0
	}
	return int64(sec)
}
