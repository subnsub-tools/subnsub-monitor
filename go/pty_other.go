//go:build !linux && !darwin && !freebsd

package main

// Platforms with no pseudo-terminal here: the terminal (pty.go) is not
// offered, and these keep the package compiling.

import (
	"errors"
	"os"
)

const ptySupported = false

func ptyStart(cols, rows int) (*os.File, func(), func(), error) {
	return nil, nil, nil, errors.New("no pseudo-terminal on this platform")
}

func ptyResizeTTY(*os.File, int, int) {}
