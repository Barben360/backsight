package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/metricsfile"
	"github.com/Barben360/backsight/internal/telemetry"
)

// usedCredits journals n credits of verdict used for id, one a minute from
// start, in a session of their own.
func (x *fx) usedCredits(start time.Time, id string, n int) {
	x.t.Helper()
	for i := range n {
		x.credit(start.Add(time.Duration(i)*time.Minute), "cs-"+id, "", "alpha", "used", id)
	}
}

// TestHelpedMost: the three memories with the most used credits in the
// window; ties go to the one shown more in the window, then to the id; the
// window is the view's; with none there is an empty list, not null.
func TestHelpedMost(t *testing.T) {
	x := newFx(t, fxNow)
	in := paris(27, 9, 0) // inside the last 7 days
	x.usedCredits(in, "mem-a", 7)
	x.usedCredits(in, "mem-b", 5)
	x.usedCredits(in, "mem-c", 5)
	x.usedCredits(in, "mem-d", 2)
	for i := range 4 { // reports are not help
		x.credit(in.Add(time.Duration(i)*time.Minute), "cs-e", "", "alpha", "wrong", "mem-e")
	}
	x.usedCredits(paris(10, 9, 0), "mem-d", 5) // 20 days back: only a 30-day window sees them
	x.shows(paris(27, 8, 0), "v1", "t1", "", "alpha", "prompt", "alpha", "mem-a")
	for i := range 3 {
		x.shows(paris(27, 8, 10+i), fmt.Sprintf("v%d", i+2), "t1", "", "alpha", "prompt", "alpha", "mem-b")
	}
	x.shows(paris(27, 8, 30), "v5", "t1", "", "alpha", "prompt", "alpha", "mem-c")
	x.shows(paris(27, 8, 40), "v6", "t1", "", "alpha", feedback.PathSources, "alpha", "mem-c") // an answer from sources is not a show
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: "mem-a", Outcome: "found", DocKind: "none", CreatedDay: "2026-10-20", FactType: "experience"})

	rows := x.run("overview", x.query(7)).Tables.Helped
	want := []HelpedRow{
		{Memory: "mem-a", Project: "alpha", FactType: "experience", Used: 7, Shown: 1},
		{Memory: "mem-b", Project: "alpha", Used: 5, Shown: 3},
		{Memory: "mem-c", Project: "alpha", Used: 5, Shown: 1},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("7 days: %+v\nwant %+v", rows, want)
	}
	rows = x.run("overview", x.query(30)).Tables.Helped // d now has 7: a tie with a, broken by shows (1 against 0)
	want = []HelpedRow{
		{Memory: "mem-a", Project: "alpha", FactType: "experience", Used: 7, Shown: 1},
		{Memory: "mem-d", Used: 7},
		{Memory: "mem-b", Project: "alpha", Used: 5, Shown: 3},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("30 days: %+v\nwant %+v", rows, want)
	}

	two := newFx(t, fxNow)
	two.usedCredits(in, "mem-x", 1)
	two.usedCredits(in, "mem-y", 3)
	if rows := two.run("overview", two.query(7)).Tables.Helped; len(rows) != 2 || rows[0].Memory != "mem-y" || rows[1].Memory != "mem-x" {
		t.Errorf("two used memories: %+v, want y then x", rows)
	}

	none := newFx(t, fxNow)
	r := none.run("overview", none.query(7))
	raw, err := json.Marshal(r.Tables)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"helped":[]`) {
		t.Errorf("no credits: tables = %s, want helped as an empty list", raw)
	}
}

// TestHelpedTiesBreakByID: equal used and shown fall back to the id.
func TestHelpedTiesBreakByID(t *testing.T) {
	x := newFx(t, fxNow)
	for _, id := range []string{"mem-z", "mem-m", "mem-b", "mem-q"} {
		x.usedCredits(paris(27, 9, 0), id, 2)
	}
	var got []string
	for _, r := range x.run("overview", x.query(7)).Tables.Helped {
		got = append(got, r.Memory)
	}
	if want := []string{"mem-b", "mem-m", "mem-q"}; !slices.Equal(got, want) {
		t.Errorf("helped = %v, want %v", got, want)
	}
}

// calPrompt writes a prompt at at.
func (x *fx) calPrompt(at time.Time, session, project string, p telemetry.Prompt) {
	x.t.Helper()
	x.tel(at, p, session, project)
}

// TestCalendarSpansThirteenMonths: from the Monday on or before today less
// 13 months plus a day, to today, one row per local day, whatever the
// window; days with no prompt are not recorded; the project filter applies.
func TestCalendarSpansThirteenMonths(t *testing.T) {
	x := newFx(t, fxNow)
	rec := telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Chars: 10, Plen: 80}
	x.calPrompt(time.Date(2025, 9, 28, 10, 0, 0, 0, fxParis), "s0", "alpha", rec) // the Sunday before the first day
	x.calPrompt(time.Date(2025, 9, 29, 10, 0, 0, 0, fxParis), "s1", "alpha", rec)
	x.calPrompt(paris(29, 9, 0), "s2", "alpha", rec)
	x.calPrompt(paris(29, 9, 5), "s2", "alpha", telemetry.Prompt{N: 2, Decision: "recall", Empty: "below_floor", Plen: 80})
	x.calPrompt(paris(29, 9, 30), "s3", "beta", rec)
	x.calPrompt(paris(29, 9, 40), "s4", "alpha", telemetry.Prompt{N: 1, Decision: "recall", Reason: "machine", Injected: 1, Plen: 80})
	x.declaredTurn(paris(29, 10, 0), "s2", "declared", "used", "m1")
	x.calPrompt(paris(30, 8, 0), "s5", "alpha", rec)
	x.calPrompt(paris(31, 8, 0), "s6", "alpha", rec) // tomorrow: not in the calendar

	cal := x.run("days", x.query(7)).Tables.Calendar
	var days int
	for d := time.Date(2025, 9, 29, 0, 0, 0, 0, time.UTC); !d.After(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
		days++
	}
	if len(cal) != days || days != 397 {
		t.Fatalf("calendar has %d rows, want %d (397)", len(cal), days)
	}
	if cal[0].Day != "2025-09-29" || cal[len(cal)-1].Day != "2026-10-30" {
		t.Errorf("calendar runs %s..%s, want 2025-09-29..2026-10-30", cal[0].Day, cal[len(cal)-1].Day)
	}
	if wd, _ := time.Parse("2006-01-02", cal[0].Day); wd.Weekday() != time.Monday {
		t.Errorf("first day %s is a %s, want a Monday", cal[0].Day, wd.Weekday())
	}
	by := map[string]CalendarDay{}
	for _, d := range cal {
		by[d.Day] = d
	}
	if len(by) != len(cal) {
		t.Errorf("%d distinct days in %d rows", len(by), len(cal))
	}
	want := map[string]CalendarDay{
		"2025-09-29": {Day: "2025-09-29", Prompts: 1, PromptsWithMemory: 1, Sessions: 1, Recorded: true},
		"2026-10-28": {Day: "2026-10-28"},
		// s2 two prompts, one with memory; s3 one with memory; s4 machine (a recorded line, not a typed prompt)
		"2026-10-29": {Day: "2026-10-29", Prompts: 3, PromptsWithMemory: 2, UsedTurns: 1, Sessions: 3, Recorded: true},
		"2026-10-30": {Day: "2026-10-30", Prompts: 1, PromptsWithMemory: 1, Sessions: 1, Recorded: true},
	}
	for day, w := range want {
		if by[day] != w {
			t.Errorf("%s = %+v, want %+v", day, by[day], w)
		}
	}
	if _, ok := by["2025-09-28"]; ok {
		t.Error("the Sunday before the first Monday is in the calendar")
	}
	if _, ok := by["2026-10-31"]; ok {
		t.Error("tomorrow is in the calendar")
	}
	for _, d := range cal {
		if d.Recorded != (d.Day == "2025-09-29" || d.Day == "2026-10-29" || d.Day == "2026-10-30") {
			t.Errorf("%s recorded = %v", d.Day, d.Recorded)
		}
	}

	for _, n := range []int{7, 90} { // independent of the window
		if got := x.run("days", x.query(n)).Tables.Calendar; !reflect.DeepEqual(got, cal) {
			t.Errorf("a %d-day window changes the calendar", n)
		}
	}

	q := x.query(7)
	q.Project = "alpha"
	got := map[string]CalendarDay{}
	for _, d := range x.run("days", q).Tables.Calendar {
		got[d.Day] = d
	}
	if w := (CalendarDay{Day: "2026-10-29", Prompts: 2, PromptsWithMemory: 1, UsedTurns: 1, Sessions: 2, Recorded: true}); got["2026-10-29"] != w {
		t.Errorf("project alpha, 2026-10-29 = %+v, want %+v", got["2026-10-29"], w)
	}
	q.Project = "beta"
	got = map[string]CalendarDay{}
	for _, d := range x.run("days", q).Tables.Calendar {
		got[d.Day] = d
	}
	if w := (CalendarDay{Day: "2026-10-29", Prompts: 1, PromptsWithMemory: 1, Sessions: 1, Recorded: true}); got["2026-10-29"] != w {
		t.Errorf("project beta, 2026-10-29 = %+v, want %+v", got["2026-10-29"], w)
	}
	if got["2025-09-29"].Recorded {
		t.Error("project beta: a day with only alpha prompts is recorded")
	}
}

// TestCalendarOneRowPerLocalDayAcrossDST: the days the clocks change in
// Paris (2026-03-29, 2026-10-25) have one row each, and a prompt belongs to
// the local date it was typed on.
func TestCalendarOneRowPerLocalDayAcrossDST(t *testing.T) {
	x := newFx(t, fxNow)
	rec := telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Plen: 80}
	for i, utc := range []string{
		"2026-03-28T23:30:00Z", // 00:30 CET on the 29th
		"2026-03-29T21:59:00Z", // 23:59 CEST on the 29th
		"2026-03-29T22:01:00Z", // 00:01 CEST on the 30th
		"2026-10-24T22:10:00Z", // 00:10 CEST on the 25th
		"2026-10-25T00:30:00Z", // 02:30 CEST
		"2026-10-25T01:30:00Z", // 02:30 CET, an hour later
		"2026-10-25T22:59:00Z", // 23:59 CET
		"2026-10-25T23:30:00Z", // 00:30 CET on the 26th
	} {
		at, err := time.Parse(time.RFC3339, utc)
		if err != nil {
			t.Fatal(err)
		}
		x.calPrompt(at, fmt.Sprintf("d%d", i), "alpha", rec)
	}
	cal := x.run("days", x.query(7)).Tables.Calendar
	seen := map[string]CalendarDay{}
	for i, d := range cal {
		if _, dup := seen[d.Day]; dup {
			t.Errorf("%s appears twice", d.Day)
		}
		seen[d.Day] = d
		if i > 0 {
			prev, _ := time.Parse("2006-01-02", cal[i-1].Day)
			cur, _ := time.Parse("2006-01-02", d.Day)
			if cur.Sub(prev) != 24*time.Hour {
				t.Errorf("%s follows %s", d.Day, cal[i-1].Day)
			}
		}
	}
	for day, n := range map[string]int64{"2026-03-28": 0, "2026-03-29": 2, "2026-03-30": 1, "2026-10-24": 0, "2026-10-25": 4, "2026-10-26": 1} {
		d, ok := seen[day]
		if !ok || d.Prompts != n {
			t.Errorf("%s = %+v (present %v), want %d prompts", day, d, ok, n)
		}
	}
}

// TestCalendarCountsEachSessionOncePerDay: a session whose prompts fall in
// several quarter hours and hours of a day counts once for that day and once
// for each other day it types on; a prompt at local midnight (and one a
// minute before) belongs to the day it is typed on; a machine line makes a
// day recorded and counts its session, not a prompt; used turns are placed
// on the day of their first show, for the days they declared on only.
func TestCalendarCountsEachSessionOncePerDay(t *testing.T) {
	x := newFx(t, fxNow)
	rec := telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Chars: 10, Plen: 80}
	none := telemetry.Prompt{N: 2, Decision: "recall", Empty: "below_floor", Plen: 80}
	machine := telemetry.Prompt{N: 3, Decision: "recall", Reason: "machine", Injected: 1, Plen: 80}
	for _, p := range []struct {
		at      time.Time
		session string
		p       telemetry.Prompt
	}{
		{paris(20, 8, 0), "a", rec}, {paris(20, 8, 14), "a", none}, {paris(20, 8, 16), "a", rec}, {paris(20, 14, 0), "a", rec},
		{paris(20, 8, 10), "b", rec}, {paris(20, 23, 59), "a", none},
		{paris(21, 0, 0), "a", rec}, {paris(21, 9, 0), "a", rec}, {paris(21, 9, 1), "c", machine},
		{paris(22, 7, 0), "c", machine},
	} {
		x.calPrompt(p.at, p.session, "alpha", p.p)
	}
	x.declaredTurn(paris(20, 8, 1), "a", "declared", "used", "m1")
	x.declaredTurn(paris(21, 9, 2), "d", "declared", "used", "m1")
	x.declaredTurn(paris(21, 9, 3), "c", "undeclared", "used", "m1") // not declared: not counted

	by := map[string]CalendarDay{}
	for _, d := range x.run("days", x.query(7)).Tables.Calendar {
		by[d.Day] = d
	}
	for day, w := range map[string]CalendarDay{
		"2026-10-20": {Day: "2026-10-20", Prompts: 6, PromptsWithMemory: 4, UsedTurns: 1, Sessions: 2, Recorded: true},
		"2026-10-21": {Day: "2026-10-21", Prompts: 2, PromptsWithMemory: 2, UsedTurns: 1, Sessions: 2, Recorded: true},
		"2026-10-22": {Day: "2026-10-22", Sessions: 1, Recorded: true},
		"2026-10-23": {Day: "2026-10-23"},
	} {
		if by[day] != w {
			t.Errorf("%s = %+v, want %+v", day, by[day], w)
		}
	}
}

// TestOriginStates: where a memory came from, from the memory_doc record
// only: never a document id; the server is not asked.
func TestOriginStates(t *testing.T) {
	x := newFx(t, fxNow)
	ids := []string{"m-sA", "m-sB", "m-other", "m-note", "m-obs", "m-unknown"}
	led := feedback.Ledger{Memories: map[string]feedback.Entry{}}
	for _, id := range ids {
		led.Memories[id] = feedback.Entry{Shown: 1}
	}
	x.ledger(led)
	x.rcWhole("sA", rcBase, 5*time.Minute, 2*time.Minute, 60*time.Minute) // makes m-sA
	// sB: sent and extracted, its memory never shown
	x.rcPrompt(rcBase, "sB")
	x.rcSend(rcBase.Add(10*time.Minute), "sB")
	x.rcOp("sB", "completed", rcBase.Add(10*time.Minute+5*time.Second), rcBase.Add(13*time.Minute), 0, units(2))
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: "m-sB", Outcome: "found", DocKind: "session", Document: "conversation:sB", Session: "sB", CreatedDay: "2026-10-26", FactType: "world"})
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: "m-other", Outcome: "found", DocKind: "session", Document: "conversation:sElsewhere@proj", Session: "sElsewhere", CreatedDay: "2026-10-20", FactType: "world"})
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: "m-note", Outcome: "found", DocKind: "note", CreatedDay: "2026-10-21", FactType: "world"})
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: "m-obs", Outcome: "found", DocKind: "none", CreatedDay: "2026-10-22", FactType: "observation"})
	// An answer from sources is not a show: earlier than m-sA's first real
	// show it does not set first_shown, and m-sB has only such answers.
	x.shows(rcBase.Add(10*time.Minute), "src-s", "t1", "", "alpha", feedback.PathSources, "alpha", "m-sA", "m-sB")

	origin := func(id string) Origin {
		t.Helper()
		q := x.query(7)
		q.ID = id
		r := x.run("memories", q)
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "conversation:") {
			t.Errorf("%s: the result names a document id: %s", id, raw)
		}
		if r.Tables.Memory.Origin == nil {
			t.Fatalf("%s: no origin", id)
		}
		return *r.Tables.Memory.Origin
	}
	at := func(d time.Duration) *time.Time { v := rcBase.Add(d).UTC(); return &v }
	same := func(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }

	o := origin("m-sA")
	if o.State != "this_pc" || o.DocKind != "session" || o.Session != "sA" || o.Project != "alpha" ||
		!same(o.Ended, at(0)) || !same(o.Sent, at(5*time.Minute)) || !same(o.Extracted, at(7*time.Minute)) || !same(o.FirstShown, at(67*time.Minute)) || o.Created != nil {
		t.Errorf("this PC: %+v (ended %v sent %v extracted %v first shown %v)", o, o.Ended, o.Sent, o.Extracted, o.FirstShown)
	}
	o = origin("m-sB")
	if o.State != "this_pc" || o.FirstShown != nil || !same(o.Extracted, at(13*time.Minute)) {
		t.Errorf("never shown: %+v", o)
	}
	if o = origin("m-other"); o.State != "other_pc" || o.Session != "sElsewhere" || o.Created == nil || *o.Created != "2026-10-20" ||
		o.Ended != nil || o.Sent != nil || o.Extracted != nil || o.FirstShown != nil {
		t.Errorf("another PC: %+v", o)
	}
	if o = origin("m-note"); o.State != "no_session" || o.DocKind != "note" || o.Session != "" {
		t.Errorf("note: %+v", o)
	}
	if o = origin("m-obs"); o.State != "no_session" || o.DocKind != "none" {
		t.Errorf("observation: %+v", o)
	}
	if o = origin("m-unknown"); o.State != "unknown" || o.Session != "" || o.DocKind != "" {
		t.Errorf("no row: %+v", o)
	}
	x.server.failAll(errors.New("down")) // the origin does not depend on the server
	if o = origin("m-unknown"); o.State != "unknown" {
		t.Errorf("no row, server down: %+v", o)
	}
	if o = origin("m-other"); o.State != "other_pc" {
		t.Errorf("another PC, server down: %+v", o)
	}
	for _, s := range OriginStates {
		if s == "" {
			t.Error("empty origin state")
		}
	}
}
