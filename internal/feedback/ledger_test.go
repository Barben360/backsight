package feedback

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
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
		case x.Wrong != y.Wrong || (x.Hidden == nil) != (y.Hidden == nil) || (x.Hidden != nil && (!x.Hidden.Since.Equal(y.Hidden.Since) ||
			x.Hidden.Verdict != y.Hidden.Verdict || x.Hidden.Note != y.Hidden.Note)):
			t.Errorf("%s: wrong %d vs %d, hidden %+v vs %+v", id, x.Wrong, y.Wrong, x.Hidden, y.Hidden)
		}
	}
}

// randomJournal is n random shows over days days, with credits, stops,
// subagents, a few forgets, retirements, hidings and their undoing, in
// time order.
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
		if r.IntN(4) == 0 {
			m++
			id := fmt.Sprintf("m%d", r.IntN(30))
			kind := []string{KindRetire, KindRetire, KindHide, KindUnhide}[r.IntN(4)]
			events = append(events, Event{Kind: kind, TS: at(m), IDs: []string{id}, Verdict: "wrong", Note: "n-" + id})
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
	wrong, hidden := 0, 0
	for _, m := range live.Memories {
		if m.Wrong > 0 {
			wrong++
		}
		if m.Hidden != nil {
			hidden++
		}
	}
	if total.Shows < 500 || total.Credits == 0 || total.Misses == 0 || len(live.Memories) == 0 || wrong == 0 || hidden == 0 {
		t.Fatalf("a weak sample: %+v, %d memories, %d wrong, %d hidden", total, len(live.Memories), wrong, hidden)
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

// Two settle events carrying the same key (a journal copied twice, a
// settle replayed by hand): the fold counts the show once.
func TestFoldCountsASettleKeyOnce(t *testing.T) {
	s := Settled{Key: "k1", ID: "m1", At: at(1), Outcome: OutcomeCredit}
	l := Fold([]Event{
		{Kind: "settle", TS: at(100), Seq: 1, Settled: []Settled{s}},
		{Kind: "settle", TS: at(200), Seq: 2, Settled: []Settled{s, {Key: "k2", ID: "m1", At: at(150), Outcome: OutcomeShown}}},
	}, testParams)
	if e := l.Memories["m1"]; e.Shown != 2 || e.Credits != 1 {
		t.Errorf("m1: %+v, want shown 2 and one credit", e)
	}
}

// A rebuild after the half-lives changed reproduces the live ledger: each
// settle event is folded with the half-lives it recorded.
func TestRebuildUsesTheRecordedHalfLives(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	day := 60 * 24
	writeJournal(t, dir,
		evTurn("s1", "p1", 0),
		evShows("s1", "p1", "", "prompt", 1, Item{ID: "m1", Rank: 1}, Item{ID: "m2", Rank: 1}),
		evCredit("s1", "p1", "", "used", 2, "m2"),
		evStop("s1", "p1", "", "declared", 3),
	)
	if _, err := Settle(dir, testParams, at(120)); err != nil {
		t.Fatal(err)
	}
	writeJournal(t, dir,
		evTurn("s1", "p2", 40*day),
		evShows("s1", "p2", "", "prompt", 40*day+1, Item{ID: "m1", Rank: 1}, Item{ID: "m2", Rank: 1}),
		evCredit("s1", "p2", "", "used", 40*day+2, "m2"),
		evStop("s1", "p2", "", "declared", 40*day+3),
	)
	if _, err := Settle(dir, testParams, at(40*day+120)); err != nil {
		t.Fatal(err)
	}
	live, _ := LoadLedger(dir)
	if e := live.Memories["m1"]; e.Misses <= 1 || e.Misses >= 2 {
		t.Fatalf("a weak sample: m1 %+v", e)
	}
	shorter := testParams
	shorter.MissHalfLife, shorter.CreditHalfLife = 30*24*time.Hour, 30*24*time.Hour
	rebuilt, err := Rebuild(dir, shorter, false)
	if err != nil {
		t.Fatal(err)
	}
	assertSameEntries(t, live, rebuilt, 1e-12)
	// Settle events without params (none are written so) fold with the
	// caller's.
	bare := Fold([]Event{
		{Kind: "settle", TS: at(100), Seq: 1, Settled: []Settled{{Key: "k1", ID: "m1", At: at(0), Outcome: OutcomeMiss}}},
		{Kind: "settle", TS: at(200), Seq: 2, Settled: []Settled{{Key: "k2", ID: "m1", At: at(30 * day), Outcome: OutcomeMiss}}},
	}, shorter)
	if got := bare.Memories["m1"].Misses; math.Abs(got-1.5) > 1e-12 {
		t.Errorf("misses without params %v, want 1.5 under the caller's 30 days", got)
	}
}

// Retirements fold into the entry: each retire counts one more wrong, a
// hide sets the hiding with its time, verdict and note, an unhide clears
// both; a rebuilt ledger equals the live one at each step.
func TestLedgerFoldsRetirements(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, append(sampleSession(),
		Event{Kind: KindRetire, TS: at(20), IDs: []string{"m1"}, Verdict: "wrong", Outcome: "invalidated", Note: "n1"},
		Event{Kind: KindRetire, TS: at(21), IDs: []string{"m1"}, Verdict: "outdated", Outcome: "invalidated", Note: "n2"},
		Event{Kind: KindHide, TS: at(22), IDs: []string{"o1"}, Verdict: "outdated", Note: "n3"},
	)...)
	settleAndCompare := func(now time.Time) Ledger {
		t.Helper()
		if _, err := Settle(dir, testParams, now); err != nil {
			t.Fatal(err)
		}
		live, err := LoadLedger(dir)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt, err := Rebuild(dir, testParams, false)
		if err != nil {
			t.Fatal(err)
		}
		assertSameEntries(t, live, rebuilt, 0)
		return live
	}
	l := settleAndCompare(at(120))
	if m := l.Memories["m1"]; m.Wrong != 2 || m.Hidden != nil || m.Shown != 1 {
		t.Errorf("m1 after two retires: %+v", m)
	}
	h := l.Memories["o1"].Hidden
	if h == nil || !h.Since.Equal(at(22)) || h.Verdict != "outdated" || h.Note != "n3" || l.Memories["o1"].Wrong != 0 {
		t.Fatalf("o1 after a hide: %+v", l.Memories["o1"])
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "ledger.json"))
	if !strings.Contains(string(raw), `"wrong":2`) || !strings.Contains(string(raw), `"hidden":{"since":"2026-09-01T09:22:00Z","verdict":"outdated","note":"n3"}`) {
		t.Errorf("ledger.json: %s", raw)
	}

	writeJournal(t, dir, Event{Kind: KindUnhide, TS: at(130), IDs: []string{"m1", "o1", "never-seen"}, Outcome: "restored"})
	l = settleAndCompare(at(140))
	if m := l.Memories["m1"]; m.Wrong != 0 || m.Hidden != nil || m.Shown != 1 {
		t.Errorf("m1 after an unhide: %+v", m)
	}
	if m, ok := l.Memories["o1"]; !ok || m.Wrong != 0 || m.Hidden != nil {
		t.Errorf("o1 after an unhide: %+v (kept: %v)", m, ok)
	}
	if _, ok := l.Memories["never-seen"]; ok {
		t.Error("an unhide of an unknown memory made an entry")
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "ledger.json"))
	if strings.Contains(string(raw), `"wrong"`) || strings.Contains(string(raw), `"hidden"`) {
		t.Errorf("cleared fields still written: %s", raw)
	}
}

