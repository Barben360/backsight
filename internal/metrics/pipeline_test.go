package metrics

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The pipeline family's fixtures. Window: the 7 days before fxNow
// (2026-10-30T12:00+01:00). Every number below is counted by hand in the
// test's comment.

func pipeFx(t *testing.T) *fx {
	x := newFx(t, fxNow)
	x.need = PipelineServerNeeds()
	return x
}

func pipeOf(x *fx, q Query) map[string]Metric { return x.metrics(pipelineIDs, q) }

// retained writes a retain event for document doc at ts.
func (x *fx) retained(ts time.Time, doc, status string, ms int) {
	x.t.Helper()
	x.tel(ts, telemetry.Retain{Document: doc, Items: 1, Status: status, MS: ms}, "s-"+doc, "alpha")
}

func extraInt(t *testing.T, m Metric, key string) int64 {
	t.Helper()
	v, ok := m.Extra[key].(int64)
	if !ok {
		t.Fatalf("extra.%s = %#v, want an int64", key, m.Extra[key])
	}
	return v
}

func wantStatus(t *testing.T, id string, m Metric, want Status) {
	t.Helper()
	if m.Status != want {
		t.Errorf("%s: %s (%q), want %s", id, m.Status, m.Reason, want)
	}
}

// TestQueuedThenOkCountsOnce: d1 queued then ok (ok), d2 queued (waiting),
// d3 ok then failed (failed), d4 quarantined (set aside): 4 documents, 1 ok.
// d2's wait began 3 h ago, so the backlog is 10,800,000 ms: watch, not bad.
// All four last events fall on 2026-10-30 (Paris), the day's point.
func TestQueuedThenOkCountsOnce(t *testing.T) {
	x := pipeFx(t)
	x.retained(ago(5*time.Hour), "d1", "queued", 0)
	x.retained(ago(4*time.Hour), "d1", "ok", 100)
	x.retained(ago(3*time.Hour), "d2", "queued", 0)
	x.retained(ago(6*time.Hour), "d3", "ok", 100)
	x.retained(ago(2*time.Hour), "d3", "failed", 100)
	x.retained(ago(time.Hour), "d4", "quarantined", 0)
	m := pipeOf(x, x.query(7))["pipeline.flow"]
	if m.Num == nil || *m.Num != 1 || m.Den == nil || *m.Den != 4 {
		t.Fatalf("num/den = %v/%v, want 1/4", m.Num, m.Den)
	}
	for k, want := range map[string]int64{"ok": 1, "failed": 1, "set_aside": 1, "waiting": 1, "backlog_oldest_ms": 10_800_000} {
		if got := extraInt(t, m, k); got != want {
			t.Errorf("extra.%s = %d, want %d", k, got, want)
		}
	}
	wantStatus(t, "pipeline.flow", m, StatusWatch)
	last := m.Series[len(m.Series)-1]
	if last.Day != "2026-10-30" || last.Den == nil || *last.Den != 4 || *last.Num != 1 {
		t.Errorf("last point = %+v, want 2026-10-30 1/4", last)
	}
}

// TestFlowAllOk: one document sent: 1/1 ok, nothing waiting.
func TestFlowAllOk(t *testing.T) {
	x := pipeFx(t)
	x.retained(ago(time.Hour), "d1", "ok", 100)
	m := pipeOf(x, x.query(7))["pipeline.flow"]
	wantStatus(t, "pipeline.flow", m, StatusOK)
	if extraInt(t, m, "backlog_oldest_ms") != 0 {
		t.Errorf("backlog = %v, want 0", m.Extra["backlog_oldest_ms"])
	}
}

// TestFlowCountsTheWindowsDocuments: d1 sent 10 days ago is not in the
// 7-day window (it is the previous window's: previous value 1/1); d2 is the
// window's only document. Under project beta nothing is counted (both
// documents are alpha's).
func TestFlowCountsTheWindowsDocuments(t *testing.T) {
	x := pipeFx(t)
	x.retained(ago(10*day), "d1", "ok", 100)
	x.retained(ago(time.Hour), "d2", "ok", 100)
	m := pipeOf(x, x.query(7))["pipeline.flow"]
	if *m.Den != 1 {
		t.Errorf("den = %d, want 1", *m.Den)
	}
	if m.Previous == nil || *m.Previous != 1 {
		t.Errorf("previous = %v, want 1 (d1's window holds d1: 1/1)", m.Previous)
	}
	q := x.query(7)
	q.Project = "beta"
	if got := pipeOf(x, q)["pipeline.flow"]; *got.Den != 0 || got.Value != nil {
		t.Errorf("beta: den %d value %v, want 0 and null", *got.Den, got.Value)
	}
}

// TestFlowWithoutAnySend: no retain event ever: no_data, no_events.
func TestFlowWithoutAnySend(t *testing.T) {
	x := pipeFx(t)
	x.prompts("alpha", "p", fxNow, 1, nil)
	ms := pipeOf(x, x.query(7))
	wantNoData(t, "pipeline.flow", ms["pipeline.flow"], ReasonNoEvents)
	wantNoData(t, "retain.outcomes", ms["retain.outcomes"], ReasonNoEvents)
}

