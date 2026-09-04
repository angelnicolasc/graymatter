//go:build !windows

package harness

import (
	"fmt"
	"os"
	"syscall"
)

type pidKillTarget int

func openKillTarget(pid int) (processKillTarget, error) {
	return pidKillTarget(pid), nil
}

func (t pidKillTarget) startTime() (int64, error) {
	return processStartTime(int(t))
}

func (t pidKillTarget) terminate() error {
	return killPID(int(t))
}

func (pidKillTarget) close() {}

// killPID sends SIGTERM to the process with the given PID.
// Returns an error if the process is not found or the signal fails.
func killPID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid PID %d", pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal process %d: %w", pid, err)
	}
	return nil
}