// Current is the ledger with the retirements journaled since its last
// settle: a hide or unhide counts at once, the file stays as it was.
func TestCurrentAppliesRetirementsSinceTheWatermark(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	if _, err := Settle(dir, testParams, at(120)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "ledger.json"))
	writeJournal(t, dir,
		Event{Kind: KindHide, TS: at(130), IDs: []string{"o1"}, Verdict: "wrong", Note: "n1"},
		Event{Kind: KindRetire, TS: at(131), IDs: []string{"m1"}, Verdict: "wrong", Note: "n2"},
		Event{Kind: KindHide, TS: at(132), IDs: []string{"o2"}, Verdict: "wrong", Note: "n3"},
		Event{Kind: KindUnhide, TS: at(133), IDs: []string{"o2"}, Outcome: "rewritten"},
	)
	l, err := Current(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l.Memories["o1"].Hidden == nil || l.Memories["m1"].Wrong != 1 || l.Memories["o2"].Hidden != nil || l.Memories["m1"].Shown != 1 {
		t.Errorf("current: %+v", l.Memories)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "ledger.json")); string(after) != string(before) {
		t.Error("Current wrote the ledger")
	}
}

// A retirement journaled at the very instant of the last settle folds at
// the next one, as a rebuild folds it.
func TestAnEventAtTheWatermarkFolds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	if _, err := Settle(dir, testParams, at(120)); err != nil {
		t.Fatal(err)
	}
	if err := NewJournal(dir, Common{}).AppendFolded(Event{Kind: KindRetire, TS: at(120), IDs: []string{"m1"}, Verdict: "wrong", Note: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(dir, testParams, at(140)); err != nil {
		t.Fatal(err)
	}
	live, _ := LoadLedger(dir)
	rebuilt, err := Rebuild(dir, testParams, false)
	if err != nil {
		t.Fatal(err)
	}
	assertSameEntries(t, live, rebuilt, 0)
	if live.Memories["m1"].Wrong != 1 {
		t.Errorf("m1: %+v", live.Memories["m1"])
	}
}

// A retirement journaled while a settle runs, stamped before the settle's
// time, waits for the ledger lock and lands after the settle's watermark:
// the next settle folds it, as a rebuild does.
func TestARetirementDuringASettleFolds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	if _, err := Settle(dir, testParams, at(100)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	afterSettleRead = func() {
		go func() {
			done <- NewJournal(dir, Common{}).AppendFolded(Event{Kind: KindRetire, TS: at(110), IDs: []string{"m1"}, Verdict: "wrong", Note: "n1"})
		}()
		time.Sleep(100 * time.Millisecond) // the append runs now, unless it waits for the lock
	}
	t.Cleanup(func() { afterSettleRead = nil })
	if _, err := Settle(dir, testParams, at(120)); err != nil {
		t.Fatal(err)
	}
	afterSettleRead = nil
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(dir, testParams, at(140)); err != nil {
		t.Fatal(err)
	}
	live, _ := LoadLedger(dir)
	rebuilt, err := Rebuild(dir, testParams, false)
	if err != nil {
		t.Fatal(err)
	}
	assertSameEntries(t, live, rebuilt, 0)
	if live.Memories["m1"].Wrong != 1 {
		t.Errorf("m1: %+v", live.Memories["m1"])
	}
}

// A ledger lock held past the wait still journals the event (a session's
// retirements are counted from the journal) and says the lock was held.
func TestAppendFoldedJournalsWhenTheLockStaysHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	release, err := lockLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = NewJournal(dir, Common{Session: "s1"}).AppendFolded(Event{Kind: KindRetire, IDs: []string{"m1"}, Verdict: "wrong"})
	var held *state.HeldError
	if !errors.As(err, &held) {
		t.Errorf("want the held lock, got %v", err)
	}
	if events, _, _ := Read(dir, time.Time{}); len(events) != 1 || events[0].Kind != KindRetire {
		t.Errorf("journal %+v", events)
	}
}

