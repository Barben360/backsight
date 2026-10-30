//go:build !testhooks

package cli

// smokeHook can fail the server upgrade's smoke in a testhooks build only.
func smokeHook() error { return nil }

// switchFailHook can fail a runtime switch step in a testhooks build only.
func switchFailHook(string) error { return nil }
