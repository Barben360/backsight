//go:build !testhooks

package cli

// smokeHook can fail the server upgrade's smoke in a testhooks build only.
func smokeHook() error { return nil }
