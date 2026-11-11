package ledgerdb

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
)

// lookupLedger is what a prompt hook looks up, under dir: a ledger.db of
// 5000 memories folded (50 of them hidden), with 50 journal lines past its last fold naming
// the 20 hits of one search (its settles, shows and credits read and
// skipped, its hides applied). It returns the options, the hits, the
// folded lines and the tail.
func lookupLedger(tb testing.TB, dir string) (o Options, ids []string, folded, tail []feedback.Event) {
	tb.Helper()
	o = Options{
		Dirs:          paths.Dirs{State: filepath.Join(dir, "state")},
		Getenv:        func(string) string { return "" },
		BinaryVersion: "v-test",
		Now:           func() time.Time { return clock },
	}
	const memories, perSettle = 5000, 10
	id := func(i int) string { return fmt.Sprintf("mem-%05d", i) }
	outcomes := []string{feedback.OutcomeShown, feedback.OutcomeCredit, feedback.OutcomeMiss}
	for i := 0; i < memories; i += perSettle {
		ts := oct(0).Add(time.Duration(i) * time.Minute)
		var s []feedback.Settled
		for k := range perSettle {
			s = append(s, show(fmt.Sprintf("k%d", i+k), id(i+k), ts.Add(-time.Hour), outcomes[(i+k)%len(outcomes)]))
		}
		folded = append(folded, settleOf(ts, s...))
		if i%100 == 0 { // a hide a reader would apply again past no cursor (not a hit's)
			folded = append(folded, hideOf(ts, id(i+7)))
		}
	}
	// appendLines's, for a benchmark too
	journal := func(events []feedback.Event) {
		j := feedback.NewJournal(o.Dirs.FeedbackDir(), feedback.Common{Session: "s1", Binary: "v-test"})
		for _, e := range events {
			if err := j.Append(e); err != nil {
				tb.Fatal(err)
			}
		}
	}
	journal(folded)
	w, err := OpenWriter(context.Background(), o)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := w.Fold(context.Background(), testParams); err != nil {
		tb.Fatal(err)
	}
	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}

	ids = make([]string, 20)
	for i := range ids {
		ids[i] = id(i * (memories / len(ids)))
	}
	for i := range 50 {
		ts := nov(i)
		hit := ids[i%len(ids)]
		switch i % 5 {
		case 0, 1:
			tail = append(tail, settleOf(ts, show(fmt.Sprintf("tail%d", i), hit, ts.Add(-time.Minute), feedback.OutcomeCredit)))
		case 2:
			tail = append(tail, showsOf(ts, hit, id(memories-1-i)))
		case 3:
			tail = append(tail, feedback.Event{Kind: "credit", TS: ts, IDs: []string{hit}, V: "used"})
		default:
			tail = append(tail, hideOf(ts, hit))
		}
	}
	journal(tail)
	return o, ids, folded, tail
}

// TestLedgerLookupReadsTheKeyAndTheTail: what a prompt hook spends on the
// ledger is bounded by its work, not timed. GetAny of one search's 20 hits
// over 5000 memories gives each hit's entry, the tail applied; its rows
// are looked up by the primary key (no scan of the table), and the
// journal is read past the fold only, not the folded lines again.
func TestLedgerLookupReadsTheKeyAndTheTail(t *testing.T) {
	o, ids, folded, tail := lookupLedger(t, t.TempDir())
	var got map[string]feedback.Entry
	var plan []string
	var read []feedback.Event
	if err := Read(o, 20*time.Millisecond, func(r *Reader) error {
		var err error
		if got, _, err = r.GetAny(ids); err != nil {
			return err
		}
		return r.read(func(tx *sql.Tx) error {
			if plan, err = queryPlan(tx, entriesQuery(len(ids)), ids); err != nil {
				return err
			}
			read, err = readTail(tx, r.dir())
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("GetAny gave %d of the %d hits", len(got), len(ids))
	}
	// the folded lines, and the tail's lines a reader applies (its
	// settles wait for the next fold)
	seen := slices.Clone(folded)
	var tailFolds int
	for _, e := range tail {
		if feedback.Folds(e.Kind) {
			seen = append(seen, e)
			tailFolds++
		}
	}
	want := feedback.Fold(seen, testParams)
	for _, id := range ids {
		sameEntry(t, id, got[id], want.Memories[id])
	}
	if got[ids[4]].Hidden == nil {
		t.Fatalf("the tail's hide of %s is not applied: %+v", ids[4], got[ids[4]])
	}

	keyed := slices.ContainsFunc(plan, func(d string) bool {
		return strings.HasPrefix(d, "SEARCH memories USING PRIMARY KEY (memory_id=?)")
	})
	if !keyed || slices.ContainsFunc(plan, func(d string) bool { return strings.HasPrefix(d, "SCAN memories") }) {
		t.Errorf("the hits' rows are not looked up by the primary key: plan %q", plan)
	}
	if tailFolds == 0 || len(read) != tailFolds {
		t.Errorf("the reader read %d journal lines past the fold, want the tail's %d (of %d lines in all)", len(read), tailFolds, len(folded)+len(tail))
	}
}

// queryPlan is the detail lines of q's EXPLAIN QUERY PLAN, ids its
// arguments.
func queryPlan(tx *sql.Tx, q string, ids []string) ([]string, error) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := tx.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			return nil, err
		}
		plan = append(plan, detail)
	}
	return plan, rows.Err()
}

// BenchmarkLedgerLookup times TestLedgerLookupReadsTheKeyAndTheTail's
// lookup: one Read (the open, one read transaction, the close) and GetAny
// of the 20 hits (go test -run '^$' -bench LedgerLookup
// ./internal/feedback/ledgerdb/). The product's target is 2 ms at p95
// (R21); no test holds it to that, a loaded machine would fail it.
func BenchmarkLedgerLookup(b *testing.B) {
	o, ids, _, _ := lookupLedger(b, b.TempDir())
	for b.Loop() {
		err := Read(o, 20*time.Millisecond, func(r *Reader) error {
			m, _, err := r.GetAny(ids)
			if err == nil && len(m) != len(ids) {
				err = fmt.Errorf("%d of the %d hits", len(m), len(ids))
			}
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
