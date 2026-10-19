//go:build !linux

package main

// The Android seams, where there is no Android: android_linux.go says why
// each exists. Nothing here is reachable code on these platforms — the
// functions are called from files without a build tag and have to compile.

func onAndroid() bool        { return false }
func androidPrefix() string  { return "" }
func androidInit()           {}
func androidSelftest()       {}
func procTreeVisible() bool  { return true }
func procStatReadable() bool { return true }
