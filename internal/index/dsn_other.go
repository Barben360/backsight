//go:build !windows

package index

// writerPragmas: elsewhere the writer keeps SQLite's checkpoint and cache
// defaults (dsn_windows.go says why Windows does not).
const writerPragmas = ""