// TestBacklogOverADayIsBad: d2 queued 25 h ago, its only event: the
// backlog is 25 h = 90,000,000 ms: bad.
func TestBacklogOverADayIsBad(t *testing.T) {
	x := pipeFx(t)
	x.retained(ago(25*time.Hour), "d2", "queued", 0)
	m := pipeOf(x, x.query(7))["pipeline.flow"]
	wantStatus(t, "pipeline.flow", m, StatusBad)
	if got := extraInt(t, m, "backlog_oldest_ms"); got != 90_000_000 {
		t.Errorf("backlog_oldest_ms = %d, want 90000000", got)
	}
}

// TestBacklogCountsFromTheStartOfTheWait: a document retried every hour for
// 26 h is 26 h old (bad), not 1 h; a document ok 30 h ago and queued again
// 3 h ago has waited 3 h, not 30; a note queued 4 h ago on this PC counts
// too, and the oldest of all wins. Set-aside items do not wait.
func TestBacklogCountsFromTheStartOfTheWait(t *testing.T) {
	t.Run("a retry loop", func(t *testing.T) {
		x := pipeFx(t)
		x.retained(ago(26*time.Hour), "d5", "queued", 0)
		for h := 25; h >= 1; h-- {
			x.retained(ago(time.Duration(h)*time.Hour), "d5", "retry", 0)
		}
		m := pipeOf(x, x.query(7))["pipeline.flow"]
		wantStatus(t, "pipeline.flow", m, StatusBad)
		if got := extraInt(t, m, "backlog_oldest_ms"); got != 26*3_600_000 {
			t.Errorf("backlog_oldest_ms = %d, want %d", got, 26*3_600_000)
		}
	})
	t.Run("ok before the wait", func(t *testing.T) {
		x := pipeFx(t)
		x.retained(ago(30*time.Hour), "d6", "ok", 100)
		x.retained(ago(3*time.Hour), "d6", "queued", 0)
		x.retained(ago(2*time.Hour), "d6", "retry", 0)
		m := pipeOf(x, x.query(7))["pipeline.flow"]
		if got := extraInt(t, m, "backlog_oldest_ms"); got != 3*3_600_000 {
			t.Errorf("backlog_oldest_ms = %d, want %d", got, 3*3_600_000)
		}
		wantStatus(t, "pipeline.flow", m, StatusOK)
	})
	t.Run("a note, and a set-aside document", func(t *testing.T) {
		x := pipeFx(t)
		x.local(index.LocalStatus{NotesQueued: 1, NotesOldestAt: ago(4 * time.Hour)})
		x.retained(ago(3*time.Hour), "d7", "queued", 0)
		x.retained(ago(50*time.Hour), "d8", "quarantined", 0)
		m := pipeOf(x, x.query(7))["pipeline.flow"]
		if got := extraInt(t, m, "backlog_oldest_ms"); got != 4*3_600_000 {
			t.Errorf("backlog_oldest_ms = %d, want %d", got, 4*3_600_000)
		}
	})
}

// TestRetainOutcomesP95OkOnly: 100 documents sent ok at 200 ms, 1 minute
// apart (today, 2026-10-30), 3 failed at 9,000 ms two days ago (10-28):
// p95 of the ok events is 200, not 9,000; 103 documents, today 100 ok,
// 10-28 three failed. With 99 ok sends the p95 is null.
func TestRetainOutcomesP95OkOnly(t *testing.T) {
	fill := func(oks int) *fx {
		x := pipeFx(t)
		for i := range oks {
			x.retained(ago(time.Duration(i+1)*time.Minute), fmt.Sprintf("ok%d", i), "ok", 200)
		}
		for i := range 3 {
			x.retained(ago(50*time.Hour+time.Duration(i)*time.Minute), fmt.Sprintf("bad%d", i), "failed", 9000)
		}
		return x
	}
	x := fill(100)
	m := pipeOf(x, x.query(7))["retain.outcomes"]
	if p, ok := pf(t, m, "p95_ms"); !ok || p != 200 {
		t.Errorf("p95_ms = %v (%v), want 200", p, ok)
	}
	if val(m) != 103 {
		t.Errorf("value = %v, want 103", val(m))
	}
	by, _ := m.Extra["by_status"].(map[string]int64)
	if by["ok"] != 100 || by["failed"] != 3 || by["queued"] != 0 {
		t.Errorf("by_status = %v, want ok 100, failed 3", by)
	}
	days := map[string]Point{}
	for _, p := range m.Series {
		days[p.Day] = p
	}
	if p := days["2026-10-30"]; val2(p) != 100 || p.Parts["ok"] != 100 {
		t.Errorf("10-30 = %+v, want 100 ok", p)
	}
	if p := days["2026-10-28"]; val2(p) != 3 || p.Parts["failed"] != 3 {
		t.Errorf("10-28 = %+v, want 3 failed", p)
	}
	few := fill(99)
	m = pipeOf(few, few.query(7))["retain.outcomes"]
	if _, ok := pf(t, m, "p95_ms"); ok {
		t.Errorf("p95_ms with 99 ok sends is not null")
	}
}

func val2(p Point) float64 {
	if p.Value == nil {
		return -1
	}
	return *p.Value
}

