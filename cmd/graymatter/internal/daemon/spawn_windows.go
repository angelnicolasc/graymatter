//go:build windows

package daemon

import "syscall"

const (
	detachedProcess    = 0x00000008
	createNewProcGroup = 0x00000200
)

// detachSysProcAttr starts the daemon detached from the spawning client's
// console (DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP) so it survives the
// client and never pops a window.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcGroup,
		HideWindow:    true,
	}
}
