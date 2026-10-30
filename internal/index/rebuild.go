package index

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
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
	// StopNotCleared: the new index is in place, but status.json could
	// not be written, so an automatic recovery's stop may still show.
	StopNotCleared bool `json:"stop_not_cleared,omitempty"`
}

// Rebuild builds the index again from its sources, beside the old one,
// and renames it over it. It waits for an uncommitted update (it applies
// migrations), and works on metrics.db or, when that is newer than this
// binary, on the side file. The rows of the files that are gone (Lost),
// or whatever an unreadable old index held (LostUnknown), cannot come
// back: the rebuild needs Yes then, and changes nothing without it. Plan
// reports and changes nothing. A rebuild holds rebuild.lock for its run
// (another one running: ErrInUse). The new file is this run's own
// <file>.tmp-<pid>-<random> (old temp files of crashed runs swept first),
// migrated and filled with no deadline, then renamed over the old file
// under its exclusive lock: another connection, or a failed rename, is
// ErrInUse, the old file kept, this run's temp file removed. A rebuild
// lifts the stop of the automatic recovery (StopNotCleared when
// status.json cannot be written).
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
	lock, err := rebuildLock(o.Dirs)
	if err != nil {
		return res, err
	}
	defer func() { _ = lock.Release() }()
	sweepTmp(res.File, time.Now())
	tmp, err := tmpName(res.File)
	if err != nil {
		return res, err
	}
	lines, err := buildTmp(ctx, o, target{reg: reg, head: head, path: tmp, side: side, known: true})
	if err == nil {
		err = swapIn(ctx, tmp, res.File)
	}
	if err != nil {
		_ = removeTmp(tmp)
		return res, err
	}
	res.Lines, res.MS = lines, o.Now().Sub(start).Milliseconds()
	// The new index is in place: a status that cannot be written does not
	// undo that, the result says so.
	res.StopNotCleared = updateStatus(o.Dirs, func(st *recoveryState) bool {
		if st.StoppedAt.IsZero() {
			return false
		}
		st.StoppedAt = time.Time{}
		return true
	}) != nil
	return res, nil
}

// rebuildLockName is the lock a rebuild and an automatic set-aside hold
// for their whole run, so that one never removes or renames the other's
// files.
const rebuildLockName = "rebuild.lock"

// rebuildLock takes <state>/index/rebuild.lock: held by a live process,
// ErrInUse; a lock whose owner is gone (a crash, a kill) is taken over.
// Only the owner's death makes a lock stale, never its age: a long
// rebuild that is still running keeps its lock.
func rebuildLock(d paths.Dirs) (*state.Lock, error) {
	l, _, err := state.Acquire(filepath.Join(d.IndexDir(), rebuildLockName), "metrics index rebuild")
	var held *state.HeldError
	if errors.As(err, &held) {
		return nil, ErrInUse
	}
	return l, err
}

// tmpStale is how long a rebuild's temp file must have been left
// unwritten before another rebuild removes it (a live one writes its own
// at every batch).
const tmpStale = time.Hour

// tmpName is this run's own temp file beside file:
// <file>.tmp-<pid>-<random>.
func tmpName(file string) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.tmp-%d-%s", file, os.Getpid(), hex.EncodeToString(b[:])), nil
}

// sweepTmp removes the temp files beside file (<file>.tmp…, with their
// -wal, -shm, -journal) that nobody wrote for tmpStale: a crashed
// rebuild's. A fresh one may be a live rebuild's (one whose lock was
// taken over) and is kept.
func sweepTmp(file string, now time.Time) {
	dir, prefix := filepath.Dir(file), filepath.Base(file)+".tmp"
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	newest := map[string]time.Time{} // main temp name → its group's latest write
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		main := name
		for _, sfx := range []string{"-wal", "-shm", "-journal"} {
			main = strings.TrimSuffix(main, sfx)
		}
		info, err := e.Info()
		if err != nil {
			newest[main] = now // unknown: kept
			continue
		}
		if t := info.ModTime(); t.After(newest[main]) {
			newest[main] = t
		}
	}
	for main, t := range newest {
		if now.Sub(t) >= tmpStale {
			_ = removeTmp(filepath.Join(dir, main))
		}
	}
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

// buildTmp builds t.path, this run's own, from nothing: migrations, every line with no
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
