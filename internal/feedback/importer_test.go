package feedback

import (
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

const (
	impA = "0a000000-0000-4000-8000-00000000000a"
	impB = "0b000000-0000-4000-8000-00000000000b"
	impC = "0c000000-0000-4000-8000-00000000000c"
	impD = "0d000000-0000-4000-8000-00000000000d"
)

var impStart = time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)

// transcriptLine is one transcript record as Claude Code writes it.
func transcriptLine(t *testing.T, typ, uuid string, at time.Time, content any, extra map[string]any) string {
	t.Helper()
	rec := map[string]any{"type": typ, "uuid": uuid, "sessionId": "s-old", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": typ, "content": content}}
	for k, v := range extra {
		rec[k] = v
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func toolUse(id string) []any {
	return []any{map[string]any{"type": "tool_use", "id": id, "name": "mcp__plugin_backsight_memory__memory_search", "input": map[string]any{"query": "billing"}}}
}

func toolResult(id, text string) []any {
	return []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": []any{map[string]any{"type": "text", "text": text}}}}
}

func textBlocks(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }

func ranks(items []Item) map[string]int {
	out := map[string]int{}
	for _, it := range items {
		out[it.ID] = it.Rank
	}
	return out
}

// An output older than the #n markers is ranked from its printed scores,
// best first, a tie in print order; a line without a parseable id is
// skipped, never guessed. With markers, the markers rank.
func TestImportRanksOldSearchOutputs(t *testing.T) {
	old := "[billing]\n" +
		"- PostgreSQL 16 stays (2026-10-20, 3d ago, " + impA + ", 0.71)\n" +
		"- a hit whose id was cut (2026-10-20, 3d ago, 0.90)\n" +
		"- a hit with no memory id (2026-10-20, 3d ago, not-an-id, 0.95)\n" +
		"- tie, printed first (2026-10-20 [pitfall], 3d ago, " + impB + ", 0.83)\n" +
		"[other]\n" +
		"- tie, printed second (2026-10-21, 2d ago, " + impC + ", 0.83)\n" +
		"- the best (5d ago, " + impD + ", 0.92)\n"
	got := SearchHits(old)
	if len(got) != 4 {
		t.Fatalf("hits %+v, want 4 (the lines without an id skipped)", got)
	}
	if r := ranks(got); r[impD] != 1 || r[impB] != 2 || r[impC] != 3 || r[impA] != 4 {
		t.Errorf("ranks %v, want D 1, B 2 (tie printed first), C 3, A 4", r)
	}
	marked := "[billing]\n- #2 second (2026-10-20, 3d ago, " + impA + ", 0.95)\n[other]\n- #1 first (2026-10-21, 2d ago, " + impB + ", 0.50)\n"
	if r := ranks(SearchHits(marked)); r[impA] != 2 || r[impB] != 1 {
		t.Errorf("marked ranks %v, want the markers", r)
	}
	if hits := SearchHits("nothing in memory\n"); len(hits) != 0 {
		t.Errorf("empty output: %+v", hits)
	}

	// The same output read from a transcript: one show per hit, keyed by
	// the record that holds the result, at its time.
	at := impStart
	tr := transcriptLine(t, "user", "u1", at, "how is billing stored?", nil) +
		transcriptLine(t, "assistant", "a1", at.Add(time.Second), toolUse("t1"), nil) +
		transcriptLine(t, "user", "r1", at.Add(2*time.Second), toolResult("t1", old), nil) +
		transcriptLine(t, "assistant", "a2", at.Add(3*time.Second), textBlocks("It stays on PostgreSQL."), nil)
	turns, err := TranscriptTurns(strings.NewReader(tr))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || len(turns[0].Shows) != 4 {
		t.Fatalf("turns %+v", turns)
	}
	for _, s := range turns[0].Shows {
		if s.Session != "s-old" || s.Ref != "r1" || s.Path != "search" || !s.At.Equal(at.Add(2*time.Second)) {
			t.Errorf("show %+v", s)
		}
		if want := "s-old|r1|search|" + s.ID; s.Key() != want {
			t.Errorf("key %q, want %q", s.Key(), want)
		}
	}
	if r := map[string]int{}; true {
		for _, s := range turns[0].Shows {
			r[s.ID] = s.Rank
		}
		if r[impD] != 1 || r[impA] != 4 {
			t.Errorf("transcript ranks %v", r)
		}
	}
}

// A credit only for a turn whose reply cites memory and that showed
// exactly one memory; two shown, or no citation, are shows only. Never a
// miss.
func TestImportCreditsOnlyUnambiguousTurns(t *testing.T) {
	exact, free := "exact", "free"
	ev := func(m int, kind string, n int) TelemetryEvent {
		return TelemetryEvent{TS: impStart.Add(time.Duration(m) * time.Minute), Event: kind, Session: "s1", N: n}
	}
	p1 := ev(0, "prompt", 1)
	p1.IDs = []string{impA}
	r1 := ev(1, "reply", 1)
	r1.Credit = exact
	p2 := ev(10, "prompt", 2)
	p2.IDs = []string{impA, impB}
	r2 := ev(11, "reply", 2)
	r2.Credit = free
	p3 := ev(20, "prompt", 3)
	p3.IDs = []string{impC}
	r3 := ev(21, "reply", 3) // silent
	p4 := ev(30, "prompt", 4)
	p4.IDs = []string{impD}
	s4 := ev(31, "tool_use", 4)
	s4.Name, s4.MemoryIDs = "mcp__plugin_backsight_memory__memory_search", []string{impD}
	o4 := ev(40, "outcome", 4) // the next prompt's judgment of turn 4
	o4.Credited = &free
	machine := ev(41, "prompt", 0)
	machine.Reason, machine.IDs = "machine", []string{impB}
	turns := TelemetryTurns([]TelemetryEvent{p1, r1, p2, r2, p3, r3, p4, s4, o4, machine})

	var shows []ImportShow
	for _, tr := range turns {
		shows = append(shows, tr.Credited()...)
	}
	credits := map[string]int{}
	for _, s := range shows {
		if s.Credit {
			credits[s.Key()]++
		}
	}
	want := map[string]int{"s1|1|prompt|" + impA: 1, "s1|4|search|" + impD: 1}
	if len(credits) != len(want) || credits["s1|1|prompt|"+impA] != 1 || credits["s1|4|search|"+impD] != 1 {
		t.Errorf("credits %v, want %v (turn 1, and the last show of turn 4)", credits, want)
	}
	if len(shows) != 6 { // A; A, B; C; D twice (prompt and search); the machine message shows nothing
		t.Errorf("%d shows, want 6: %+v", len(shows), shows)
	}

	// Folded: shown and credited counts, imported apart; no miss at all.
	l := newLedger()
	l.Apply(ImportEvent("telemetry", "s1", shows, testParams), Params{})
	if a := l.Memories[impA]; a.Shown != 2 || a.Imported.Shown != 2 || a.Credits != 1 || a.Imported.Credits != 1 || !a.CreditsAt.Equal(impStart) {
		t.Errorf("A: %+v", a)
	}
	if b := l.Memories[impB]; b.Shown != 1 || b.Credits != 0 || b.Imported.Shown != 1 {
		t.Errorf("B: %+v", b)
	}
	for id, m := range l.Memories {
		if m.Misses != 0 {
			t.Errorf("%s has misses %v", id, m.Misses)
		}
	}
	// An import item with any other outcome counts nowhere.
	l.Apply(Event{Kind: KindImport, Settled: []Settled{{Key: "k", ID: "mx", At: impStart, Outcome: OutcomeMiss}, {Key: "k2", ID: "my", At: impStart, Outcome: OutcomeUnknown}}}, testParams)
	if _, ok := l.Memories["mx"]; ok || len(l.Memories) != 4 {
		t.Errorf("an imported miss or unknown counted: %+v", l.Memories)
	}

	// A transcript turn shows only, even with one memory shown and a
	// citation: the memories injected with its prompt are not named there.
	one := "[billing]\n- #1 the fact (2026-10-20, 3d ago, " + impA + ", 0.90)\n"
	two := "[billing]\n- #1 the fact (2026-10-20, 3d ago, " + impA + ", 0.90)\n- #2 another (2026-10-20, 3d ago, " + impB + ", 0.80)\n"
	tr := transcriptLine(t, "user", "u1", impStart, "first question", nil) +
		transcriptLine(t, "assistant", "a1", impStart.Add(time.Second), toolUse("t1"), nil) +
		transcriptLine(t, "user", "r1", impStart.Add(2*time.Second), toolResult("t1", one), nil) +
		transcriptLine(t, "assistant", "a2", impStart.Add(3*time.Second), textBlocks("PostgreSQL (memory 0a000000)."), nil) +
		transcriptLine(t, "user", "u2", impStart.Add(time.Minute), textBlocks("second question"), nil) +
		transcriptLine(t, "assistant", "a3", impStart.Add(61*time.Second), toolUse("t2"), nil) +
		transcriptLine(t, "user", "r2", impStart.Add(62*time.Second), toolResult("t2", two), nil) +
		transcriptLine(t, "assistant", "a4", impStart.Add(63*time.Second), textBlocks("Both apply (memory 0a000000)."), nil) +
		transcriptLine(t, "assistant", "side", impStart.Add(64*time.Second), toolUse("t3"), map[string]any{"isSidechain": true})
	trTurns, err := TranscriptTurns(strings.NewReader(tr))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tt := range trTurns {
		for _, s := range tt.Credited() {
			got = append(got, s.Ref+" "+s.ID[:2]+map[bool]string{true: " credit", false: ""}[s.Credit])
		}
	}
	if strings.Join(got, ", ") != "r1 0a, r2 0a, r2 0b" {
		t.Errorf("transcript shows %q", got)
	}
}

// An import event folds like a settle, from each show's own time, kept
// apart: Current equals the fold after it, a rebuild without imports
// leaves them out, and a settle after a rebuild applies nothing twice.
func TestImportFoldsApartAndOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	if _, err := Settle(dir, testParams, fixedClock(at(100))); err != nil {
		t.Fatal(err)
	}
	live, _ := LoadLedger(dir)
	shownAt := at(-60 * 24 * 30) // a month before the journal began
	shows := []ImportShow{
		{Session: "s0", Ref: "3", Path: "prompt", ID: "m1", Rank: 1, At: shownAt, Credit: true},
		{Session: "s0", Ref: "3", Path: "prompt", ID: "m9", Rank: 2, At: shownAt},
	}
	j := NewJournal(dir, Common{})
	j.now = func() time.Time { return at(110) }
	if err := j.AppendFolded(ImportEvent("telemetry", "s0", shows, testParams)); err != nil {
		t.Fatal(err)
	}
	cur, err := Current(dir)
	if err != nil {
		t.Fatal(err)
	}
	all, _, _ := Read(dir, time.Time{})
	assertSameEntries(t, cur, Fold(all, testParams), 1e-12)
	m1 := cur.Memories["m1"]
	if m1.Shown != live.Memories["m1"].Shown+1 || m1.Imported.Shown != 1 || m1.Imported.Credits != 1 || cur.Memories["m9"].Imported.Shown != 1 {
		t.Errorf("m1 %+v (live %+v), m9 %+v", m1, live.Memories["m1"], cur.Memories["m9"])
	}
	// The imported credit counts from its show, a month before the live one.
	if got, want := m1.DecayedTo(at(1), testParams).Credits, 1+decay(1, shownAt, at(1), testParams.CreditHalfLife); math.Abs(got-want) > 1e-9 || want > 1.95 {
		t.Errorf("m1 credits %v at the live credit's time, want %v", got, want)
	}

	// The mark of a complete import: in Current, and kept without imports.
	if err := j.AppendFolded(Event{Kind: KindImport, Outcome: ImportComplete}); err != nil {
		t.Fatal(err)
	}
	if cur, _ := Current(dir); !cur.HistoryImported.Equal(at(110).Add(time.Nanosecond)) && !cur.HistoryImported.Equal(at(110)) {
		t.Errorf("history imported %v", cur.HistoryImported)
	}
	all, _, _ = Read(dir, time.Time{})
	without, err := Rebuild(dir, testParams, false, false)
	if err != nil {
		t.Fatal(err)
	}
	assertSameEntries(t, without, Fold(DropImports(all), testParams), 0)
	if _, ok := without.Memories["m9"]; ok || without.Memories["m1"].Imported.Shown != 0 || without.HistoryImported.IsZero() {
		t.Errorf("rebuild without imports: %+v", without.Memories)
	}

	// Rebuilt with imports and written, then settled: nothing twice.
	if _, err := Rebuild(dir, testParams, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(dir, testParams, fixedClock(at(200))); err != nil {
		t.Fatal(err)
	}
	after, _ := LoadLedger(dir)
	if after.Memories["m9"].Shown != 1 || after.Memories["m1"].Imported.Shown != 1 {
		t.Errorf("after rebuild and settle: m1 %+v, m9 %+v", after.Memories["m1"], after.Memories["m9"])
	}
	if cur, _ := Current(dir); cur.Memories["m9"].Shown != 1 {
		t.Errorf("Current after rebuild and settle: %+v", cur.Memories["m9"])
	}
}

// A rebuild folds the retirements after the last settle; the next settle
// does not apply them a second time.
func TestSettleAfterARebuildAppliesNothingTwice(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	j := NewJournal(dir, Common{Session: "s"})
	j.now = func() time.Time { return start }
	if err := j.Append(Event{Kind: "settle", Settled: []Settled{{Key: "k", ID: "m1", At: start, Outcome: OutcomeCredit}}}); err != nil {
		t.Fatal(err)
	}
	j.now = func() time.Time { return start.Add(time.Minute) }
	if err := j.AppendFolded(Event{Kind: KindRetire, IDs: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Rebuild(dir, Params{}, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Settle(dir, Params{TurnTimeout: TurnTimeout}, fixedClock(start.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if l, _ := LoadLedger(dir); l.Memories["m1"].Wrong != 1 {
		t.Errorf("wrong %d after a rebuild and a settle, want 1", l.Memories["m1"].Wrong)
	}
}

// Current reads the journal from journal_at: a folded event journaled
// while a settle holds the lock (after its read, before its ledger write:
// AppendFolded gave up waiting) is found, and Current equals the fold.
// A settle that recorded where the journal ended after its read would
// skip it.
func TestCurrentFindsAFoldedEventAppendedDuringASettle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	writeJournal(t, dir, sampleSession()...)
	old := foldedLockWait
	foldedLockWait = time.Millisecond
	t.Cleanup(func() { foldedLockWait, afterSettleRead = old, nil })
	var appendErr error
	afterSettleRead = func() {
		j := NewJournal(dir, Common{})
		j.now = func() time.Time { return at(105) }
		appendErr = j.AppendFolded(Event{Kind: KindHide, IDs: []string{"m2"}, Verdict: "wrong"})
	}
	if _, err := Settle(dir, testParams, fixedClock(at(100))); err != nil {
		t.Fatal(err)
	}
	afterSettleRead = nil
	var held *state.HeldError
	if !errors.As(appendErr, &held) {
		t.Fatalf("the append did not run under the settle's lock: %v", appendErr)
	}
	cur, err := Current(dir)
	if err != nil {
		t.Fatal(err)
	}
	all, _, _ := Read(dir, time.Time{})
	assertSameEntries(t, cur, Fold(all, testParams), 1e-12)
	if cur.Memories["m2"].Hidden == nil {
		t.Errorf("hide appended during the settle lost: %+v", cur.Memories["m2"])
	}
}

// journal_at in last month's file, folded events in this month's: Current
// reads the rest of the one and all of the other, and equals the fold.
func TestCurrentReadsAcrossAMonthBoundary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	end := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	j := NewJournal(dir, Common{Session: "s1"})
	j.now = func() time.Time { return end }
	for _, e := range []Event{
		{Kind: "turn", Turn: "p1", TS: end.Add(-3 * time.Hour)},
		{Kind: "shows", Turn: "p1", Path: "prompt", TS: end.Add(-3*time.Hour + time.Minute), Items: []Item{{ID: "m1", Rank: 1}, {ID: "m2", Rank: 2}}},
		{Kind: "credit", Turn: "p1", Verdict: "used", IDs: []string{"m1"}, TS: end.Add(-3*time.Hour + 2*time.Minute)},
	} {
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Settle(dir, testParams, fixedClock(end)); err != nil {
		t.Fatal(err)
	}
	if l, _ := LoadLedger(dir); l.JournalAt.File != "journal-2026-09.jsonl" {
		t.Fatalf("journal_at %+v, want in September's file", l.JournalAt)
	}
	j.now = func() time.Time { return end.Add(2 * time.Hour) } // October
	for _, e := range []Event{{Kind: KindRetire, IDs: []string{"m1"}}, {Kind: KindHide, IDs: []string{"m2"}, Verdict: "outdated"},
		ImportEvent("telemetry", "s0", []ImportShow{{Session: "s0", Ref: "1", Path: "prompt", ID: "m3", Rank: 1, At: end.AddDate(0, -1, 0), Credit: true}}, testParams)} {
		if err := j.AppendFolded(e); err != nil {
			t.Fatal(err)
		}
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "journal-2026-10.jsonl")); len(files) != 1 {
		t.Fatal("no October file")
	}
	cur, err := Current(dir)
	if err != nil {
		t.Fatal(err)
	}
	all, _, _ := Read(dir, time.Time{})
	assertSameEntries(t, cur, Fold(all, testParams), 1e-12)
	if cur.Memories["m1"].Wrong != 1 || cur.Memories["m2"].Hidden == nil || cur.Memories["m3"].Imported.Credits != 1 {
		t.Errorf("October's events not applied: %+v", cur.Memories)
	}
}
