package ledgerdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// ErrExport: Settle settled and folded, and only ledger.json could not
// be written. The SettleResult returned with it stands; the next Settle
// exports again.
var ErrExport = errors.New("ledger.json was not exported")

// afterDecide, when set (tests), runs once Settle decided, before it
// journals the settle.
var afterDecide func()

// Settle is the tick's settle:
//  1. it opens the writer (an uncommitted update: nothing done, no
//     error);
//  2. reads the clock once, then decides the shows of the last
//     feedback.SettleWindow of the journal that can settle then
//     (feedback.Decide);
//  3. journals them as one settle event at that time carrying p, when
//     a show settled or a credit came late;
//  4. folds the journal into ledger.db (Writer.Fold), then exports
//     ledger.json (Writer.Export) when its last export is older than the
//     fold's generation, a fold of nothing included (a failed export is
//     retried), or when ledger.json is gone or does not parse.
//
// It counts the shows decided by outcome, forgotten ones left out, and
// the late credits; Rebuilt is ReasonSetAside when the fold set a corrupt
// ledger.db aside and built a fresh one from the journal. It takes no
// file lock: two settles at once may both journal the same shows, and
// the fold counts each key once. A failed
// open, read, append or fold is the error, with a zero result; a failed
// export is an error wrapping ErrExport, with the result.
func Settle(ctx context.Context, o Options, p feedback.Params, clock func() time.Time) (feedback.SettleResult, error) {
	var res feedback.SettleResult
	o = o.withDefaults()
	w, err := OpenWriter(ctx, o)
	if errors.Is(err, ErrUpdateInProgress) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	defer func() { _ = w.Close() }()
	now := clock()
	dir := w.journalDir()
	events, _, err := feedback.Read(dir, now.Add(-feedback.SettleWindow))
	if err != nil {
		return res, err
	}
	settled, late := feedback.Decide(events, p, now)
	if afterDecide != nil {
		afterDecide()
	}
	if len(settled) > 0 || late > 0 {
		// Even with no show, the event marks where these late credits were
		// counted: the next settle counts only later ones.
		e := feedback.Event{Kind: "settle", TS: now.UTC(), Settled: settled, Params: p.Record()}
		if err := feedback.NewJournal(dir, feedback.Common{Binary: o.BinaryVersion}).Append(e); err != nil {
			return res, err
		}
	}
	fr, err := w.Fold(ctx, p)
	if err != nil {
		return res, err
	}
	if fr.Reason == ReasonSetAside {
		res.Rebuilt = ReasonSetAside
	}
	res.Late = late
	for _, s := range settled {
		if s.Outcome == feedback.OutcomeForgotten {
			continue
		}
		res.Shows++
		switch s.Outcome {
		case feedback.OutcomeCredit:
			res.Credits++
		case feedback.OutcomeMiss:
			res.Misses++
		case feedback.OutcomeUnknown:
			res.Unknown++
		}
	}
	if err := w.Export(); err != nil {
		return res, fmt.Errorf("%w: %w", ErrExport, err)
	}
	return res, nil
}
