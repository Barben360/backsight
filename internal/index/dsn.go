package index

import (
	"path/filepath"
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
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=journal_size_limit(67108864)"
	if readOnly {
		q += "&mode=ro&_txlock=deferred"
	} else {
		q += "&_txlock=immediate"
	}
	return "file:" + uriEscaper.Replace(filepath.ToSlash(path)) + "?" + q
}

// probeDSN reads a file without changing it: read only, and no pragma
// that could write (a journal mode). A read-only connection to a WAL file
// creates the -wal and -shm it lacks and cannot remove them, so an idle
// WAL file (no -wal: its main file holds every commit) opens immutable:
// no lock, no journal file.
func probeDSN(path string, immutable bool) string {
	q := "?mode=ro&_pragma=busy_timeout(5000)"
	if immutable {
		q += "&immutable=1"
	}
	return "file:" + uriEscaper.Replace(filepath.ToSlash(path)) + q
}
