//go:build !linux && !darwin

package main

// Anything build.sh does not currently target. Nothing here is unreachable
// code: someone will eventually build this for their own platform, and the
// helper should compile and report honestly rather than fail to build or claim
// a healthy machine it never measured.

func systemSnapshot() System {
	s := baseSystem()
	s.miss(mCPU)
	s.miss(mMemory)
	s.miss(mSwap)
	s.miss(mDisk)
	s.miss(mLoad)
	s.miss(mUptime)
	return s
}
