package metrics

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The bank fixtures: the default window is the 7 days before fxNow
// (2026-10-30T12:00+01:00), from 2026-10-23T13:00+02:00 (11:00Z); Paris
// leaves summer time on 2026-10-25 at 03:00. The server's buckets are
// daily, at UTC midnight.

func bankOf(x *fx, q Query) map[string]Metric { return x.metrics(bankIDs, q) }

// utcDay is midnight UTC of 2026-10-d.
func utcDay(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }

func wantNoData(t *testing.T, id string, m Metric, reason string) {
	t.Helper()
	if m.Status != StatusNoData || m.Reason != reason {
		t.Errorf("%s: %s/%q, want no_data/%q", id, m.Status, m.Reason, reason)
	}
}

// TestBankSize: 900 + 300 + 41 = 1,241 nodes; under a project filter the
// value is that project's tag count (120), not tagged not_filtered, and
// the extras are null.
func TestBankSize(t *testing.T) {
	x := newFx(t, fxNow)
	x.server.set(func(d *fakeData) {
		d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": 900, "experience": 300, "observation": 41},
			TotalDocuments: 239, TotalLinks: 5000}
		d.Tags = []hindsight.TagCount{{Tag: "project:alpha", Count: 120}, {Tag: "project:beta", Count: 7}, {Tag: "kind:decision", Count: 3}}
	})
	m := bankOf(x, x.query(7))["bank.size"]
	if val(m) != 1241 || m.Status != StatusOK {
		t.Fatalf("size = %v (%s), want 1241 ok", m.Value, m.Status)
	}
	want := map[string]any{"world": int64(900), "experience": int64(300), "observation": int64(41), "documents": int64(239), "links": int64(5000)}
	if !reflect.DeepEqual(m.Extra, want) {
		t.Errorf("extra = %v, want %v", m.Extra, want)
	}
	if slices.Contains(m.Tags, tagNotFiltered) {
		t.Errorf("unfiltered size tagged %v", m.Tags)
	}

	q := x.query(7)
	q.Project = "alpha"
	m = bankOf(x, q)["bank.size"]
	if val(m) != 120 {
		t.Fatalf("size under --project alpha = %v, want 120", m.Value)
	}
	if slices.Contains(m.Tags, tagNotFiltered) {
		t.Errorf("a project's size is filtered, tags = %v", m.Tags)
	}
	for _, k := range bankSizeExtras {
		if v, ok := m.Extra[k]; !ok || v != nil {
			t.Errorf("extra %s = %v (present %v), want null", k, v, ok)
		}
	}
	q.Project = "gamma" // no tag: no memory
	if m = bankOf(x, q)["bank.size"]; val(m) != 0 || m.Status != StatusOK {
		t.Errorf("size of a project without tag = %v (%s), want 0 ok", m.Value, m.Status)
	}
}

// TestBankSizeHasNoPrevious: the server's figures are as of now, so the
// previous window reads nothing and there is no delta.
func TestBankSizeHasNoPrevious(t *testing.T) {
	x := newFx(t, fxNow)
	x.server.set(func(d *fakeData) {
		d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": 10}}
		d.Series = hindsight.Timeseries{Buckets: []hindsight.SeriesBucket{{Time: utcDay(26), World: 3}}}
	})
	for id, m := range bankOf(x, x.query(7)) {
		if m.Previous != nil || m.Delta != nil {
			t.Errorf("%s: previous %v delta %v, want none", id, m.Previous, m.Delta)
		}
	}
}

