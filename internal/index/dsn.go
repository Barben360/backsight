package index

import (
	"path/filepath"
	"strconv"
	"strings"
)

var uriEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// dsn is the connection string for the index file. WAL needs shared memory,
// which network and synced folders do not give, so they get the rollback
// journal. The writer takes the write lock at BEGIN; the reader never does.
func dsn(path string, fs FSInfo, readOnly bool) string {
	journal := "WAL"
	if fs.Kind != FSLocal {
		journal = "DELETE"
	}
	q := "_pragma=journal_mode(" + journal + ")" +
		"&_pragma=busy_timeout(" + busyMS() + ")" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=journal_size_limit(67108864)"
	if readOnly {
		q += "&mode=ro&_txlock=deferred"
	} else {
		q += "&_txlock=immediate" + writerPragmas
	}
	return fileURI(path) + "?" + q
}

// probeDSN reads a file without changing it: read only, and no pragma
// that could write (a journal mode). A read-only connection to a WAL file
// creates the -wal and -shm it lacks and cannot remove them, so an idle
// WAL file (no -wal: its main file holds every commit) opens immutable:
// no lock, no journal file.
func probeDSN(path string, immutable bool) string {
	q := "?mode=ro&_pragma=busy_timeout(" + busyMS() + ")"
	if immutable {
		q += "&immutable=1"
	}
	return fileURI(path) + q
}

// busyMS is busyWait in milliseconds, as busy_timeout takes it.
func busyMS() string { return strconv.FormatInt(busyWait.Milliseconds(), 10) }

// fileURI is path as a SQLite URI filename, without its query: the one
// form every DSN of the index uses.
func fileURI(path string) string { return uriPath(path, filepath.Separator == '\\') }

// uriPath is fileURI for a Windows path (windows) or a POSIX one. A
// Windows path loses its \\?\ prefix (\\?\UNC\ becomes \\) and takes
// forward slashes; a drive path stays as it is (file:C:/x). A path that
// starts with // (a UNC share) gets an empty authority, file:////srv/x:
// file://srv/x would name srv as the URI's authority, which SQLite
// refuses. %, ? and # are escaped.
func uriPath(path string, windows bool) string {
	if windows {
		if rest, ok := strings.CutPrefix(path, `\\?\UNC\`); ok {
			path = `\\` + rest
		} else if rest, ok := strings.CutPrefix(path, `\\?\`); ok {
			path = rest
		}
		path = strings.ReplaceAll(path, `\`, "/")
	}
	path = uriEscaper.Replace(path)
	if strings.HasPrefix(path, "//") {
		return "file://" + path
	}
	return "file:" + path
}
