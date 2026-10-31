//go:build !windows

package server

import "errors"

// The task host runs on Windows only; elsewhere its files render (tests
// set a SID) and nothing starts.
var errWindowsOnly = errors.New("the scheduled task host runs on Windows only")

func currentUserSID() (string, error) { return "", errWindowsOnly }

func startDetachedSupervisor(...string) error { return errWindowsOnly }
