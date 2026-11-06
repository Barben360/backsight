package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metricsfile"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The fixtures of pipeline.time_to_recall: a session is its last prompt
// (tel_prompt), its send (tel_retain, ok), its operations and memories
// (the memory-docs record, written as the collection writes it) and the
// shows of those memories. Times are Europe/Paris around fxNow
// (2026-10-30T12:00+01:00).

// rcBase is the day the sessions of the fixtures end on, well inside the
// default 7-day window.
var rcBase = time.Date(2026, 10, 26, 10, 0, 0, 0, fxParis)

// mrec appends records to the metrics files as written at ts.
func (x *fx) mrec(ts time.Time, recs ...metricsfile.Record) {
	x.t.Helper()
	w := metricsfile.New(x.dirs.MetricsDir(), x.binary)
	w.Now = func() time.Time { return ts }
	if err := w.Append(recs...); err != nil {
		x.t.Fatal(err)
	}
}

// rcPrompt is session's last prompt at at.
func (x *fx) rcPrompt(at time.Time, session string) {
	x.t.Helper()
	x.tel(at, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, session, "alpha")
}

// rcSend is the successful send of session's document at at.
func (x *fx) rcSend(at time.Time, session string) {
	x.t.Helper()
	x.tel(at, telemetry.Retain{Document: "conversation:" + session, Items: 1, Status: "ok", MS: 100}, session, "alpha")
}

// rcOp is an operation of session's document; units nil: not read yet.
func (x *fx) rcOp(session, status string, created, updated time.Time, retries int, units *int64) {
	x.t.Helper()
	x.mrec(updated, metricsfile.DocOp{Operation: "op-" + session + "-" + created.Format("150405"), Document: "conversation:" + session,
		Session: session, Status: status, CreatedAt: created, UpdatedAt: updated, Retries: retries, Units: units})
}

func units(n int64) *int64 { return &n }

// rcMemory records memory as made from session's document on day (a UTC
// date).
func (x *fx) rcMemory(session, memory, day string) {
	x.t.Helper()
	x.mrec(rcBase, metricsfile.MemoryDoc{Memory: memory, Outcome: "found", DocKind: "session", Document: "conversation:" + session,
		Session: session, CreatedDay: day, FactType: "world"})
}

// rcShow is a show of memory in another session at at.
func (x *fx) rcShow(at time.Time, memory string) {
	x.t.Helper()
	x.shows(at, "viewer", "t1", "", "alpha", "prompt", "", memory)
}

// rcWhole writes a session that reaches the end: last prompt at t0, sent
// sent after it, extracted ext after the send, its memory first shown
// shown after the extraction.
func (x *fx) rcWhole(session string, t0 time.Time, sent, ext, shown time.Duration) {
	x.t.Helper()
	x.rcPrompt(t0, session)
	t1 := t0.Add(sent)
	x.rcSend(t1, session)
	t2 := t1.Add(ext)
	x.rcOp(session, "completed", t1.Add(5*time.Second), t2, 0, units(4))
	x.rcMemory(session, "m-"+session, t0.UTC().Format("2006-01-02"))
	x.rcShow(t2.Add(shown), "m-"+session)
}

func ttr(x *fx, q Query) Metric {
	return x.metrics([]string{"pipeline.time_to_recall"}, q)["pipeline.time_to_recall"]
}

func funnelOf(t *testing.T, m Metric) map[string]int64 {
	t.Helper()
	f, ok := m.Extra["funnel"].(map[string]int64)
	if !ok {
		t.Fatalf("extra.funnel = %#v, want a map of counts", m.Extra["funnel"])
	}
	return f
}

func hopsOf(t *testing.T, m Metric) map[string]Hop {
	t.Helper()
	hs, ok := m.Extra["hops"].([]Hop)
	if !ok {
		t.Fatalf("extra.hops = %#v, want a list of hops", m.Extra["hops"])
	}
	out := map[string]Hop{}
	for _, h := range hs {
		out[h.Name] = h
	}
	return out
}

