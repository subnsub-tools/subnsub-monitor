//go:build !unix && !windows

package main

// Platforms build.sh does not target — plan9, js/wasm, whatever someone builds
// this for next. Nothing here is unreachable code; it is what keeps the package
// compiling everywhere, and both answers are the CLOSED one.
//
// A link count that cannot be established means the Codex reader refuses the
// file, and a binary that cannot be vetted is never run. Losing a collector on
// an exotic platform is the cheap failure; running an unvetted binary or
// reading a planted hard link is not.

import "os"

func singleLink(_ *os.File, _ os.FileInfo) bool { return false }

func linkCount(_ string) uint64 { return 0 }

func usableBinary(_ string) bool { return false }
