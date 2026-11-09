package ledgerdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/fsutil"
)

// betweenExportReads, when set (tests), runs inside the export's read
// transaction, after it read the rows.
var betweenExportReads func()

// Export writes ledger.json (schema 1, atomically, 0600) from one read
// transaction of ledger.db: every row, watermark_ns as its watermark,
// history_imported_ns, and the latest month file with its cursor as its
// journal_at. An older binary, or the fallback reader, then applies
// only the lines after them, so nothing counts twice. It then records
// the generation it wrote as exported_generation, and does nothing while
// that is the current one: a failed export is retried by the next call,
// even when no fold changed anything since. Two processes exporting at
// once may leave the older snapshot in place: each is whole, and the
// next fold's export replaces it.
func (w *Writer) Export() error {
	ctx := context.Background()
	l, gen, err := w.snapshot(ctx)
	if err != nil || l == nil {
		return err
	}
	raw, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(filepath.Join(w.journalDir(), "ledger.json"), raw, 0o600); err != nil {
		return fmt.Errorf("export of the feedback ledger: %w", err)
	}
	_, err = w.db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES ('exported_generation', ?)`, strconv.FormatInt(gen, 10))
	return err
}

// snapshot is the ledger to export and its generation, read in one
// transaction; nil when that generation was exported already, or the
// file was never folded.
func (w *Writer) snapshot(ctx context.Context) (*feedback.Ledger, int64, error) {
	tx, err := w.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}) // a deferred BEGIN: the snapshot starts at the first read
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	m, err := readMeta(ctx, tx)
	if err != nil || !m.built || m.exported == m.generation {
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
	return l, m.generation, nil
}