func wantHop(t *testing.T, hops map[string]Hop, name string, p50 float64, p95 *float64, n int64) {
	t.Helper()
	h, ok := hops[name]
	if !ok {
		t.Fatalf("no hop %q in %v", name, hops)
	}
	if h.P50 == nil || *h.P50 != p50 {
		t.Errorf("hop %s p50 = %v, want %v", name, h.P50, p50)
	}
	if (h.P95 == nil) != (p95 == nil) || (p95 != nil && *h.P95 != *p95) {
		t.Errorf("hop %s p95 = %v, want %v", name, h.P95, p95)
	}
	if h.N != n {
		t.Errorf("hop %s n = %d, want %d", name, h.N, n)
	}
}

func TestTimeToRecallHops(t *testing.T) {
	x := newFx(t, fxNow)
	for k := 1; k <= 20; k++ {
		m := time.Duration(k) * time.Minute
		x.rcWhole(fmt.Sprintf("sess-%d", k), rcBase, m, 2*m, 60*m)
	}
	m := ttr(x, x.query(7))
	if m.Status != StatusOK {
		t.Errorf("status %q, want ok", m.Status)
	}
	if m.Value == nil || *m.Value != 37_800_000 {
		t.Errorf("value %v, want 37 800 000 ms (630 min)", m.Value)
	}
	if m.Num == nil || *m.Num != 20 || m.Den == nil || *m.Den != 20 {
		t.Errorf("num/den %v/%v, want recalled 20 of sent 20", m.Num, m.Den)
	}
	if len(m.Series) != 0 {
		t.Errorf("series %v, want none", m.Series)
	}
	wantF := map[string]int64{"ended": 20, "sent": 20, "waiting_or_failed": 0, "extracted": 20, "never": 0, "on_server": 0,
		"recalled": 20, "not_recalled": 0, "unresolved": 0, "before_record": 0, "gone": 0}
	if f := funnelOf(t, m); fmt.Sprint(f) != fmt.Sprint(wantF) {
		t.Errorf("funnel %v, want %v", f, wantF)
	}
	for k := range funnelOf(t, m) {
		if !slices.Contains(FunnelKeys, k) {
			t.Errorf("funnel key %q is not in FunnelKeys", k)
		}
	}
	hops := hopsOf(t, m)
	if len(hops) != len(HopNames) {
		t.Errorf("%d hops, want %v", len(hops), HopNames)
	}
	mn := 60_000.0
	wantHop(t, hops, "last_to_sent", 10*mn, nil, 20)
	wantHop(t, hops, "sent_to_extracted", 20*mn, nil, 20)
	wantHop(t, hops, "extracted_to_recalled", 600*mn, nil, 20)
	wantHop(t, hops, "whole_way", 630*mn, nil, 20)
	if len(m.Tags) != 0 {
		t.Errorf("tags %v, want none", m.Tags)
	}
}

// edgeSessions writes the sessions of TestTimeToRecallEdgeSessions and
// returns the session of the memory shown before its extraction.
func edgeSessions(x *fx) {
	day := func(d int, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, fxParis) }
	// never sent
	x.rcPrompt(day(28, 10, 0), "never-sent")
	// sent 30 h ago, the operation failed three times
	x.rcPrompt(day(29, 5, 55), "failed")
	x.rcSend(day(29, 6, 0), "failed")
	x.rcOp("failed", "failed", day(29, 6, 0).Add(30*time.Second), day(29, 6, 5), 3, nil)
	// sent 2 h ago, nothing on the server yet
	x.rcPrompt(day(30, 8, 30), "recent")
	x.rcSend(day(30, 10, 0), "recent")
	// completed, with nothing worth keeping
	x.rcPrompt(day(27, 11, 0), "empty")
	x.rcSend(day(27, 11, 5), "empty")
	x.rcOp("empty", "completed", day(27, 11, 5).Add(10*time.Second), day(27, 11, 6), 0, units(0))
	// extracted, its memory shown before the extraction only
	x.rcPrompt(day(26, 9, 0), "early")
	x.rcSend(day(26, 9, 5), "early")
	x.rcOp("early", "completed", day(26, 9, 5).Add(10*time.Second), day(26, 9, 8), 0, units(4))
	x.rcMemory("early", "m-early", "2026-10-26")
	x.rcShow(day(26, 9, 7), "m-early")
	// an operation created 5 minutes before the send is another send's
	x.rcPrompt(day(26, 14, 0), "skew")
	x.rcSend(day(26, 14, 10), "skew")
	x.rcOp("skew", "completed", day(26, 14, 5), day(26, 14, 6), 0, units(3))
	// still open: not ended
	x.rcPrompt(day(30, 11, 50), "open")
}

