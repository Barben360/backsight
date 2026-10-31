//go:build !windows

package fsutil

import "path/filepath"

func normal(p string) string { return filepath.Clean(p) }

func samePath(string, string) bool { return false }
