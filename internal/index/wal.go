package index

import (
	"errors"
	"path/filepath"

	"github.com/Barben360/backsight/internal/sqlitedb"
)

// walVerdict is the WAL probe's answer per index folder, for the life of
// the process (guarded by mu).
var walVerdict = map[string]bool{}

func resetWALVerdicts() {
	mu.Lock()
	defer mu.Unlock()
	clear(walVerdict)
}

// walProbe is the seam: nil when dir takes the write-ahead log.
var walProbe = probeWAL

// useWAL decides the journal mode of the index at path, mu held. Only a
// local folder can have WAL; it still may refuse it (a FUSE filesystem
// that is not sshfs, a mapped drive, a share the OS calls local): there
// the first write fails after journal_mode=WAL has already stamped the
// file, and a rollback-journal connection cannot open such a file, so the
// folder is tried on a scratch file first. A file that is already in WAL
// mode skips the probe: its folder took WAL once, and the rollback journal
// could not open it now anyway (an existing WAL file on a folder that
// refuses WAL now fails to write, as before; the way out is a local
// folder). The verdict is kept per folder for the process.
func useWAL(path string, fsi FSInfo) bool {
	if fsi.Kind != FSLocal {
		return false
	}
	if sqlitedb.HeaderIsWAL(path) {
		return true
	}
	dir := filepath.Dir(path)
	if v, ok := walVerdict[dir]; ok {
		return v
	}
	v := walProbe(dir) == nil
	walVerdict[dir] = v
	return v
}

// errWALRefused: the folder failed the WAL probe (sqlitedb.ProbeWAL).
var errWALRefused = errors.New("the folder refused the write-ahead log")

// probeName is the scratch file of this process's probe
// (sqlitedb.ProbeWALName).
func probeName() string { return sqlitedb.ProbeWALName() }

// probeWAL does in dir what the index will do, on a scratch file it
// removes whatever happened (sqlitedb.ProbeWAL): nil when dir takes the
// write-ahead log.
func probeWAL(dir string) error {
	if !sqlitedb.ProbeWAL(dir) {
		return errWALRefused
	}
	return nil
}
