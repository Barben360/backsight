package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// The ledger and the local status are not month files: their errors are
// recorded once, at offset 0, against these names.
const (
	ledgerSource = "feedback/ledger.json"
	stateSource  = "state.json"
)

// metaLedgerSHA is the meta key of the SHA-256 of the ledger the ledger
// table mirrors.
const metaLedgerSHA = "ledger_sha256"

// stamper is a LedgerReader that can stamp what it reads
// (feedback.CurrentReader).
type stamper interface {
	Stamp() (feedback.Stamp, error)
}

// stampSettle is how much older than a read's start the newest file of
// its stamp must be for the stamp to be trusted: a write in the same
// mtime tick as the stamp (2 s on the coarsest filesystems) could leave it
// unchanged.
const stampSettle = 2 * time.Second

// ledgerSkip is the last good read's stamp and SHA, in memory only; ok
// when a catch-up may skip a read whose stamp is equal.
type ledgerSkip struct {
	mu    sync.Mutex
	stamp feedback.Stamp
	sha   string
	ok    bool
}

// refreshLedger mirrors the ledger read through Options.Ledger into the
// ledger table: when its JSON's SHA-256 differs from the one recorded,
// the table is emptied and refilled in one transaction. A reader error
// keeps the table and is recorded once (ledger_unreadable) until a good
// read clears it. Only the index's own errors come back.
//
// The ledger is read before the write transaction (the reader may be
// slow; the write lock is not held meanwhile): of two processes catching
// up at once, the one that read first may commit last and store the
// older read for a moment. The next catch-up reads it again and heals it.
//
// On a local feedback directory, a reader with a stamp is not read again
// while its stamp, taken before each read, equals the one of the last
// good read, that stamp was settled (its newest file older than that
// read's start by stampSettle), and the index still holds that read: its
// SHA and no ledger_unreadable row (another process storing an older read
// is healed as above). Anything else reads.
func (ix *Index) refreshLedger(ctx context.Context) error {
	ix.skip.mu.Lock()
	defer ix.skip.mu.Unlock()
	readStart := ix.opts.Now()
	st, stamped := ix.ledgerStamp()
	if stamped && ix.skip.ok && st.Equal(ix.skip.stamp) {
		seen, flagged, err := ix.mirroredLedger(ctx)
		if err != nil {
			ix.skip.ok = false
			return err
		}
		if seen == ix.skip.sha && !flagged {
			return nil
		}
	}
	ix.skip.ok = false
	sum, good, err := ix.mirrorLedger(ctx)
	if err != nil || !good {
		return err
	}
	if stamped && st.Newest.Before(readStart.Add(-stampSettle)) {
		ix.skip.stamp, ix.skip.sha, ix.skip.ok = st, sum, true
	}
	return nil
}

// ledgerStamp is the reader's stamp when the skip may apply: a local
// feedback directory, a reader with a stamp, no error.
func (ix *Index) ledgerStamp() (feedback.Stamp, bool) {
	s, ok := ix.opts.Ledger.(stamper)
	if !ix.localFeedback || !ok {
		return feedback.Stamp{}, false
	}
	st, err := s.Stamp()
	return st, err == nil
}

// mirroredLedger is the SHA of the ledger the table holds and whether a
// ledger_unreadable row is there, read without the write lock.
func (ix *Index) mirroredLedger(ctx context.Context) (seen string, flagged bool, err error) {
	err = ix.ReadTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaLedgerSHA).Scan(&seen)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		flagged, err = hasErrorRow(ctx, tx, ledgerSource, 0)
		return err
	})
	return seen, flagged, err
}

// mirrorLedger reads the ledger and mirrors it (refreshLedger without the
// skip): good when the read succeeded and the table now holds it, sum its
// SHA.
func (ix *Index) mirrorLedger(ctx context.Context) (sum string, good bool, err error) {
	l, err := ix.opts.Ledger.All()
	var raw []byte
	if err == nil {
		raw, err = json.Marshal(l) // fails on a NaN: as unreadable
	}
	if err != nil {
		return "", false, ix.recordOnce(ctx, ledgerSource, 0, errLedgerUnreadable)
	}
	sum = hexSHA(raw)
	seen, flagged, err := ix.mirroredLedger(ctx) // unchanged: no write lock
	if err != nil {
		return "", false, err
	}
	if seen == sum && !flagged {
		return sum, true, nil
	}
	tx, err := ix.beginWrite(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	var old string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaLedgerSHA).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	cleared, err := clearError(ctx, tx, ledgerSource)
	if err != nil {
		return "", false, err
	}
	if old == sum && !cleared {
		return sum, true, nil // unchanged: no write
	}
	if old != sum {
		if err := replaceLedger(ctx, tx, l); err != nil {
			return "", false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaLedgerSHA, sum); err != nil {
			return "", false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return sum, true, nil
}

// replaceLedger empties the ledger table and inserts every entry whose id
// is a name, without the hiding's verdict and note.
func replaceLedger(ctx context.Context, tx *sql.Tx, l feedback.Ledger) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM ledger`); err != nil {
		return err
	}
	st, err := tx.PrepareContext(ctx, `INSERT INTO ledger(memory_id, shown, credits, credits_at, misses, misses_at,
		last_shown, last_credited, wrong, hidden_since, unhidden_at, imported_shown, imported_credits)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ids := make([]string, 0, len(l.Memories))
	for id := range l.Memories {
		if nameOK(id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		m := l.Memories[id]
		var hidden any
		if m.Hidden != nil {
			hidden = msOrNil(m.Hidden.Since)
		}
		if _, err := st.ExecContext(ctx, id, m.Shown, m.Credits, msOrNil(m.CreditsAt), m.Misses, msOrNil(m.MissesAt),
			msOrNil(m.LastShown), msOrNil(m.LastCredited), m.Wrong, hidden, msOrNil(m.UnhiddenAt),
			m.Imported.Shown, m.Imported.Credits); err != nil {
			return err
		}
	}
	return nil
}

// clearError removes rel's error row at offset 0, reporting whether there
// was one.
func clearError(ctx context.Context, tx *sql.Tx, rel string) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM ingest_errors WHERE source_file = ? AND "offset" = 0`, rel)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
