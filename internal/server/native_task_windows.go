//go:build windows

package server

import (
	"os"

	"golang.org/x/sys/windows"

	"github.com/Barben360/backsight/internal/detach"
)

// currentUserSID is this process's user, as an SDDL string.
func currentUserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// startDetachedSupervisor starts conhost.exe --headless with args, as
// the task's action does: detached, out of the caller's job (Windows
// OpenSSH kills a session's job at logout, E34 W8), with no standard
// handle (conhost exits at its stdin's end).
func startDetachedSupervisor(args ...string) error {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return detach.StartNoStdio(winJoin(root, "System32", "conhost.exe"), args...)
}
