//go:build !darwin

package main

// No keychain anywhere but macOS: the credential rung reads files here, and
// a table row naming a keychain item is simply one more place with nothing
// in it (keychain_darwin.go has the real one).
func keychainPassword(string) ([]byte, bool) { return nil, false }