func TestTimeToRecallEdgeSessions(t *testing.T) {
	x := newFx(t, fxNow)
	edgeSessions(x)
	m := ttr(x, x.query(7))
	want := map[string]int64{"ended": 6, "sent": 5, "waiting_or_failed": 1, "extracted": 1, "never": 3, "on_server": 1,
		"recalled": 0, "not_recalled": 1, "unresolved": 0, "before_record": 0, "gone": 0}
	if f := funnelOf(t, m); fmt.Sprint(f) != fmt.Sprint(want) {
		t.Errorf("funnel %v, want %v", f, want)
	}
	rows := x.run("pipeline", x.query(7)).Tables.NeverExtracted
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = fmt.Sprintf("%s/%s/%s/%d", r.Session, r.Project, r.State, r.Retries)
	}
	// newest send first
	wantRows := []string{"failed/alpha/failed/3", "empty/alpha/no_memory/0", "skew/alpha/none/0"}
	if fmt.Sprint(got) != fmt.Sprint(wantRows) {
		t.Errorf("never_extracted %v, want %v", got, wantRows)
	}
	if len(rows) > 0 && !rows[0].Sent.Equal(time.Date(2026, 10, 29, 6, 0, 0, 0, fxParis)) {
		t.Errorf("sent %v, want 2026-10-29 06:00 Paris", rows[0].Sent)
	}
	for _, r := range rows {
		if !slices.Contains(NeverStates, r.State) {
			t.Errorf("state %q is not in NeverStates", r.State)
		}
	}
}

// TestTimeToRecallUnderAProjectKeepsTheSessionsLastTurn: a session that ran
// in two projects is chosen by the project, but its last turn (T0) is its
// last prompt of any project: a send between its two prompts is not a send
// after the session's end, filtered or not.
func TestTimeToRecallUnderAProjectKeepsTheSessionsLastTurn(t *testing.T) {
	day := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, fxParis) }
	x := newFx(t, fxNow)
	x.tel(day(26, 10, 0), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "multi", "alpha")
	x.tel(day(26, 12, 0), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "multi", "beta")
	x.rcSend(day(26, 11, 0), "multi") // before the session's last prompt
	x.tel(day(27, 10, 0), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "only-beta", "beta")
	for _, project := range []string{"", "alpha"} {
		q := x.query(7)
		q.Project = project
		f := funnelOf(t, ttr(x, q))
		want := int64(2)
		if project != "" {
			want = 1 // only-beta is no session of alpha
		}
		if f["ended"] != want || f["sent"] != 0 {
			t.Errorf("project %q: ended %d sent %d, want ended %d sent 0", project, f["ended"], f["sent"], want)
		}
	}
}

// manyExtracted writes n sessions, session i sent a minute after its last
// prompt and extracted i minutes after the send.
func manyExtracted(x *fx, n int) {
	for i := 1; i <= n; i++ {
		s := fmt.Sprintf("sess-%d", i)
		x.rcPrompt(rcBase, s)
		x.rcSend(rcBase.Add(time.Minute), s)
		x.rcOp(s, "completed", rcBase.Add(time.Minute+5*time.Second), rcBase.Add(time.Minute+time.Duration(i)*time.Minute), 0, units(2))
	}
}

