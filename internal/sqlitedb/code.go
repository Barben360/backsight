package sqlitedb

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Code is err's class, the only form in which a database error is stored
// or shown: corrupt | notadb | full | busy | readonly | io | cantopen |
// other ("" for nil). SQLite's own code decides; failing that, a full
// disk is full, a file system error io, and ErrInUse busy.
func Code(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrInUse) {
		return "busy"
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_CORRUPT:
			return "corrupt"
		case sqlite3.SQLITE_NOTADB:
			return "notadb"
		case sqlite3.SQLITE_FULL:
			return "full"
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return "busy"
		case sqlite3.SQLITE_READONLY:
			return "readonly"
		case sqlite3.SQLITE_CANTOPEN:
			return "cantopen"
		case sqlite3.SQLITE_IOERR, sqlite3.SQLITE_PERM, sqlite3.SQLITE_NOLFS:
			return "io"
		}
		return "other"
	}
	if errors.Is(err, syscall.ENOSPC) {
		return "full"
	}
	var pe *fs.PathError
	var le *os.LinkError
	var sc *os.SyscallError
	if errors.As(err, &pe) || errors.As(err, &le) || errors.As(err, &sc) ||
		errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
		return "io"
	}
	return "other"
}
