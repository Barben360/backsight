package ledgerdb

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

// LedgerFacts is what doctor reports of the ledger database, read without
// writing it: numbers, times and error classes only.
type LedgerFacts struct {
	// Built: the file in use was folded once. Newer: ledger.db is of a
	// newer schema than this binary's, which uses File, its side file.
	Built, Newer bool
	File         string // ledger.db or ledger-v<head>.db
	Schema, Head int    // File's user_version (0: none), this binary's
	Generation   int64
	// FoldedAt is the last fold's wall time, lines or none; Watermark the
	// latest applied event's.
	FoldedAt, Watermark time.Time
	ExportBehind        bool // exported_generation < generation
	// OpenError is why a writer could not open, build or fold File: the
	// class (sqlitedb.Code) of the file's read, or "readonly" for a file or
	// folder this user cannot write; "" when fine.
	OpenError        string
	UpdateInProgress bool
	Memories         int    // entries, a restore's alone left out (feedback.Ledger.Counted)
	Params           string // meta.params: the parameters the last fold used
}

// factsBusy is how long Facts waits for a writer's lock.
const factsBusy = writerBusy

// Facts reads o's ledger database for doctor, creating, migrating and
// writing nothing: which file this binary uses (ledger.db, or its side
// file beside a newer one), whether it was built and its meta, and why a
// writer would fail on it. Only the registry failing is an error; any
// failure of the file is its OpenError.
func Facts(o Options) (LedgerFacts, error) {
	o = o.withDefaults()
	head, err := Head(o)
	if err != nil {
		return LedgerFacts{}, err
	}
	f := LedgerFacts{File: FileName, Head: head, UpdateInProgress: errors.Is(sqlitedb.UpdateGate(o.Dirs), sqlitedb.ErrUpdateInProgress)}
	dir := o.Dirs.FeedbackDir()
	ctx := context.Background()
	v, err := fileVersion(ctx, filepath.Join(dir, FileName))
	if err == nil && v > head {
		f.Newer, f.File = true, SideFileName(head)
		v, err = fileVersion(ctx, filepath.Join(dir, f.File))
	}
	if err != nil {
		f.OpenError = sqlitedb.Code(err)
		return f, nil
	}
	f.Schema = v
	path := filepath.Join(dir, f.File)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		f.OpenError = writeProblem(dir, "")
		return f, nil
	}
	err = openAndRead(o, f.Newer, factsBusy, func(r *Reader) error {
		return r.read(f.readMeta)
	})
	switch {
	case errors.Is(err, ErrMissing): // migrated, never folded
	case errors.Is(err, ErrSchema): // older: the next writer migrates it
	case err != nil:
		f.OpenError = sqlitedb.Code(err)
		return f, nil
	}
	f.OpenError = writeProblem(dir, path)
	return f, nil
}

// readMeta fills f from a built file's meta and rows, in tx.
func (f *LedgerFacts) readMeta(tx *sql.Tx) error {
	ctx := context.Background()
	m, err := readMeta(ctx, tx)
	if err != nil {
		return err
	}
	f.Built, f.Generation, f.Params = m.built, m.generation, m.params
	f.FoldedAt, f.Watermark = fromNS(m.foldedAt), fromNS(m.watermark)
	f.ExportBehind = m.exported < m.generation
	l := feedback.Ledger{Memories: map[string]feedback.Entry{}}
	if err := scanEntries(tx, l.Memories, `SELECT `+memoryColumns+` FROM memories`); err != nil {
		return err
	}
	f.Memories = l.Counted()
	return nil
}

// writeProblem is why a writer could not write path (a file there; ""
// none) or create its companions in dir: "readonly" when this user may
// not or the file system is read-only, another sqlitedb.Code class for
// any other failure, "" when it can. dir is probed with a temporary file,
// removed at once; a dir that does not exist is fine (the writer makes
// it).
func writeProblem(dir, path string) string {
	if path != "" {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return writeClass(err)
		}
		_ = f.Close()
	}
	f, err := os.CreateTemp(dir, ".ledger-probe-*")
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		return writeClass(err)
	}
	name := f.Name()
	_ = f.Close()
	_ = fsutil.RemoveRetry(name)
	return ""
}

func writeClass(err error) string {
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return "readonly"
	}
	return sqlitedb.Code(err)
}