func TestTimeToRecallWatch(t *testing.T) {
	x := newFx(t, fxNow)
	manyExtracted(x, 100)
	m := ttr(x, x.query(7))
	if m.Status != StatusWatch {
		t.Errorf("100 sessions, p95 95 min: status %q, want watch", m.Status)
	}
	p95 := 95 * 60_000.0
	wantHop(t, hopsOf(t, m), "sent_to_extracted", 50*60_000, &p95, 100)

	y := newFx(t, fxNow)
	manyExtracted(y, 99)
	m = ttr(y, y.query(7))
	if m.Status != StatusOK {
		t.Errorf("99 sessions: status %q, want ok (no p95 under 100)", m.Status)
	}
	wantHop(t, hopsOf(t, m), "sent_to_extracted", 50*60_000, nil, 99)
}

func TestTimeToRecallTooFew(t *testing.T) {
	x := newFx(t, fxNow)
	for i := 1; i <= 19; i++ {
		s := fmt.Sprintf("sess-%d", i)
		if i <= 4 {
			x.rcWhole(s, rcBase, time.Minute, time.Minute, time.Minute)
			continue
		}
		x.rcPrompt(rcBase, s)
		x.rcSend(rcBase.Add(time.Minute), s)
		x.rcOp(s, "completed", rcBase.Add(time.Minute+5*time.Second), rcBase.Add(2*time.Minute), 0, units(2))
	}
	m := ttr(x, x.query(7))
	if m.Status != StatusTooFew {
		t.Errorf("status %q, want too_few (19 extracted)", m.Status)
	}
	if m.Value != nil {
		t.Errorf("value %v, want none: 4 sessions recalled", *m.Value)
	}
	if m.Num == nil || *m.Num != 4 || m.Den == nil || *m.Den != 19 {
		t.Errorf("num/den %v/%v, want 4/19", m.Num, m.Den)
	}
	if f := funnelOf(t, m); f["recalled"] != 4 || f["extracted"] != 19 {
		t.Errorf("funnel %v", f)
	}
}

func TestNeverExtractedFilter(t *testing.T) {
	x := newFx(t, fxNow)
	edgeSessions(x)
	q := x.query(7)
	q.Has = "never_extracted"
	rows := x.run("sessions", q).Tables.Sessions
	var got []string
	for _, r := range rows {
		got = append(got, r.ID)
	}
	slices.Sort(got)
	if want := []string{"empty", "failed", "skew"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sessions %v, want %v", got, want)
	}
	q.Has = "bogus"
	_, err := Run(context.Background(), "sessions", x.source(), q)
	if err == nil || !strings.Contains(err.Error(), "never_extracted") {
		t.Errorf("unknown --has: %v, want an error listing never_extracted", err)
	}
}

// TestNeverExtractedFilterIsNotCapped: the table keeps the 50 newest, the
// filter all of them.
func TestNeverExtractedFilterIsNotCapped(t *testing.T) {
	x := newFx(t, fxNow)
	x.rcOp("anchor", "completed", rcBase.Add(-time.Hour), rcBase.Add(-50*time.Minute), 0, units(1)) // the record began before the sends
	for i := range 55 {
		s := fmt.Sprintf("sess-%d", i)
		x.rcPrompt(rcBase, s)
		x.rcSend(rcBase.Add(time.Duration(i+1)*time.Minute), s)
	}
	if rows := x.run("pipeline", x.query(7)).Tables.NeverExtracted; len(rows) != 50 || rows[0].Session != "sess-54" {
		t.Errorf("table: %d rows, first %v, want the 50 newest", len(rows), rows)
	}
	q := x.query(7)
	q.Limit = 500
	q.Has = "never_extracted"
	if rows := x.run("sessions", q).Tables.Sessions; len(rows) != 55 {
		t.Errorf("filter: %d sessions, want all 55", len(rows))
	}
}

