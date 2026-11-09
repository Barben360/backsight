package sqlitedb

import (
	"os"
	"runtime"
)

// Companions are the files SQLite keeps beside a database file.
var Companions = []string{"-wal", "-shm", "-journal"}

// MakePrivate makes the database file path, and the companions it already
// has, readable by their owner alone (0600) before a writer opens it, as
// every other state file is. A missing file is created empty with that
// mode (SQLite takes an empty file for a new database), and SQLite gives
// the -wal, -shm and -journal it creates later the main file's mode. On
// Windows, whose files have no such mode, it does nothing. Best effort:
// SQLite reports a file it cannot open itself.
func MakePrivate(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	if f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600); err == nil {
		_ = f.Close()
	}
	_ = os.Chmod(path, 0o600)
	for _, c := range Companions {
		_ = os.Chmod(path+c, 0o600)
	}
}
