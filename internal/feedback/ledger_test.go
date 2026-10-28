package feedback

import (
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"
)

// assertSameEntries: a and b hold the same memories, with the same counts
// and times, and decayed sums equal within tol.
func assertSameEntries(t *testing.T, a, b Ledger, tol float64) {
	t.Helper()
	if len(a.Memories) != len(b.Memories) {
		t.Errorf("%d memories vs %d", len(a.Memories), len(b.Memories))
	}
	for id, x := range a.Memories {
		y, ok := b.Memories[id]
		switch {
		case !ok:
			t.Errorf("%s missing", id)
		case x.Shown != y.Shown || !x.CreditsAt.Equal(y.CreditsAt) || !x.MissesAt.Equal(y.MissesAt) ||
			!x.LastShown.Equal(y.LastShown) || !x.LastCredited.Equal(y.LastCredited):
			t.Errorf("%s: %+v vs %+v", id, x, y)
		case math.Abs(x.Credits-y.Credits) > tol || math.Abs(x.Misses-y.Misses) > tol:
			t.Errorf("%s: credits %v vs %v, misses %v vs %v", id, x.Credits, y.Credits, x.Misses, y.Misses)
		}
	}
}

// randomJournal is n random shows over days days, with credits, stops,
// subagents and a few forgets, in time order.
func randomJournal(r *rand.Rand, n, days int) []Event {
	paths := []string{"prompt", "search", "gate", "briefing", "read", "failure"}
	var events []Event
	m, shows := 0, 0
	for s := 0; shows < n; s++ {
		m += r.IntN(days*24*60*2/(n/10)) + 1 // about n/10 sessions over days
		sess := fmt.Sprintf("s%d", s)
		events = append(events, evShows(sess, "", "", "briefing", m, Item{ID: fmt.Sprintf("m%d", r.IntN(30)), Rank: 1}))
		shows++
		for turn := range r.IntN(4) + 1 {
			id := fmt.Sprintf("%s-p%d", sess, turn)
			m += r.IntN(20) + 1
			events = append(events, evTurn(sess, id, m))
			var shown []string
			for range r.IntN(3) + 1 {
				m++
				agent := ""
				if r.IntN(5) == 0 {
					agent = "sub"
				}
				var items []Item
				first := r.IntN(30)
				for k := range r.IntN(3) + 1 {
					mem := fmt.Sprintf("m%d", (first+k)%30) // distinct ids within one list
					items = append(items, Item{ID: mem, Rank: k + 1})
					shown = append(shown, mem)
				}
				events = append(events, evShows(sess, id, agent, paths[r.IntN(len(paths))], m, items...))
				shows += len(items)
			}
			m++
			switch r.IntN(4) {
			case 0: // undeclared or interrupted
			case 1:
				events = append(events, evCredit(sess, id, "", "none", m))
			default:
				events = append(events, evCredit(sess, id, "", "used", m, shown[r.IntN(len(shown))]))
			}
			if r.IntN(3) > 0 {
				events = append(events, evStop(sess, id, "", "declared", m))
			}
			if r.IntN(10) == 0 { // a credit a few turns later, or after its show settled
				events = append(events, evCredit(sess, id, "", "used", m+r.IntN(300), shown[0]))
			}
		}
		if r.IntN(8) == 0 {
			events = append(events, Event{Kind: "forget", TS: at(m), IDs: []string{fmt.Sprintf("m%d", r.IntN(30))}})
		}
	}
	return events
}

func TestRebuildEqualsTheLiveLedger(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	events := randomJournal(rand.New(rand.NewPCG(7, 74)), 500, 200)
	end := t0
	for _, e := range events {
		if e.TS.After(end) {
			end = e.TS
		}
	}
	writeJournal(t, dir, events...)
	total := SettleResult{}
	for round := 1; round <= 7; round++ {
		now := t0.Add(end.Sub(t0) * time.Duration(round) / 7)
		if round == 7 {
			now = end.Add(3 * time.Hour)
		}
		res, err := Settle(dir, testParams, now)
		if err != nil {
			t.Fatal(err)
		}
		total.Shows, total.Credits, total.Misses = total.Shows+res.Shows, total.Credits+res.Credits, total.Misses+res.Misses
	}
	live, err := LoadLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	if total.Shows < 500 || total.Credits == 0 || total.Misses == 0 || len(live.Memories) == 0 {
		t.Fatalf("a weak sample: %+v, %d memories", total, len(live.Memories))
	}
	rebuilt, err := Rebuild(dir, testParams, false)
	if err != nil {
		t.Fatal(err)
	}
	assertSameEntries(t, live, rebuilt, 1e-9)
	if !rebuilt.Watermark.Equal(end.Add(3 * time.Hour)) {
		t.Errorf("rebuilt watermark %v, want the last settle's", rebuilt.Watermark)
	}
	// Without write, the ledger file is left as it was.
	if again, _ := LoadLedger(dir); !again.Watermark.Equal(live.Watermark) {
		t.Errorf("Rebuild without write changed the ledger")
	}
}

