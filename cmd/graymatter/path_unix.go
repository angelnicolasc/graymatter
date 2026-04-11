//go:build !windows

package main

// addExeDirToUserPath is a no-op on non-Windows platforms.
// On macOS/Linux the binary is placed in /usr/local/bin which is already on PATH.
func addExeDirToUserPath() (bool, error) { return false, nil }
