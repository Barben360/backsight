//go:build !windows

package cli

// The user PATH is a Windows registry value; elsewhere install.sh and the
// shell's own profile decide.
func ensureUserPath(dir string) (added bool, err error) { return false, nil }

func removeUserPath(dir string) (removed bool, err error) { return false, nil }

func userPathHas(dir string) (bool, error) { return false, nil }