// TestOutcomesDayOfTheLastEvent: x1 queued 2026-10-28 and ok on 10-30 is
// one document, on 10-30 with its last status; 10-28 holds none.
func TestOutcomesDayOfTheLastEvent(t *testing.T) {
	x := pipeFx(t)
	x.retained(ago(50*time.Hour), "x1", "queued", 0)
	x.retained(ago(2*time.Hour), "x1", "ok", 90)
	m := pipeOf(x, x.query(7))["retain.outcomes"]
	days := map[string]Point{}
	for _, p := range m.Series {
		days[p.Day] = p
	}
	if p := days["2026-10-30"]; val2(p) != 1 || p.Parts["ok"] != 1 {
		t.Errorf("10-30 = %+v, want 1 ok", p)
	}
	if p := days["2026-10-28"]; val2(p) != 0 || len(p.Parts) != 0 {
		t.Errorf("10-28 = %+v, want 0", p)
	}
	if val(m) != 1 {
		t.Errorf("value = %v, want 1", val(m))
	}
}

// TestLocalBacklog: 2 notes, 1 session waiting, 3 quarantined = 6; the
// oldest note 61 min old: watch (3,660,000 ms); 25 h: bad; exactly 60 min:
// ok. A 30-day view reads the same (the backlog is now's). No
// local_status row: no_data.
func TestLocalBacklog(t *testing.T) {
	run := func(oldest time.Duration, days int) Metric {
		x := pipeFx(t)
		x.local(index.LocalStatus{NotesQueued: 2, NotesOldestAt: ago(oldest), SessionsWaiting: 1, Quarantined: 3})
		return pipeOf(x, x.query(days))["local.backlog"]
	}
	m := run(61*time.Minute, 7)
	wantStatus(t, "local.backlog", m, StatusWatch)
	if val(m) != 6 || extraInt(t, m, "oldest_ms") != 3_660_000 || extraInt(t, m, "notes_queued") != 2 ||
		extraInt(t, m, "sessions_waiting") != 1 || extraInt(t, m, "quarantined") != 3 {
		t.Errorf("value %v extra %v, want 6 / 2 / 1 / 3 and 3660000 ms", val(m), m.Extra)
	}
	if m.Previous != nil {
		t.Errorf("previous = %v, a figure as of now has none", *m.Previous)
	}
	wantStatus(t, "25 h", run(25*time.Hour, 7), StatusBad)
	wantStatus(t, "60 min", run(60*time.Minute, 7), StatusOK)
	wantStatus(t, "30 days", run(61*time.Minute, 30), StatusWatch)

	x := pipeFx(t)
	x.localFail = true
	wantNoData(t, "local.backlog", pipeOf(x, x.query(7))["local.backlog"], ReasonNoEvents)
}

// fakeOps builds n operations of status and type, updated at updated(i).
func fakeOps(n int, status string, updated func(i int) (u, c time.Time)) []hindsight.Operation {
	var ops []hindsight.Operation
	for i := range n {
		u, c := updated(i)
		ops = append(ops, hindsight.Operation{ID: fmt.Sprintf("%s-%d", status, i), Type: "retain", Status: status, UpdatedAt: u, CreatedAt: c})
	}
	return ops
}

// TestServerQueueFromOperations: 3 pending + 1 processing = 4; of 5 failed
// operations one was updated 2 days ago, four 10 days ago: failed 1,
// failed_total 5, watch; the same under 30 days. The stats' own pending
// count (0 here) and failed count (9) are never read.
func TestServerQueueFromOperations(t *testing.T) {
	x := pipeFx(t)
	x.server.set(func(d *fakeData) {
		d.Stats = hindsight.BankStats{PendingOperations: 0, FailedOperations: 9}
		d.Ops = slices.Concat(
			fakeOps(3, "pending", func(int) (time.Time, time.Time) { return ago(time.Hour), ago(time.Hour) }),
			fakeOps(1, "processing", func(int) (time.Time, time.Time) { return ago(time.Minute), ago(time.Hour) }),
			fakeOps(1, "failed", func(int) (time.Time, time.Time) { return ago(2 * day), ago(2 * day) }),
			fakeOps(4, "failed", func(int) (time.Time, time.Time) { return ago(10 * day), ago(10 * day) }))
	})
	for _, days := range []int{7, 30} {
		m := pipeOf(x, x.query(days))["server.queue"]
		if val(m) != 4 || extraInt(t, m, "pending") != 3 || extraInt(t, m, "processing") != 1 ||
			extraInt(t, m, "failed") != 1 || extraInt(t, m, "failed_total") != 5 {
			t.Errorf("%d days: value %v extra %v, want 4, failed 1 of 5", days, val(m), m.Extra)
		}
		wantStatus(t, "server.queue", m, StatusWatch)
	}
	for _, c := range x.server.called() {
		if len(c) >= 9 && c[:9] == "BankStats" {
			t.Errorf("the stats were read: %s", c)
		}
	}
}

