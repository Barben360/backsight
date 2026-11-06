package metrics

import (
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/metricsfile"
)

const (
	secretsMetric = "secrets.suspects"
	doctorMetric  = "doctor.history"
)

func parisDay(month time.Month, d, h int) time.Time {
	return time.Date(2026, month, d, h, 0, 0, 0, fxParis)
}

// scan records a complete scan written at ts.
func (x *fx) scan(ts time.Time, count int64) {
	x.t.Helper()
	x.mrec(ts, metricsfile.Scan{Count: count, Live: count, Forgotten: 0, Pages: 3, MS: 1200})
}

// TestSecretsStatus: the last scan's count, any window; watch from one
// suspect, immediately on Flowing, with the item and banner reason words.
func TestSecretsStatus(t *testing.T) {
	t.Run("no scan", func(t *testing.T) {
		x := newFx(t, fxNow)
		m := x.metrics([]string{secretsMetric}, x.query(7))[secretsMetric]
		if m.Status != StatusNoData || m.Reason != ReasonNoEvents || m.Value != nil {
			t.Errorf("no scan: status %q reason %q value %v, want no_data no_events nil", m.Status, m.Reason, m.Value)
		}
		a := x.assess(x.query(7))
		wantAxes(t, "no scan", a.Verdict, axes{early, early, early, okay})
		if len(a.Items) != 0 || len(a.Verdict.Axes["flowing"].Reasons) != 0 {
			t.Errorf("no scan: items %+v, flowing reasons %+v, want none", a.Items, a.Verdict.Axes["flowing"].Reasons)
		}
	})
	t.Run("last scan found none", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.scan(parisDay(10, 28, 14), 0)
		m := x.metrics([]string{secretsMetric}, x.query(7))[secretsMetric]
		wantFloat(t, "value", m.Value, 0)
		if m.Status != StatusOK || len(m.Tags) != 0 {
			t.Errorf("status %q tags %v, want ok, no tags", m.Status, m.Tags)
		}
		wantAxes(t, "count 0", x.assess(x.query(7)).Verdict, axes{early, early, early, okay})
	})
	t.Run("two suspects", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.scan(parisDay(10, 28, 14), 2)
		m := x.metrics([]string{secretsMetric}, x.query(7))[secretsMetric]
		wantFloat(t, "value", m.Value, 2)
		if m.Status != StatusWatch {
			t.Errorf("status %q, want watch", m.Status)
		}
		for k, want := range map[string]int64{"ms": 1200, "live": 2, "forgotten": 0, "pages": 3, "age_days": 2} {
			if got, ok := m.Extra[k].(int64); !ok || got != want {
				t.Errorf("extra %s = %v, want %d", k, m.Extra[k], want)
			}
		}
		if at, ok := m.Extra["scanned_at"].(time.Time); !ok || !at.Equal(parisDay(10, 28, 14)) {
			t.Errorf("scanned_at = %v, want %v", m.Extra["scanned_at"], parisDay(10, 28, 14))
		}
		a := x.assess(x.query(7))
		wantAxes(t, "two suspects", a.Verdict, axes{early, early, early, attn})
		wantItem := Item{Metric: secretsMetric, Severity: SeverityWatch, Axis: "flowing", Text: "Possible secrets in memory: run backsight memory scan.",
			Why:     "The last scan, 2 days ago, found 2 memories that look like a key, token or password. Memory is shown to Claude in later sessions; the scan says which ones and how to forget them.",
			Command: "backsight memory scan"}
		if len(a.Items) != 1 {
			t.Fatalf("items = %+v, want one", a.Items)
		}
		got := a.Items[0]
		if got.Metric != wantItem.Metric || got.Severity != wantItem.Severity || got.Axis != wantItem.Axis || got.Text != wantItem.Text || got.Why != wantItem.Why || got.Command != wantItem.Command {
			t.Errorf("item = %+v, want %+v", got, wantItem)
		}
		want := Reason{Metric: secretsMetric, Severity: SeverityWatch, Text: "2 possible secrets in memory (scan of Oct 28).", Command: "backsight memory scan"}
		if rs := a.Verdict.Axes["flowing"].Reasons; len(rs) != 1 || rs[0] != want {
			t.Errorf("flowing reasons = %+v, want %+v", rs, want)
		}
	})
	t.Run("one suspect says possible secret", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.scan(parisDay(10, 30, 9), 1)
		a := x.assess(x.query(7))
		rs := a.Verdict.Axes["flowing"].Reasons
		if len(rs) != 1 || rs[0].Text != "1 possible secret in memory (scan of Oct 30)." {
			t.Errorf("reasons = %+v, want the singular", rs)
		}
		if len(a.Items) != 1 || a.Items[0].Why != "The last scan, today, found 1 memory that looks like a key, token or password. Memory is shown to Claude in later sessions; the scan says which ones and how to forget them." {
			t.Errorf("items = %+v, want the singular why", a.Items)
		}
	})
	t.Run("a later scan wins, whatever its window", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.scan(parisDay(10, 20, 14), 2)
		x.scan(parisDay(10, 28, 14), 0)
		for _, days := range []int{1, 7, 90} {
			m := x.metrics([]string{secretsMetric}, x.query(days))[secretsMetric]
			wantFloat(t, "value", m.Value, 0)
			if m.Status != StatusOK {
				t.Errorf("%d days: status %q, want ok after the later clean scan", days, m.Status)
			}
		}
		wantAxes(t, "later scan", x.assess(x.query(7)).Verdict, axes{early, early, early, okay})
	})
	t.Run("an old scan is a tag, not a status", func(t *testing.T) {
		x := newFx(t, fxNow)
		x.scan(fxNow.AddDate(0, 0, -41), 0)
		m := x.metrics([]string{secretsMetric}, x.query(7))[secretsMetric]
		if m.Status != StatusOK || !slices.Contains(m.Tags, "old_scan") {
			t.Errorf("status %q tags %v, want ok tagged old_scan", m.Status, m.Tags)
		}
		if got, _ := m.Extra["age_days"].(int64); got != 41 {
			t.Errorf("age_days = %v, want 41", m.Extra["age_days"])
		}
		wantAxes(t, "old scan", x.assess(x.query(7)).Verdict, axes{early, early, early, okay})
		x2 := newFx(t, fxNow)
		x2.scan(fxNow.AddDate(0, 0, -30), 0)
		if m := x2.metrics([]string{secretsMetric}, x2.query(7))[secretsMetric]; slices.Contains(m.Tags, "old_scan") {
			t.Errorf("a 30-day-old scan is tagged old_scan, want it only past 30 days")
		}
	})
}

