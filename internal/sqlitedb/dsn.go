package sqlitedb

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var uriEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// Mode is a connection's journal and access: WAL or the rollback
// journal, read-only or the writer.
type Mode struct{ WAL, ReadOnly bool }

// DSN is the connection string of a file a writer or its reader pool
// opens. WAL needs shared memory, which network and synced folders do not
// give, so they get the rollback journal (WAL false), as does a local
// folder that failed a WAL probe. The rollback journal with
// synchronous=NORMAL can lose the file on a power cut: it gets FULL, which
// syncs at each commit (slower, on a file written in batches); NORMAL is
// for WAL, where the next checkpoint syncs the commits. The writer takes
// the write lock at BEGIN; the reader never does. extra ("" or the
// writer's own pragmas, WriterPragmas for the metrics index) ends the
// query.
func DSN(path string, m Mode, busy time.Duration, extra string) string {
	journal, sync := "WAL", "NORMAL"
	if !m.WAL {
		journal, sync = "DELETE", "FULL"
	}
	q := "_pragma=journal_mode(" + journal + ")" +
		"&_pragma=busy_timeout(" + ms(busy) + ")" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=synchronous(" + sync + ")" +
		"&_pragma=journal_size_limit(67108864)"
	if m.ReadOnly {
		q += "&mode=ro&_txlock=deferred"
	} else {
		q += "&_txlock=immediate"
	}
	return URI(path) + "?" + q + extra
}

// ReaderDSN is a plain read-only connection: mode=ro and the busy
// timeout, nothing else. It sets no journal mode (SQLite reads it from
// the file's header, and setting one is a write a read-only connection is
// refused), and it never creates the file.
func ReaderDSN(path string, busy time.Duration) string { return ProbeDSN(path, false, busy) }

// ProbeDSN reads a file without changing it: read only, and no pragma
// that could write (a journal mode). A read-only connection to a WAL file
// creates the -wal and -shm it lacks and cannot remove them, so an idle
// WAL file (no -wal: its main file holds every commit) is opened
// immutable: no lock, no journal file.
func ProbeDSN(path string, immutable bool, busy time.Duration) string {
	q := "?mode=ro&_pragma=busy_timeout(" + ms(busy) + ")"
	if immutable {
		q += "&immutable=1"
	}
	return URI(path) + q
}

// LockDSN connects to a file to take it whole: no wait, and exclusive
// locking (no shared memory: the lock fails while any other connection
// has the file open in WAL mode, even idle).
func LockDSN(path string) string {
	return URI(path) + "?_pragma=busy_timeout(0)&_pragma=locking_mode(EXCLUSIVE)"
}

// ms is d in milliseconds, as busy_timeout takes it.
func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

// URI is path as a SQLite URI filename, without its query: the one form
// every DSN uses.
func URI(path string) string { return URIPath(path, filepath.Separator == '\\') }

// URIPath is URI for a Windows path (windows) or a POSIX one. A Windows
// path loses its \\?\ prefix (\\?\UNC\ becomes \\) and takes forward
// slashes; a drive path stays as it is (file:C:/x). A path that starts
// with // (a UNC share) gets an empty authority, file:////srv/x:
// file://srv/x would name srv as the URI's authority, which SQLite
// refuses. %, ? and # are escaped.
func URIPath(path string, windows bool) string {
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
