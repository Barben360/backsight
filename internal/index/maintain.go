package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// What the index keeps: the month in progress and the 24 before it, and
// 30 days of ingest errors.
const (
	KeepMonths     = 25
	ErrorsKeepDays = 30
)

// MaintainResult is what Maintain pruned and removed.
type MaintainResult struct {
	RowsPruned   int64    `json:"rows_pruned"`
	ErrorsPruned int64    `json:"errors_pruned"`
	SideRemoved  []string `json:"side_removed,omitempty"`
	SideKept     []string `json:"side_kept,omitempty"` // in use: the next run retries
	// TempRemoved: the temp files crashed rebuilds left; TempSkipped: a
	// rebuild runs (rebuild.lock), so none was looked at.
	TempRemoved []string `json:"temp_removed,omitempty"`
	TempSkipped bool     `json:"temp_skipped,omitempty"`
}

// Maintain is the daily upkeep of the index: on the file in use
// (metrics.db, or this binary's side file beside a newer one), the rows
// before pruneCutoff, the ingest errors older than ErrorsKeepDays, the
// cursors of files gone whose rows are all pruned, and the ingest errors
// of files gone without a cursor (the ledger's and the local status's
// aside: those clear on their next good read); then every side file of a
// lower schema than this binary's (with its -wal and -shm) that nobody
// holds (seize), and the stale temp files of crashed rebuilds, under
// rebuild.lock. A missing index is nothing to do: no file is created. An
// index that needs a migration during an uncommitted update is not opened
// (ErrUpdateInProgress); the side files and temp files are still looked
// at.
func Maintain(ctx context.Context, o Options) (MaintainResult, error) {
	o = o.withDefaults()
	var res MaintainResult
	if _, err := os.Lstat(o.Dirs.IndexDir()); errors.Is(err, fs.ErrNotExist) {
		return res, nil
	}
	reg, err := registry(o)
	if err != nil {
		return res, err
	}
	head := Head(reg)
	pruneErr := prune(ctx, o, &res)
	sideErr := removeLowerSides(ctx, o, head, &res)
	return res, errors.Join(pruneErr, sideErr, sweepTemps(o, head, &res))
}

// pruneCutoff is the start of the UTC month KeepMonths-1 months before
// now's: older rows go.
func pruneCutoff(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month()-(KeepMonths-1), 1, 0, 0, 0, 0, time.UTC)
}

// prune opens the file in use when it exists, and prunes it.
func prune(ctx context.Context, o Options, res *MaintainResult) error {
	t, err := resolve(ctx, o)
	if err == nil {
		if _, serr := os.Lstat(t.path); errors.Is(serr, fs.ErrNotExist) {
			return nil // not built (or no side file yet): nothing to prune
		}
	} // else an unreadable metrics.db is there: Open sets it aside
	ix, err := Open(ctx, o)
	if err != nil {
		return err
	}
	defer func() { _ = ix.Close() }()
	return ix.prune(ctx, res)
}

// timeColumn is the column of a row table that pruning compares.
func timeColumn(table string) string {
	if table == "settles" {
		return "settle_ts"
	}
	return "ts"
}

func (ix *Index) prune(ctx context.Context, res *MaintainResult) error {
	now := ix.opts.Now()
	cut := pruneCutoff(now).UnixMilli()
	for _, t := range rowTables { // a transaction per table: the write lock is held briefly
		n, err := ix.execCount(ctx, `DELETE FROM `+t+` WHERE `+timeColumn(t)+` < ?`, cut)
		if err != nil {
			return fmt.Errorf("prune %s: %w", t, err)
		}
		res.RowsPruned += n
	}
	if err := ix.pruneCursors(ctx); err != nil {
		return err
	}
	n, err := ix.execCount(ctx, `DELETE FROM ingest_errors WHERE at < ?`, now.Add(-ErrorsKeepDays*24*time.Hour).UnixMilli())
	if err != nil {
		return fmt.Errorf("prune ingest_errors: %w", err)
	}
	res.ErrorsPruned += n
	n, err = ix.pruneOrphanErrors(ctx)
	res.ErrorsPruned += n
	return err
}

// execCount runs q in its own write transaction and returns the rows it
// changed; none changed, nothing is committed.
func (ix *Index) execCount(ctx context.Context, q string, args ...any) (int64, error) {
	tx, err := ix.sh.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, ix.statementError(err)
	}
	defer func() { _ = tx.Rollback() }()
	r, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, ix.statementError(err)
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return 0, err
	}
	return n, ix.statementError(tx.Commit())
}

// sourceGone: rel's file is not on disk (gone).
func (ix *Index) sourceGone(rel string) bool {
	return gone(filepath.Join(ix.opts.Dirs.State, filepath.FromSlash(rel)))
}

