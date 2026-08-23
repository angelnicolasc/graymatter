//go:build windows

package harness

import (
	"os"

	"golang.org/x/sys/windows"
)

// processAlive reports whether pid still exists.
//
// Windows has no signal-0 equivalent, so this opens the process and asks for
// its exit code: STILL_ACTIVE (259) means running. os.FindProcess alone proves
// nothing here — it succeeds for a PID that has already gone.
func processAlive(pid int) bool {
	if _, err := os.FindProcess(pid); err != nil {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
