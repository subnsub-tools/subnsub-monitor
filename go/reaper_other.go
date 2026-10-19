//go:build !darwin

package main

// Only macOS can leave a forked child spinning before exec (reaper_darwin.go
// says why): Linux, FreeBSD and Android fork straight through the kernel and
// run nothing of libc's in the child, and Windows does not fork. Nothing to
// sweep here.
func startForkReaper() {}
