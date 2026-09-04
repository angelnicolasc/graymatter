//go:build windows

package harness

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsKillTargetTerminatesVerifiedHandleWithoutReopeningPID(t *testing.T) {
	const wantStart = 123456789
	pid := startSleeper(t)
	var (
		openCalls        int
		closeCalls       int
		openedHandle     windows.Handle
		handleClosed     bool
		verifiedHandle   windows.Handle
		terminatedHandle windows.Handle
	)
	t.Cleanup(func() {
		if openedHandle != 0 && !handleClosed {
			_ = windows.CloseHandle(openedHandle)
		}
	})
	api := windowsProcessAPI{
		openProcess: func(access uint32, inherit bool, gotPID uint32) (windows.Handle, error) {
			openCalls++
			if access != windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE {
				t.Errorf("OpenProcess access = %#x, want query|terminate", access)
			}
			if inherit {
				t.Error("OpenProcess requested an inheritable handle")
			}
			if gotPID != uint32(pid) {
				t.Errorf("OpenProcess PID = %d, want %d", gotPID, pid)
			}
			h, err := windows.OpenProcess(access, inherit, gotPID)
			openedHandle = h
			return h, err
		},
		getProcessTimes: func(h windows.Handle, created, _, _, _ *windows.Filetime) error {
			verifiedHandle = h
			created.LowDateTime = wantStart
			return nil
		},
		terminateProcess: func(h windows.Handle, exitCode uint32) error {
			terminatedHandle = h
			if exitCode != 1 {
				t.Errorf("TerminateProcess exit code = %d, want 1", exitCode)
			}
			return nil
		},
		closeHandle: func(h windows.Handle) error {
			closeCalls++
			if h != openedHandle {
				t.Errorf("CloseHandle handle = %#x, want %#x", h, openedHandle)
				return nil
			}
			err := windows.CloseHandle(h)
			handleClosed = err == nil
			return err
		},
	}

	target, err := openWindowsKillTarget(pid, api)
	if err != nil {
		t.Fatalf("open kill target: %v", err)
	}
	started, err := target.startTime()
	if err != nil {
		t.Fatalf("verify process start time: %v", err)
	}
	if started != wantStart {
		t.Fatalf("start time = %d, want %d", started, wantStart)
	}
	if err := target.terminate(); err != nil {
		t.Fatalf("terminate target: %v", err)
	}
	target.close()

	if openCalls != 1 {
		t.Fatalf("OpenProcess calls = %d, want exactly 1", openCalls)
	}
	if verifiedHandle != openedHandle {
		t.Errorf("GetProcessTimes handle = %#x, want %#x", verifiedHandle, openedHandle)
	}
	if terminatedHandle != verifiedHandle {
		t.Errorf("TerminateProcess handle = %#x, verified handle = %#x", terminatedHandle, verifiedHandle)
	}
	if closeCalls != 1 {
		t.Errorf("CloseHandle calls = %d, want 1", closeCalls)
	}
	if !handleClosed {
		t.Error("verified process handle was not closed")
	}
}
