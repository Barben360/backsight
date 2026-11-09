package ledgerdb

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"

	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

// sideFileRE is a side file's name (SideFileName), its head captured.
var sideFileRE = regexp.MustCompile(`^ledger-v([0-9]{1,9})\.db$`)

// Maintain is the daily upkeep of the ledger database: it removes every
// side file of a lower head than this binary's (an older binary's, beside
// a ledger.db it found newer), with its -wal, -shm and -journal, once
// nobody holds it (sqlitedb.Seize); one held is kept for the next run.
// This head's side file, a higher one's and ledger.db stay. removed is the
// names it removed, sorted. A missing feedback folder is nothing to do:
// nothing is created.
func Maintain(ctx context.Context, o Options) (removed []string, err error) {
	o = o.withDefaults()
	reg, err := registry(o)
	if err != nil {
		return nil, err
	}
	head := sqlitedb.Head(reg)
	dir := o.Dirs.FeedbackDir()
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		m := sideFileRE.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n < head {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	var errs []error
	for _, name := range names {
		ok, err := removeSide(ctx, filepath.Join(dir, name))
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			removed = append(removed, name)
		}
	}
	return removed, errors.Join(errs...)
}

// removeSide removes path and its companions under its exclusive lock
// (Seize): held by another connection, it is kept (false, nil). Windows
// cannot remove an open file: there the lock is released first, and a
// process that opens the file in between keeps it.
func removeSide(ctx context.Context, path string) (bool, error) {
	release, err := sqlitedb.Seize(ctx, path)
	if errors.Is(err, sqlitedb.ErrInUse) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !holdWhileRenaming {
		release()
	}
	err = fsutil.RemoveRetry(path)
	release()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		if !holdWhileRenaming {
			return false, nil // opened in between
		}
		return false, err
	}
	// After the release: closing the lock's connection may leave a journal.
	return true, removeCompanions(path)
}