func TestOverviewCarriesTimeToRecall(t *testing.T) {
	with := newFx(t, fxNow)
	manyExtracted(with, 100)
	without := newFx(t, fxNow)
	for i := 1; i <= 100; i++ { // the same sessions and sends, no operation record
		s := fmt.Sprintf("sess-%d", i)
		without.rcPrompt(rcBase, s)
		without.rcSend(rcBase.Add(time.Minute), s)
	}
	a, b := with.run("overview", with.query(7)), without.run("overview", without.query(7))
	m, ok := a.Metrics["pipeline.time_to_recall"]
	if !ok {
		t.Fatal("the overview does not carry pipeline.time_to_recall")
	}
	if m.Status != StatusWatch {
		t.Errorf("status %q, want watch: otherwise this test proves nothing", m.Status)
	}
	if len(m.Series) != 0 {
		t.Errorf("series %v, want none", m.Series)
	}
	for _, c := range []struct {
		name     string
		got, ref any
	}{{"verdict", a.Verdict, b.Verdict}, {"attention", a.Attention, b.Attention}, {"more", a.AttentionMore, b.AttentionMore}} {
		g, _ := json.Marshal(c.got)
		r, _ := json.Marshal(c.ref)
		if string(g) != string(r) {
			t.Errorf("%s changes with the metric's rows:\n%s\n%s", c.name, g, r)
		}
	}
}

// coverage writes the days lists up to lastDay (UTC date), the first of
// them complete: the record covers the bank until lastDay.
func coverage(x *fx, first string, lastDay string) {
	d, _ := time.Parse("2006-01-02", first)
	end, _ := time.Parse("2006-01-02", lastDay)
	for ; !d.After(end); d = d.AddDate(0, 0, 1) {
		x.mrec(rcBase, metricsfile.Listed{Day: d.Format("2006-01-02"), N: 3, Complete: d.Equal(mustDay(first))})
	}
}

func mustDay(s string) time.Time {
	d, _ := time.Parse("2006-01-02", s)
	return d
}

func TestTimeToRecallUnresolved(t *testing.T) {
	day := func(d int, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, fxParis) }
	t.Run("the record is not complete", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.rcPrompt(day(28, 10, 0), "old")
		x.rcPrompt(fxNow.Add(-10*time.Minute), "open")
		x.rcShow(day(29, 10, 0), "u1")
		x.rcShow(day(29, 11, 0), "u2")
		m := ttr(x, x.query(7))
		f := funnelOf(t, m)
		if f["unresolved"] != 2 || f["gone"] != 0 {
			t.Errorf("unresolved %d gone %d, want 2 and 0", f["unresolved"], f["gone"])
		}
		if f["ended"] != 1 {
			t.Errorf("ended %d, want 1: a session 10 minutes old is still open", f["ended"])
		}
		if !slices.Contains(m.Tags, "partial") {
			t.Errorf("tags %v, want partial", m.Tags)
		}
	})
	t.Run("forgotten before its day was listed", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.rcPrompt(day(28, 10, 0), "old")
		coverage(x, "2026-10-20", "2026-10-29") // generated_at is on the 30th (UTC)
		x.rcShow(day(27, 10, 0), "g1")          // first shown 3 days before
		m := ttr(x, x.query(7))
		f := funnelOf(t, m)
		if f["gone"] != 1 || f["unresolved"] != 0 {
			t.Errorf("gone %d unresolved %d, want 1 and 0", f["gone"], f["unresolved"])
		}
		if slices.Contains(m.Tags, "partial") {
			t.Errorf("tags %v: a gone memory is no reason for partial", m.Tags)
		}
		// made after the listed days: unresolved
		x.rcShow(day(30, 9, 0), "u3")
		m = ttr(x, x.query(7))
		f = funnelOf(t, m)
		if f["gone"] != 1 || f["unresolved"] != 1 {
			t.Errorf("gone %d unresolved %d, want 1 and 1", f["gone"], f["unresolved"])
		}
		if !slices.Contains(m.Tags, "partial") {
			t.Errorf("tags %v, want partial", m.Tags)
		}
	})
	t.Run("an observation is never a recall", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.rcPrompt(rcBase, "s1")
		x.rcSend(rcBase.Add(time.Minute), "s1")
		x.rcOp("s1", "completed", rcBase.Add(time.Minute+5*time.Second), rcBase.Add(5*time.Minute), 0, units(2))
		x.mrec(rcBase, metricsfile.MemoryDoc{Memory: "obs-1", Outcome: "found", DocKind: "none", CreatedDay: "2026-10-26", FactType: "observation"})
		x.rcShow(rcBase.Add(time.Hour), "obs-1")
		m := ttr(x, x.query(7))
		f := funnelOf(t, m)
		if f["extracted"] != 1 || f["recalled"] != 0 || f["not_recalled"] != 1 {
			t.Errorf("funnel %v, want extracted 1, recalled 0", f)
		}
		if f["unresolved"] != 0 {
			t.Errorf("unresolved %d: the observation has a row", f["unresolved"])
		}
	})
}

