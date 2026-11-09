package ledgerdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/state"
)

// betweenExportReads, when set (tests), runs inside the export's read
// transaction, after it read the rows.
var betweenExportReads func()

// Export writes ledger.json (schema 1, atomically, 0600) from one read
// transaction of ledger.db: every row, watermark_ns as its watermark,
// history_imported_ns, and the latest month file with its cursor as its
// journal_at. An older binary, or the fallback reader, then applies
// only the lines after them, so nothing counts twice. Once the file is
// written it records the generation it wrote as exported_generation,
// and does nothing while that is the current one: a failed export is
// retried by the next call, even when no fold changed anything since. A
// ledger.json that is gone, does not parse or has another schema is
// written again whatever generation it last held, as an older binary's
// tick repaired it at once.
// Exports are serialized by <state>/locks/feedback-export.lock, so
// exported_generation always names the snapshot ledger.json holds; one
// that waits more than exportLockWait for it does nothing (the holder
// exports, the next call retries). A corruption the snapshot meets is
// set aside and the fresh file exported (recover.go).
func (w *Writer) Export() error {
	if w.broken != nil {
		return w.broken
	}
	release, err := lockExport(w.opts.Dirs)
	if err != nil || release == nil {
		return err
	}
	defer release()
	file := filepath.Join(w.journalDir(), "ledger.json")
	_, statErr := os.Stat(file) // LoadLedger reads a missing file as an empty ledger
	_, loadErr := feedback.LoadLedger(w.journalDir())
	// Gone, unparsable or of another schema: written again at once. One
	// that cannot be read (a permission) is written once per generation,
	// as any other: rewriting it at every call would not make it readable.
	force := errors.Is(statErr, fs.ErrNotExist) || errors.Is(loadErr, feedback.ErrCorruptLedger) || errors.Is(loadErr, feedback.ErrLedgerSchema)
	ctx := context.Background()
	l, gen, err := w.snapshot(ctx, force)
	if isCorrupt(err) {
		if err := w.recoverCorrupt(ctx, err); err != nil {
			return err
		}
		l, gen, err = w.snapshot(ctx, force)
	}
	if err != nil || l == nil {
		return err
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(file, raw, 0o600); err != nil {
		return fmt.Errorf("export of the feedback ledger: %w", err)
	}
	_, err = w.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES ('exported_generation', ?)`, strconv.FormatInt(gen, 10))
	return err
}

// exportLockWait is how long an export waits for another's lock (a
// variable for the tests).
var exportLockWait = 2 * time.Second

// lockExport takes <state>/locks/feedback-export.lock, breaking a lock
// whose owner is gone, waiting at most exportLockWait while a live owner
// holds it; release is nil (and err too) when it still holds it then.
func lockExport(d paths.Dirs) (release func(), err error) {
	path := filepath.Join(d.LocksDir(), "feedback-export.lock")
	deadline := time.Now().Add(exportLockWait)
	for {
		l, _, err := state.Acquire(path, "feedback ledger export")
		if err == nil {
			return func() { _ = l.Release() }, nil
		}
		var held *state.HeldError
		if !errors.As(err, &held) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// snapshot is the ledger to export and its generation, read in one
// transaction; nil when the file was never folded, or (unless force)
// when that generation was exported already.
func (w *Writer) snapshot(ctx context.Context, force bool) (*feedback.Ledger, int64, error) {
	tx, err := w.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}) // a deferred BEGIN: the snapshot starts at the first read
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	m, err := readMeta(ctx, tx)
	if err != nil || !m.built || (!force && m.exported == m.generation) {
		return nil, 0, err
	}
	l := &feedback.Ledger{Schema: feedback.LedgerSchema, Memories: map[string]feedback.Entry{}}
	if err := scanEntries(tx, l.Memories, `SELECT `+memoryColumns+` FROM memories`); err != nil {
		return nil, 0, err
	}
	if betweenExportReads != nil {
		betweenExportReads()
	}
	if m, err = readMeta(ctx, tx); err != nil {
		return nil, 0, err
	}
	cursors, err := readCursors(ctx, tx)
	if err != nil {
		return nil, 0, err
	}
	l.Watermark, l.HistoryImported, l.JournalAt = fromNS(m.watermark), fromNS(m.historyImported), latestPos(cursors)
	// A journal that already ends in a later month than now's (a line
	// dated in the future): a line journaled now goes to now's month,
	// before that end, so the reader of ledger.json must read the months
	// since the watermark, not on from JournalAt (as v0.1.32 wrote it).
	if l.JournalAt.File > feedback.MonthFile(w.opts.Now()) {
		l.JournalAt = feedback.JournalPos{}
	}
	return l, m.generation, nil
}
