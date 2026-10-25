//go:build windows

package server

import (
	"os/exec"
	"syscall"
)

// CREATE_NEW_PROCESS_GROUP: a console Ctrl-C does not reach the child.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
