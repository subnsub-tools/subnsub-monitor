//go:build !linux

package main

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
)

// Network namespaces are a Linux thing.
func spacesAvailable() bool { return false }
func netnsInfo() *Netns     { return nil }
func spacesFound() []string { return nil }
func spaceDial(context.Context, string, string, *atomic.Bool) (net.Conn, error) {
	return nil, errors.New("unsupported")
}

var spacesDirs []string