// pruneCursors removes the cursor of each file gone whose rows are all
// pruned (or that had none): nothing is left to tell about it. A file
// still on disk keeps its cursor, so that it is not read again.
func (ix *Index) pruneCursors(ctx context.Context) error {
	var rels []string
	err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		rels, err = scanStrings(ctx, tx, `SELECT source_file FROM cursors ORDER BY source_file`)
		return err
	})
	if err != nil {
		return err
	}
	for _, rel := range rels {
		if !ix.sourceGone(rel) {
			continue
		}
		if err := ix.dropCursorIfEmpty(ctx, rel); err != nil {
			return fmt.Errorf("prune cursor %s: %w", rel, err)
		}
	}
	return nil
}

// dropCursorIfEmpty removes rel's cursor when no row table holds a row of
// it; the look and the delete are one transaction.
func (ix *Index) dropCursorIfEmpty(ctx context.Context, rel string) error {
	tx, err := ix.sh.w.BeginTx(ctx, nil)
	if err != nil {
		return ix.statementError(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range rowTables {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+t+` WHERE source_file = ? LIMIT 1`, rel).Scan(&one)
		if err == nil {
			return nil // rows kept: so is the cursor
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ix.statementError(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM cursors WHERE source_file = ?`, rel); err != nil {
		return ix.statementError(err)
	}
	return ix.statementError(tx.Commit())
}

// pruneOrphanErrors removes the ingest errors of the files gone that have
// no cursor (a file unreadable until it was deleted: no ingest visits it
// again). The ledger's and the local status's rows stay: those sources
// may be absent for a while, and their next good read clears them.
func (ix *Index) pruneOrphanErrors(ctx context.Context) (int64, error) {
	var rels []string
	err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		rels, err = scanStrings(ctx, tx, `SELECT DISTINCT source_file FROM ingest_errors
			WHERE source_file NOT IN (?, ?) AND source_file NOT IN (SELECT source_file FROM cursors) ORDER BY source_file`,
			ledgerSource, stateSource)
		return err
	})
	if err != nil {
		return 0, err
	}
	var total int64
	for _, rel := range rels {
		if !ix.sourceGone(rel) {
			continue
		}
		n, err := ix.execCount(ctx, `DELETE FROM ingest_errors WHERE source_file = ?
			AND source_file NOT IN (SELECT source_file FROM cursors)`, rel)
		if err != nil {
			return total, fmt.Errorf("prune ingest_errors of %s: %w", rel, err)
		}
		total += n
	}
	return total, nil
}

func scanStrings(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

var sideFileRE = regexp.MustCompile(`^metrics-v([1-9][0-9]{0,8})\.db$`)

// sideFiles lists the side files in dir with their schema: metrics-v<N>.db.
func sideFiles(dir string) (map[string]int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, e := range ents {
		if m := sideFileRE.FindStringSubmatch(e.Name()); m != nil && e.Type().IsRegular() {
			n, _ := strconv.Atoi(m[1])
			out[e.Name()] = n
		}
	}
	return out, nil
}

// removeLowerSides removes each side file below head that nobody holds;
// one held is kept for the next run.
func removeLowerSides(ctx context.Context, o Options, head int, res *MaintainResult) error {
	dir := o.Dirs.IndexDir()
	sides, err := sideFiles(dir)
	if err != nil {
		return err
	}
	var names []string
	for name, n := range sides {
		if n < head {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var errs []error
	for _, name := range names {
		removed, err := removeSide(ctx, filepath.Join(dir, name))
		switch {
		case err != nil:
			errs = append(errs, err)
		case removed:
			res.SideRemoved = append(res.SideRemoved, name)
		default:
			res.SideKept = append(res.SideKept, name)
		}
	}
	return errors.Join(errs...)
}

// removeSide removes path and its companions under its exclusive lock
// (seize), with no open of this process in between: held by this process
// or another connection, it is kept (false, nil). Windows cannot remove an
// open file: there the lock is released first, and a process that opens
// the file in between keeps it.
func removeSide(ctx context.Context, path string) (bool, error) {
	mu.Lock()
	defer mu.Unlock()
	if _, held := files[path]; held {
		return false, nil
	}
	release, err := seize(ctx, path)
	if errors.Is(err, ErrInUse) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !holdWhileRenaming {
		release()
	}
	err = os.Remove(path)
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

// sweepTemps removes the stale temp files of crashed rebuilds beside
// metrics.db and the side files up to head (a newer binary's are its
// own), under rebuild.lock: while a live rebuild holds it, none is looked
// at (TempSkipped).
func sweepTemps(o Options, head int, res *MaintainResult) error {
	lock, err := rebuildLock(o.Dirs)
	if errors.Is(err, ErrInUse) {
		res.TempSkipped = true
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	bases := []string{FileName}
	for n := 1; n <= head; n++ {
		bases = append(bases, SideFileName(n))
	}
	now := time.Now() // file times are the wall clock's
	for _, b := range bases {
		res.TempRemoved = append(res.TempRemoved, sweepTmp(filepath.Join(o.Dirs.IndexDir(), b), now)...)
	}
	slices.Sort(res.TempRemoved)
	return nil
}
