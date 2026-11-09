//go:build !windows

package sqlitedb

// WriterPragmas: elsewhere a writer keeps SQLite's checkpoint and cache
// defaults (dsn_windows.go says why Windows does not).
const WriterPragmas = ""
