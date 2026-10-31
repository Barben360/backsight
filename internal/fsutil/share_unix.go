//go:build !windows

package fsutil

import "os"

// OpenShared opens path for reading. On Unix a rename or remove never waits
// for a reader, so it is os.Open.
func OpenShared(path string) (*os.File, error) { return os.Open(path) }

// RemoveRetry removes path; Unix has no sharing violation to retry.
func RemoveRetry(path string) error { return os.Remove(path) }

// renameRetry is os.Rename on Unix.
func renameRetry(from, to string) error { return os.Rename(from, to) }
