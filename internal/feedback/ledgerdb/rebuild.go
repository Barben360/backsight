package ledgerdb

import (
	"context"
	"database/sql"

	"github.com/Barben360/backsight/internal/feedback"
)

// Rebuild empties and refills the ledger from the whole journal in one
// transaction, in place (readers see the old ledger or the new one,
// nothing is renamed), as a new build. Without withImport, every import
// journaled until now is left out, and stays out of the folds that
// follow (an import journaled later applies) until a Rebuild with
// withImport brings them back. Then it exports; the database stays
// rebuilt when only the export fails, whose error it returns with the
// ledger.
func (w *Writer) Rebuild(ctx context.Context, p feedback.Params, withImport bool) (feedback.Ledger, error) {
	if w.broken != nil {
		return feedback.Ledger{}, w.broken
	}
	cut := int64(0)
	if !withImport {
		cut = toNS(w.opts.Now())
	}
	l, err := w.rebuild(ctx, p, cut)
	if isCorrupt(err) {
		if err := w.recoverCorrupt(ctx, err); err != nil {
			return feedback.Ledger{}, err
		}
		l, err = w.rebuild(ctx, p, cut)
	}
	if err != nil {
		return feedback.Ledger{}, err
	}
	return l, w.Export()
}

func (w *Writer) rebuild(ctx context.Context, p feedback.Params, cut int64) (feedback.Ledger, error) {
	var l feedback.Ledger
	err := w.write(ctx, func(tx *sql.Tx) error {
		m, err := readMeta(ctx, tx)
		if err != nil {
			return err
		}
		m.importsCut = cut
		_, l, err = w.build(ctx, tx, p, m, nil, true)
		return err
	})
	return l, err
}

// firstBuild folds the whole journal into the file OpenWriter just
// created, the imports journaled at or before cut left out (0: none),
// unless another process built it first. Opening takes no params, so it
// folds under none: every settle records its own half-lives, and an
// import that recorded none folds without decay until the first fold
// under the tick's params refolds it (a params refold).
func (w *Writer) firstBuild(ctx context.Context, cut int64) error {
	return w.write(ctx, func(tx *sql.Tx) error {
		m, err := readMeta(ctx, tx)
		if err != nil || m.built {
			return err
		}
		m.importsCut = cut
		w.built, _, err = w.build(ctx, tx, feedback.Params{}, m, nil, true)
		return err
	})
}
