package ledgerdb

import (
	"context"
	"database/sql"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/sqlitedb"
)

// FoldResult is what one fold did: the complete journal lines it read
// (the new ones; every one on a refold or a build) and the complete ones
// it skipped (not parsed, another schema), whether it rebuilt the ledger
// from the whole journal and why, and the generation it left.
type FoldResult struct {
	Lines, Skipped int
	Refolded       bool
	Reason         string // "", "late_line", "file_changed", "params", "set_aside"
	Generation     int64
}

// Why a fold rebuilds the ledger from the whole journal (refold.go), or
// built a fresh file after setting a corrupt one aside (set_aside).
const (
	ReasonLateLine    = "late_line"
	ReasonFileChanged = "file_changed"
	ReasonParams      = "params"
	ReasonSetAside    = "set_aside"
)

// Test seams.
var (
	// afterFoldRead runs after a fold read the new lines, before its
	// write transaction.
	afterFoldRead func()
	// beforeFoldCommit runs before a fold, a build or a rebuild commits;
	// an error aborts the transaction, as a crash would.
	beforeFoldCommit func() error
)

// Fold applies the journal lines past the cursors to the ledger, each
// once, in one transaction:
//  1. the new complete lines of every month file are read from its
//     cursor (a file without one from 0), before the transaction;
//  2. under BEGIN IMMEDIATE the cursors are read again, and the lines
//     too when another fold moved them;
//  3. the ledger is rebuilt from the whole journal instead when the
//     order of the lines already folded cannot hold (refold.go);
//  4. otherwise the batch, in time order, goes through Ledger.Apply over
//     the rows of every id it names, each settle without the keys
//     already settled; the changed rows, the new keys, the cursors and
//     meta are written, and the transaction commits.
//
// A ledger.db never folded (no generation row) is built. A fold that
// meets a corrupt page sets the file aside and builds a fresh one
// (recover.go): Refolded, reason set_aside. It takes no lock but
// SQLite's: BEGIN IMMEDIATE serializes the folds of every process.
func (w *Writer) Fold(ctx context.Context, p feedback.Params) (FoldResult, error) {
	if w.broken != nil {
		return FoldResult{}, w.broken
	}
	res, err := w.fold(ctx, p)
	if !isCorrupt(err) {
		return res, err
	}
	if err := w.recoverCorrupt(ctx, err); err != nil {
		return FoldResult{}, err
	}
	// The fresh file's first build folded the journal; this fold, the
	// lines journaled since, or the whole journal again when it refolds
	// (the first build's params were none).
	res, err = w.fold(ctx, p)
	if err != nil {
		return FoldResult{}, err
	}
	if !res.Refolded {
		res.Lines, res.Skipped = res.Lines+w.built.Lines, res.Skipped+w.built.Skipped
	}
	res.Refolded, res.Reason = true, ReasonSetAside
	return res, nil
}

func (w *Writer) fold(ctx context.Context, p feedback.Params) (FoldResult, error) {
	dir := w.journalDir()
	seen, err := w.position(ctx)
	if err != nil {
		return FoldResult{}, err
	}
	b, err := readBatch(dir, seen.cursors)
	if err != nil {
		return FoldResult{}, err
	}
	if afterFoldRead != nil {
		afterFoldRead()
	}
	var res FoldResult
	err = w.write(ctx, func(tx *sql.Tx) error {
		m, err := readMeta(ctx, tx)
		if err != nil {
			return err
		}
		if !m.built {
			res, _, err = w.build(ctx, tx, p, m, nil, true)
			return err
		}
		cursors, err := readCursors(ctx, tx)
		if err != nil {
			return err
		}
		if m.generation != seen.generation || !maps.Equal(cursors, seen.cursors) { // another fold committed
			if b, err = readBatch(dir, cursors); err != nil {
				return err
			}
		}
		reason, all, err := refoldReason(dir, cursors, b, m, p)
		if err != nil {
			return err
		}
		if reason != "" {
			res, _, err = w.build(ctx, tx, p, m, all, false)
			res.Refolded, res.Reason = true, reason
			return err
		}
		res, err = w.apply(ctx, tx, p, m, b)
		return err
	})
	if err != nil {
		return FoldResult{}, err
	}
	return res, nil
}

