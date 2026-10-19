//go:build darwin

package main

// A child of this process that is still THIS program a minute after it was
// made is a fork that never reached exec, and it is spinning on a core.
//
// What happens (2026-10-09, m1): on macOS Go has to create a child through
// Apple's libc fork(), and libc runs its own atfork handlers in the child —
// _dirhelper_init, getenv and friends. The parent is many threads; if one of
// them held a libc lock at the instant of the fork, the child inherits the
// lock with nobody to release it, and sits in __findenv_locked at 100% CPU
// for as long as the machine is up. m1 carried one for two hours after an
// update restart and the user saw it as the machine running hot. Go's own
// issue 56784 has the same stack shape and no fix: posix_spawn is not an
// option for the runtime.
//
// Why nothing else catches it: the parent's Start() reads a pipe the child
// closes on exec, and a child that never execs never closes it. Start() has
// not returned, so the context deadline every tool run carries has not been
// armed yet; the one guard this program has for hung children is exactly the
// one that cannot see this child.
//
// This collector forks several times a reading on macOS — sysctl, vm_stat,
// ps, netstat, and its own binary as `temps` (mactemps.go) — so the race
// gets thousands of tries a day and lands every few weeks. Linux, FreeBSD and
// Android go straight to the kernel for fork and run no handlers in the child;
// Windows has no fork. Only this build needs the sweep.
//
// What is swept: children whose parent is this process, whose p_comm is this
// process's p_comm — a child that has exec'd something else has that program's
// name — and that have been alive longer than forkReapAfter. The one
// legitimate child that keeps the name is the `temps` run of this same binary,
// and it is held to a five-second deadline by runMacTool; at a minute it is
// broken whichever way, and SIGKILL is the right answer for it too. Killing
// the stuck child closes the exec pipe, which lets the waiting Start() return
// and that reading fail normally.
//
// The process table is read through sysctl, not by running ps: a reaper that
// forks to find stuck forks would be its own best customer.

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	forkReapEvery = time.Minute
	forkReapAfter = time.Minute
)

func startForkReaper() {
	go func() {
		self := os.Getpid()
		for {
			time.Sleep(forkReapEvery)
			reapStuckForks(self)
		}
	}()
}

// The name the kernel holds for a process: the executable's basename, cut to
// sixteen bytes. Read for ourselves rather than derived from os.Args[0] so an
// install under any name compares like with like.
func procComm(pid int) string {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return ""
	}
	return unix.ByteSliceToString(kp.Proc.P_comm[:])
}

func reapStuckForks(self int) {
	comm := procComm(self)
	if comm == "" {
		return
	}
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return
	}
	now := time.Now()
	for i := range procs {
		p := &procs[i]
		pid := int(p.Proc.P_pid)
		if int(p.Eproc.Ppid) != self || pid == self || pid <= 1 {
			continue
		}
		if unix.ByteSliceToString(p.Proc.P_comm[:]) != comm {
			continue
		}
		started := time.Unix(p.Proc.P_starttime.Sec, int64(p.Proc.P_starttime.Usec)*1000)
		age := now.Sub(started)
		if age < forkReapAfter {
			continue
		}
		// ESRCH (gone between the listing and now) and a zombie that Wait()
		// has not reaped yet are both fine: nothing to do, nothing to say.
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			continue
		}
		warnf("reaper: child %d was still this program %s after it was forked — a fork that never reached exec; killed", pid, age.Round(time.Second))
	}
}