// TestServerQueueFailedWindowEdges: a failed operation with no update time
// counts by its creation (1 day ago: counted); one dated in the future is
// not; one failed exactly 7 days ago is (the window opens on it, [now - 7 d,
// now)). Only old failures: ok, the total keeps them.
func TestServerQueueFailedWindowEdges(t *testing.T) {
	x := pipeFx(t)
	x.server.set(func(d *fakeData) {
		d.Ops = []hindsight.Operation{
			{ID: "a", Type: "retain", Status: "failed", CreatedAt: ago(day)},
			{ID: "b", Type: "retain", Status: "failed", UpdatedAt: fxNow.Add(time.Hour), CreatedAt: ago(time.Hour)},
			{ID: "c", Type: "retain", Status: "failed", UpdatedAt: ago(7 * day), CreatedAt: ago(8 * day)},
			{ID: "d", Type: "retain", Status: "failed", UpdatedAt: ago(7*day + time.Second), CreatedAt: ago(8 * day)},
		}
	})
	m := pipeOf(x, x.query(7))["server.queue"]
	if extraInt(t, m, "failed") != 2 || extraInt(t, m, "failed_total") != 4 {
		t.Errorf("failed %v of %v, want 2 of 4", m.Extra["failed"], m.Extra["failed_total"])
	}
	x.server.set(func(d *fakeData) { d.Ops = d.Ops[3:] })
	m = pipeOf(x, x.query(7))["server.queue"]
	wantStatus(t, "server.queue", m, StatusOK)
	if extraInt(t, m, "failed") != 0 || extraInt(t, m, "failed_total") != 1 {
		t.Errorf("failed %v of %v, want 0 of 1", m.Extra["failed"], m.Extra["failed_total"])
	}
}

// TestPipelineServerReadsNeedTheirKeys: a need list lacking a key the
// metric reads gives no_data/server_unsupported (the doctor: no_events),
// never a 0; the other metrics of the family are still computed.
func TestPipelineServerReadsNeedTheirKeys(t *testing.T) {
	x := pipeFx(t)
	x.doctor = []DoctorCheck{{ID: "server", Status: "ok"}}
	x.local(index.LocalStatus{})
	for _, c := range []struct {
		lack, metric, reason string
	}{
		{"queue", "server.queue", ReasonServerUnsupported},
		{"failed", "server.queue", ReasonServerUnsupported},
		{"completed", "server.extraction_delay", ReasonServerUnsupported},
		{"doctor", "doctor.check", ReasonNoEvents},
	} {
		x.need = slices.DeleteFunc(PipelineServerNeeds(), func(k string) bool { return k == c.lack })
		ms := pipeOf(x, x.query(7))
		wantNoData(t, c.metric+" without "+c.lack, ms[c.metric], c.reason)
		wantStatus(t, "local.backlog without "+c.lack, ms["local.backlog"], StatusOK)
	}
	x.need = PipelineServerNeeds()
	ms := pipeOf(x, x.query(7))
	wantStatus(t, "server.queue", ms["server.queue"], StatusOK)
	wantStatus(t, "doctor.check", ms["doctor.check"], StatusOK)
	x.server.failAll(hindsight.ErrUnreachable)
	ms = pipeOf(x, x.query(7))
	wantNoData(t, "server.queue", ms["server.queue"], ReasonServerDown)
	wantNoData(t, "server.extraction_delay", ms["server.extraction_delay"], ReasonServerDown)
}

// TestExtractionDelay: 120 completed retains updated 1..120 minutes ago,
// each taking i minutes (i = 1..120): the nearest-rank p95 is the 114th =
// 114 min = 6,840,000 ms, p50 the 60th = 3,600,000 ms: watch (over 30 min).
// A retain updated 8 days ago (10 h) is outside the window; with it the
// p95 would be the 115th. With 99 operations the metric is too_few and
// has no p95.
func TestExtractionDelay(t *testing.T) {
	ops := func(n int) []hindsight.Operation {
		return slices.Concat(
			fakeOps(n, "completed", func(i int) (time.Time, time.Time) {
				u := ago(time.Duration(i+1) * time.Minute)
				return u, u.Add(-time.Duration(i+1) * time.Minute)
			}),
			fakeOps(1, "completed", func(int) (time.Time, time.Time) {
				return ago(8 * day), ago(8*day + 10*time.Hour)
			}))
	}
	x := pipeFx(t)
	x.server.set(func(d *fakeData) { d.Ops = ops(120) })
	m := pipeOf(x, x.query(7))["server.extraction_delay"]
	if val(m) != 6_840_000 {
		t.Errorf("p95 = %v, want 6840000", val(m))
	}
	if p, ok := pf(t, m, "p50"); !ok || p != 3_600_000 {
		t.Errorf("p50 = %v (%v), want 3600000", p, ok)
	}
	wantStatus(t, "server.extraction_delay", m, StatusWatch)

	x.server.set(func(d *fakeData) { d.Ops = ops(99) })
	m = pipeOf(x, x.query(7))["server.extraction_delay"]
	wantStatus(t, "server.extraction_delay", m, StatusTooFew)
	if m.Value != nil {
		t.Errorf("value with 99 operations = %v, want null", *m.Value)
	}
}

