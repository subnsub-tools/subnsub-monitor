//go:build !linux && !darwin

package main

// No sensors this build knows how to read: `subnsub-monitor temps` prints an
// empty list, which is the truth.
func localTemps() []Temp { return nil }
