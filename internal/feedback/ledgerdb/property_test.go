package ledgerdb

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
)

// TestRebuildEqualsIncrementalProperty: over a random journal of every
// kind, back-dated lines and partial last lines included, folded in
// random batches, ledger.db always equals a rebuild of the journal,
// floats bit for bit. Seeds 1-20 back-date 5 % of their lines; seeds
// 21-30 none, so that their whole history folds incrementally (no
// refold at all). Under the race detector, seeds 1-5 and 21-23 only.
// The seeds run in parallel: they set no package variable and share no
// file.
func TestRebuildEqualsIncrementalProperty(t *testing.T) {
	type run struct {
		seed      uint64
		backDated int // percent of the lines
	}
	var runs []run
	for seed := uint64(1); seed <= 30; seed++ {
		if raceEnabled && (seed > 5 && seed <= 20 || seed > 23) {
			continue
		}
		pct := 5
		if seed > 20 {
			pct = 0
		}
		runs = append(runs, run{seed, pct})
	}
	start := time.Now()
	var mu sync.Mutex
	folds, refolds := 0, 0 // over the back-dating seeds
	t.Run("seeds", func(t *testing.T) {
		for _, r := range runs {
			t.Run(fmt.Sprint(r.seed), func(t *testing.T) {
				t.Parallel()
				st := propertyRun(t, r.seed, r.backDated)
				if r.backDated == 0 && st.refolds != 0 {
					t.Errorf("seed %d, nothing back-dated: %d refolds, want none", r.seed, st.refolds)
				}
				if r.backDated > 0 {
					mu.Lock()
					folds, refolds = folds+st.folds, refolds+st.refolds
					mu.Unlock()
				}
			})
		}
	})
	if incremental := folds - refolds; incremental*4 < folds {
		t.Errorf("%d of %d folds incremental over the back-dating seeds, want at least 25 %%", incremental, folds)
	}
	t.Logf("%d seeds in %v; back-dating seeds: %d folds, %d refolds", len(runs), time.Since(start), folds, refolds) //nolint:forbidigo // logged, never asserted
}

type propertyStats struct{ folds, refolds, partials int }

func propertyRun(t *testing.T, seed uint64, backDated int) propertyStats {
	o := tempOpts(t)
	dir := feedbackDir(o)
	w := mustOpenWriter(t, o)
	defer mustCloseWriter(t, w)
	mustFold(t, w, testParams) // the folding params recorded: no params refold
	r := rand.New(rand.NewPCG(seed, seed*0x9e3779b97f4a7c15))
	g := &generator{r: r}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	const n = 2000
	span := 90 * day // September to November: 3 month files
	folds, refolds, partials := 0, 0, 0
	check := func(what string) {
		t.Helper()
		res := mustFold(t, w, testParams)
		folds++
		if res.Refolded {
			refolds++
		}
		want := oracle(t, dir, testParams)
		sameLedger(t, what+": folded", folded(t, o), want)
		sameLedger(t, what+": All", readerAll(t, o), want)
		if c, ends := cursorsOf(t, o), journalEnds(t, o); !maps.Equal(c, ends) {
			t.Fatalf("%s: cursors %v, want %v", what, c, ends)
		}
	}
	next := 1 + r.IntN(150)
	for i := range n {
		ts := base.Add(span / n * time.Duration(i)).Add(time.Duration(r.IntN(3600)) * time.Second)
		if r.IntN(100) < backDated { // back-dated, maybe into an earlier month
			ts = ts.Add(-time.Duration(r.IntN(40*24)) * time.Hour)
			if ts.Before(base) {
				ts = base.Add(time.Duration(r.IntN(3600)) * time.Second)
			}
		}
		e := g.event(ts)
		if r.IntN(100) == 0 { // a partial last line, completed after a fold
			partials++
			e.Seq = int64(1_000_000 + i)
			line := encodeLine(t, e)
			file := filepath.Join(dir, feedback.MonthFile(ts))
			cut := 1 + r.IntN(len(line)-1)
			appendBytes(t, file, line[:cut])
			check(fmt.Sprintf("line %d partial", i))
			appendBytes(t, file, append(line[cut:], '\n'))
		} else {
			appendLines(t, o, e)
		}
		if next--; next == 0 || i == n-1 {
			check(fmt.Sprintf("line %d", i))
			next = 1 + r.IntN(150)
		}
		if t.Failed() {
			t.Fatalf("seed %d diverged at line %d", seed, i)
		}
	}
	files, err := feedback.MonthFiles(dir)
	if err != nil || len(files) != 3 {
		t.Fatalf("month files %v (%v), want 3", files, err)
	}
	t.Logf("seed %d: %d folds, %d refolds, %d partial lines, %d memories", seed, folds, refolds, partials, len(folded(t, o).Memories))
	return propertyStats{folds, refolds, partials}
}