// apply folds batch b into the ledger: Ledger.Apply over the rows of
// every id the batch names, in time order, each settle without the keys
// settled_keys or an earlier settle of the batch holds; the rows that
// changed are written (a row Apply dropped is deleted), then the new
// keys, the cursors and meta.
func (w *Writer) apply(ctx context.Context, tx *sql.Tx, p feedback.Params, m meta, b batch) (FoldResult, error) {
	ids := namedIDs(b.events)
	l := feedback.Ledger{Memories: map[string]feedback.Entry{}, HistoryImported: fromNS(m.historyImported)}
	if err := loadEntries(tx, l.Memories, ids); err != nil {
		return FoldResult{}, err
	}
	before := maps.Clone(l.Memories)
	isKey, err := tx.PrepareContext(ctx, `SELECT EXISTS (SELECT 1 FROM settled_keys WHERE key = ?)`)
	if err != nil {
		return FoldResult{}, err
	}
	defer func() { _ = isKey.Close() }()
	known := map[string]bool{} // keys met: settled before, or earlier in the batch
	var fresh []string
	var qerr error
	seen := func(key string) bool {
		if known[key] {
			return true
		}
		known[key] = true
		var in bool
		if err := isKey.QueryRowContext(ctx, key).Scan(&in); err != nil {
			qerr = err
			return true
		}
		if !in {
			fresh = append(fresh, key)
		}
		return in
	}
	for _, e := range feedback.SortByTime(dropImports(b.events, m.importsCut)) {
		if e.Kind == "settle" {
			e = feedback.Unapplied(e, seen)
		}
		l.Apply(e, p)
	}
	if qerr != nil {
		return FoldResult{}, qerr
	}

	upsert, err := tx.PrepareContext(ctx, upsertMemory)
	if err != nil {
		return FoldResult{}, err
	}
	defer func() { _ = upsert.Close() }()
	changed := false
	for _, id := range ids {
		old, had := before[id]
		e, has := l.Memories[id]
		switch {
		case !has && had:
			if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE memory_id = ?`, id); err != nil {
				return FoldResult{}, err
			}
			changed = true
		case has && (!had || rowFromEntry(id, old) != rowFromEntry(id, e)):
			if _, err := upsert.ExecContext(ctx, rowFromEntry(id, e).args()...); err != nil {
				return FoldResult{}, err
			}
			changed = true
		}
	}
	if err := insertKeys(ctx, tx, fresh); err != nil {
		return FoldResult{}, err
	}
	if err := writeCursors(ctx, tx, b.ends); err != nil {
		return FoldResult{}, err
	}
	wm := max(m.watermark, toNS(latestApplied(b.events)))
	hi := toNS(l.HistoryImported)
	if changed || wm != m.watermark || hi != m.historyImported {
		m.generation++
	}
	m.watermark, m.historyImported, m.params = wm, hi, paramsJSON(p)
	if err := m.write(ctx, tx, toNS(w.opts.Now())); err != nil {
		return FoldResult{}, err
	}
	return FoldResult{Lines: len(b.events), Skipped: b.skipped, Generation: m.generation}, nil
}

// write runs fn in one write transaction (BEGIN IMMEDIATE: the writer's
// _txlock) and commits it.
func (w *Writer) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if beforeFoldCommit != nil {
		if err := beforeFoldCommit(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// journalDir is the folder of w's file, which holds the journal.
func (w *Writer) journalDir() string { return filepath.Dir(w.path) }

// position is what a fold read its lines after: the generation and the
// cursors, from one read transaction.
type position struct {
	generation int64
	cursors    map[string]int64
}

func (w *Writer) position(ctx context.Context) (position, error) {
	tx, err := w.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}) // a deferred BEGIN: no write lock
	if err != nil {
		return position{}, err
	}
	defer func() { _ = tx.Rollback() }()
	m, err := readMeta(ctx, tx)
	if err != nil {
		return position{}, err
	}
	cursors, err := readCursors(ctx, tx)
	if err != nil {
		return position{}, err
	}
	return position{generation: m.generation, cursors: cursors}, nil
}

// batch is a read of the journal: its complete lines, each month file's
// base name to where its complete lines end (its new cursor), and the
// complete lines skipped.
type batch struct {
	events  []feedback.Event
	ends    map[string]int64
	skipped int
}

// readBatch reads each month file of dir from its cursor (from 0 without
// one) up to its last newline.
func readBatch(dir string, cursors map[string]int64) (batch, error) {
	files, err := feedback.MonthFiles(dir)
	if err != nil {
		return batch{}, err
	}
	b := batch{ends: make(map[string]int64, len(files))}
	for _, name := range files {
		ev, end, sk, err := feedback.ReadComplete(filepath.Join(dir, name), cursors[name])
		if err != nil {
			return batch{}, err
		}
		b.events, b.skipped, b.ends[name] = append(b.events, ev...), b.skipped+sk, end
	}
	return b, nil
}

// readAll is the whole journal of dir, as a rebuild reads it (ReadAll).
func readAll(dir string) (batch, error) {
	events, ends, skipped, err := feedback.ReadAll(dir)
	if err != nil {
		return batch{}, err
	}
	return batch{events: events, ends: ends, skipped: skipped}, nil
}

// namedIDs are the memory ids events name, sorted, each once: the ids of
// a forget, retire, hide or unhide, the settled ids of a settle or an
// import. Ledger.Apply changes no other entry.
func namedIDs(events []feedback.Event) []string {
	set := map[string]bool{}
	for _, e := range events {
		for _, id := range eventIDs(e) {
			set[id] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

func eventIDs(e feedback.Event) []string {
	switch e.Kind {
	case "forget", feedback.KindRetire, feedback.KindHide, feedback.KindUnhide:
		return e.IDs
	case "settle", feedback.KindImport:
		ids := make([]string, len(e.Settled))
		for i, s := range e.Settled {
			ids[i] = s.ID
		}
		return ids
	}
	return nil
}

// latestApplied is the latest ts of events' applied lines (a settle, or
// a kind feedback.Folds); zero when none.
func latestApplied(events []feedback.Event) (at time.Time) {
	for _, e := range events {
		if feedback.Applied(e.Kind) && e.TS.After(at) {
			at = e.TS
		}
	}
	return at
}

// loadEntries adds the entries of ids that have a row to out.
func loadEntries(tx *sql.Tx, out map[string]feedback.Entry, ids []string) error {
	for start := 0; start < len(ids); start += maxIDs {
		chunk := ids[start:min(start+maxIDs, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		q := `SELECT ` + memoryColumns + ` FROM memories WHERE memory_id IN (?` + strings.Repeat(", ?", len(chunk)-1) + `)`
		if err := scanEntries(tx, out, q, args...); err != nil {
			return err
		}
	}
	return nil
}

// insertKeys records settle keys as applied.
func insertKeys(ctx context.Context, tx *sql.Tx, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO settled_keys(key) VALUES (?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, k := range keys {
		if _, err := stmt.ExecContext(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// isCorrupt: err is SQLite finding the file corrupt, or not a database.
func isCorrupt(err error) bool {
	c := sqlitedb.Code(err)
	return c == "corrupt" || c == "notadb"
}
