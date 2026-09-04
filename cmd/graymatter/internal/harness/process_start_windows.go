//go:build windows

package harness

import (
	"fmt"
	"math"

	"golang.org/x/sys/windows"
)

func processStartTime(pid int) (int64, error) {
	processID, err := windowsProcessID(pid)
	if err != nil {
		return 0, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, processID)
	if err != nil {
		return 0, fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return processStartTimeFromHandle(pid, h, windows.GetProcessTimes)
}

type getProcessTimesFunc func(windows.Handle, *windows.Filetime, *windows.Filetime, *windows.Filetime, *windows.Filetime) error

func processStartTimeFromHandle(pid int, h windows.Handle, getTimes getProcessTimesFunc) (int64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := getTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, fmt.Errorf("get process %d times: %w", pid, err)
	}
	raw := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if raw > math.MaxInt64 {
		return 0, fmt.Errorf("process %d start time exceeds int64", pid)
	}
	started := int64(raw)
	if started <= 0 {
		return 0, fmt.Errorf("process %d has invalid start time %d", pid, started)
	}
	return started, nil
}

func windowsProcessID(pid int) (uint32, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid PID %d", pid)
	}
	if uint64(pid) > math.MaxUint32 {
		return 0, fmt.Errorf("PID %d exceeds the Windows process ID range", pid)
	}
	return uint32(pid), nil
}
