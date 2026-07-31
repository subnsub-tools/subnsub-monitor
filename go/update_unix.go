//go:build !windows

package main

// Installing the new binary over the running one, on a platform where a file
// can be replaced while it is being executed.

import (
	"fmt"
	"os"
)

// Keep the outgoing binary under a second name, then swap.
//
// The backup costs a few megabytes and it is the difference between "restore
// the previous helper" being a move and being another download on a machine
// whose helper is the thing that just broke.
//
// A HARD LINK, not a move, and that is the whole difference between one step
// and two. Renaming self aside first leaves NOTHING at the service path until
// the second rename lands, and a kill or a power cut inside that window leaves
// a machine whose ExecStart does not exist — a helper that is not merely
// un-updated but gone, on a box nobody has a route to. Linking means self is
// never absent for an instant: the old inode simply gains a second name, and
// the single rename below swaps the directory entry atomically with no window
// at all.
func installOver(staged, self, prev string) error {
	os.Remove(prev)
	if err := os.Link(self, prev); err != nil {
		// A filesystem without hard links, or one that allows writing here but
		// not linking. A copy buys the same guarantee for the price of the
		// bytes, and the guarantee is the part that matters.
		if cerr := copyFile(self, prev); cerr != nil {
			return fmt.Errorf("could not keep a copy of the running binary: %w — nothing was replaced", cerr)
		}
	}
	if err := os.Rename(staged, self); err != nil {
		// Nothing was replaced: self still names the old inode, which is still
		// running. The link is the only thing to undo.
		os.Remove(prev)
		return fmt.Errorf("could not install the new binary: %w — the previous one is still in place", err)
	}
	return nil
}