// A memory shown before its own session was extracted does not count, even
// when another session was extracted earlier.
func TestTimeToRecallShowBeforeOwnExtraction(t *testing.T) {
	x := newFx(t, fxNow)
	x.rcWhole("early-bird", rcBase, time.Minute, time.Minute, 4*time.Hour) // extracted 10:02, its memory shown 14:02
	late := rcBase.Add(2 * time.Hour)
	x.rcPrompt(late, "late")
	x.rcSend(late.Add(time.Minute), "late")
	x.rcOp("late", "completed", late.Add(time.Minute+5*time.Second), late.Add(2*time.Minute), 0, units(2)) // extracted 12:02
	x.rcMemory("late", "m-late", "2026-10-26")
	x.rcShow(rcBase.Add(3*time.Hour), "m-late") // 13:00, after its own extraction (12:02)
	f := funnelOf(t, ttr(x, x.query(7)))
	if f["recalled"] != 2 {
		t.Fatalf("recalled %d, want 2 (the control: both memories shown after their extraction)", f["recalled"])
	}
	y := newFx(t, fxNow)
	y.rcWhole("early-bird", rcBase, time.Minute, time.Minute, 4*time.Hour)
	y.rcPrompt(late, "late")
	y.rcSend(late.Add(time.Minute), "late")
	y.rcOp("late", "completed", late.Add(time.Minute+5*time.Second), late.Add(2*time.Minute), 0, units(2))
	y.rcMemory("late", "m-late", "2026-10-26")
	y.rcShow(rcBase.Add(time.Hour+30*time.Minute), "m-late") // 11:30, after the other extraction, before its own (12:02)
	f = funnelOf(t, ttr(y, y.query(7)))
	if f["recalled"] != 1 || f["not_recalled"] != 1 {
		t.Errorf("funnel %v, want one recalled and one not", f)
	}
}

// A send from before the operations record began says nothing about what
// became of it: unresolved, never never.
func TestTimeToRecallBeforeTheOperationsRecord(t *testing.T) {
	day := func(d int, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, fxParis) }
	t.Run("no record at all", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.rcPrompt(day(27, 9, 55), "old")
		x.rcSend(day(27, 10, 0), "old") // 3 days ago
		m := ttr(x, x.query(7))
		f := funnelOf(t, m)
		if f["never"] != 0 || f["before_record"] != 1 || f["unresolved"] != 0 || f["sent"] != 1 {
			t.Errorf("funnel %v, want never 0, before_record 1, unresolved 0", f)
		}
		if !slices.Contains(m.Tags, "partial") {
			t.Errorf("tags %v, want partial", m.Tags)
		}
		if rows := x.run("pipeline", x.query(7)).Tables.NeverExtracted; len(rows) != 0 {
			t.Errorf("rows %v, want none", rows)
		}
		q := x.query(7)
		q.Has = "never_extracted"
		if rows := x.run("sessions", q).Tables.Sessions; len(rows) != 0 {
			t.Errorf("--has never_extracted lists %v, want none", rows)
		}
	})
	t.Run("a send before the record's first line, and one after", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.rcPrompt(day(26, 9, 55), "before")
		x.rcSend(day(26, 10, 0), "before")
		x.rcPrompt(day(28, 9, 55), "after")
		x.rcSend(day(28, 10, 0), "after")
		// the record's first line is written on the 27th, for another session
		x.rcOp("other", "completed", day(27, 8, 0), day(27, 8, 5), 0, units(1))
		m := ttr(x, x.query(7))
		f := funnelOf(t, m)
		if f["never"] != 1 || f["before_record"] != 1 {
			t.Errorf("funnel %v, want never 1 (after) and before_record 1 (before)", f)
		}
		rows := x.run("pipeline", x.query(7)).Tables.NeverExtracted
		if len(rows) != 1 || rows[0].Session != "after" || rows[0].State != "none" {
			t.Errorf("rows %v, want only the session sent after the record began", rows)
		}
	})
}

