//go:build !unix

package main

// No portable O_NOFOLLOW/O_NONBLOCK here, so the open takes no extra flags
// and the SameFile comparison in the handler is what stands against a swap
// raced in between the Lstat and the open. The platforms this covers do not
// grow FIFOs the way a Unix directory does, which is what makes the weaker
// guard acceptable rather than merely convenient.

const distOpenExtra = 0
