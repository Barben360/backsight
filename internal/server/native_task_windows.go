//go:build windows

package server

import (
	"os"
	"unsafe"

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

// processElevated: this process's token is UAC's full token of a user who
// also has a filtered one (TokenElevationTypeFull: "Run as
// administrator"), so the user's other processes run at a lower integrity
// and cannot open what this one creates. A token with no split (UAC off,
// a CI runner: TokenElevationTypeDefault) has no such neighbour, and a
// token that cannot be read is taken as not elevated.
func processElevated() bool {
	var typ uint32
	var n uint32
	err := windows.GetTokenInformation(windows.GetCurrentProcessToken(), windows.TokenElevationType, (*byte)(unsafe.Pointer(&typ)), uint32(unsafe.Sizeof(typ)), &n)
	return err == nil && typ == tokenElevationTypeFull
}

// tokenElevationTypeFull is TOKEN_ELEVATION_TYPE's TokenElevationTypeFull,
// which x/sys lacks.
const tokenElevationTypeFull = 2
