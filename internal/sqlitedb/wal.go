package sqlitedb

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// probeBusy is the WAL probe's busy timeout: its scratch file is its own,
// so only a stray connection to it could make it wait.
const probeBusy = 5 * time.Second

// HeaderIsWAL reports whether path is a database whose header (bytes 18
// and 19) says WAL. A missing or short file is not.
func HeaderIsWAL(path string) bool {
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

// ProbeWALName is the scratch file of this process's WAL probe, in the
// folder probed.
func ProbeWALName() string { return fmt.Sprintf(".wal-probe-%d.db", os.Getpid()) }

// ProbeWAL reports whether dir takes the write-ahead log. Only a local
// folder can, and some still refuse it (a FUSE filesystem that is not
// sshfs, a mapped drive, a share the OS calls local): there the first
// write fails after journal_mode=WAL has already stamped the file, and a
// rollback-journal connection cannot open such a file. So the probe does
// in dir what a writer will do, on a scratch file (ProbeWALName): it sets
// journal_mode=WAL, creates a table and writes a row (the step a folder
// without shared memory fails), then deletes the file and its -wal, -shm
// and -journal, whatever happened. Each caller keeps its own verdicts.
func ProbeWAL(dir string) bool { return probeWAL(dir) == nil }

// probeWAL is ProbeWAL with the reason it failed.
func probeWAL(dir string) (err error) {
	p := filepath.Join(dir, ProbeWALName())
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
	db, err := sql.Open(driverName, URI(p)+"?_pragma=busy_timeout("+ms(probeBusy)+")")
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
