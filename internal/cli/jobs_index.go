package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/paths"
)

// tickIndexBudget bounds the tick's catch-up of the metrics index: a
// longer one stops between batches, and the next tick goes on.
var tickIndexBudget = index.TickBudget

// indexStepTimeout bounds the whole index step of a job (open, daily
// check, catch-up, checkpoint, pruning): a lock or a disk that hangs
// never holds the job.
const indexStepTimeout = 60 * time.Second

// tickIndex catches the metrics index up within tickIndexBudget and folds
// its write-ahead log into the file. Best effort: what it did, never an
// error of the tick (a failure is a class, never a path); no progress
// output.
func tickIndex(ctx context.Context, d paths.Dirs, home string) string {
	ctx, cancel := context.WithTimeout(ctx, indexStepTimeout)
	defer cancel()
	ix, err := index.Open(ctx, indexOptions(d, home, nil))
	if err != nil {
		return indexStepError(err)
	}
	defer func() { _ = ix.Close() }()
	side := ""
	if ix.Side() {
		side = " (" + filepath.Base(ix.Path()) + ": a newer backsight's index is kept)"
	}
	fr, err := ix.CatchUp(ctx, tickIndexBudget)
	if err != nil {
		return indexStepError(err) + side
	}
	// A reader that keeps the checkpoint from finishing is no failure:
	// the next tick finishes it.
	_ = ix.Checkpoint(ctx)
	if fr.Partial {
		return fmt.Sprintf("partial: %d line(s), the next tick goes on", fr.Lines) + side
	}
	return fmt.Sprintf("caught up: %d line(s)", fr.Lines) + side
}

// maintainIndex prunes the metrics index (index.Maintain) by the cli's
// clock; best effort, as tickIndex.
func maintainIndex(ctx context.Context, d paths.Dirs, home string) string {
	ctx, cancel := context.WithTimeout(ctx, indexStepTimeout)
	defer cancel()
	o := indexOptions(d, home, nil)
	o.Now = now
	r, err := index.Maintain(ctx, o)
	return maintainIndexText(r, err)
}

// maintainIndexText is maintainIndex's line: what was pruned (or why
// not), then the files removed and those kept.
func maintainIndexText(r index.MaintainResult, err error) string {
	out := fmt.Sprintf("pruned %d row(s) and %d ingest error(s)", r.RowsPruned, r.ErrorsPruned)
	if err != nil {
		out = indexStepError(err)
	}
	if removed := append(append([]string{}, r.SideRemoved...), r.TempRemoved...); len(removed) > 0 {
		out += ", removed " + strings.Join(removed, ", ")
	}
	if len(r.SideKept) > 0 {
		out += ", kept " + strings.Join(r.SideKept, ", ") + " (in use)"
	}
	if r.TempSkipped {
		out += ", rebuild temp files left: a rebuild is running"
	}
	return out
}

// indexStepError is an index error as a job reports it: never its text,
// which may hold a path.
func indexStepError(err error) string {
	switch {
	case errors.Is(err, index.ErrUpdateInProgress):
		return "skipped: an update is being applied"
	case errors.Is(err, index.ErrSchemaChanged):
		return "skipped: backsight was updated meanwhile"
	case errors.Is(err, context.DeadlineExceeded):
		return "failed: timeout"
	}
	return "failed: " + index.Class(err)
}