// The edges of the two timings: an operation created 120 s before the send
// belongs to it, 121 s does not; a send exactly 24 h old is never, one
// 23 h 59 min old is still on the server.
func TestTimeToRecallBoundaries(t *testing.T) {
	anchor := func(x *fx) {
		x.rcOp("anchor", "completed", rcBase.Add(-time.Hour), rcBase.Add(-50*time.Minute), 0, units(1))
	}
	for _, c := range []struct {
		skew      time.Duration
		extracted int64
	}{{120 * time.Second, 1}, {121 * time.Second, 0}} {
		x := newFx(t, fxNow)
		anchor(x)
		x.rcPrompt(rcBase, "s1")
		t1 := rcBase.Add(10 * time.Minute)
		x.rcSend(t1, "s1")
		x.rcOp("s1", "completed", t1.Add(-c.skew), t1.Add(time.Minute), 0, units(2))
		if f := funnelOf(t, ttr(x, x.query(7))); f["extracted"] != c.extracted {
			t.Errorf("operation %v before the send: extracted %d, want %d", c.skew, f["extracted"], c.extracted)
		}
	}
	for _, c := range []struct {
		age             time.Duration
		never, onServer int64
	}{{24 * time.Hour, 1, 0}, {24*time.Hour - time.Minute, 0, 1}} {
		x := newFx(t, fxNow)
		x.rcOp("anchor", "completed", fxNow.Add(-48*time.Hour), fxNow.Add(-47*time.Hour), 0, units(1))
		t1 := fxNow.Add(-c.age)
		x.rcPrompt(t1.Add(-5*time.Minute), "s1")
		x.rcSend(t1, "s1")
		f := funnelOf(t, ttr(x, x.query(7)))
		if f["never"] != c.never || f["on_server"] != c.onServer {
			t.Errorf("sent %v ago: never %d on_server %d, want %d and %d", c.age, f["never"], f["on_server"], c.never, c.onServer)
		}
	}
}

// rcOpID is rcOp with the operation's id given.
func (x *fx) rcOpID(id, session, status string, created, updated time.Time, ts time.Time, units *int64) {
	x.t.Helper()
	x.mrec(ts, metricsfile.DocOp{Operation: id, Document: "conversation:" + session, Session: session, Status: status,
		CreatedAt: created, UpdatedAt: updated, Units: units})
}

// Two operations created at the same instant: the one written first, then
// the one with the lower id, is the first (not whichever a map yields).
func TestTimeToRecallOperationTies(t *testing.T) {
	x := newFx(t, fxNow)
	for i := 1; i <= 20; i++ {
		s := fmt.Sprintf("sess-%d", i)
		x.rcPrompt(rcBase, s)
		t1 := rcBase.Add(time.Minute)
		x.rcSend(t1, s)
		at := t1.Add(5 * time.Second)
		// written at the same time; op-b sorts after op-a and updates later
		x.rcOpID("op-b-"+s, s, "completed", at, t1.Add(9*time.Minute), t1.Add(10*time.Minute), units(2))
		x.rcOpID("op-a-"+s, s, "completed", at, t1.Add(time.Minute), t1.Add(10*time.Minute), units(2))
	}
	for range 10 {
		hops := hopsOf(t, ttr(x, x.query(7)))
		if h := hops["sent_to_extracted"]; h.P50 == nil || *h.P50 != 60_000 {
			t.Fatalf("sent_to_extracted p50 %v, want 60 000 (op-a's update)", h.P50)
		}
	}
}

