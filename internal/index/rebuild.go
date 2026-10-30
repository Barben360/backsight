package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"
)

var (
	// ErrInUse: another connection holds the index, or the rename over it
	// failed; the new file is removed, the old one kept.
	ErrInUse = errors.New("the metrics index is in use: close the dashboard and run backsight stats index rebuild again; the old index is kept")
	// ErrNeedsYes: a rebuild would drop rows whose source files are gone.
	ErrNeedsYes = errors.New("some months would be lost")
)

// RebuildOptions: Plan only reports; Yes accepts losing the rows of the
// files that are gone.
type RebuildOptions struct{ Plan, Yes bool }

// LostSource is a source file whose rows a rebuild cannot bring back.
type LostSource struct {
	SourceFile string `json:"source_file"`
	Month      string `json:"month"` // YYYY-MM from the file name
	Rows       int64  `json:"rows"`
}

// RebuildResult is what Rebuild found and did.
type RebuildResult struct {
	Plan        bool         `json:"plan"`
	File        string       `json:"file"`
	Lost        []LostSource `json:"lost"`
	LostUnknown bool         `json:"lost_unknown"` // the old index could not be read
	NeedsYes    bool         `json:"needs_yes"`
	Lines       int64        `json:"lines"`
	MS          int64        `json:"ms"`
}

// Rebuild builds the index again from its sources, beside the old one,
// and renames it over it. It waits for an uncommitted update (it applies
// migrations), and works on metrics.db or, when that is newer than this
// binary, on the side file. The rows of the files that are gone (Lost),
// or whatever an unreadable old index held (LostUnknown), cannot come
// back: the rebuild needs Yes then, and changes nothing without it. Plan
// reports and changes nothing. The new file is <file>.tmp (a stale one
// removed first), migrated and filled with no deadline, then renamed over
// the old file under its exclusive lock: another connection, or a failed
// rename, is ErrInUse, the old file kept. A rebuild lifts the stop of the
// automatic recovery.
func Rebuild(ctx context.Context, o Options, ro RebuildOptions) (RebuildResult, error) {
	o = o.withDefaults()
	start := o.Now()
	res := RebuildResult{Plan: ro.Plan, Lost: []LostSource{}}
	if err := updateGate(o.Dirs); err != nil {
		return res, err
	}
	reg, err := registry(o)
	if err != nil {
		return res, err
	}
	head := Head(reg)
	res.File = filepath.Join(o.Dirs.IndexDir(), FileName)
	side := false
	if v, _, err := probeVersion(ctx, res.File); err == nil && v > head {
		res.File, side = filepath.Join(o.Dirs.IndexDir(), SideFileName(head)), true
	}
	if lost, err := lostSources(ctx, o, res.File); err != nil {
		res.LostUnknown = true
	} else {
		res.Lost = lost
	}
	res.NeedsYes = len(res.Lost) > 0 || res.LostUnknown
	if ro.Plan {
		return res, nil
	}
	if res.NeedsYes && !ro.Yes {
		return res, ErrNeedsYes
	}
	tmp := res.File + ".tmp"
	lines, err := buildTmp(ctx, o, target{reg: reg, head: head, path: tmp, side: side, known: true})
	if err == nil {
		err = swapIn(ctx, tmp, res.File)
	}
	if err != nil {
		_ = removeTmp(tmp)
		return res, err
	}
	res.Lines, res.MS = lines, o.Now().Sub(start).Milliseconds()
	return res, updateStatus(o.Dirs, func(st *recoveryState) bool {
		if st.StoppedAt.IsZero() {
			return false
		}
		st.StoppedAt = time.Time{}
		return true
	})
}

var lostMonthRE = regexp.MustCompile(`(\d{4}-(?:0[1-9]|1[0-2]))\.jsonl$`)

// lostSources reads file, without changing it, for the cursors whose file
// is gone (marked missing, or absent now) and their row counts; a source
// without rows loses nothing. No file: nothing lost. An error: the old
// index cannot be read.
func lostSources(ctx context.Context, o Options, file string) ([]LostSource, error) {
	out := []LostSource{}
	if _, err := os.Lstat(file); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	db, err := sql.Open(driverName, probeDSN(file, idleWAL(file)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	present := map[string]bool{}
	tables, err := tableNames(ctx, db, `SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		present[t] = true
	}
	type cur struct {
		rel     string
		missing bool
	}
	var curs []cur
	rows, err := db.QueryContext(ctx, `SELECT source_file, missing FROM cursors ORDER BY source_file`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c cur
		if err := rows.Scan(&c.rel, &c.missing); err != nil {
			_ = rows.Close()
			return nil, err
		}
		curs = append(curs, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range curs {
		if !c.missing && !gone(filepath.Join(o.Dirs.State, filepath.FromSlash(c.rel))) {
			continue
		}
		var total int64
		for _, t := range rowTables {
			if !present[t] {
				continue
			}
			var n int64
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+t+` WHERE source_file = ?`, c.rel).Scan(&n); err != nil {
				return nil, err
			}
			total += n
		}
		if total == 0 {
			continue
		}
		var month string
		if m := lostMonthRE.FindStringSubmatch(path.Base(c.rel)); m != nil {
			month = m[1]
		}
		out = append(out, LostSource{SourceFile: c.rel, Month: month, Rows: total})
	}
	return out, nil
}

// gone: no regular file at p (the ingest marks such a file missing).
func gone(p string) bool {
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return err == nil && !info.Mode().IsRegular()
}

func tableNames(ctx context.Context, db *sql.DB, q string) ([]string, error) {
	rows, err := db.QueryContext(ctx, q)
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

// buildTmp builds t.path from nothing: migrations, every line with no
// deadline, the ledger, the local status; then out of WAL mode, so that
// it is one file to rename, and closed. It returns the lines read.
func buildTmp(ctx context.Context, o Options, t target) (int64, error) {
	if err := removeTmp(t.path); err != nil {
		return 0, err
	}
	ix, _, err := openOnce(ctx, o, t, false)
	if err != nil {
		return 0, err
	}
	_, st, err := ix.refresh(ctx, o.Now(), time.Time{})
	if err == nil {
		err = ix.leaveWAL(ctx)
	}
	if cerr := ix.Close(); err == nil {
		err = cerr
	}
	if classify(err) == "full" {
		err = ErrDiskFull
	}
	return st.Lines, err
}

// leaveWAL switches the file to the rollback journal, which folds the
// -wal into it; the reader pool is closed first (an open connection
// keeps a WAL file in WAL mode).
func (ix *Index) leaveWAL(ctx context.Context) error {
	if err := ix.sh.r.Close(); err != nil {
		return err
	}
	var mode string
	if err := ix.sh.w.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		return err
	}
	if mode != "delete" {
		return fmt.Errorf("journal mode %s", mode)
	}
	return nil
}

// removeTmp removes a rebuild's file and its companions.
func removeTmp(tmp string) error {
	err := os.Remove(tmp)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return errors.Join(err, removeCompanions(tmp))
}

// swapIn renames tmp over file under file's exclusive lock (seize), with
// no open of this process in between: a handle of this process or
// another connection on file, or a failed rename, is ErrInUse.
func swapIn(ctx context.Context, tmp, file string) error {
	mu.Lock()
	defer mu.Unlock()
	if _, held := files[file]; held {
		return ErrInUse
	}
	release, err := seize(ctx, file)
	if err != nil {
		return err
	}
	defer release()
	if !holdWhileRenaming {
		release()
	}
	if err := removeCompanions(file); err != nil {
		return err
	}
	if err := renameFile(tmp, file); err != nil {
		return ErrInUse
	}
	return nil
}