// generator draws journal lines of every kind the ledger reads, over a
// few memory ids, settle keys sometimes repeated.
type generator struct {
	r    *rand.Rand
	keys []string
	n    int
}

var propertyIDs = func() []string {
	ids := make([]string, 24)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%02d", i)
	}
	return ids
}()

func (g *generator) id() string { return propertyIDs[g.r.IntN(len(propertyIDs))] }

func (g *generator) ids() []string {
	out := []string{g.id()}
	if g.r.IntN(4) == 0 {
		out = append(out, g.id())
	}
	return out
}

func (g *generator) key() string {
	if len(g.keys) > 0 && g.r.IntN(100) < 15 {
		return g.keys[g.r.IntN(len(g.keys))]
	}
	g.n++
	k := fmt.Sprintf("k%d", g.n)
	g.keys = append(g.keys, k)
	return k
}

// params is what a settle or an import records: testParams mostly,
// others now and then, or none (the folding params then apply).
func (g *generator) params() map[string]float64 {
	switch v := g.r.IntN(10); {
	case v < 7:
		return testParams.Record()
	case v < 8:
		p := testParams
		p.CreditHalfLife, p.MissHalfLife = 15*day, 7*day
		return p.Record()
	}
	return nil
}

func (g *generator) event(ts time.Time) feedback.Event {
	before := func(max time.Duration) time.Time { return ts.Add(-time.Duration(g.r.Int64N(int64(max)))) }
	switch v := g.r.IntN(100); {
	case v < 22:
		return showsOf(ts, g.ids()...)
	case v < 30:
		return feedback.Event{Kind: "credit", TS: ts, IDs: g.ids(), V: "used"}
	case v < 35:
		return feedback.Event{Kind: "turn", TS: ts, Turn: fmt.Sprint(g.r.IntN(100))}
	case v < 62:
		outcomes := []string{feedback.OutcomeCredit, feedback.OutcomeMiss, feedback.OutcomeShown, feedback.OutcomeUnknown, feedback.OutcomeForgotten}
		e := feedback.Event{Kind: "settle", TS: ts, Params: g.params()}
		for range 1 + g.r.IntN(4) {
			s := show(g.key(), g.id(), before(10*day), outcomes[g.r.IntN(len(outcomes))])
			if g.r.IntN(10) == 0 {
				s.Path = feedback.PathStore
			}
			e.Settled = append(e.Settled, s)
		}
		return e
	case v < 66:
		return feedback.Event{Kind: "forget", TS: ts, IDs: g.ids()}
	case v < 72:
		return retireOf(ts, g.ids()...)
	case v < 82:
		e := hideOf(ts, g.ids()...)
		e.Note = fmt.Sprintf("doc-%d", g.r.IntN(5))
		return e
	case v < 90:
		return unhideOf(ts, g.ids()...)
	case v < 92:
		return feedback.Event{Kind: feedback.KindImport, TS: ts, Outcome: feedback.ImportComplete}
	}
	e := feedback.Event{Kind: feedback.KindImport, TS: ts, Source: "transcripts", Params: g.params()}
	for range 1 + g.r.IntN(3) {
		outcome := feedback.OutcomeShown
		if g.r.IntN(2) == 0 {
			outcome = feedback.OutcomeCredit
		}
		g.n++
		e.Settled = append(e.Settled, show(fmt.Sprintf("imp%d", g.n), g.id(), before(200*day), outcome))
	}
	return e
}
