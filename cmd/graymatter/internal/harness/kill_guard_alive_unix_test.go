//go:build unix

package harness

import (
	"os"
	"syscall"
)

// processAlive reports whether pid still exists.
//
// Signal 0 is the portable probe: the kernel runs the permission and existence
// checks and delivers nothing. It has to be syscall.Signal(0) rather than a
// nil os.Signal — os.Process.Signal type-asserts its argument, so nil returns
// an error and would make every live process look dead.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
