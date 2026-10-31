//go:build !windows

package cli

// The user PATH is a Windows registry value; elsewhere install.sh and the
// shell's own profile decide.
func ensureUserPath(dir string) (added bool, err error) { return false, nil }

//nolint:unused // uninstall calls it (Task 13)
func removeUserPath(dir string) (removed bool, err error) { return false, nil }
