//go:build windows

package main

import "os"

// OpenWrt is not Windows; ensureAutostart never gets this far there.
func fileOwner(os.FileInfo) (uint32, bool) { return 0, false }