// TestFlowingRootCauseStillCollapses: a server that does not answer gives
// Flowing its root-cause reason first; the secrets reason stays beside it.
func TestFlowingRootCauseStillCollapses(t *testing.T) {
	x := newFx(t, fxNow)
	x.server.failAll(hindsight.ErrUnreachable)
	x.scan(parisDay(10, 28, 14), 2)
	a := x.assess(x.query(7))
	wantAxes(t, "down and secrets", a.Verdict, axes{early, early, early, attn})
	rs := a.Verdict.Axes["flowing"].Reasons
	if len(rs) != 2 || rs[0].Metric != "server.reach" || rs[1].Metric != secretsMetric || rs[1].Text != "2 possible secrets in memory (scan of Oct 28)." {
		t.Errorf("flowing reasons = %+v, want the root cause then the secrets reason", rs)
	}
}

// doctorRun records a doctor run written at ts with the checks' statuses.
func (x *fx) doctorRun(ts time.Time, checks map[string]string) {
	x.t.Helper()
	x.mrec(ts, metricsfile.DoctorRun{Checks: checks})
}

// octWindow is the 30 local days of October 1 to 30 (Paris), whole.
func (x *fx) octWindow() Query {
	x.t.Helper()
	w, err := NewWindow(fxNow, 0, parisDay(10, 1, 0), parisDay(10, 31, 0), fxParis, "Europe/Paris")
	if err != nil {
		x.t.Fatal(err)
	}
	return Query{Window: w, Machines: "this"}
}

func doctorHistoryOf(t *testing.T, x *fx, q Query) (Metric, []DoctorHistoryRow) {
	t.Helper()
	r := x.run("system", q)
	return r.Metrics[doctorMetric], r.Tables.DoctorHistory
}

func rowOf(t *testing.T, rows []DoctorHistoryRow, check string) DoctorHistoryRow {
	t.Helper()
	for _, r := range rows {
		if r.Check == check {
			return r
		}
	}
	t.Fatalf("no row for %q in %+v", check, rows)
	return DoctorHistoryRow{}
}

