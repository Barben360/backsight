package ledgerdb

import (
	"context"
	"database/sql"

	"github.com/Barben360/backsight/internal/feedback"
)

// Rebuild empties and refills the ledger from the whole journal in one
// transaction, in place (readers see the old ledger or the new one,
// nothing is renamed), as a new build: imports left out without
// withImport, a choice that refolds keep until the next Rebuild. Then it
// exports; the database stays rebuilt when only the export fails, whose
// error it returns with the ledger.
func (w *Writer) Rebuild(ctx context.Context, p feedback.Params, withImport bool) (feedback.Ledger, error) {
	l, err := w.rebuild(ctx, p, withImport)
	if isCorrupt(err) {
		if err := w.recoverCorrupt(ctx, err); err != nil {
			return feedback.Ledger{}, err
		}
		l, err = w.rebuild(ctx, p, withImport)
	}
	if err != nil {
		return feedback.Ledger{}, err
	}
	return l, w.Export()
}

func (w *Writer) rebuild(ctx context.Context, p feedback.Params, withImport bool) (feedback.Ledger, error) {
	imports := importsDropped
	if withImport {
		imports = importsKept
	}
	var l feedback.Ledger
	err := w.write(ctx, func(tx *sql.Tx) error {
		m, err := readMeta(ctx, tx)
		if err != nil {
			return err
		}
		_, l, err = w.build(ctx, tx, p, m, imports, nil, true)
		return err
	})
	return l, err
}

// firstBuild folds the whole journal into the file OpenWriter just
// created, imports kept, unless another process built it first. Opening
// takes no params, so it folds under none: every settle records its own
// half-lives, and an import that recorded none folds without decay until
// the first fold under the tick's params refolds it (a params refold).
func (w *Writer) firstBuild(ctx context.Context) error {
	return w.write(ctx, func(tx *sql.Tx) error {
		m, err := readMeta(ctx, tx)
		if err != nil || m.built {
			return err
		}
		_, _, err = w.build(ctx, tx, feedback.Params{}, m, importsKept, nil, true)
		return err
	})
}
