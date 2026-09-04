//go:build windows

package harness

import (
	"fmt"

	"golang.org/x/sys/windows"
)

type windowsProcessAPI struct {
	openProcess      func(uint32, bool, uint32) (windows.Handle, error)
	getProcessTimes  getProcessTimesFunc
	terminateProcess func(windows.Handle, uint32) error
	closeHandle      func(windows.Handle) error
}

var nativeWindowsProcessAPI = windowsProcessAPI{
	openProcess:      windows.OpenProcess,
	getProcessTimes:  windows.GetProcessTimes,
	terminateProcess: windows.TerminateProcess,
	closeHandle:      windows.CloseHandle,
}

type windowsKillTarget struct {
	pid    int
	handle windows.Handle
	api    windowsProcessAPI
}

func openKillTarget(pid int) (processKillTarget, error) {
	return openWindowsKillTarget(pid, nativeWindowsProcessAPI)
}

func openWindowsKillTarget(pid int, api windowsProcessAPI) (processKillTarget, error) {
	processID, err := windowsProcessID(pid)
	if err != nil {
		return nil, err
	}
	h, err := api.openProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE,
		false,
		processID,
	)
	if err != nil {
		return nil, fmt.Errorf("open process %d: %w", pid, err)
	}
	return &windowsKillTarget{pid: pid, handle: h, api: api}, nil
}

func (t *windowsKillTarget) startTime() (int64, error) {
	return processStartTimeFromHandle(t.pid, t.handle, t.api.getProcessTimes)
}

func (t *windowsKillTarget) terminate() error {
	// Match os.Process.Kill's Windows exit code while keeping the HANDLE that
	// supplied the verified creation time.
	if err := t.api.terminateProcess(t.handle, 1); err != nil {
		return fmt.Errorf("kill process %d: %w", t.pid, err)
	}
	return nil
}

func (t *windowsKillTarget) close() {
	_ = t.api.closeHandle(t.handle)
}
