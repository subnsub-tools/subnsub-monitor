//go:build windows

package main

// The same swap on the one platform that will not let a running program's file
// be replaced.
//
// Windows maps the executable image and keeps a handle on it for as long as the
// process lives. The loader does open it with FILE_SHARE_DELETE, which buys one
// specific thing and not the other: the file may be RENAMED while it runs, but
// it may not be replaced in place — a MoveFileEx with REPLACE_EXISTING onto a
// running image fails with access denied. So the Unix order (link the old one
// aside, then rename over it) cannot work here and the order has to be
// inverted: move the running binary out of the way first, which frees the name,
// then move the new one in.
//
// THAT INVERSION COSTS A GUARANTEE, and it is worth naming rather than
// discovering. Between the two renames there is an instant with no file at the
// service path. A power cut inside it leaves a machine whose scheduled task
// points at nothing, with the previous helper sitting beside it under
// `.prev` — recoverable by hand, and only by hand. The window is one rename
// wide and there is no way to close it on this platform: a second name for a
// running image is obtainable (CreateHardLink works), but the name that has to
// be replaced is the locked one either way.
//
// What IS closed is the ordinary failure. If the second rename fails for any
// reason short of the machine stopping, the first one is undone, and the
// helper that was running is back at the path its service points at before this
// function returns.

import (
	"fmt"
	"os"
)

func installOver(staged, self, prev string) error {
	// A stale .prev from an earlier update still holds the name we are about to
	// need. Removing it can fail on Windows for a reason it never fails for on
	// Unix — something still has it open — and that has to stop the update
	// rather than be swallowed, because the rename below would fail anyway and
	// it would fail AFTER the running binary had been moved.
	if err := os.Remove(prev); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not clear %s: %w — nothing was replaced", prev, err)
	}
	if err := os.Rename(self, prev); err != nil {
		return fmt.Errorf("could not move the running binary aside: %w — nothing was replaced", err)
	}
	if err := os.Rename(staged, self); err != nil {
		// Put it back. This is the whole reason the failure above is survivable:
		// without it the service path stays empty and the machine comes up with
		// no helper at all.
		if back := os.Rename(prev, self); back != nil {
			return fmt.Errorf("could not install the new binary: %w — and the previous one could not be "+
				"put back either (%v). The helper is at %s; rename it to %s to recover", err, back, prev, self)
		}
		return fmt.Errorf("could not install the new binary: %w — the previous one is back in place", err)
	}
	return nil
}
