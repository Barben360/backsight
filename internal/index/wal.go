package index

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	if headerIsWAL(path) {
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

// headerIsWAL: path is a database whose header (bytes 18 and 19) says WAL.
func headerIsWAL(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var h [20]byte
	if n, _ := f.Read(h[:]); n < len(h) {
		return false
	}
	return h[18] == 2 && h[19] == 2
}

// probeName is the scratch file of this process's probe.
func probeName() string { return fmt.Sprintf(".wal-probe-%d.db", os.Getpid()) }

// probeWAL does in dir what the index will do: opens a scratch database,
// sets journal_mode=WAL, creates a table and writes a row (the step a
// folder without shared memory fails), then deletes the file and its
// -wal and -shm, whatever happened.
func probeWAL(dir string) (err error) {
	p := filepath.Join(dir, probeName())
	remove := func() error {
		var errs []error
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if e := os.Remove(p + suffix); e != nil && !errors.Is(e, fs.ErrNotExist) {
				errs = append(errs, e)
			}
		}
		return errors.Join(errs...)
	}
	_ = remove() // a leftover of a run that died
	defer func() {
		if e := remove(); err == nil {
			err = e
		}
	}()
	db, err := sql.Open(driverName, fileURI(p)+"?_pragma=busy_timeout("+busyMS()+")")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if e := db.Close(); err == nil {
			err = e
		}
	}()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("journal_mode=WAL answered %q", mode)
	}
	_, err = db.Exec("CREATE TABLE probe(x INTEGER); INSERT INTO probe VALUES (1)")
	return err
}
