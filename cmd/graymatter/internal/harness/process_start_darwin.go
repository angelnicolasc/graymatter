//go:build darwin

package harness

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func processStartTime(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid PID %d", pid)
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("sysctl process %d: %w", pid, err)
	}
	if int(info.Proc.P_pid) != pid {
		return 0, fmt.Errorf("sysctl process %d returned pid %d", pid, info.Proc.P_pid)
	}
	started := info.Proc.P_starttime
	if started.Sec <= 0 || started.Usec < 0 || started.Usec >= 1e6 {
		return 0, fmt.Errorf("process %d has invalid start time %d.%06d", pid, started.Sec, started.Usec)
	}
	if started.Sec > (math.MaxInt64-int64(started.Usec))/1e6 {
		return 0, fmt.Errorf("process %d start time exceeds int64 microseconds", pid)
	}
	micros := started.Sec*1e6 + int64(started.Usec)
	return micros, nil
}
