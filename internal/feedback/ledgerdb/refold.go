package ledgerdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/fsutil"
)

// refoldReason says why batch b, read past cursors, cannot simply be
// applied on top of the folded ledger m: then the fold rebuilds from the
// whole journal, all (non-nil when this already read it). Exact equality
// with a rebuild, floats bit for bit, rests on one global (ts, seq)
// order of the applied lines: sums decayed in another order differ in
// their last bits, so no kind skips these checks because it "commutes".
//   - file_changed: a cursor's file is gone, or shorter than its cursor,
//     or its byte before the cursor is not a newline: it was truncated,
//     replaced or rewritten, and the cursor no longer marks what was
//     folded.
//   - late_line: an applied line of the batch (a settle, a kind
//     feedback.Folds) is not after watermark_ns. Every applied line
//     folded so far is at or before the watermark, so a batch whose
//     applied lines are all later is a time-ordered suffix of the
//     rebuild's order; one at the watermark may come from another
//     process, whose seq orders it anywhere among equals. Shows, turns,
//     credits and stops never refold: Apply ignores them.
//   - params: p's record is not the one the ledger was folded with, and
//     the journal holds an import: an import that recorded no half-life
//     decays under the folding ones, so a rebuild would differ.
func refoldReason(dir string, cursors map[string]int64, b batch, m meta, p feedback.Params) (string, *batch, error) {
	for _, name := range slices.Sorted(maps.Keys(cursors)) {
		if _, listed := b.ends[name]; !listed { // gone, or not a month file
			return ReasonFileChanged, nil, nil
		}
		changed, err := changedUnder(filepath.Join(dir, name), cursors[name])
		if err != nil {
			return "", nil, err
		}
		if changed {
			return ReasonFileChanged, nil, nil
		}
	}
	if m.watermark != 0 {
		for _, e := range b.events {
			if feedback.Applied(e.Kind) && toNS(e.TS) <= m.watermark {
				return ReasonLateLine, nil, nil
			}
		}
	}
	if m.params != paramsJSON(p) {
		all, err := readAll(dir)
		if err != nil {
			return "", nil, err
		}
		if slices.ContainsFunc(all.events, func(e feedback.Event) bool { return e.Kind == feedback.KindImport }) {
			return ReasonParams, &all, nil
		}
	}
	return "", nil, nil
}

// changedUnder: path is gone, shorter than off, or its byte at off-1 is
// not a newline (every cursor ends a complete line).
func changedUnder(path string, off int64) (bool, error) {
	f, err := fsutil.OpenShared(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if fi.Size() < off {
		return true, nil
	}
	if off == 0 {
		return false, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], off-1); err != nil {
		return false, err
	}
	return last[0] != '\n', nil
}

// build empties the ledger and folds the whole journal into it, all (nil:
// read now with ReadAll): a refold, a rebuild, a first build. The
// imports journaled at or before m.importsCut are left out (the cut is
// kept); the cursors are exactly the read's ends; the generation moves
// on, and a new build (fresh) gets a new build_id. It returns the ledger
// it wrote, with its watermark and journal position as the export gives
// them.
func (w *Writer) build(ctx context.Context, tx *sql.Tx, p feedback.Params, m meta, all *batch, fresh bool) (FoldResult, feedback.Ledger, error) {
	if all == nil {
		a, err := readAll(w.journalDir())
		if err != nil {
			return FoldResult{}, feedback.Ledger{}, err
		}
		all = &a
	}
	kept := dropImports(all.events, m.importsCut)
	l := feedback.Fold(kept, p)
	for _, q := range []string{`DELETE FROM memories`, `DELETE FROM settled_keys`, `DELETE FROM ledger_cursors`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return FoldResult{}, feedback.Ledger{}, err
		}
	}
	upsert, err := tx.PrepareContext(ctx, upsertMemory)
	if err != nil {
		return FoldResult{}, feedback.Ledger{}, err
	}
	defer func() { _ = upsert.Close() }()
	for _, id := range slices.Sorted(maps.Keys(l.Memories)) {
		if _, err := upsert.ExecContext(ctx, rowFromEntry(id, l.Memories[id]).args()...); err != nil {
			return FoldResult{}, feedback.Ledger{}, err
		}
	}
	keys := map[string]bool{}
	for _, e := range kept {
		if e.Kind == "settle" {
			for _, s := range e.Settled {
				keys[s.Key] = true
			}
		}
	}
	if err := insertKeys(ctx, tx, slices.Sorted(maps.Keys(keys))); err != nil {
		return FoldResult{}, feedback.Ledger{}, err
	}
	if err := writeCursors(ctx, tx, all.ends); err != nil {
		return FoldResult{}, feedback.Ledger{}, err
	}
	// The watermark counts the imports left out too: an older binary
	// reading the export must not apply them.
	m.watermark = toNS(latestApplied(all.events))
	m.historyImported = toNS(l.HistoryImported)
	m.params = paramsJSON(p)
	m.generation++
	if fresh || m.buildID == "" {
		m.buildID = newBuildID()
	}
	if err := m.write(ctx, tx, toNS(w.opts.Now())); err != nil {
		return FoldResult{}, feedback.Ledger{}, err
	}
	l.Watermark, l.JournalAt = fromNS(m.watermark), latestPos(all.ends)
	return FoldResult{Lines: len(all.events), Skipped: all.skipped, Generation: m.generation}, l, nil
}

// newBuildID is 16 random hex characters: a build's identity, so that a
// rebuilt file restarting its generation never stamps as the old one.
func newBuildID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return hex.EncodeToString(b)
}

// latestPos is the latest month file of cursors and its cursor (zero
// without one).
func latestPos(cursors map[string]int64) feedback.JournalPos {
	if len(cursors) == 0 {
		return feedback.JournalPos{}
	}
	name := slices.Max(slices.Collect(maps.Keys(cursors)))
	return feedback.JournalPos{File: name, Offset: cursors[name]}
}
