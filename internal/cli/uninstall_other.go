//go:build !windows

package cli

import "errors"

// heldError: only Windows refuses to delete a file a program holds.
func heldError(error) bool { return false }

// startTrashCleaner: only Windows keeps a trash (uninstall_windows.go).
func startTrashCleaner(string) error { return errors.New("the trash cleaner is a Windows form") }