func TestDecayIsOrderIndependent(t *testing.T) {
	h := 90 * 24 * time.Hour
	times := []time.Time{at(0), at(60 * 24 * 10), at(60 * 24 * 45)}
	var want float64
	var wantRef time.Time
	for i, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		var v float64
		var ref time.Time
		for _, k := range order {
			v, ref = add(v, ref, times[k], h)
		}
		if i == 0 {
			want, wantRef = v, ref
		}
		if math.Abs(v-want) > 1e-12 || !ref.Equal(wantRef) {
			t.Errorf("order %v: %v at %v, want %v at %v", order, v, ref, want, wantRef)
		}
	}
	exact := math.Exp2(-45.0/90) + math.Exp2(-35.0/90) + 1
	if math.Abs(want-exact) > 1e-12 || !wantRef.Equal(times[2]) {
		t.Errorf("sum %v at %v, want %v at the latest", want, wantRef, exact)
	}
	// One half-life later the sum has halved.
	if v, ref := add(want, wantRef, wantRef.Add(h), h); math.Abs(v-(want/2+1)) > 1e-12 || !ref.Equal(wantRef.Add(h)) {
		t.Errorf("a half-life later: %v, want %v", v, want/2+1)
	}
	// A half-life of 0: no decay.
	var v float64
	var ref time.Time
	for _, tm := range []time.Time{times[2], times[0], times[1]} {
		v, ref = add(v, ref, tm, 0)
	}
	if v != 3 || !ref.Equal(times[2]) {
		t.Errorf("no decay: %v at %v", v, ref)
	}
}

func TestForgetDropsTheEntry(t *testing.T) {
	settle := Event{Kind: "settle", TS: at(100), Seq: 1, Settled: []Settled{
		{Key: "k1", ID: "m1", At: at(1), Outcome: "credit"},
		{Key: "k2", ID: "m2", At: at(1), Outcome: "confident_miss"},
	}}
	forget := Event{Kind: "forget", TS: at(200), IDs: []string{"m1"}}
	// Fold sorts by time: the forget comes after the settle, whatever the
	// order of the lines.
	l := Fold([]Event{forget, settle}, testParams)
	if _, ok := l.Memories["m1"]; ok || l.Memories["m2"].Misses != 1 || l.Memories["m2"].Shown != 1 {
		t.Errorf("fold: %+v", l.Memories)
	}

	// The live ledger drops it at the next settle, as a rebuild does.
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	if _, err := Settle(dir, testParams, at(120)); err != nil {
		t.Fatal(err)
	}
	if l, _ := LoadLedger(dir); l.Memories["m1"].Shown != 1 {
		t.Fatalf("m1 not settled: %+v", l.Memories)
	}
	writeJournal(t, dir, Event{Kind: "forget", TS: at(130), IDs: []string{"m1", "m3"}})
	if _, err := Settle(dir, testParams, at(140)); err != nil {
		t.Fatal(err)
	}
	live, _ := LoadLedger(dir)
	if _, ok := live.Memories["m1"]; ok || len(live.Memories) != 1 || live.Memories["m2"].Shown != 1 {
		t.Errorf("live after forget: %+v", live.Memories)
	}
	rebuilt, _ := Rebuild(dir, testParams, false)
	assertSameEntries(t, live, rebuilt, 0)
}

// An entry read at a later time: credits and misses each fade from their
// own reference time by their own half-life; a time before the reference,
// or a half-life of 0, leaves the sum as it is.
func TestEntryDecayedTo(t *testing.T) {
	p := Params{MissHalfLife: 90 * 24 * time.Hour, CreditHalfLife: 365 * 24 * time.Hour}
	e := Entry{Shown: 3, Credits: 2, CreditsAt: at(0), Misses: 1, MissesAt: at(60 * 24 * 90), LastShown: at(5)}
	got := e.DecayedTo(at(60*24*365), p)
	if math.Abs(got.Credits-1) > 1e-12 || !got.CreditsAt.Equal(at(60*24*365)) {
		t.Errorf("credits %v at %v, want 1 a credit half-life later", got.Credits, got.CreditsAt)
	}
	if want := math.Exp2(-275.0 / 90); math.Abs(got.Misses-want) > 1e-12 || !got.MissesAt.Equal(at(60*24*365)) {
		t.Errorf("misses %v at %v, want %v", got.Misses, got.MissesAt, want)
	}
	if got.Shown != 3 || !got.LastShown.Equal(at(5)) {
		t.Errorf("counts and times changed: %+v", got)
	}
	if back := e.DecayedTo(at(-60), p); back.Credits != 2 || back.Misses != 1 {
		t.Errorf("before the reference: %+v", back)
	}
	if flat := e.DecayedTo(at(60*24*365), Params{}); flat.Credits != 2 || flat.Misses != 1 {
		t.Errorf("no decay: %+v", flat)
	}
	if empty := (Entry{Shown: 1}).DecayedTo(at(10), p); empty.Credits != 0 || !empty.CreditsAt.IsZero() {
		t.Errorf("no credit: %+v", empty)
	}
}
