//go:build unix

package main

// The extra flags /dl opens files with, where the platform has them. Between
// the Lstat that screens a name and the open that serves it there is an
// instant, and a writer inside the directory can spend it: O_NOFOLLOW makes
// the open itself refuse a symlink raced into the name, and O_NONBLOCK makes
// it impossible to hang on a raced-in FIFO — a nonblocking open of one
// returns at once and the fstat that follows refuses it. On a regular file,
// which is the only thing ever served, neither flag changes anything.

import "syscall"

const distOpenExtra = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