func calledPeriods(x *fx) []string {
	var out []string
	for _, c := range x.server.called() {
		if rest, ok := strings.CutPrefix(c, "MemoriesTimeseries?"); ok {
			for _, kv := range strings.Split(rest, "&") {
				if p, ok := strings.CutPrefix(kv, "period="); ok {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// TestBankGrowthPicksThePeriod: the smallest period covering the window
// (7d, 30d, then 90d with days_covered); a bucket before the window is
// dropped.
func TestBankGrowthPicksThePeriod(t *testing.T) {
	// 7 days, from 2026-10-23T11:00Z. 10-22 ends at 10-23T00:00Z, before the
	// window: dropped (world 1000). 10-23 straddles the start and counts
	// whole (world 7), in the first day. 10-24: world 14 + experience 7.
	// 10-25: observation 7. 10-30 (00:00Z, before the end): world 7.
	// Total 7 + 21 + 7 + 7 = 42 over the 8 daily buckets kept (10-23 to
	// 10-30, the zero days padded by the server): 42 / 8 = 5.25.
	x := newFx(t, fxNow)
	x.server.set(func(d *fakeData) {
		d.Series.Buckets = []hindsight.SeriesBucket{
			{Time: utcDay(22), World: 1000},
			{Time: utcDay(23), World: 7},
			{Time: utcDay(24), World: 14, Experience: 7},
			{Time: utcDay(25), Observation: 7},
			{Time: utcDay(26)}, {Time: utcDay(27)}, {Time: utcDay(28)}, {Time: utcDay(29)},
			{Time: utcDay(30), World: 7},
		}
	})
	m := bankOf(x, x.query(7))["bank.growth"]
	if val(m) != 5.25 || m.Status != StatusOK {
		t.Fatalf("growth = %v (%s), want 5.25 ok", m.Value, m.Status)
	}
	if got := calledPeriods(x); !reflect.DeepEqual(got, []string{"7d"}) {
		t.Errorf("periods asked = %v, want [7d]", got)
	}
	if !slices.Contains(m.Tags, tagSnapshot) {
		t.Errorf("growth tags = %v, want snapshot", m.Tags)
	}
	for k, want := range map[string]int64{"world": 28, "experience": 7, "observation": 7} { // 7 + 14 + 7; 7; 7
		if m.Extra[k] != want {
			t.Errorf("growth extra %s = %v, want %d", k, m.Extra[k], want)
		}
	}
	if _, ok := m.Extra["days_covered"]; ok {
		t.Errorf("days_covered on a 7-day window: %v", m.Extra)
	}
	// Daily, by kind, in local days: the window spans 8 local days from
	// 10-23 (07 world), 10-24 (14 + 7), 10-25 (7 obs) to 10-30 (7 world).
	if len(m.Series) != 8 {
		t.Fatalf("series has %d days, want 8: %+v", len(m.Series), m.Series)
	}
	wantDays := map[string]map[string]int64{
		"2026-10-23": {"world": 7, "experience": 0, "observation": 0},
		"2026-10-24": {"world": 14, "experience": 7, "observation": 0},
		"2026-10-25": {"world": 0, "experience": 0, "observation": 7},
		"2026-10-26": {"world": 0, "experience": 0, "observation": 0},
		"2026-10-30": {"world": 7, "experience": 0, "observation": 0},
	}
	var sum float64
	for _, p := range m.Series {
		sum += *p.Value
		if want, ok := wantDays[p.Day]; ok && !reflect.DeepEqual(p.Parts, want) {
			t.Errorf("%s parts = %v, want %v", p.Day, p.Parts, want)
		}
	}
	if sum != 42 {
		t.Errorf("series sums to %v, want the 42 of the value", sum)
	}
	if p := m.Series[1]; p.Day != "2026-10-24" || *p.Value != 21 {
		t.Errorf("second day = %+v, want 2026-10-24 value 21", p)
	}

	x = newFx(t, fxNow)
	bankOf(x, x.query(30))
	if got := calledPeriods(x); !reflect.DeepEqual(got, []string{"30d"}) {
		t.Errorf("30-day window asked %v, want [30d]", got)
	}

	// 100 days: the series reaches back 90 days, one world memory a day:
	// 90 / 90 days covered = 1.
	x = newFx(t, fxNow)
	x.server.set(func(d *fakeData) {
		for i := range 90 {
			d.Series.Buckets = append(d.Series.Buckets, hindsight.SeriesBucket{Time: utcDay(30).AddDate(0, 0, -i), World: 1})
		}
	})
	m = bankOf(x, x.query(100))["bank.growth"]
	if got := calledPeriods(x); !reflect.DeepEqual(got, []string{"90d"}) {
		t.Errorf("100-day window asked %v, want [90d]", got)
	}
	if m.Extra["days_covered"] != 90 || val(m) != 1 {
		t.Errorf("100-day growth = %v extra %v, want 1 with days_covered 90", m.Value, m.Extra)
	}
}

// TestBankGrowthWithoutBucketsHasNoData: a server answering no bucket in
// the window gives no 0.
func TestBankGrowthWithoutBucketsHasNoData(t *testing.T) {
	x := newFx(t, fxNow)
	wantNoData(t, "bank.growth", bankOf(x, x.query(7))["bank.growth"], ReasonNoEvents)
}

// TestByProjectCount: 250 project tags → 250, tagged not_filtered under a
// project filter (and not otherwise).
func TestByProjectCount(t *testing.T) {
	x := newFx(t, fxNow)
	x.server.set(func(d *fakeData) {
		for i := range 250 {
			d.Tags = append(d.Tags, hindsight.TagCount{Tag: "project:p" + strings.Repeat("x", i%7) + string(rune('a'+i/26)) + string(rune('a'+i%26)), Count: 1})
		}
		d.Tags = append(d.Tags, hindsight.TagCount{Tag: "kind:decision", Count: 9})
	})
	m := bankOf(x, x.query(7))["bank.by_project"]
	if val(m) != 250 {
		t.Fatalf("by_project = %v, want 250", m.Value)
	}
	if slices.Contains(m.Tags, tagNotFiltered) {
		t.Errorf("unfiltered tags = %v", m.Tags)
	}
	q := x.query(7)
	q.Project = "alpha"
	m = bankOf(x, q)["bank.by_project"]
	if val(m) != 250 || !slices.Contains(m.Tags, tagNotFiltered) {
		t.Errorf("under --project alpha: %v tags %v, want 250 not_filtered", m.Value, m.Tags)
	}
}

// TestConsolidationFailedWatches: failed 0 → ok; 2 → watch; the value is
// the pending count.
func TestConsolidationFailedWatches(t *testing.T) {
	last := time.Date(2026, 10, 30, 9, 0, 0, 0, fxParis)
	for _, c := range []struct {
		failed int
		want   Status
	}{{0, StatusOK}, {2, StatusWatch}} {
		x := newFx(t, fxNow)
		x.server.set(func(d *fakeData) {
			d.Stats = hindsight.BankStats{PendingConsolidation: 4, FailedConsolidation: c.failed, LastConsolidatedAt: last}
		})
		m := bankOf(x, x.query(7))["bank.consolidation"]
		if m.Status != c.want || val(m) != 4 {
			t.Errorf("failed %d: %s value %v, want %s 4", c.failed, m.Status, m.Value, c.want)
		}
		if m.Extra["failed"] != int64(c.failed) || m.Extra["pending"] != int64(4) {
			t.Errorf("failed %d: extra %v", c.failed, m.Extra)
		}
		if at, ok := m.Extra["last_consolidated_at"].(time.Time); !ok || !at.Equal(last) {
			t.Errorf("last_consolidated_at = %v, want %v", m.Extra["last_consolidated_at"], last)
		}
	}
	x := newFx(t, fxNow) // never consolidated: null, not the zero time
	if m := bankOf(x, x.query(7))["bank.consolidation"]; m.Extra["last_consolidated_at"] != nil {
		t.Errorf("last_consolidated_at = %v, want null", m.Extra["last_consolidated_at"])
	}
}

func (x *fx) retire(at time.Time, kind string) {
	x.t.Helper()
	x.journal(feedback.Event{TS: at, Kind: kind, Session: "s1", IDs: []string{"m-" + at.Format("150405") + kind}})
}

// TestChurnPerWeek: 2 forgets in W43, a hide and an unhide in W44 (the
// window is the two weeks W43 and W44: 10-23 is a Friday).
func TestChurnPerWeek(t *testing.T) {
	x := newFx(t, fxNow)
	x.retire(time.Date(2026, 10, 23, 14, 0, 0, 0, fxParis), "forget")  // W43
	x.retire(time.Date(2026, 10, 25, 23, 30, 0, 0, fxParis), "forget") // W43 (Sunday night, Paris time)
	x.retire(time.Date(2026, 10, 26, 0, 30, 0, 0, fxParis), "hide")    // W44 (Monday, 23:30Z Sunday)
	x.retire(time.Date(2026, 10, 29, 9, 0, 0, 0, fxParis), "unhide")   // W44
	x.retire(time.Date(2026, 10, 22, 9, 0, 0, 0, fxParis), "retire")   // before the window
	m := bankOf(x, x.query(7))["bank.churn"]
	if val(m) != 4 || m.Status != StatusOK {
		t.Fatalf("churn = %v (%s), want 4", m.Value, m.Status)
	}
	wantSeries := []Point{
		{Week: "2026-W43", Value: ptr(2.0), Parts: map[string]int64{"forget": 2}, Partial: true},
		{Week: "2026-W44", Value: ptr(2.0), Parts: map[string]int64{"hide": 1, "unhide": 1}, Partial: true},
	}
	if !reflect.DeepEqual(m.Series, wantSeries) {
		t.Errorf("series = %+v, want %+v", m.Series, wantSeries)
	}
	wantExtra := map[string]any{"forget": int64(2), "retire": int64(0), "hide": int64(1), "unhide": int64(1)}
	if !reflect.DeepEqual(m.Extra, wantExtra) {
		t.Errorf("extra = %v, want %v", m.Extra, wantExtra)
	}
}

// TestChurnWeeksWithoutEventsAreZero: a window over three ISO weeks draws
// all three, the middle one at 0.
func TestChurnWeeksWithoutEventsAreZero(t *testing.T) {
	x := newFx(t, fxNow)
	x.retire(time.Date(2026, 10, 15, 9, 0, 0, 0, fxParis), "retire") // W42
	x.retire(time.Date(2026, 10, 29, 9, 0, 0, 0, fxParis), "hide")   // W44
	m := bankOf(x, x.query(16))["bank.churn"]                        // from 10-14 13:00: W42, W43, W44
	var weeks []string
	var vals []float64
	for _, p := range m.Series {
		weeks, vals = append(weeks, p.Week), append(vals, *p.Value)
	}
	if !reflect.DeepEqual(weeks, []string{"2026-W42", "2026-W43", "2026-W44"}) || !reflect.DeepEqual(vals, []float64{1, 0, 1}) {
		t.Errorf("weeks %v values %v, want W42..W44 [1 0 1]", weeks, vals)
	}
}

// TestChurnWeeksFlagPartial: the ISO weeks the window starts or ends inside
// are partial (16 days from 10-14 13:00: W42 and W44, not W43); a window
// from Monday 00:00 to Monday 00:00 has no partial week.
func TestChurnWeeksFlagPartial(t *testing.T) {
	x := newFx(t, fxNow)
	x.retire(time.Date(2026, 10, 20, 9, 0, 0, 0, fxParis), "retire")
	partial := func(ps []Point) (out []string) {
		for _, p := range ps {
			if p.Partial {
				out = append(out, p.Week)
			}
		}
		return out
	}
	if got := partial(bankOf(x, x.query(16))["bank.churn"].Series); !reflect.DeepEqual(got, []string{"2026-W42", "2026-W44"}) {
		t.Errorf("16 days: partial weeks %v, want W42, W44", got)
	}
	mon := time.Date(2026, 10, 12, 0, 0, 0, 0, fxParis)
	w, err := NewWindow(fxNow, 0, mon, mon.AddDate(0, 0, 14), fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	m := bankOf(x, Query{Window: w, Machines: "this"})["bank.churn"]
	if got := partial(m.Series); len(m.Series) != 2 || len(got) != 0 {
		t.Errorf("Monday to Monday: %d weeks, partial %v; want 2, none", len(m.Series), got)
	}
}

// TestChurnUsesTheMemorysProject: under --project alpha a retirement counts
// by its memory's project (its latest show carrying one), whatever the
// session's; a memory never shown falls back to its session's project.
func TestChurnUsesTheMemorysProject(t *testing.T) {
	x := newFx(t, fxNow)
	at := time.Date(2026, 10, 27, 10, 0, 0, 0, fxParis)
	x.turnEvent(at.Add(-time.Hour), "sa", "alpha")
	x.turnEvent(at.Add(-time.Hour), "sb", "beta")
	// ma was shown in beta's session but belongs to alpha; mb the reverse.
	x.shows(at.Add(-30*time.Minute), "sb", "t1", "", "beta", "prompt", "alpha", "ma")
	x.shows(at.Add(-30*time.Minute), "sa", "t1", "", "alpha", "prompt", "beta", "mb")
	retire := func(at time.Time, kind, session, id string) {
		x.journal(feedback.Event{TS: at, Kind: kind, Session: session, IDs: []string{id}})
	}
	retire(at, "retire", "sb", "ma")                  // memory alpha, session beta -> alpha
	retire(at.Add(time.Minute), "hide", "sa", "mb")   // memory beta, session alpha -> beta
	retire(at.Add(2*time.Minute), "hide", "sa", "mc") // never shown, session alpha -> alpha
	retire(at.Add(3*time.Minute), "hide", "sb", "md") // never shown, session beta -> beta
	q := x.query(7)
	q.Project = "alpha"
	m := bankOf(x, q)["bank.churn"]
	if val(m) != 2 || m.Extra["retire"] != int64(1) || m.Extra["hide"] != int64(1) {
		t.Errorf("alpha churn = %v %v, want 2 (ma retired, mc hidden)", m.Value, m.Extra)
	}
	if m := bankOf(x, x.query(7))["bank.churn"]; val(m) != 4 {
		t.Errorf("all-project churn = %v, want 4", m.Value)
	}
}

// TestChurnWithoutJournalRows: no retirement row at all is no_events, and
// the feedback switch off is feedback_off.
func TestChurnWithoutJournalRows(t *testing.T) {
	x := newFx(t, fxNow)
	wantNoData(t, "bank.churn", bankOf(x, x.query(7))["bank.churn"], ReasonNoEvents)
	x = newFx(t, fxNow)
	x.cfg = func(c *config.Config) { c.Feedback.Enabled = false }
	wantNoData(t, "bank.churn", bankOf(x, x.query(7))["bank.churn"], ReasonFeedbackOff)
}

func (x *fx) store(at time.Time, result, kind, project string) {
	x.t.Helper()
	x.tel(at, telemetry.Store{Result: result, Kind: kind}, "s1", project)
}

// TestNotesStored: saved 3 (decision ×2, pitfall) + updated 1 = 4; the
// refused ones are counted by result and kind but not in the value.
func TestNotesStored(t *testing.T) {
	x := newFx(t, fxNow)
	base := fxNow.Add(-48 * time.Hour)
	x.store(base, "saved", "decision", "alpha")
	x.store(base.Add(time.Minute), "saved", "decision", "alpha")
	x.store(base.Add(2*time.Minute), "saved", "pitfall", "alpha")
	x.store(base.Add(3*time.Minute), "updated", "convention", "beta")
	x.store(base.Add(4*time.Minute), "refused", "decision", "alpha")
	x.store(base.Add(5*time.Minute), "refused", "state", "alpha")
	x.store(fxNow.Add(-10*24*time.Hour), "saved", "decision", "alpha") // before the window
	m := bankOf(x, x.query(7))["notes.stored"]
	if val(m) != 4 || m.Status != StatusOK {
		t.Fatalf("stored = %v (%s), want 4", m.Value, m.Status)
	}
	if want := map[string]int64{"saved": 3, "updated": 1, "refused": 2}; !reflect.DeepEqual(m.Extra["by_result"], want) {
		t.Errorf("by_result = %v, want %v", m.Extra["by_result"], want)
	}
	if want := map[string]int64{"decision": 3, "pitfall": 1, "convention": 1, "state": 1}; !reflect.DeepEqual(m.Extra["by_kind"], want) {
		t.Errorf("by_kind = %v, want %v", m.Extra["by_kind"], want)
	}
	q := x.query(7)
	q.Project = "beta"
	m = bankOf(x, q)["notes.stored"]
	if val(m) != 1 {
		t.Errorf("beta stored = %v, want 1", m.Value)
	}
	if want := map[string]int64{"saved": 0, "updated": 1, "refused": 0}; !reflect.DeepEqual(m.Extra["by_result"], want) {
		t.Errorf("beta by_result = %v, want %v", m.Extra["by_result"], want)
	}
	// The previous window (7 to 14 days ago) holds the one store event.
	if m := bankOf(x, x.query(7))["notes.stored"]; m.Previous == nil || *m.Previous != 1 {
		t.Errorf("previous = %v, want 1", m.Previous)
	}
}

// TestNotesStoredWithoutEvents: no store event at all is no_events.
func TestNotesStoredWithoutEvents(t *testing.T) {
	x := newFx(t, fxNow)
	wantNoData(t, "notes.stored", bankOf(x, x.query(7))["notes.stored"], ReasonNoEvents)
}

// TestOneEndpointMissingOrSlow: /stats answers, the time series is 404 and
// the project tags never answer: size is computed, growth reads
// server_unsupported, by_project server_down, and the server is OK.
func TestOneEndpointMissingOrSlow(t *testing.T) {
	lowerTimeouts(t, 10*time.Millisecond, time.Minute) // the fake blocks until the call's deadline
	x := newFx(t, fxNow)
	x.need = BankServerNeeds()
	x.server.set(func(d *fakeData) {
		d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": 5}, PendingConsolidation: 1}
	})
	x.server.fail("MemoriesTimeseries", &hindsight.APIError{Kind: hindsight.NotFound, Status: 404})
	x.server.block("ProjectTags")
	var st *ServerState
	var ms map[string]Metric
	err := withTx(t.Context(), x.source(), x.query(7), x.need, func(e env) error {
		st = e.server
		var err error
		ms, err = computeMetrics(e, e.q.Window, bankIDs)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if val(ms["bank.size"]) != 5 || ms["bank.size"].Status != StatusOK {
		t.Errorf("size = %v (%s), want 5 ok", ms["bank.size"].Value, ms["bank.size"].Status)
	}
	if val(ms["bank.consolidation"]) != 1 {
		t.Errorf("consolidation = %v, want 1", ms["bank.consolidation"].Value)
	}
	wantNoData(t, "bank.growth", ms["bank.growth"], ReasonServerUnsupported)
	wantNoData(t, "bank.by_project", ms["bank.by_project"], ReasonServerDown)
	if st == nil || !st.OK {
		t.Errorf("server state = %+v, want OK", st)
	}
	// A project size needs the tags, which failed: its own reason.
	q := x.query(7)
	q.Project = "alpha"
	var size Metric
	err = withTx(t.Context(), x.source(), q, x.need, func(e env) error {
		ms, err := computeMetrics(e, q.Window, []string{"bank.size"})
		size = ms["bank.size"]
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	wantNoData(t, "bank.size", size, ReasonServerDown)
}

// TestUnreachableServer: every server metric reads no_data with the
// server's reason; the machine's metrics are unaffected.
func TestUnreachableServer(t *testing.T) {
	x := newFx(t, fxNow)
	x.server.failAll(hindsight.ErrUnreachable)
	x.store(fxNow.Add(-time.Hour), "saved", "decision", "alpha")
	ms := bankOf(x, x.query(7))
	for _, id := range []string{"bank.size", "bank.growth", "bank.by_project", "bank.consolidation"} {
		wantNoData(t, id, ms[id], ReasonServerDown)
	}
	if ms["notes.stored"].Status != StatusOK {
		t.Errorf("notes.stored = %s, want ok", ms["notes.stored"].Status)
	}
	x = newFx(t, fxNow)
	x.server = nil // none configured
	wantNoData(t, "bank.size", bankOf(x, x.query(7))["bank.size"], ReasonServerDown)
}

// TestServerOnlyHost: the server's metrics are computed, the machine's
// (store events, journal) read server_only_host.
func TestServerOnlyHost(t *testing.T) {
	x := newFx(t, fxNow)
	x.cfg = func(c *config.Config) { c.Server.Role = config.RoleServer }
	x.server.set(func(d *fakeData) {
		d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": 8}}
	})
	ms := bankOf(x, x.query(7))
	if val(ms["bank.size"]) != 8 {
		t.Errorf("size = %v, want 8", ms["bank.size"].Value)
	}
	wantNoData(t, "notes.stored", ms["notes.stored"], ReasonServerOnlyHost)
	wantNoData(t, "bank.churn", ms["bank.churn"], ReasonServerOnlyHost)
}

// TestUnrequestedReadsAreNotZero: a view whose need list lacks a read gets
// server_unsupported for the metrics built on it, never an invented 0.
func TestUnrequestedReadsAreNotZero(t *testing.T) {
	run := func(need []string, project string) map[string]Metric {
		x := newFx(t, fxNow)
		x.server.set(func(d *fakeData) {
			d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": 5}, FailedConsolidation: 3}
			d.Tags = []hindsight.TagCount{{Tag: "project:alpha", Count: 2}}
			d.Series.Buckets = []hindsight.SeriesBucket{{Time: utcDay(26), World: 1}}
		})
		x.need = need
		q := x.query(7)
		q.Project = project
		return bankOf(x, q)
	}
	ms := run(nil, "")
	for _, id := range []string{"bank.size", "bank.growth", "bank.by_project", "bank.consolidation"} {
		wantNoData(t, id, ms[id], ReasonServerUnsupported)
	}
	ms = run([]string{"series", "tags"}, "") // no stats
	wantNoData(t, "bank.size", ms["bank.size"], ReasonServerUnsupported)
	wantNoData(t, "bank.consolidation", ms["bank.consolidation"], ReasonServerUnsupported)
	if val(ms["bank.by_project"]) != 1 || ms["bank.growth"].Status != StatusOK {
		t.Errorf("asked reads: by_project %v growth %s", ms["bank.by_project"].Value, ms["bank.growth"].Status)
	}
	ms = run([]string{"stats", "series"}, "") // no tags
	wantNoData(t, "bank.by_project", ms["bank.by_project"], ReasonServerUnsupported)
	ms = run([]string{"stats", "series"}, "alpha") // a project's size needs the tags
	wantNoData(t, "bank.size", ms["bank.size"], ReasonServerUnsupported)
	ms = run([]string{"tags"}, "alpha") // and only the tags
	if val(ms["bank.size"]) != 2 {
		t.Errorf("project size with only tags = %v, want 2", ms["bank.size"].Value)
	}
	ms = run([]string{"stats", "tags"}, "") // no series: unsupported, not no_events
	wantNoData(t, "bank.growth", ms["bank.growth"], ReasonServerUnsupported)
}

// TestSnapshotMetricsSayNoHistory: the server's figures carry the snapshot
// tag.
func TestSnapshotMetricsSayNoHistory(t *testing.T) {
	x := newFx(t, fxNow)
	x.server.set(func(d *fakeData) {
		d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": 5}}
		d.Series.Buckets = []hindsight.SeriesBucket{{Time: utcDay(26), World: 1}}
	})
	for _, id := range []string{"bank.size", "bank.growth", "bank.by_project", "bank.consolidation"} {
		if m := bankOf(x, x.query(7))[id]; !slices.Contains(m.Tags, tagSnapshot) {
			t.Errorf("%s tags = %v, want snapshot", id, m.Tags)
		}
	}
}