// TestHookFailures24h: the 24-hour counts judge, the window counts. timeout
// x2 in 24 h: ok; + exit x1: watch; exit 5 days ago only: ok with value 1;
// killed x10: bad; timeout x3: watch. A window that ended 3 days ago
// still judges the failures of the last 24 h.
func TestHookFailures24h(t *testing.T) {
	fail := func(x *fx, ago_ time.Duration, kind string, n int) {
		for i := range n {
			x.tel(ago(ago_+time.Duration(i)*time.Minute), telemetry.HookFailure{Hook: "prompt", Kind: kind, MS: 5}, "s", "alpha")
		}
	}
	run := func(fill func(x *fx)) Metric {
		x := pipeFx(t)
		fill(x)
		return pipeOf(x, x.query(7))["hooks.failures"]
	}
	m := run(func(x *fx) { fail(x, time.Hour, "timeout", 2) })
	wantStatus(t, "timeout x2", m, StatusOK)
	if by := m.Extra["by_kind"].(map[string]int64); val(m) != 2 || by["timeout"] != 2 {
		t.Errorf("value %v by_kind %v, want 2 timeouts", val(m), by)
	}
	m = run(func(x *fx) { fail(x, time.Hour, "timeout", 2); fail(x, 2*time.Hour, "exit", 1) })
	wantStatus(t, "timeout x2 + exit", m, StatusWatch)
	if l := m.Extra["last_24h"].(map[string]int64); l["exit"] != 1 || l["timeout"] != 2 {
		t.Errorf("last_24h = %v", l)
	}
	m = run(func(x *fx) { fail(x, 5*day, "exit", 1) })
	wantStatus(t, "exit 5 days ago", m, StatusOK)
	if val(m) != 1 || len(m.Extra["last_24h"].(map[string]int64)) != 0 {
		t.Errorf("value %v last_24h %v, want 1 and none", val(m), m.Extra["last_24h"])
	}
	wantStatus(t, "killed x10", run(func(x *fx) { fail(x, time.Hour, "killed", 10) }), StatusBad)
	wantStatus(t, "timeout x3", run(func(x *fx) { fail(x, time.Hour, "timeout", 3) }), StatusWatch)

	x := pipeFx(t)
	fail(x, 2*time.Hour, "exit", 1)
	w, err := NewWindow(fxNow, 0, fxNow.Add(-10*day), fxNow.Add(-3*day), fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	m = pipeOf(x, Query{Window: w, Machines: "this"})["hooks.failures"]
	if val(m) != 0 {
		t.Errorf("value in a window that ended 3 days ago = %v, want 0", val(m))
	}
	wantStatus(t, "old window", m, StatusWatch)
}

// TestHookFailuresHealthyAndAbsent: with prompts recorded and no failure the
// value is 0 and ok; with no telemetry at all there is nothing to read.
func TestHookFailuresHealthyAndAbsent(t *testing.T) {
	x := pipeFx(t)
	wantNoData(t, "hooks.failures", pipeOf(x, x.query(7))["hooks.failures"], ReasonNoEvents)
	x.prompts("alpha", "p", fxNow, 1, nil)
	m := pipeOf(x, x.query(7))["hooks.failures"]
	wantStatus(t, "hooks.failures", m, StatusOK)
	if m.Value == nil || *m.Value != 0 {
		t.Errorf("value = %v, want 0", m.Value)
	}
}

// TestServerReachAndBreaker: failing since 4 h ago with a 2 h backlog:
// watch; with a 25 h backlog: bad; 7 prompts skipped by the breaker (and
// 2 skipped for another reason): value 7, skipped_prompts 7. An open
// breaker alone is watch; an elapsed one, not failing, is ok.
func TestServerReachAndBreaker(t *testing.T) {
	run := func(ls index.LocalStatus) Metric {
		x := pipeFx(t)
		x.local(ls)
		x.prompts("alpha", "b", fxNow, 7, func(_ int, p *telemetry.Prompt) { p.Decision, p.Reason = "skip", "breaker" })
		x.prompts("alpha", "g", fxNow.Add(-20*time.Hour), 2, func(_ int, p *telemetry.Prompt) { p.Decision, p.Reason = "skip", "gap" })
		return pipeOf(x, x.query(7))["server.reach"]
	}
	failing := index.LocalStatus{ReachFailingSince: ago(4 * time.Hour), ReachLastOK: ago(5 * time.Hour), NotesQueued: 1, NotesOldestAt: ago(2 * time.Hour)}
	m := run(failing)
	wantStatus(t, "failing, 2 h", m, StatusWatch)
	if val(m) != 7 || extraInt(t, m, "skipped_prompts") != 7 {
		t.Errorf("value %v skipped %v, want 7", val(m), m.Extra["skipped_prompts"])
	}
	if got, ok := m.Extra["last_ok"].(time.Time); !ok || !got.Equal(ago(5*time.Hour)) {
		t.Errorf("last_ok = %v, want 5 h ago", m.Extra["last_ok"])
	}
	if got, ok := m.Extra["failing_since"].(time.Time); !ok || !got.Equal(ago(4*time.Hour)) {
		t.Errorf("failing_since = %v, want 4 h ago", m.Extra["failing_since"])
	}
	if m.Extra["breaker_open_until"] != nil {
		t.Errorf("breaker_open_until = %v, want null", m.Extra["breaker_open_until"])
	}
	failing.NotesOldestAt = ago(25 * time.Hour)
	wantStatus(t, "failing, 25 h", run(failing), StatusBad)
	wantStatus(t, "breaker open", run(index.LocalStatus{BreakerOpenUntil: fxNow.Add(10 * time.Minute)}), StatusWatch)
	wantStatus(t, "breaker elapsed", run(index.LocalStatus{BreakerOpenUntil: ago(time.Hour)}), StatusOK)
	x := pipeFx(t)
	x.localFail = true
	wantNoData(t, "server.reach", pipeOf(x, x.query(7))["server.reach"], ReasonNoEvents)
}

// TestDoctorCheck: server ok, update warn, bank-config ok + warn (one entry,
// warn), metrics-index error: 4 entries, 1 error: value 1, watch; ok 1,
// warn 2, error 1, info 0. A doctor that never answers reads no_data
// (doctorTimeout lowered) and the rest of the family is still computed.
func TestDoctorCheck(t *testing.T) {
	x := pipeFx(t)
	x.doctor = []DoctorCheck{{ID: "server", Status: "ok"}, {ID: "update", Status: "warn"},
		{ID: "bank-config", Status: "ok"}, {ID: "bank-config", Status: "warn"},
		{ID: "metrics-index", Status: "error", Fix: "backsight stats index rebuild"}}
	m := pipeOf(x, x.query(7))["doctor.check"]
	wantStatus(t, "doctor.check", m, StatusWatch)
	if val(m) != 1 {
		t.Errorf("value = %v, want 1", val(m))
	}
	for k, want := range map[string]int64{"ok": 1, "info": 0, "warn": 2, "error": 1} {
		if got := extraInt(t, m, k); got != want {
			t.Errorf("extra.%s = %d, want %d", k, got, want)
		}
	}

	t.Run("blocked", func(t *testing.T) {
		lowerTimeouts(t, time.Second, 10*time.Millisecond)
		x := pipeFx(t)
		x.retained(ago(time.Hour), "d1", "ok", 100)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		src := x.source()
		src.Doctor = func(context.Context) ([]DoctorCheck, error) { <-release; return nil, nil }
		var ms map[string]Metric
		err := withTx(t.Context(), src, x.query(7), x.need, func(e env) error {
			var err error
			ms, err = computeMetrics(e, e.q.Window, pipelineIDs)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		wantNoData(t, "doctor.check", ms["doctor.check"], ReasonNoEvents)
		if ms["pipeline.flow"].Den == nil || *ms["pipeline.flow"].Den != 1 {
			t.Errorf("pipeline.flow = %+v, want computed (1 document)", ms["pipeline.flow"])
		}
	})
	t.Run("no doctor run", func(t *testing.T) {
		x := pipeFx(t) // x.doctor nil: Source.Doctor nil
		wantNoData(t, "doctor.check", pipeOf(x, x.query(7))["doctor.check"], ReasonNoEvents)
	})
	t.Run("all fine", func(t *testing.T) {
		x := pipeFx(t)
		x.doctor = []DoctorCheck{{ID: "server", Status: "ok"}}
		m := pipeOf(x, x.query(7))["doctor.check"]
		wantStatus(t, "doctor.check", m, StatusOK)
		if m.Value == nil || *m.Value != 0 {
			t.Errorf("value = %v, want 0", m.Value)
		}
	})
}

// TestDoctorEntries: one entry per id carrying {id, status, fix} only; the
// worst status of the checks sharing an id wins, with the fix of a check of
// that status; a fix that is not a `backsight ` command is blanked (the
// doctor's fixes can be shell lines or prose); a check of an unknown status
// is ignored.
func TestDoctorEntries(t *testing.T) {
	got := doctorEntries([]DoctorCheck{
		{ID: "server", Status: "ok", Fix: "backsight doctor"},
		{ID: "binary", Status: "error", Fix: "curl -fsSL https://example.test/install.sh | sh"},
		{ID: "bank-config", Status: "warn", Fix: "backsight config set bank x"},
		{ID: "bank-config", Status: "ok", Fix: "backsight nothing"},
		{ID: "index", Status: "info", Fix: ""},
		{ID: "index", Status: "info", Fix: "backsight stats index rebuild"},
		{ID: "update", Status: "warn", Fix: "run backsight update"},
		{ID: "hook", Status: "error", Fix: "backsightfoo"},
		{ID: "weird", Status: "fatal", Fix: "backsight x"},
		{ID: "hook2", Status: "error", Fix: "backsight "},
		{ID: "hook3", Status: "error", Fix: "backsight"},
	})
	want := []DoctorCheck{
		{ID: "server", Status: "ok", Fix: "backsight doctor"},
		{ID: "binary", Status: "error"},
		{ID: "bank-config", Status: "warn", Fix: "backsight config set bank x"},
		{ID: "index", Status: "info", Fix: "backsight stats index rebuild"},
		{ID: "update", Status: "warn"},
		{ID: "hook", Status: "error"},
		{ID: "hook2", Status: "error"},
		{ID: "hook3", Status: "error"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("entries =\n%+v\nwant\n%+v", got, want)
	}
}

// TestJobsLastRun: a tick 40 min ago (twice its 15 min cadence is 30) with
// a typed prompt 10 min ago (after the run was due, 25 min ago): bad; with
// nothing since: ok; with only a machine prompt, or a prompt before it was
// due (35 min ago): ok; a maintain 49 h ago (over 2 x 24 h) with a tool
// event 1 h ago: bad, 47 h ago: ok. The latest run of a job is the one
// read.
func TestJobsLastRun(t *testing.T) {
	job := func(x *fx, ts time.Time, name, status string, ms int) {
		x.tel(ts, telemetry.Job{Name: name, Status: status, MS: ms}, "", "")
	}
	run := func(fill func(x *fx)) Metric {
		x := pipeFx(t)
		fill(x)
		return pipeOf(x, x.query(7))["jobs.last_run"]
	}
	m := run(func(x *fx) {
		job(x, ago(2*time.Hour), "tick", "done", 50)
		job(x, ago(40*time.Minute), "tick", "failed", 120)
		x.tel(ago(10*time.Minute), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "s", "alpha")
	})
	wantStatus(t, "tick late with a prompt", m, StatusBad)
	tick, _ := m.Extra["tick"].(map[string]any)
	if val(m) != 2_400_000 || tick["age_ms"] != int64(2_400_000) || tick["status"] != "failed" || tick["ms"] != int64(120) {
		t.Errorf("value %v tick %v, want age 2400000 (40 min), failed, 120 ms", val(m), tick)
	}
	if at, ok := tick["last_at"].(time.Time); !ok || !at.Equal(ago(40*time.Minute)) {
		t.Errorf("last_at = %v, want 40 min ago", tick["last_at"])
	}
	if m.Extra["maintain"] != nil {
		t.Errorf("maintain = %v, want null (never ran)", m.Extra["maintain"])
	}
	wantStatus(t, "tick late, nothing since", run(func(x *fx) { job(x, ago(40*time.Minute), "tick", "done", 1) }), StatusOK)
	wantStatus(t, "machine prompt only", run(func(x *fx) {
		job(x, ago(40*time.Minute), "tick", "done", 1)
		x.tel(ago(10*time.Minute), telemetry.Prompt{N: 1, Decision: "skip", Reason: "machine"}, "s", "alpha")
	}), StatusOK)
	wantStatus(t, "prompt before the run was due", run(func(x *fx) {
		job(x, ago(40*time.Minute), "tick", "done", 1)
		x.tel(ago(35*time.Minute), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "s", "alpha")
	}), StatusOK)
	maintain := func(age time.Duration) Metric {
		return run(func(x *fx) {
			job(x, ago(5*time.Minute), "tick", "done", 1)
			job(x, ago(age), "maintain", "done", 900)
			x.tel(ago(time.Hour), telemetry.Tool{Name: "memory_search", OK: true}, "s", "alpha")
		})
	}
	wantStatus(t, "maintain 49 h", maintain(49*time.Hour), StatusBad)
	wantStatus(t, "maintain 47 h", maintain(47*time.Hour), StatusOK)

	x := pipeFx(t)
	x.prompts("alpha", "p", fxNow, 1, nil)
	wantNoData(t, "jobs.last_run", pipeOf(x, x.query(7))["jobs.last_run"], ReasonNoEvents)
}

// TestFutureRowsAndAges: rows stamped after now are not in the window or
// the activity (a typed prompt at now + 2 h: no breaker skip counted, the
// late tick not made bad); a tick stamped now + 1 h has age 0, ok; a note
// dated in the future has an oldest age of 0.
func TestFutureRowsAndAges(t *testing.T) {
	t.Run("a prompt in the future", func(t *testing.T) {
		x := pipeFx(t)
		x.local(index.LocalStatus{})
		x.tel(ago(40*time.Minute), telemetry.Job{Name: "tick", Status: "done", MS: 1}, "", "")
		x.tel(fxNow.Add(2*time.Hour), telemetry.Prompt{N: 1, Decision: "skip", Reason: "breaker"}, "s", "alpha")
		ms := pipeOf(x, x.query(7))
		if val(ms["server.reach"]) != 0 {
			t.Errorf("breaker skips = %v, want 0 (the prompt is after now)", val(ms["server.reach"]))
		}
		wantStatus(t, "jobs.last_run", ms["jobs.last_run"], StatusOK)
	})
	t.Run("a tick in the future", func(t *testing.T) {
		// The tick stamped now + 1 h is ignored: the real one, 40 min ago,
		// is the latest run; with a typed prompt since it is due, bad.
		x := pipeFx(t)
		x.tel(ago(40*time.Minute), telemetry.Job{Name: "tick", Status: "done", MS: 1}, "", "")
		x.tel(fxNow.Add(time.Hour), telemetry.Job{Name: "tick", Status: "done", MS: 1}, "", "")
		x.tel(ago(10*time.Minute), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "s", "alpha")
		m := pipeOf(x, x.query(7))["jobs.last_run"]
		tick, _ := m.Extra["tick"].(map[string]any)
		if tick["age_ms"] != int64(2_400_000) {
			t.Errorf("age = %v, want 2400000 (the future tick is ignored)", tick["age_ms"])
		}
		wantStatus(t, "jobs.last_run", m, StatusBad)
		// Only a future tick: no run yet.
		y := pipeFx(t)
		y.tel(fxNow.Add(time.Hour), telemetry.Job{Name: "tick", Status: "done", MS: 1}, "", "")
		wantNoData(t, "jobs.last_run", pipeOf(y, y.query(7))["jobs.last_run"], ReasonNoEvents)
	})
	t.Run("a tool event in the future", func(t *testing.T) {
		x := pipeFx(t)
		x.tel(ago(40*time.Minute), telemetry.Job{Name: "tick", Status: "done", MS: 1}, "", "")
		x.tel(fxNow.Add(2*time.Hour), telemetry.Tool{Name: "memory_search", OK: true}, "s", "alpha")
		wantStatus(t, "jobs.last_run", pipeOf(x, x.query(7))["jobs.last_run"], StatusOK)
	})
	t.Run("a note in the future", func(t *testing.T) {
		x := pipeFx(t)
		x.local(index.LocalStatus{NotesQueued: 1, NotesOldestAt: fxNow.Add(time.Hour)})
		m := pipeOf(x, x.query(7))["local.backlog"]
		if extraInt(t, m, "oldest_ms") != 0 {
			t.Errorf("oldest_ms = %v, want 0", m.Extra["oldest_ms"])
		}
		wantStatus(t, "local.backlog", m, StatusOK)
	})
}

// TestHookKind: the supervisor's eight kinds pass, anything else (an empty
// or unknown value) counts under "other".
func TestHookKind(t *testing.T) {
	for _, k := range []string{"panic", "fatal", "timeout", "killed", "exit", "invalid_output", "input_too_large", "spawn"} {
		if hookKind(k) != k {
			t.Errorf("hookKind(%q) = %q", k, hookKind(k))
		}
	}
	for _, k := range []string{"", "foo", "Exit", "exit "} {
		if hookKind(k) != "other" {
			t.Errorf("hookKind(%q) = %q, want other", k, hookKind(k))
		}
	}
}

// TestServerQueueCappedFailedList: 520 failed operations, updated 1..520
// minutes ago, listed newest first; 5 pages of 100 are read (500). failed_total
// is the server's 520 and failed_capped is true, the 7-day count is the
// 500 read (a lower bound). With 5 failed operations nothing is capped.
func TestServerQueueCappedFailedList(t *testing.T) {
	x := pipeFx(t)
	x.server.set(func(d *fakeData) {
		d.Ops = fakeOps(520, "failed", func(i int) (time.Time, time.Time) {
			u := ago(time.Duration(i+1) * time.Minute)
			return u, u
		})
	})
	m := pipeOf(x, x.query(7))["server.queue"]
	if extraInt(t, m, "failed_total") != 520 || extraInt(t, m, "failed") != 500 || m.Extra["failed_capped"] != true {
		t.Errorf("extra = %v, want failed_total 520, failed 500, capped", m.Extra)
	}
	wantStatus(t, "server.queue", m, StatusWatch)
	x.server.set(func(d *fakeData) { d.Ops = d.Ops[:5] })
	m = pipeOf(x, x.query(7))["server.queue"]
	if extraInt(t, m, "failed_total") != 5 || m.Extra["failed_capped"] != false {
		t.Errorf("extra = %v, want failed_total 5, not capped", m.Extra)
	}
}

// TestFailedReadSortsNewestFirst: six failed operations listed in no order
// (one with only a creation time) are held newest first after the read, so
// the first is the latest failure; four of them changed in the last 7 days
// (1 h, 5 h, 2 days, 3 days ago).
func TestFailedReadSortsNewestFirst(t *testing.T) {
	x := pipeFx(t)
	agos := []time.Duration{9 * day, time.Hour, 3 * day, 20 * day, 5 * time.Hour, 2 * day}
	x.server.set(func(d *fakeData) {
		d.Ops = fakeOps(len(agos), "failed", func(i int) (time.Time, time.Time) { return ago(agos[i]), ago(agos[i]) })
		d.Ops[2].UpdatedAt = time.Time{} // 3 days: read from its creation time
	})
	d, _ := loadServer(context.Background(), x.source(), x.window(7), []string{"failed"})
	var got []string
	for _, o := range d.Failed {
		got = append(got, o.ID)
	}
	want := []string{"failed-1", "failed-4", "failed-5", "failed-2", "failed-0", "failed-3"}
	if !slices.Equal(got, want) {
		t.Errorf("failed read in order %v, want newest first %v", got, want)
	}
	m := pipeOf(x, x.query(7))["server.queue"]
	if extraInt(t, m, "failed") != 4 || extraInt(t, m, "failed_total") != 6 {
		t.Errorf("extra = %v, want failed 4 of 6", m.Extra)
	}
}

// TestHookFailureUnknownKind: a failure of a kind the supervisor does not
// report counts under "other", in the window and in the last 24 h; one is
// not serious, so the metric stays ok. The index already files an unknown
// kind as other (its closed set), so this pins the end-to-end reading; the
// family's own hookKind is the second guard, pinned by TestHookKind.
func TestHookFailureUnknownKind(t *testing.T) {
	x := pipeFx(t)
	x.tel(ago(time.Hour), telemetry.HookFailure{Hook: "prompt", Kind: "foo", MS: 5}, "s", "alpha")
	m := pipeOf(x, x.query(7))["hooks.failures"]
	wantStatus(t, "hooks.failures", m, StatusOK)
	by, last := m.Extra["by_kind"].(map[string]int64), m.Extra["last_24h"].(map[string]int64)
	if !maps.Equal(by, map[string]int64{"other": 1}) || !maps.Equal(last, map[string]int64{"other": 1}) {
		t.Errorf("by_kind %v last_24h %v, want {other: 1} in both", by, last)
	}
}
