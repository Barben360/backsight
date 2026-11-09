package ledgerdb

import (
	"bytes"
	"testing"

	"github.com/Barben360/backsight/internal/feedback"
)

func TestReaderAppliesTheJournalTail(t *testing.T) {
	o := tempOpts(t)
	w := mustOpenWriter(t, o)
	appendLines(t, o,
		settleOf(nov(2), show("k1", "m1", nov(1), feedback.OutcomeShown), show("k2", "m2", nov(1), feedback.OutcomeShown)),
		hideOf(nov(3), "m2"))
	mustFold(t, w, testParams)
	mustCloseWriter(t, w)
	before, gen := readBytes(t, mainPath(o)), metaOf(t, o)["generation"]

	appendLines(t, o,
		hideOf(nov(5), "m1"), unhideOf(nov(6), "m2"),
		settleOf(nov(7), show("k3", "m1", nov(4), feedback.OutcomeCredit)), // a settle waits for its fold
		hideOf(nov(8), "m4")) // a memory without a row
	r := mustOpenReader(t, o)
	check := func(what string, got map[string]feedback.Entry) {
		t.Helper()
		if e, ok := got["m1"]; !ok || e.Hidden == nil || e.Shown != 1 || e.Credits != 0 {
			t.Errorf("%s: m1 %+v (%v), want hidden by the tail, its settle not applied", what, e, ok)
		}
		if e, ok := got["m2"]; ok && e.Hidden != nil {
			t.Errorf("%s: m2 %+v, want unhidden by the tail", what, e)
		}
	}
	got, err := r.Get([]string{"m1", "m2"})
	if err != nil {
		t.Fatal(err)
	}
	check("Get", got)
	if e, ok := got["m2"]; !ok || !e.UnhiddenAt.Equal(nov(6)) || e.Shown != 1 {
		t.Errorf("Get: m2 %+v (%v), want its row, unhidden at %v", e, ok, nov(6))
	}
	if _, ok := got["m4"]; ok {
		t.Error("Get returned an id not asked for")
	}
	hidden, err := r.Hidden()
	if err != nil {
		t.Fatal(err)
	}
	check("Hidden", hidden)
	if _, ok := hidden["m2"]; ok {
		t.Error("Hidden lists the unhidden m2")
	}
	if _, ok := hidden["m4"]; !ok || len(hidden) != 2 {
		t.Errorf("Hidden: %v, want m1 and m4", hidden)
	}
	all, err := r.All()
	if err != nil {
		t.Fatal(err)
	}
	check("All", all.Memories)
	if all.Memories["m4"].Hidden == nil || len(all.Memories) != 3 {
		t.Errorf("All: %+v, want m1, m2 and the hidden m4", all.Memories)
	}
	if !bytes.Equal(readBytes(t, mainPath(o)), before) || metaOf(t, o)["generation"] != gen {
		t.Error("the reader wrote ledger.db")
	}
}

// TestAnyCountsTheTail: a ledger.db with no row and a hide in the tail
// has an entry, as All says.
func TestAnyCountsTheTail(t *testing.T) {
	o := tempOpts(t)
	mustCloseWriter(t, mustOpenWriter(t, o))
	r := mustOpenReader(t, o)
	if has, err := r.Any(); err != nil || has {
		t.Fatalf("Any of an empty ledger: %v, %v", has, err)
	}
	appendLines(t, o, hideOf(nov(1), "m1"))
	if has, err := r.Any(); err != nil || !has {
		t.Errorf("Any with a hide in the tail: %v, %v", has, err)
	}
	appendLines(t, o, feedback.Event{Kind: "forget", TS: nov(2), IDs: []string{"m1"}})
	if has, err := r.Any(); err != nil || has {
		t.Errorf("Any once the tail forgets it: %v, %v", has, err)
	}
}