// The boost follows k × ((c + α) / (c + m + α + β) − α / (α + β)), with c
// and m decayed to now: a memory with no entry, or with nothing settled,
// scores exactly 0; credits raise it, misses lower it, both fade.
func TestBoostFormula(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	p := Params{Weight: 0.1, PriorCredits: 3, PriorMisses: 9, CreditHalfLife: 365 * 24 * time.Hour, MissHalfLife: 90 * 24 * time.Hour}
	l := Ledger{Schema: LedgerSchema, Memories: map[string]Entry{
		"used":   {Shown: 3, Credits: 3, CreditsAt: at},
		"shown":  {Shown: 4},
		"missed": {Shown: 2, Misses: 2, MissesAt: at},
	}}
	if b := l.Boost("absent", p, at); b != 0 {
		t.Errorf("no entry: %v, want exactly 0", b)
	}
	if b := l.Boost("shown", p, at); b != 0 {
		t.Errorf("shown, never settled: %v, want exactly 0", b)
	}
	if b, want := l.Boost("used", p, at), 0.1*(6.0/15-0.25); math.Abs(b-want) > 1e-12 {
		t.Errorf("credits 3: %v, want %v", b, want)
	}
	if b, want := l.Boost("missed", p, at), 0.1*(3.0/14-0.25); math.Abs(b-want) > 1e-12 || b >= 0 {
		t.Errorf("misses 2: %v, want %v", b, want)
	}
	// One miss half-life later the two misses weigh one.
	if b, want := l.Boost("missed", p, at.Add(90*24*time.Hour)), 0.1*(3.0/13-0.25); math.Abs(b-want) > 1e-12 {
		t.Errorf("misses decayed: %v, want %v", b, want)
	}
	// One credit half-life later the three credits weigh 1.5.
	if b, want := l.Boost("used", p, at.Add(365*24*time.Hour)), 0.1*(4.5/13.5-0.25); math.Abs(b-want) > 1e-12 {
		t.Errorf("credits decayed: %v, want %v", b, want)
	}
	if b := l.Boost("used", Params{PriorCredits: 3, PriorMisses: 9}, at); b != 0 {
		t.Errorf("k = 0: %v", b)
	}
}