// appendRaw appends a hand-written line to the memory-docs month file.
func (x *fx) appendRaw(month, line string) {
	x.t.Helper()
	if err := os.MkdirAll(x.dirs.MetricsDir(), 0o700); err != nil {
		x.t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(x.dirs.MetricsDir(), "memory-docs-"+month+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		x.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		x.t.Fatal(err)
	}
}

// An operation is what its latest line says: when that line has no
// creation time, an older line's does not stand in.
func TestTimeToRecallLatestOperationLineDecides(t *testing.T) {
	x := newFx(t, fxNow)
	x.rcOp("anchor", "completed", rcBase.Add(-time.Hour), rcBase.Add(-50*time.Minute), 0, units(1))
	x.rcPrompt(rcBase, "s1")
	t1 := rcBase.Add(time.Minute)
	x.rcSend(t1, "s1")
	x.rcOpID("o1", "s1", "completed", t1.Add(5*time.Second), t1.Add(time.Minute), t1.Add(time.Minute), units(3))
	// a later line of the same operation that does not give its times
	later := t1.Add(2 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")
	x.appendRaw("2026-10", `{"schema":1,"ts":"`+later+`","kind":"op","v":"v1","binary_version":"1","operation":"o1","document":"conversation:s1","session":"s1","status":"completed","retries":0}`)
	f := funnelOf(t, ttr(x, x.query(7)))
	if f["extracted"] != 0 || f["never"] != 1 {
		t.Errorf("funnel %v, want extracted 0 and never 1: the latest line has no creation time", f)
	}
}

// A memory listed twice belongs to the document of its latest line.
func TestTimeToRecallLatestMemoryLineDecides(t *testing.T) {
	x := newFx(t, fxNow)
	for _, s := range []string{"s1", "s2"} {
		x.rcPrompt(rcBase, s)
		t1 := rcBase.Add(time.Minute)
		x.rcSend(t1, s)
		done := t1.Add(2 * time.Minute) // s2 is extracted before the show, s1 after it
		if s == "s1" {
			done = t1.Add(3 * time.Hour)
		}
		x.rcOp(s, "completed", t1.Add(5*time.Second), done, 0, units(2))
	}
	doc := func(s string) metricsfile.MemoryDoc {
		return metricsfile.MemoryDoc{Memory: "m1", Outcome: "found", DocKind: "session", Document: "conversation:" + s, Session: s,
			CreatedDay: "2026-10-26", FactType: "world"}
	}
	x.mrec(rcBase, doc("s2"))                   // first listed as s2's
	x.mrec(rcBase.Add(24*time.Hour), doc("s1")) // later listed as s1's: the one that stands
	x.rcShow(rcBase.Add(time.Hour), "m1")
	f := funnelOf(t, ttr(x, x.query(7)))
	if f["extracted"] != 2 || f["recalled"] != 0 {
		t.Errorf("funnel %v, want nobody recalled: m1 is s1's, shown before s1's extraction", f)
	}
}

// The verdict computes none of the yield, the time to recall and the
// doctor's history, which no rule reads (each reads the day record, the
// telemetry or the doctor's runs): the overview makes the time to recall
// itself, the cost view the yield. The other metrics are there, and the
// earlier window of those a rule judges over two weeks.
func TestVerdictLeavesOutYieldAndEarlierTimeToRecall(t *testing.T) {
	x := newFx(t, fxNow)
	x.rcPrompt(rcBase, "s1")
	a := x.assess(x.query(7))
	for _, id := range []string{yieldID, timeToRecallID, doctorHistoryID} { // no rule reads them
		if _, ok := a.Now[id]; ok {
			t.Errorf("the verdict computed %s", id)
		}
		if _, ok := a.Prev[id]; ok {
			t.Errorf("the verdict computed the earlier window of %s", id)
		}
	}
	if _, ok := a.Now["recall.hit_rate"]; !ok { // the control: a judged metric is there
		t.Errorf("the verdict lacks recall.hit_rate")
	}
	if _, ok := a.Prev["recall.hit_rate"]; !ok { // the control: a metric judged over two weeks is there
		t.Errorf("the verdict lacks the earlier window of recall.hit_rate")
	}
	// The overview still shows the time to recall, made beside the verdict.
	if _, ok := x.run("overview", x.query(7)).Metrics[timeToRecallID]; !ok {
		t.Errorf("the overview lacks %s", timeToRecallID)
	}
}
