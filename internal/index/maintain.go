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
	"strings"
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
// before pruneCutoff and the ingest errors older than ErrorsKeepDays (in
// batches of pruneBatch), the cursors of files gone whose rows are all
// pruned, and the ingest errors of files gone without a cursor (the
// ledger's and the local status's aside: those clear on their next good
// read); then every side file of a lower schema than this binary's (with
// its -wal and -shm) that nobody holds (seize), and the stale temp files
// of crashed rebuilds, under rebuild.lock. A missing index is nothing to
// do: no file is created. An index that needs a migration during an
// uncommitted update is not opened (ErrUpdateInProgress); the side files
// and temp files are still looked at.
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
	for _, t := range rowTables {
		n, err := ix.deleteBatched(ctx, t, timeColumn(t)+` < ?`, cut)
		res.RowsPruned += n
		if err != nil {
			return fmt.Errorf("prune %s: %w", t, err)
		}
	}
	if err := ix.pruneCursors(ctx); err != nil {
		return err
	}
	n, err := ix.deleteBatched(ctx, "ingest_errors", `at < ?`, now.Add(-ErrorsKeepDays*24*time.Hour).UnixMilli())
	res.ErrorsPruned += n
	if err != nil {
		return fmt.Errorf("prune ingest_errors: %w", err)
	}
	n, err = ix.pruneOrphanErrors(ctx)
	res.ErrorsPruned += n
	return err
}

// pruneBatch is the most rows one pruning transaction deletes, so that
// the write lock (a tick's ingest waits for it) is held briefly.
var pruneBatch = maxBatchLines

// deleteBatched deletes the rows of table matching where, at most
// pruneBatch per transaction (picked by the table's primary key), until
// none is left; the context is checked between batches. It returns the
// rows deleted, those of the batches committed before an error included.
func (ix *Index) deleteBatched(ctx context.Context, table, where string, args ...any) (int64, error) {
	pk, err := ix.primaryKey(ctx, table)
	if err != nil {
		return 0, err
	}
	q := `DELETE FROM ` + table + ` WHERE (` + pk + `) IN (SELECT ` + pk + ` FROM ` + table + ` WHERE ` + where + ` LIMIT ?)`
	args = append(slices.Clip(args), pruneBatch)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := ix.execCount(ctx, q, args...)
		total += n
		if err != nil || n < int64(pruneBatch) {
			return total, err
		}
	}
}

// primaryKey is table's primary key columns, quoted and comma-joined, in
// key order.
func (ix *Index) primaryKey(ctx context.Context, table string) (string, error) {
	var cols []string
	err := ix.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		cols, err = scanStrings(ctx, tx, `SELECT '"' || name || '"' FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
		return err
	})
	if err == nil && len(cols) == 0 {
		err = fmt.Errorf("table %s has no primary key", table)
	}
	return strings.Join(cols, ", "), err
}

// execCount runs q in its own write transaction and returns the rows it
// changed; none changed, nothing is committed.
func (ix *Index) execCount(ctx context.Context, q string, args ...any) (int64, error) {
	tx, err := ix.beginWrite(ctx)
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
	if err := ix.commit(tx); err != nil {
		return 0, ix.statementError(err)
	}
	return n, nil
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
	tx, err := ix.beginWrite(ctx)
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
	return ix.statementError(ix.commit(tx))
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
		n, err := ix.deleteBatched(ctx, "ingest_errors", `source_file = ?
			AND source_file NOT IN (SELECT source_file FROM cursors)`, rel)
		total += n
		if err != nil {
			return total, fmt.Errorf("prune ingest_errors of %s: %w", rel, err)
		}
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
// at (TempSkipped). With no temp file there, the lock is not even taken:
// nothing is written.
func sweepTemps(o Options, head int, res *MaintainResult) error {
	bases := []string{FileName}
	for n := 1; n <= head; n++ {
		bases = append(bases, SideFileName(n))
	}
	if !hasTemps(o.Dirs.IndexDir(), bases) {
		return nil
	}
	lock, err := rebuildLock(o.Dirs)
	if errors.Is(err, ErrInUse) {
		res.TempSkipped = true
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	now := time.Now() // file times are the wall clock's
	for _, b := range bases {
		res.TempRemoved = append(res.TempRemoved, sweepTmp(filepath.Join(o.Dirs.IndexDir(), b), now)...)
	}
	slices.Sort(res.TempRemoved)
	return nil
}

// hasTemps: dir holds a temp file of one of bases (<base>.tmp…).
func hasTemps(dir string, bases []string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		for _, b := range bases {
			if strings.HasPrefix(e.Name(), b+".tmp") {
				return true
			}
		}
	}
	return false
}
