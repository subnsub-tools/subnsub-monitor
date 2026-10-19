//go:build !linux && !darwin

package main

// systemBootUnix: not read on this platform; a start of the helper is then
// told from a restart of the machine by the alive-stamp's age alone.
func systemBootUnix() int64 { return 0 }