// TestDoctorHistory: a run a day for 26 of October's 30 days, Backups ok
// for 20 days then warning for 6.
func TestDoctorHistory(t *testing.T) {
	x := newFx(t, fxNow)
	for d := 1; d <= 26; d++ {
		st := "ok"
		if d > 20 {
			st = "warn"
		}
		x.doctorRun(parisDay(10, d, 8), map[string]string{"backup": st, "server": "info"})
	}
	m, rows := doctorHistoryOf(t, x, x.octWindow())
	wantFloat(t, "runs", m.Value, 26)
	wantInt(t, "num", m.Num, 26)
	if m.Status != StatusOK {
		t.Errorf("status %q, want ok (no status of its own)", m.Status)
	}
	if got, _ := m.Extra["days_without"].(int64); got != 4 {
		t.Errorf("days_without = %v, want 4", m.Extra["days_without"])
	}
	if len(rows) != 2 || rows[0].Check != "backup" || rows[1].Check != "server" {
		t.Fatalf("rows = %+v, want backup (it has a non-ok day) then server", rows)
	}
	b := rows[0]
	if b.Now != "watch" || b.OK != 20 || b.Watch != 6 || b.Bad != 0 {
		t.Errorf("backup = now %q ok %d watch %d bad %d, want watch 20 6 0", b.Now, b.OK, b.Watch, b.Bad)
	}
	if b.LastChange == nil || *b.LastChange != (DoctorChange{Day: "2026-10-21", From: "ok", To: "watch"}) {
		t.Errorf("last_change = %+v, want 2026-10-21 ok to watch", b.LastChange)
	}
	if len(b.Days) != 30 || b.Days[0] != (DoctorDay{Day: "2026-10-01", Status: "ok"}) || b.Days[20] != (DoctorDay{Day: "2026-10-21", Status: "watch"}) ||
		b.Days[26] != (DoctorDay{Day: "2026-10-27", Status: "none"}) || b.Days[29] != (DoctorDay{Day: "2026-10-30", Status: "none"}) {
		t.Errorf("days = %+v", b.Days)
	}
	if s := rowOf(t, rows, "server"); s.Now != "ok" || s.OK != 26 || s.LastChange != nil {
		t.Errorf("server = %+v, want ok 26 days, no change (info reads ok)", s)
	}
}

// A day with two runs takes the last; an error reads bad; a check that
// joined later is none before; the history is the same on the plain page.
func TestDoctorHistoryDayRules(t *testing.T) {
	x := newFx(t, fxNow)
	x.doctorRun(parisDay(10, 29, 8), map[string]string{"disk": "error"})
	x.doctorRun(parisDay(10, 29, 18), map[string]string{"disk": "ok"})
	x.doctorRun(parisDay(10, 30, 8), map[string]string{"disk": "error", "gpu": "warn"})
	m, rows := doctorHistoryOf(t, x, x.octWindow())
	wantFloat(t, "runs", m.Value, 3)
	d := rowOf(t, rows, "disk")
	if d.Days[28] != (DoctorDay{Day: "2026-10-29", Status: "ok"}) || d.Days[29].Status != "bad" || d.OK != 1 || d.Bad != 1 || d.Now != "bad" {
		t.Errorf("disk = %+v, want the last run of the 29th (ok) then bad", d)
	}
	if d.LastChange == nil || *d.LastChange != (DoctorChange{Day: "2026-10-30", From: "ok", To: "bad"}) {
		t.Errorf("disk last_change = %+v", d.LastChange)
	}
	if g := rowOf(t, rows, "gpu"); g.Days[28].Status != "none" || g.Days[29].Status != "watch" || g.LastChange != nil {
		t.Errorf("gpu = %+v, want none the 29th, watch the 30th, no change (first run)", g)
	}
	if got, _ := m.Extra["days_without"].(int64); got != 28 {
		t.Errorf("days_without = %v, want 28", m.Extra["days_without"])
	}
	// runs outside the window do not count
	x.doctorRun(parisDay(9, 30, 23), map[string]string{"disk": "error"})
	m, _ = doctorHistoryOf(t, x, x.octWindow())
	wantFloat(t, "runs with one before the window", m.Value, 3)
}

func TestDoctorHistoryNoRun(t *testing.T) {
	x := newFx(t, fxNow)
	m, rows := doctorHistoryOf(t, x, x.octWindow())
	if m.Status != StatusNoData || m.Reason != ReasonNoEvents {
		t.Errorf("no run: %q %q, want no_data no_events", m.Status, m.Reason)
	}
	if rows == nil || len(rows) != 0 {
		t.Errorf("rows = %#v, want an empty, non-nil list", rows)
	}
}
