//go:build !windows

package daemon

import "syscall"

// detachSysProcAttr starts the daemon in a new session (Setsid), detached
// from the spawning client's controlling terminal so it survives the client.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
