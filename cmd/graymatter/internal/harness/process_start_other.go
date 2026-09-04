//go:build !windows && !darwin

package harness

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

func processStartTime(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid PID %d", pid)
	}
	if runtime.GOOS != "linux" {
		return 0, fmt.Errorf("process start time is unsupported on %s", runtime.GOOS)
	}
	bootData, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return 0, fmt.Errorf("read Linux boot ID: %w", err)
	}
	bootID := strings.TrimSpace(string(bootData))
	if bootID == "" {
		return 0, fmt.Errorf("read Linux boot ID: empty value")
	}

	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read process %d stat: %w", pid, err)
	}
	// After comm's final ')', starttime is field 22 (index 19 from state at field 3).
	close := strings.LastIndexByte(string(data), ')')
	if close < 0 {
		return 0, fmt.Errorf("parse process %d stat: missing command terminator", pid)
	}
	fields := strings.Fields(string(data[close+1:]))
	if len(fields) <= 19 {
		return 0, fmt.Errorf("parse process %d stat: got %d fields after command", pid, len(fields))
	}
	started, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse process %d start time %q: %w", pid, fields[19], err)
	}
	if started == 0 {
		return 0, fmt.Errorf("process %d has invalid start time %d", pid, started)
	}
	// Bind boot-relative field 22 to the boot ID before persisting its identity.
	token := processIdentityToken(bootID, started)
	if token == 0 {
		return 0, fmt.Errorf("process %d identity token is zero", pid)
	}
	return token, nil
}