// Current reads the journal from where the last settle stopped reading,
// not from the start of the month: the ledger it returns is the one a
// full read gives, and only the tail is decoded.
func TestCurrentReadsOnlyTheJournalsTail(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	j := NewJournal(dir, Common{Session: "s"})
	j.now = func() time.Time { return base }
	for i := range 500 {
		at := base.Add(time.Duration(i) * time.Second)
		if err := j.Append(Event{Kind: "shows", TS: at, Turn: "t", Path: "search", Items: []Item{{ID: fmt.Sprintf("m%d", i%7), Rank: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.AppendFolded(Event{Kind: KindRetire, TS: base.Add(time.Hour), IDs: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	p := Params{ConfidentRank: 2, TurnTimeout: TurnTimeout, MissHalfLife: 90 * 24 * time.Hour, CreditHalfLife: 365 * 24 * time.Hour}
	settleAt := base.Add(3 * time.Hour)
	if _, err := Settle(dir, p, settleAt); err != nil {
		t.Fatal(err)
	}
	j.now = func() time.Time { return settleAt.Add(time.Minute) }
	for _, e := range []Event{{Kind: KindRetire, IDs: []string{"m2"}}, {Kind: KindHide, IDs: []string{"m3"}, Verdict: "wrong"}, {Kind: "forget", IDs: []string{"m4"}}} {
		if err := j.AppendFolded(e); err != nil {
			t.Fatal(err)
		}
	}
	// What a read of the whole month gives.
	want, err := LoadLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	all, _, _ := Read(dir, want.Watermark)
	for _, e := range sortedByTime(all) {
		if folded(e.Kind) && e.TS.After(want.Watermark) {
			want.Apply(e, Params{})
		}
	}
	n := 0
	decodedLine = func() { n++ }
	t.Cleanup(func() { decodedLine = nil })
	got, err := Current(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertSameEntries(t, got, want, 1e-12)
	if got.Memories["m2"].Wrong != 1 || got.Memories["m3"].Hidden == nil || got.Memories["m1"].Wrong != 1 {
		t.Errorf("tail not applied: %+v", got.Memories)
	}
	if _, ok := got.Memories["m4"]; ok {
		t.Error("m4 forgotten after the settle is still there")
	}
	if n != 4 { // the settle event and the three folded ones
		t.Errorf("decoded %d lines, want the 4 after the settle's read", n)
	}

	// A journal file shorter than where the ledger stopped (rewritten,
	// truncated): Current reads the month again, as before.
	file := filepath.Join(dir, got.JournalAt.File)
	raw, _ := os.ReadFile(file)
	tail := raw[got.JournalAt.Offset:]
	if err := os.WriteFile(file, tail, 0o600); err != nil {
		t.Fatal(err)
	}
	n = 0
	if again, err := Current(dir); err != nil || n != 4 || again.Memories["m2"].Wrong != 1 || again.Memories["m3"].Hidden == nil {
		t.Errorf("truncated journal: %v, %d lines, %+v", err, n, again.Memories)
	}
}

// Rebuild records where it read too: what it folded is not applied a
// second time by Current.
func TestCurrentAfterARebuildAppliesNothingTwice(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	j := NewJournal(dir, Common{Session: "s"})
	j.now = func() time.Time { return at }
	if err := j.Append(Event{Kind: "settle", Settled: []Settled{{Key: "k", ID: "m1", At: at, Outcome: OutcomeCredit}}}); err != nil {
		t.Fatal(err)
	}
	j.now = func() time.Time { return at.Add(time.Minute) }
	if err := j.AppendFolded(Event{Kind: KindRetire, IDs: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Rebuild(dir, Params{}, true); err != nil {
		t.Fatal(err)
	}
	n := 0
	decodedLine = func() { n++ }
	t.Cleanup(func() { decodedLine = nil })
	l, err := Current(dir)
	if err != nil || l.Memories["m1"].Wrong != 1 || n != 0 {
		t.Errorf("after rebuild: %v, wrong %d, %d lines decoded", err, l.Memories["m1"].Wrong, n)
	}
}

// A folded event read by a settle but stamped after it is left for
// Current, which must then read from the watermark to find it.
func TestCurrentFindsAFoldedEventNewerThanTheSettleItPreceded(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	j := NewJournal(dir, Common{Session: "s"})
	j.now = func() time.Time { return at }
	if err := j.AppendFolded(Event{Kind: KindHide, TS: at.Add(time.Hour), IDs: []string{"m1"}, Verdict: "wrong"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(dir, Params{TurnTimeout: TurnTimeout}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	l, err := Current(dir)
	if err != nil || l.Memories["m1"].Hidden == nil {
		t.Errorf("hide stamped after the settle lost: %v %+v", err, l.Memories)
	}
}
