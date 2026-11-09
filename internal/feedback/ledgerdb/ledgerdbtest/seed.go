// Package ledgerdbtest builds a ledger database for tests.
package ledgerdbtest

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/feedback/ledgerdb"
	"github.com/Barben360/backsight/internal/paths"
)

// Seed builds d's ledger.db holding l's memories, the way a real one comes
// to be: it journals the events whose fold is l (settles, an import,
// retires, a hide, an unhide), then folds them into the database. It is
// never a database-only state: a rebuild from the journal gives it back.
// Seed calls t.Fatal on a ledger no journal produces exactly: a credit or
// miss sum that is not a whole number (the sums are counts without decay),
// credits dated before the last credit, or any other entry the journal it
// built does not fold to. A Watermark or HistoryImported that is zero is
// not asserted; the others are.
func Seed(t testing.TB, d paths.Dirs, l feedback.Ledger) {
	t.Helper()
	j := feedback.NewJournal(d.FeedbackDir(), feedback.Common{})
	n := 0
	key := func() string { n++; return fmt.Sprintf("seed-%d", n) }
	add := func(e feedback.Event) {
		t.Helper()
		if err := j.Append(e); err != nil {
			t.Fatalf("ledgerdbtest.Seed: journal: %v", err)
		}
	}
	// No decay, pinned in every settle: a later fold under any config
	// gives the same sums.
	rec := feedback.Params{}.Record()
	ids := make([]string, 0, len(l.Memories))
	for id := range l.Memories {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		for _, e := range entryEvents(t, id, l.Memories[id], key, rec) {
			add(e)
		}
	}
	if !l.HistoryImported.IsZero() {
		add(feedback.Event{Kind: feedback.KindImport, TS: l.HistoryImported, Outcome: feedback.ImportComplete, Params: rec})
	}
	if !l.Watermark.IsZero() {
		// forgetting a memory the ledger never held changes nothing but
		// the watermark, the latest applied line's time
		add(feedback.Event{Kind: "forget", TS: l.Watermark, IDs: []string{"seed-watermark"}})
	}

	o := ledgerdb.Options{Dirs: d, Getenv: func(string) string { return "" }}
	w, err := ledgerdb.OpenWriter(context.Background(), o)
	if err != nil {
		t.Fatalf("ledgerdbtest.Seed: OpenWriter: %v", err)
	}
	if _, err := w.Fold(context.Background(), feedback.Params{}); err != nil {
		_ = w.Close()
		t.Fatalf("ledgerdbtest.Seed: Fold: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("ledgerdbtest.Seed: Close: %v", err)
	}
	r, err := ledgerdb.OpenReader(o, 5*time.Second)
	if err != nil {
		t.Fatalf("ledgerdbtest.Seed: OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := r.All()
	if err != nil {
		t.Fatalf("ledgerdbtest.Seed: All: %v", err)
	}
	check(t, l, got)
}

// entryEvents are the journal lines that fold to e.
func entryEvents(t testing.TB, id string, e feedback.Entry, key func() string, rec map[string]float64) []feedback.Event {
	t.Helper()
	if e.Credits != math.Trunc(e.Credits) || e.Credits < 0 {
		t.Fatalf("ledgerdbtest.Seed: %s: Credits %v is no whole count a journal produces", id, e.Credits)
	}
	if e.Misses != math.Trunc(e.Misses) || e.Misses < 0 {
		t.Fatalf("ledgerdbtest.Seed: %s: Misses %v is no whole count a journal produces", id, e.Misses)
	}
	if e.Credits > 0 && !e.CreditsAt.IsZero() && e.CreditsAt.Before(e.LastCredited) {
		t.Fatalf("ledgerdbtest.Seed: %s: CreditsAt %v is before LastCredited %v", id, e.CreditsAt, e.LastCredited)
	}
	credits, misses := int(e.Credits), int(e.Misses)
	creditAt := e.CreditsAt
	if creditAt.IsZero() {
		creditAt = e.LastCredited
	}
	if e.Imported.Shown < 0 || e.Imported.Credits != math.Trunc(e.Imported.Credits) || int(e.Imported.Credits) > e.Imported.Shown ||
		int(e.Imported.Credits) > credits || e.Imported.Shown > e.Shown {
		t.Fatalf("ledgerdbtest.Seed: %s: Imported %+v does not fit the counts", id, e.Imported)
	}
	impCredits := int(e.Imported.Credits)
	shown := e.Shown - credits - misses // shows that were neither
	impShown := e.Imported.Shown - impCredits
	if shown < impShown {
		t.Fatalf("ledgerdbtest.Seed: %s: Shown %d is below its credits, misses and imports", id, e.Shown)
	}

	var out []feedback.Event
	base := latest(e.LastShown, e.LastCredited, e.CreditsAt, e.MissesAt, e.UnhiddenAt)
	if e.Hidden != nil {
		base = latest(base, e.Hidden.Since)
	}
	if base.IsZero() {
		base = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if !e.UnhiddenAt.IsZero() {
		out = append(out, feedback.Event{Kind: feedback.KindUnhide, TS: e.UnhiddenAt, IDs: []string{id}})
	}
	show := func(at time.Time, outcome string, count int) []feedback.Settled {
		var s []feedback.Settled
		for range count {
			s = append(s, feedback.Settled{Key: key(), ID: id, At: at, Outcome: outcome})
		}
		return s
	}
	var imp, set []feedback.Settled
	imp = append(imp, show(creditAt, feedback.OutcomeCredit, impCredits)...)
	imp = append(imp, show(e.LastShown, feedback.OutcomeShown, impShown)...)
	set = append(set, show(creditAt, feedback.OutcomeCredit, credits-impCredits)...)
	set = append(set, show(e.MissesAt, feedback.OutcomeMiss, misses)...)
	set = append(set, show(e.LastShown, feedback.OutcomeShown, shown-impShown)...)
	// a line's own time is its latest show's, or the entry's when no
	// show has one (a show without a time is kept as the entry has it)
	at := func(s []feedback.Settled) time.Time {
		if ts := latestAt(s); !ts.IsZero() {
			return ts
		}
		return base
	}
	if len(imp) > 0 {
		out = append(out, feedback.Event{Kind: feedback.KindImport, TS: at(imp), Params: rec, Settled: imp})
	}
	if len(set) > 0 {
		out = append(out, feedback.Event{Kind: "settle", TS: at(set), Params: rec, Settled: set})
	}
	for range e.Wrong {
		ts := base
		if !e.UnhiddenAt.IsZero() {
			ts = e.UnhiddenAt
		}
		out = append(out, feedback.Event{Kind: feedback.KindRetire, TS: ts, IDs: []string{id}})
	}
	if e.Hidden != nil {
		out = append(out, feedback.Event{Kind: feedback.KindHide, TS: e.Hidden.Since, IDs: []string{id},
			Verdict: e.Hidden.Verdict, Note: e.Hidden.Note})
	}
	return out
}

func latest(ts ...time.Time) time.Time {
	var m time.Time
	for _, t := range ts {
		if t.After(m) {
			m = t
		}
	}
	return m
}

func latestAt(s []feedback.Settled) time.Time {
	var m time.Time
	for _, x := range s {
		m = latest(m, x.At)
	}
	return m
}

// check fails t when got is not want: every memory, and the watermark and
// the import time when want sets them.
func check(t testing.TB, want, got feedback.Ledger) {
	t.Helper()
	var bad []string
	for id, w := range want.Memories {
		g, ok := got.Memories[id]
		if !ok {
			bad = append(bad, id+": no journal gives this entry")
			continue
		}
		if d := entryDiff(w, g); d != "" {
			bad = append(bad, id+": "+d)
		}
	}
	for id := range got.Memories {
		if _, ok := want.Memories[id]; !ok {
			bad = append(bad, id+": the journal holds it, the ledger asked for does not")
		}
	}
	if !want.Watermark.IsZero() && !want.Watermark.Equal(got.Watermark) {
		bad = append(bad, fmt.Sprintf("watermark %v, wanted %v", got.Watermark, want.Watermark))
	}
	if !want.HistoryImported.IsZero() && !want.HistoryImported.Equal(got.HistoryImported) {
		bad = append(bad, fmt.Sprintf("history imported %v, wanted %v", got.HistoryImported, want.HistoryImported))
	}
	if len(bad) > 0 {
		slices.Sort(bad)
		t.Fatalf("ledgerdbtest.Seed: no journal produces this ledger exactly:\n  %s", strings.Join(bad, "\n  "))
	}
}

func entryDiff(w, g feedback.Entry) string {
	var d []string
	chk := func(name string, ok bool, w, g any) {
		if !ok {
			d = append(d, fmt.Sprintf("%s %v, wanted %v", name, g, w))
		}
	}
	chk("Shown", w.Shown == g.Shown, w.Shown, g.Shown)
	chk("Credits", w.Credits == g.Credits, w.Credits, g.Credits)
	chk("CreditsAt", w.CreditsAt.Equal(g.CreditsAt), w.CreditsAt, g.CreditsAt)
	chk("Misses", w.Misses == g.Misses, w.Misses, g.Misses)
	chk("MissesAt", w.MissesAt.Equal(g.MissesAt), w.MissesAt, g.MissesAt)
	chk("LastShown", w.LastShown.Equal(g.LastShown), w.LastShown, g.LastShown)
	chk("LastCredited", w.LastCredited.Equal(g.LastCredited), w.LastCredited, g.LastCredited)
	chk("Wrong", w.Wrong == g.Wrong, w.Wrong, g.Wrong)
	chk("UnhiddenAt", w.UnhiddenAt.Equal(g.UnhiddenAt), w.UnhiddenAt, g.UnhiddenAt)
	chk("Imported", w.Imported == g.Imported, w.Imported, g.Imported)
	switch {
	case (w.Hidden == nil) != (g.Hidden == nil):
		d = append(d, fmt.Sprintf("Hidden %v, wanted %v", g.Hidden, w.Hidden))
	case w.Hidden != nil && (!w.Hidden.Since.Equal(g.Hidden.Since) || w.Hidden.Verdict != g.Hidden.Verdict || w.Hidden.Note != g.Hidden.Note):
		d = append(d, fmt.Sprintf("Hidden %+v, wanted %+v", *g.Hidden, *w.Hidden))
	}
	return strings.Join(d, "; ")
}
