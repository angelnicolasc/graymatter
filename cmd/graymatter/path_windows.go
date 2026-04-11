//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// addExeDirToUserPath appends the executable's directory to HKCU\Environment\Path.
// Returns (true, nil) if added, (false, nil) if already present, (false, err) on failure.
func addExeDirToUserPath() (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	exeDir := filepath.Dir(exe)

	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.READ|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()

	current, _, err := k.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return false, err
	}

	for _, seg := range strings.Split(current, ";") {
		if strings.EqualFold(strings.TrimSpace(seg), exeDir) {
			return false, nil // already present
		}
	}

	updated := exeDir
	if strings.TrimSpace(current) != "" {
		updated = current + ";" + exeDir
	}
	if err := k.SetStringValue("Path", updated); err != nil {
		return false, err
	}

	broadcastEnvironmentChange() // best-effort, not checked
	return true, nil
}

func broadcastEnvironmentChange() {
	user32 := syscall.NewLazyDLL("user32.dll")
	sendMsg := user32.NewProc("SendMessageTimeoutW")
	env, _ := syscall.UTF16PtrFromString("Environment")
	sendMsg.Call(
		uintptr(0xFFFF),        // HWND_BROADCAST
		uintptr(0x001A),        // WM_SETTINGCHANGE
		0,
		uintptr(unsafe.Pointer(env)),
		0x0002, // SMTO_ABORTIFHUNG
		500,
		0,
	)
}
