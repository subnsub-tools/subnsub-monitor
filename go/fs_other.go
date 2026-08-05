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

import (
	"context"
	"os"
	"os/exec"
)

func openLinkCount(_ *os.File, _ os.FileInfo) (uint64, bool) { return 0, false }

func linkCount(_ string) uint64 { return 0 }

func usableBinary(_ string) bool { return false }

// Never reached — usableBinary above refuses everything, so nothing gets this
// far — and defined anyway so the package compiles. Directly, which is the
// answer everywhere that is not Windows.
func toolCommand(ctx context.Context, bin string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}
