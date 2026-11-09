package ledgerdb

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/testtool"
)

// lookupLimit is the ledger lookup budget (R21: p95 ≤ 2 ms), ×3 on CI.
func lookupLimit() time.Duration {
	if os.Getenv("CI") != "" {
		return 6 * time.Millisecond
	}
	return 2 * time.Millisecond
}

// TestLedgerLookupBudget times what a prompt hook spends on the ledger: one
// Read (the open, one read transaction, the close) and GetAny of one
// search's 20 hits, over a ledger.db of 5000 memories with 50 journal
// lines past its last fold. The p95 of 500 rounds, best of 3 runs, is
// within lookupLimit.
func TestLedgerLookupBudget(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("time budgets are measured without -race and -short")
	}
	o := Options{
		Dirs:          paths.Dirs{State: filepath.Join(testtool.BenchTempDir(t), "state")},
		Getenv:        func(string) string { return "" },
		BinaryVersion: "v-test",
		Now:           func() time.Time { return clock },
	}
	const memories, perSettle = 5000, 10
	id := func(i int) string { return fmt.Sprintf("mem-%05d", i) }
	outcomes := []string{feedback.OutcomeShown, feedback.OutcomeCredit, feedback.OutcomeMiss}
	var events []feedback.Event
	for i := 0; i < memories; i += perSettle {
		ts := oct(0).Add(time.Duration(i) * time.Minute)
		var s []feedback.Settled
		for k := range perSettle {
			s = append(s, show(fmt.Sprintf("k%d", i+k), id(i+k), ts.Add(-time.Hour), outcomes[(i+k)%len(outcomes)]))
		}
		events = append(events, settleOf(ts, s...))
	}
	appendLines(t, o, events...)
	w := mustOpenWriter(t, o)
	mustFold(t, w, testParams)
	mustCloseWriter(t, w)

	// the hits: 20 memories spread over the ledger; the tail names them
	// (its settles, shows and credits are read and skipped, its hides
	// applied)
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = id(i * (memories / len(ids)))
	}
	var tail []feedback.Event
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
	appendLines(t, o, tail...)

	// what the rounds read: every hit, the tail applied
	var got map[string]feedback.Entry
	if err := Read(o, 20*time.Millisecond, func(r *Reader) error {
		var err error
		got, _, err = r.GetAny(ids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("GetAny gave %d of the %d hits", len(got), len(ids))
	}
	// the folded lines, and the tail's lines a reader applies (its
	// settles wait for the next fold)
	seen := slices.Clone(events)
	for _, e := range tail {
		if feedback.Folds(e.Kind) {
			seen = append(seen, e)
		}
	}
	want := feedback.Fold(seen, testParams)
	for _, id := range ids {
		sameEntry(t, id, got[id], want.Memories[id])
	}
	if got[ids[4]].Hidden == nil {
		t.Fatalf("the tail's hide of %s is not applied: %+v", ids[4], got[ids[4]])
	}

	const rounds, warmUp = 500, 20
	best := time.Duration(0)
	for run := range 3 {
		took := make([]time.Duration, 0, rounds)
		for i := range rounds + warmUp {
			start := time.Now()
			err := Read(o, 20*time.Millisecond, func(r *Reader) error {
				m, _, err := r.GetAny(ids)
				if err == nil && len(m) != len(ids) {
					err = fmt.Errorf("%d of the %d hits", len(m), len(ids))
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if i >= warmUp {
				took = append(took, time.Since(start))
			}
		}
		slices.Sort(took)
		p95 := took[rounds*95/100]
		t.Logf("run %d: p50 %v, p95 %v, max %v", run+1, took[rounds/2], p95, took[rounds-1])
		if run == 0 || p95 < best {
			best = p95
		}
	}
	t.Logf("ledger lookup over %d memories and a %d-line tail, %d ids: p95 %v, best of 3 (limit %v)", memories, len(tail), len(ids), best, lookupLimit())
	if best > lookupLimit() {
		t.Errorf("ledger lookup p95 %v, best of 3, over the %v budget", best, lookupLimit())
	}
}
