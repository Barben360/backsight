//go:build !windows

package setup

import (
	"context"
	"errors"
)

// errWindowsOnly: the Windows probes have nothing to probe elsewhere.
var errWindowsOnly = errors.New("windows only")

// Conhost: conhost.exe is Windows'.
func Conhost(context.Context) error { return errWindowsOnly }

// UserSID: a SID is Windows'.
func UserSID() (string, error) { return "", errWindowsOnly }
