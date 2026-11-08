package index

import "github.com/Barben360/backsight/internal/sqlitedb"

// dsn is the connection string for the index file (sqlitedb.DSN says
// why the journal and synchronous follow wal): the rollback journal (wal
// false) on network and synced folders, as on a local folder that failed
// the WAL probe (useWAL). The writer takes its own pragmas
// (sqlitedb.WriterPragmas); the reader keeps SQLite's.
func dsn(path string, wal, readOnly bool) string {
	extra := sqlitedb.WriterPragmas
	if readOnly {
		extra = ""
	}
	return sqlitedb.DSN(path, sqlitedb.Mode{WAL: wal, ReadOnly: readOnly}, busyWait, extra)
}

// probeDSN reads a file without changing it (sqlitedb.ProbeDSN): an idle
// WAL file opens immutable.
func probeDSN(path string, immutable bool) string {
	return sqlitedb.ProbeDSN(path, immutable, busyWait)
}

// uriPath is path as a SQLite URI filename, without its query, for a
// Windows path (windows) or a POSIX one (sqlitedb.URIPath).
func uriPath(path string, windows bool) string { return sqlitedb.URIPath(path, windows) }
