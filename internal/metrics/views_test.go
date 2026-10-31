package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The page views. Clock fxNow (2026-10-30T12:00+01:00), window the 7 days
// before it unless a test says otherwise.

// TestRunDispatches: Run serves the six page views by name; a browse view
// (Task 14) and an unknown name are ErrUnknownView.
func TestRunDispatches(t *testing.T) {
	x := newFx(t, fxNow)
	for _, v := range pageViews {
		if r := x.run(v, x.query(7)); r.View != v || r.ViewSchema != ViewSchema {
			t.Errorf("%s: view %q schema %d", v, r.View, r.ViewSchema)
		}
	}
	if !slices.Equal(Views[:6], pageViews) || len(Views) != 12 {
		t.Errorf("Views = %v, want the six page views first, 12 in all", Views)
	}
	for _, v := range []string{"sessions", "nope", ""} {
		if _, err := Run(context.Background(), v, x.source(), x.query(7)); !errors.Is(err, ErrUnknownView) {
			t.Errorf("Run(%q) = %v, want ErrUnknownView", v, err)
		}
	}
}

// TestViewEnvelope: every view carries its window, the previous window,
// the scope (machines "this", the server's host, the project), the index's
// freshness and the time it was computed.
func TestViewEnvelope(t *testing.T) {
	x := newFx(t, fxNow)
	q := x.query(7)
	q.Project = "alpha"
	for _, v := range pageViews {
		r := x.run(v, q)
		prev := q.Window.Previous()
		if !r.Window.Since.Equal(q.Window.Since) || !r.Previous.Since.Equal(prev.Since) || !r.Previous.Until.Equal(prev.Until) {
			t.Errorf("%s: window %v previous %v", v, r.Window, r.Previous)
		}
		if r.Scope.Machines != "this" || r.Scope.Server != fxHost || r.Scope.Project != "alpha" {
			t.Errorf("%s: scope %+v", v, r.Scope)
		}
		if !r.GeneratedAt.Equal(fxNow) || r.Index.IngestedAt.IsZero() {
			t.Errorf("%s: generated %v, index %+v", v, r.GeneratedAt, r.Index)
		}
	}
}

// TestScopeServerOKWithoutReads: a view that reads nothing from the server
// (quality) says what this PC last recorded of it: an answer and no
// failure since is ok; a failure since, or nothing recorded, is not; and
// when that last answer was (server_ok_at). A view that read the server
// carries no server_ok_at.
func TestScopeServerOKWithoutReads(t *testing.T) {
	for _, c := range []struct {
		name  string
		local *index.LocalStatus
		want  bool
	}{
		{"answered", &index.LocalStatus{ReachLastOK: ago(time.Minute)}, true},
		{"failing since", &index.LocalStatus{ReachLastOK: ago(time.Hour), ReachFailingSince: ago(time.Minute)}, false},
		{"never recorded", nil, false},
	} {
		x := newFx(t, fxNow)
		if c.local != nil {
			x.local(*c.local)
		}
		r := x.run("quality", x.query(7))
		if r.Scope.ServerOK != c.want || r.Server != nil || len(x.server.called()) != 0 {
			t.Errorf("%s: server_ok %v (server section %v, %d server calls), want %v, no section, no call", c.name, r.Scope.ServerOK, r.Server, len(x.server.called()), c.want)
		}
		switch at := r.Scope.ServerOKAt; {
		case c.local == nil && at != nil:
			t.Errorf("%s: server_ok_at %v, want none", c.name, at)
		case c.local != nil && (at == nil || !at.Equal(c.local.ReachLastOK)):
			t.Errorf("%s: server_ok_at %v, want the last answer %v", c.name, at, c.local.ReachLastOK)
		}
	}
	x := newFx(t, fxNow) // a view that reads the server says nothing more
	x.local(index.LocalStatus{ReachLastOK: ago(time.Minute)})
	if at := x.run("overview", x.query(7)).Scope.ServerOKAt; at != nil {
		t.Errorf("overview: server_ok_at %v, want none (it read the server)", at)
	}
}

// TestServerSectionsWhenDown: every server read unreachable, the last
// answer recorded at T: each view that reads the server says {ok: false,
// error: unreachable, last_ok: T} and server_ok false, its server-scope
// metrics read no_data/server_down, and what this PC recorded is computed.
func TestServerSectionsWhenDown(t *testing.T) {
	lastOK := fxNow.Add(-26 * time.Hour)
	x := newFx(t, fxNow)
	x.local(index.LocalStatus{ReachLastOK: lastOK, ReachFailingSince: fxNow.Add(-25 * time.Hour)})
	x.server.failAll(hindsight.ErrUnreachable)
	recalls(x, "r", fxNow, 30, 10, 400, 600)
	x.retained(ago(2*time.Hour), "d1", "queued", 0)
	x.store(ago(3*time.Hour), "saved", "decision", "alpha")
	local := map[string]string{"overview": "recall.hit_rate", "quality": "recall.hit_rate", "cost": "recall.latency",
		"bank": "notes.stored", "pipeline": "pipeline.flow", "system": "server.reach"}
	for _, v := range pageViews {
		r := x.run(v, x.query(7))
		raw := string(mustJSON(t, r.Server))
		switch {
		case v == "quality": // it reads nothing from the server
			if r.Server != nil {
				t.Errorf("quality: server section %s, want none", raw)
			}
		case r.Server == nil || r.Server.OK || r.Server.Error != "unreachable" || r.Server.LastOK == nil || !r.Server.LastOK.Equal(lastOK):
			t.Errorf("%s: server = %s, want ok false, unreachable, last_ok %v", v, raw, lastOK)
		}
		if r.Scope.ServerOK {
			t.Errorf("%s: scope.server_ok true", v)
		}
		for id, m := range r.Metrics {
			if d, _ := DefOf(id); d.Scope == "server" && (m.Status != StatusNoData || m.Reason != ReasonServerDown) {
				t.Errorf("%s: %s = %s/%s, want no_data/server_down", v, id, m.Status, m.Reason)
			}
		}
		if m := r.Metrics[local[v]]; m.Status == StatusNoData {
			t.Errorf("%s: %s is no_data (%s), want computed from this PC", v, local[v], m.Reason)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestEmptyListsAndNulls: an empty state with no server configured: every
// list a view fills (attention, info, the tags of every metric, the
// verdict's reasons, every table) marshals as [], never null; the server
// section says "last_ok": null.
func TestEmptyListsAndNulls(t *testing.T) {
	x := newFx(t, fxNow)
	x.server = nil
	tables := map[string][]string{
		"overview": {"lately"}, "quality": {"outcome_mix", "reported"}, "cost": {"extraction"},
		"bank":     {"by_project", "review/never_used", "review/hidden", "review/reported"},
		"pipeline": {"failed_operations", "jobs", "doctor"}, "system": {"jobs", "doctor"},
	}
	for _, v := range pageViews {
		raw := mustJSON(t, x.run(v, x.query(7)))
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		isList := func(what string, val any, present bool) {
			if _, ok := val.([]any); !ok || !present {
				t.Errorf("%s: %s = %v (present %v), want a list", v, what, val, present)
			}
		}
		for _, k := range []string{"attention", "info"} {
			val, ok := doc[k]
			isList(k, val, ok)
		}
		metrics, _ := doc["metrics"].(map[string]any)
		if len(metrics) == 0 {
			t.Errorf("%s: no metrics", v)
		}
		for id, m := range metrics {
			val, ok := m.(map[string]any)["tags"]
			isList(id+".tags", val, ok)
		}
		for _, path := range tables[v] {
			got := lookup(doc["tables"], strings.Split(path, "/"))
			if len(got) != 1 {
				t.Errorf("%s: tables.%s missing", v, path)
				continue
			}
			isList("tables."+path, got[0], true)
		}
		if v == "overview" {
			for _, ax := range axisOrder {
				val := lookup(doc, []string{"verdict", "axes", ax, "reasons"})
				isList("verdict.axes."+ax+".reasons", val[0], len(val) == 1)
			}
		}
		if v != "quality" && !strings.Contains(string(raw), `"server":{"ok":false,"error":"not_configured","last_ok":null}`) {
			t.Errorf("%s: no server section with last_ok null in %s", v, raw)
		}
	}
}

// TestNotFilteredTags: under --project alpha the server's figures (growth,
// consolidation, extraction, the queue and the extraction delay) are
// tagged not_filtered in every view that shows them; the hit rate, this
// PC's and filtered, is not.
func TestNotFilteredTags(t *testing.T) {
	x := fullFx(t)
	q := x.query(7)
	q.Project = "alpha"
	want := []string{"bank.growth", "bank.consolidation", "extraction.tokens", "extraction.cost", "extraction.per_session", "server.queue", "server.extraction_delay"}
	checked := map[string]bool{}
	for _, v := range pageViews {
		r := x.run(v, q)
		for _, id := range want {
			if m, ok := r.Metrics[id]; ok {
				checked[id] = true
				if !slices.Contains(m.Tags, tagNotFiltered) {
					t.Errorf("%s: %s tags %v, want not_filtered", v, id, m.Tags)
				}
			}
		}
		if m, ok := r.Metrics["recall.hit_rate"]; ok && slices.Contains(m.Tags, tagNotFiltered) {
			t.Errorf("%s: recall.hit_rate tagged not_filtered", v)
		}
	}
	for _, id := range want {
		if !checked[id] {
			t.Errorf("%s is in no page view", id)
		}
	}
}

// TestOutcomeMixLeavesForgottenOut: 8 prompt shows on 2026-10-30, settled
// credit 2, confident_miss 1, unknown 1, shown 1, forgotten 2, and one not
// settled: that day reads used 2, ignored 1, not declared 1, shown only 1,
// pending 1; the 2 forgotten are only in used.share_confident's extra. A
// day of the window with no show reads zeros; the window's first and last
// days are partial. In developer mode the day is split by path.
func TestOutcomeMixLeavesForgottenOut(t *testing.T) {
	x := newFx(t, fxNow)
	keys := x.promptShows("om", "prompt", 8) // 9:00 back to 2:00 on 10-30
	x.settleAs(keys, []int{2, 1, 1, 1, 2}, []string{feedback.OutcomeCredit, feedback.OutcomeMiss, feedback.OutcomeUnknown, feedback.OutcomeShown, feedback.OutcomeForgotten})
	x.shows(ago(5*time.Hour), "om-0", "t1", "", "alpha", feedback.PathSources, "", "src-1") // never in the mix
	r := x.run("quality", x.query(7))
	mix := r.Tables.OutcomeMix
	if len(mix) != 8 || mix[0].Day != "2026-10-23" || !mix[0].Partial || !mix[7].Partial || mix[3].Partial {
		t.Fatalf("days = %+v, want 8 from 2026-10-23, the first and last partial", mix)
	}
	want := OutcomeDay{Day: "2026-10-30", Used: 2, Ignored: 1, NotDeclared: 1, ShownOnly: 1, Pending: 1, Partial: true}
	if mix[7].ByPath != nil || !reflect.DeepEqual(mix[7], want) {
		t.Errorf("10-30 = %+v, want %+v", mix[7], want)
	}
	if !reflect.DeepEqual(mix[6], OutcomeDay{Day: "2026-10-29"}) {
		t.Errorf("10-29 = %+v, want zeros", mix[6])
	}
	if f := r.Metrics["used.share_confident"].Extra["forgotten"]; f != int64(2) {
		t.Errorf("used.share_confident forgotten = %v, want 2", f)
	}
	q := x.query(7)
	q.Dev = true
	day := x.run("quality", q).Tables.OutcomeMix[7]
	if p := day.ByPath["prompt"]; len(day.ByPath) != 1 || p.Used != 2 || p.Pending != 1 || p.Ignored != 1 {
		t.Errorf("by_path = %+v, want prompt only, used 2, ignored 1, pending 1", day.ByPath)
	}
	q.Dev, q.Path = false, "search"
	if d := x.run("quality", q).Tables.OutcomeMix[7]; d.Used+d.Ignored+d.NotDeclared+d.ShownOnly+d.Pending != 0 {
		t.Errorf("--path search: %+v, want no show (all were on the prompt path)", d)
	}
}

// TestReportedList: the wrong and outdated credits of the window, the
// latest first, each at its memory's earliest show in the turn; a used
// credit and a wrong one of last week are not listed; --limit cuts.
func TestReportedList(t *testing.T) {
	x := newFx(t, fxNow)
	x.declaredTurn(ago(5*time.Hour), "s-a", "declared", "wrong", "m-a")
	x.declaredTurn(ago(4*time.Hour), "s-b", "declared", "outdated", "m-b")
	x.declaredTurn(ago(3*time.Hour), "s-c", "declared", "used", "m-c")
	x.declaredTurn(ago(9*day), "s-old", "declared", "wrong", "m-old")
	r := x.run("quality", x.query(7))
	got := r.Tables.Reported
	want := []ReportedRow{
		{MemoryID: "m-b", Verdict: "outdated", Session: "s-b", Turn: "t1", Project: "alpha", At: ago(4 * time.Hour)},
		{MemoryID: "m-a", Verdict: "wrong", Session: "s-a", Turn: "t1", Project: "alpha", At: ago(5 * time.Hour)},
	}
	if len(got) != 2 {
		t.Fatalf("reported = %+v, want 2 rows", got)
	}
	for i := range want {
		if g := got[i]; g.MemoryID != want[i].MemoryID || g.Verdict != want[i].Verdict || g.Session != want[i].Session ||
			g.Turn != want[i].Turn || g.Project != want[i].Project || !g.At.Equal(want[i].At) {
			t.Errorf("row %d = %+v, want %+v", i, g, want[i])
		}
	}
	q := x.query(7)
	q.Limit = 1
	if got := x.run("quality", q).Tables.Reported; len(got) != 1 || got[0].MemoryID != "m-b" {
		t.Errorf("--limit 1: %+v, want m-b only", got)
	}
}

// TestPipelineTables: TestServerQueueFromOperations's and TestDoctorCheck's
// fixtures through Pipeline: the 5 failed operations, the latest first,
// with their type (the task's when the operation has none), age since
// their last change (2 days = 172,800,000 ms for the first) and retries;
// the doctor's 4 entries with their backsight fixes only.
func TestPipelineTables(t *testing.T) {
	x := pipeFx(t)
	x.server.set(func(d *fakeData) {
		d.Ops = slices.Concat(
			fakeOps(3, "pending", func(int) (time.Time, time.Time) { return ago(time.Hour), ago(time.Hour) }),
			fakeOps(1, "processing", func(int) (time.Time, time.Time) { return ago(time.Minute), ago(time.Hour) }),
			fakeOps(1, "failed", func(int) (time.Time, time.Time) { return ago(2 * day), ago(2 * day) }),
			fakeOps(4, "failed", func(int) (time.Time, time.Time) { return ago(10 * day), ago(10 * day) }))
		for i := range d.Ops {
			d.Ops[i].RetryCount = i
		}
		d.Ops[5].Type, d.Ops[5].TaskType = "", "consolidation"
	})
	x.doctor = []DoctorCheck{{ID: "server", Status: "ok"}, {ID: "update", Status: "warn"},
		{ID: "bank-config", Status: "ok"}, {ID: "bank-config", Status: "warn"},
		{ID: "metrics-index", Status: "error", Fix: "backsight stats index rebuild"}}
	x.tel(ago(10*time.Minute), telemetry.Job{Name: "tick", Status: "done", MS: 30}, "", "")
	r := x.run("pipeline", x.query(7))
	ops := r.Tables.FailedOperations
	if len(ops) != 5 || ops[0] != (OperationRow{Type: "retain", AgeMS: 172_800_000, Retries: 4}) ||
		ops[1] != (OperationRow{Type: "consolidation", AgeMS: 864_000_000, Retries: 5}) {
		t.Errorf("failed_operations = %+v, want 5, the first {retain 172800000 4}, then {consolidation 864000000 5}", ops)
	}
	wantDoctor := []DoctorRow{{"server", "ok", ""}, {"update", "warn", ""}, {"bank-config", "warn", ""}, {"metrics-index", "error", "backsight stats index rebuild"}}
	if !slices.Equal(r.Tables.Doctor, wantDoctor) {
		t.Errorf("doctor = %+v, want %+v", r.Tables.Doctor, wantDoctor)
	}
	if j := r.Tables.Jobs; len(j) != 1 || j[0].Name != "tick" || j[0].Status != "done" || j[0].MS != 30 || j[0].AgeMS != 600_000 || !j[0].LastAt.Equal(ago(10*time.Minute)) {
		t.Errorf("jobs = %+v, want the tick 10 min ago", j)
	}
}

// TestOneTransactionPerView: each page view, the overview's verdict
// included, reads the index in exactly one transaction.
func TestOneTransactionPerView(t *testing.T) {
	x := fullFx(t)
	for _, v := range pageViews {
		src := x.source()
		cr := &countingReader{Reader: src.Index}
		src.Index = cr
		r, err := Run(context.Background(), v, src, x.query(7))
		if err != nil {
			t.Fatal(err)
		}
		if n := cr.n.Load(); n != 1 {
			t.Errorf("%s: %d read transactions, want 1", v, n)
		}
		if v == "overview" && r.Verdict == nil {
			t.Error("overview: no verdict")
		}
	}
}

// TestViewsSkipTheDoctor: the quality, cost and bank views never run the
// doctor; the pipeline and system views do (the seam is live).
func TestViewsSkipTheDoctor(t *testing.T) {
	x := newFx(t, fxNow)
	calls := 0
	for _, v := range pageViews {
		src := x.source()
		src.Doctor = func(context.Context) ([]DoctorCheck, error) {
			calls++
			if v == "quality" || v == "cost" || v == "bank" {
				t.Errorf("%s ran the doctor", v)
			}
			return nil, nil
		}
		if _, err := Run(context.Background(), v, src, x.query(7)); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
	}
	if calls != 3 { // overview, pipeline, system
		t.Errorf("doctor ran %d times, want 3", calls)
	}
}

// TestViewServerReads: each view asks the server only for what it shows.
// The overview: the stats, the series, the queue (pending, processing)
// and the failed and completed operations (Flowing's attention items),
// the LLM stats of all operations for the summary (no split, no 30-day
// read); under a project the tags too (the project's size). Quality:
// nothing. System: the version. Bank: stats, series, tags. Pipeline: the
// operations. Cost: the LLM stats.
func TestViewServerReads(t *testing.T) {
	methods := func(x *fx) []string {
		var out []string
		for _, c := range x.server.called() {
			m, q, _ := strings.Cut(c, "?")
			switch m {
			case "LLMStats":
				m += "(" + q + ")"
			case "OperationsPage":
				v, _ := url.ParseQuery(q)
				m += "(" + v.Get("status") + ")"
			}
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
		slices.Sort(out)
		return out
	}
	for _, c := range []struct {
		view, project string
		want          []string
	}{
		{"overview", "", []string{"BankStats", "LLMStats(period=7d)", "MemoriesTimeseries", "OperationsPage(completed)", "OperationsPage(failed)", "OperationsPage(pending)", "OperationsPage(processing)"}},
		{"overview", "alpha", []string{"BankStats", "LLMStats(period=7d)", "MemoriesTimeseries", "OperationsPage(completed)", "OperationsPage(failed)", "OperationsPage(pending)", "OperationsPage(processing)", "ProjectTags"}},
		{"quality", "", nil},
		{"system", "", []string{"Version"}},
		{"bank", "", []string{"BankStats", "MemoriesTimeseries", "ProjectTags"}},
		{"pipeline", "", []string{"OperationsPage(completed)", "OperationsPage(failed)", "OperationsPage(pending)", "OperationsPage(processing)"}},
		{"cost", "", []string{"LLMStats(operation=consolidation&period=7d)", "LLMStats(operation=reflect&period=7d)",
			"LLMStats(operation=retain_extract_facts&period=7d)", "LLMStats(period=30d)", "LLMStats(period=7d)"}},
	} {
		x := newFx(t, fxNow)
		q := x.query(7)
		q.Project = c.project
		x.run(c.view, q)
		if got := methods(x); !slices.Equal(got, c.want) {
			t.Errorf("%s (project %q) read %v, want %v", c.view, c.project, got, c.want)
		}
	}
}

// TestLLMReadOnceOverSevenDays: over 30 days the LLM stats of all
// operations are the 30-day read extraction.per_session needs: one
// request, both metrics computed from it; over 7 days they are two.
func TestLLMReadOnceOverSevenDays(t *testing.T) {
	allOps := func(x *fx) (n int, periods []string) {
		for _, c := range x.server.called() {
			if m, q, _ := strings.Cut(c, "?"); m == "LLMStats" && strings.HasPrefix(q, "period=") { // no operation: all of them
				n++
				periods = append(periods, strings.TrimPrefix(q, "period="))
			}
		}
		return n, periods
	}
	x := costFx(t)
	extractionServer(x)
	x.retained(ago(2*day), "d1", "ok", 100)
	r := x.run("cost", x.query(30))
	if n, p := allOps(x); n != 1 || p[0] != "30d" {
		t.Errorf("30 days: %d reads of all operations %v, want one 30d", n, p)
	}
	tok, per := r.Metrics["extraction.tokens"], r.Metrics["extraction.per_session"]
	if tok.Status == StatusNoData || per.Status == StatusNoData || val(tok) < 6_000_000 {
		t.Errorf("tokens %v (%s/%s), per session %s/%s; want both computed, tokens with day C (5,000,000 input)", tok.Value, tok.Status, tok.Reason, per.Status, per.Reason)
	}
	x = costFx(t)
	extractionServer(x)
	x.run("cost", x.query(7))
	if n, p := allOps(x); n != 2 || !slices.Contains(p, "7d") || !slices.Contains(p, "30d") {
		t.Errorf("7 days: %d reads of all operations %v, want 7d and 30d", n, p)
	}
	x = costFx(t)
	x.server.fail("LLMStats", hindsight.ErrUnreachable)
	r = x.run("cost", x.query(30))
	for _, id := range []string{"extraction.tokens", "extraction.per_session"} {
		if m := r.Metrics[id]; m.Status != StatusNoData || m.Reason != ReasonServerDown {
			t.Errorf("30 days, LLM stats down: %s = %s/%s, want no_data/server_down", id, m.Status, m.Reason)
		}
	}
}

// TestBucketsFlagPartial: the buckets cut by the window's edges are
// partial: a 7-day window from 13:00 has 8 days, the first and last
// partial; a 48 h window from 10:30 has 49 hours, the first and last
// partial (half an hour each); a 48 h window on the hour has 48, none.
func TestBucketsFlagPartial(t *testing.T) {
	partial := func(bs []Bucket) []int {
		var out []int
		for i, b := range bs {
			if b.Partial {
				out = append(out, i)
			}
		}
		return out
	}
	x := newFx(t, fxNow)
	if bs := x.window(7).Buckets(); len(bs) != 8 || !slices.Equal(partial(bs), []int{0, 7}) {
		t.Errorf("7 days: %d buckets, partial %v; want 8, [0 7]", len(bs), partial(bs))
	}
	until := time.Date(2026, 10, 30, 10, 30, 0, 0, fxParis)
	w, err := NewWindow(fxNow, 0, until.Add(-48*time.Hour), until, fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	bs := w.Buckets()
	if len(bs) != 49 || !slices.Equal(partial(bs), []int{0, 48}) || bs[0].Until.Sub(bs[0].Since) != 30*time.Minute || bs[48].Until.Sub(bs[48].Since) != 30*time.Minute {
		t.Errorf("48 h from 10:30: %d buckets, partial %v; want 49, [0 48], half an hour each", len(bs), partial(bs))
	}
	w, err = NewWindow(fxNow, 2, time.Time{}, time.Time{}, fxParis, "Europe/Paris") // 12:00 to 12:00
	if err != nil {
		t.Fatal(err)
	}
	if bs := w.Buckets(); len(bs) != 48 || len(partial(bs)) != 0 {
		t.Errorf("48 h on the hour: %d buckets, partial %v; want 48, none", len(bs), partial(bs))
	}
}

// TestSeriesFlagPartialBuckets: a view's series say which points cover
// part of their day or hour, so a per-day count shows no false dip at the
// window's edges: 7 days, the first and last of 8 days; 48 h from 10:30,
// the first and last of 49 hours. JSON carries "partial": true on those
// points only.
func TestSeriesFlagPartialBuckets(t *testing.T) {
	x := newFx(t, fxNow)
	recalls(x, "r", fxNow, 30, 10, 400, 600)
	flags := func(ps []Point) []int {
		var out []int
		for i, p := range ps {
			if p.Partial {
				out = append(out, i)
			}
		}
		return out
	}
	r := x.run("overview", x.query(7))
	s := r.Metrics["recall.hit_rate"].Series
	if len(s) != 8 || !slices.Equal(flags(s), []int{0, 7}) {
		t.Errorf("7 days: %d points, partial %v; want 8, [0 7]", len(s), flags(s))
	}
	if n := strings.Count(string(mustJSON(t, r.Metrics["recall.hit_rate"])), `"partial":true`); n != 2 {
		t.Errorf("7 days: %d partial points in JSON, want 2", n)
	}
	until := time.Date(2026, 10, 30, 10, 30, 0, 0, fxParis)
	w, err := NewWindow(fxNow, 0, until.Add(-48*time.Hour), until, fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Window: w, Machines: "this"}
	for _, v := range []string{"overview", "quality", "cost"} {
		for id, m := range x.run(v, q).Metrics {
			if len(m.Series) == 0 {
				continue
			}
			if len(m.Series) != 49 || !slices.Equal(flags(m.Series), []int{0, 48}) || m.Series[0].Hour != "2026-10-28T10:00+01:00" {
				t.Errorf("%s 48 h: %s has %d points, partial %v, first %q; want 49, [0 48], 10:00", v, id, len(m.Series), flags(m.Series), m.Series[0].Hour)
			}
		}
	}
}

// TestOverviewTiles: the overview's metrics are the four axes', the bank
// tiles and the review counts; only the six tiles keep their series; the
// figures of the default window are the verdict's own, equal to those
// computed for the view, and a 30-day window's are its own.
func TestOverviewTiles(t *testing.T) {
	x := fullFx(t)
	r := x.run("overview", x.query(7))
	var ids []string
	for id := range r.Metrics {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	want := slices.Sorted(slices.Values(overviewIDs()))
	if !slices.Equal(ids, want) {
		t.Errorf("metrics = %v, want %v", ids, want)
	}
	for id, m := range r.Metrics {
		if tile := slices.Contains(overviewTiles, id); tile != (len(m.Series) > 0) {
			t.Errorf("%s: %d points, tile %v", id, len(m.Series), tile)
		}
	}
	direct := x.metrics(overviewIDs(), x.query(7))
	for _, id := range overviewTiles {
		if got, w := r.Metrics[id], direct[id]; !reflect.DeepEqual(got.Value, w.Value) || !reflect.DeepEqual(got.Den, w.Den) || got.Status != w.Status || !reflect.DeepEqual(got.Delta, w.Delta) {
			t.Errorf("%s: overview %v/%v %s, computed for the window %v/%v %s", id, got.Value, got.Den, got.Status, w.Value, w.Den, w.Status)
		}
	}
	m30 := x.run("overview", x.query(30)).Metrics["recall.hit_rate"]
	if d30, d7 := den0(m30), den0(r.Metrics["recall.hit_rate"]); d30 <= d7 {
		t.Errorf("30-day hit rate n = %d, want more than the 7-day %d (the previous week's recalls too)", d30, d7)
	}
}

// TestOverviewJudged28d: when Right is judged on 28 days for want of turns,
// the overview tags its Right tiles judged_28d (and only those); judged on
// 7 days, none is.
func TestOverviewJudged28d(t *testing.T) {
	used := firstK(100, "used", "")
	x := newFx(t, fxNow)
	turns(x, "c", fxNow, 6, "declared", used)
	turns(x, "o", fxNow.Add(-8*day), 24, "declared", used)
	r := x.run("overview", x.query(7))
	if r.Verdict.Axes["right"].WindowDays != 28 {
		t.Fatalf("right judged on %d days, want 28", r.Verdict.Axes["right"].WindowDays)
	}
	for id, m := range r.Metrics {
		if right := slices.Contains(axisMetrics["right"], id); slices.Contains(m.Tags, tagJudged28d) != right {
			t.Errorf("%s: tags %v (a Right metric: %v)", id, m.Tags, right)
		}
	}
	y := newFx(t, fxNow)
	turns(y, "c", fxNow, 25, "declared", used)
	for id, m := range y.run("overview", y.query(7)).Metrics {
		if slices.Contains(m.Tags, tagJudged28d) {
			t.Errorf("judged on 7 days: %s tagged judged_28d", id)
		}
	}
}

// TestOverviewAttentionAndSummary: the overview lists the first 5
// attention items and counts the rest, carries the verdict's info items
// and the summary of its window.
func TestOverviewAttentionAndSummary(t *testing.T) {
	x := fullFx(t)
	r := x.run("overview", x.query(7))
	a := x.assess(x.query(7))
	if len(a.Items) <= attentionShown {
		t.Fatalf("the fixture has %d items, want more than %d", len(a.Items), attentionShown)
	}
	if !reflect.DeepEqual(r.Attention, a.Items[:attentionShown]) || r.AttentionMore != len(a.Items)-attentionShown {
		t.Errorf("attention %d + %d more, want the verdict's first %d + %d", len(r.Attention), r.AttentionMore, attentionShown, len(a.Items)-attentionShown)
	}
	if !reflect.DeepEqual(r.Info, a.Info) || len(r.Info) != 3 {
		t.Errorf("info = %+v, want the verdict's 3", r.Info)
	}
	if s := r.Summary; s.PromptsWithMemory != 58 || s.TurnsWithMemoryUsed != 15 || s.ExtractionCost == nil || !same(*s.ExtractionCost, 0.71) || !s.PriceSet {
		t.Errorf("summary = %+v, want 58 prompts with memory, 15 turns used, cost 0.71", *s)
	}
	if _, ok := r.Metrics["extraction.cost"]; ok {
		t.Error("extraction.cost is listed: it only feeds the summary")
	}
}

// TestActivity: the overview's agreement counts over the window: typed
// and machine prompts, recalls and those with hits, accepted credit calls
// by verdict, send events by status; last week's are not counted, nor
// another project's under --project.
func TestActivity(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "t", fxNow, 4, func(i int, p *telemetry.Prompt) { p.Injected = i % 2 })
	x.prompts("alpha", "m", fxNow, 3, func(_ int, p *telemetry.Prompt) { p.Reason = "machine" })
	x.prompts("alpha", "s", fxNow, 2, func(_ int, p *telemetry.Prompt) { p.Decision, p.Reason = "skip", "gap" })
	x.prompts("beta", "b", fxNow, 5, nil)
	x.prompts("alpha", "old", fxNow.Add(-8*day), 6, nil)
	for _, c := range []telemetry.Credit{{Verdict: "used", IDs: 1, OK: true}, {Verdict: "used", IDs: 2, OK: true}, {Verdict: "wrong", IDs: 1, OK: true}, {Verdict: "used", OK: false}} {
		x.tel(ago(time.Hour), c, "t-0", "alpha")
	}
	x.retained(ago(time.Hour), "d1", "ok", 10)
	x.retained(ago(2*time.Hour), "d2", "queued", 0)
	x.retained(ago(time.Hour), "d2", "ok", 10)
	q := x.query(7)
	q.Project = "alpha"
	got := *x.run("overview", q).Tables.Activity
	want := Activity{Prompts: 6, Machine: 3, Recalls: 7, WithHits: 2,
		Credits: map[string]int64{"used": 2, "wrong": 1}, Retains: map[string]int64{"ok": 2, "queued": 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("activity = %+v, want %+v", got, want)
	}
	if got := *x.run("overview", x.query(7)).Tables.Activity; got.Prompts != 11 || got.Recalls != 12 {
		t.Errorf("all projects: %d typed prompts, %d recalls; want 11, 12", got.Prompts, got.Recalls)
	}
}

// TestLately: the 5 sessions with the latest activity, those with a wrong
// or outdated report first; a session reported wrong but older than the 5
// is not listed. A row counts its typed prompts, those that got memory,
// the memories shown, used and reported, the characters, and the status of
// its latest send.
func TestLately(t *testing.T) {
	x := newFx(t, fxNow)
	x.declaredTurn(ago(10*time.Hour), "old-wrong", "declared", "wrong", "m-x")
	for i := range 4 {
		x.prompts("alpha", fmt.Sprintf("p%d", i), ago(time.Duration(i)*time.Hour), 1, func(_ int, p *telemetry.Prompt) { p.Injected, p.Chars = 1, 500 })
	}
	x.declaredTurn(ago(5*time.Hour), "w", "declared", "wrong", "m-a", "m-b")
	x.credit(ago(5*time.Hour-time.Minute), "w", "", "alpha", "used", "m-c")
	x.tel(ago(5*time.Hour-2*time.Minute), telemetry.Prompt{N: 1, Decision: "recall", Injected: 1, Chars: 700}, "w", "alpha")
	x.tel(ago(5*time.Hour-3*time.Minute), telemetry.Briefing{IDs: []string{"b"}, Chars: 300}, "w", "alpha")
	x.tel(ago(4*time.Hour), telemetry.Retain{Document: "doc-w", Status: "queued"}, "w", "alpha")
	x.tel(ago(3*time.Hour), telemetry.Retain{Document: "doc-w", Status: "ok"}, "w", "alpha")
	got := x.run("overview", x.query(7)).Tables.Lately
	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if want := []string{"w", "p0-0", "p1-0", "p2-0", "p3-0"}; !slices.Equal(ids, want) {
		t.Fatalf("lately = %v, want %v", ids, want)
	}
	w := got[0]
	if w.Project != "alpha" || w.Prompts != 1 || w.PromptsWithMemory != 1 || w.Shown != 2 || w.Used != 1 || w.Wrong != 2 ||
		w.ContextChars != 1000 || w.Sent != "ok" || !w.Start.Equal(ago(5*time.Hour)) || !w.Last.Equal(ago(5*time.Hour-3*time.Minute)) {
		t.Errorf("w = %+v, want alpha, 1 prompt with memory, 2 shown, 1 used, 2 wrong, 1,000 chars, sent ok, from its show to its briefing (sends are not activity)", w)
	}
	if p := got[1]; p.Prompts != 1 || p.Wrong != 0 || p.Sent != "" || !p.Last.Equal(ago(time.Hour)) {
		t.Errorf("p0-0 = %+v, want 1 prompt, nothing sent, an hour ago", p)
	}
}

// TestBankTables: the server's projects, the largest first (other tags
// left out); the three review lists of memory stale with its commands, at
// most --limit rows each, and each memory's latest known project.
func TestBankTables(t *testing.T) {
	x := newFx(t, fxNow)
	x.server.set(func(d *fakeData) {
		d.Tags = []hindsight.TagCount{{Tag: "project:beta", Count: 7}, {Tag: "project:alpha", Count: 120}, {Tag: "kind:decision", Count: 300}, {Tag: "project:gamma", Count: 7}}
	})
	x.ledger(staleLedger())
	x.shows(ago(time.Hour), "s1", "t1", "", "alpha", "prompt", "beta", "n-a")
	r := x.run("bank", x.query(7))
	if want := []ProjectCount{{"alpha", 120}, {"beta", 7}, {"gamma", 7}}; !slices.Equal(r.Tables.ByProject, want) {
		t.Errorf("by_project = %+v, want %+v", r.Tables.ByProject, want)
	}
	rv := r.Tables.Review
	ids := func(rs []ReviewRow) (out []string) {
		for _, r := range rs {
			out = append(out, r.ID+" "+r.Command)
		}
		return out
	}
	if got, want := ids(rv.NeverUsed), []string{"n-a backsight forget n-a", "n-b backsight forget n-b", "n-c backsight forget n-c"}; !slices.Equal(got, want) {
		t.Errorf("never_used = %v, want %v", got, want)
	}
	if got, want := ids(rv.Hidden), []string{"h-old backsight forget --restore h-old"}; !slices.Equal(got, want) {
		t.Errorf("hidden = %v, want %v", got, want)
	}
	if got, want := ids(rv.Reported), []string{"r-y backsight forget r-y --purge", "r-x backsight forget r-x --purge"}; !slices.Equal(got, want) {
		t.Errorf("reported = %v, want %v", got, want)
	}
	if n := rv.NeverUsed[0]; n.Project != "beta" || n.Shown != 15 {
		t.Errorf("n-a = %+v, want project beta (its show's), shown 15", n)
	}
	q := x.query(7)
	q.Limit = 2
	if rv := x.run("bank", q).Tables.Review; len(rv.NeverUsed) != 2 || len(rv.Reported) != 2 || len(rv.Hidden) != 1 {
		t.Errorf("--limit 2: %d %d %d rows, want 2 1 2", len(rv.NeverUsed), len(rv.Hidden), len(rv.Reported))
	}
}

// TestExtractionTable: TestExtractionTokensAndCost's server, per day and
// operation: retain_extract_facts on 10-28 and 10-29, consolidation on
// 10-29, the rest of 10-29 as other; reflect has no tokens, day C is out
// of the window. Without prices the cost is null; with an operation that
// cannot be read there is no "other".
func TestExtractionTable(t *testing.T) {
	x := costFx(t)
	pricesSet(x)
	extractionServer(x)
	got := x.run("cost", x.query(7)).Tables.Extraction
	cost := func(v float64) *float64 { return &v }
	want := []ExtractionRow{
		{Day: "2026-10-28", Operation: "retain_extract_facts", Input: 900_000, Cached: 400_000, Output: 150_000, Cost: cost(0.328)},
		{Day: "2026-10-29", Operation: "retain_extract_facts", Input: 300_000, Output: 100_000, Cost: cost(0.191)},
		{Day: "2026-10-29", Operation: "consolidation", Input: 200_000, Output: 60_000, Cost: cost(0.120)},
		{Day: "2026-10-29", Operation: "other", Input: 100_000, Output: 40_000, Cost: cost(0.071)},
	}
	if len(got) != len(want) {
		t.Fatalf("extraction = %+v, want %d rows", got, len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Day != w.Day || g.Operation != w.Operation || g.Input != w.Input || g.Cached != w.Cached || g.Output != w.Output || g.Cost == nil || !same(*g.Cost, *w.Cost) {
			t.Errorf("row %d = %+v (cost %v), want %+v (cost %v)", i, g, g.Cost, w, *w.Cost)
		}
	}
	y := costFx(t)
	y.cfg = func(c *config.Config) { c.LLM.Model = "no-such-model" }
	extractionServer(y)
	y.server.fail("LLMStats:reflect", &hindsight.APIError{Kind: hindsight.NotFound})
	rows := y.run("cost", y.query(7)).Tables.Extraction
	if len(rows) != 3 || slices.ContainsFunc(rows, func(r ExtractionRow) bool { return r.Operation == "other" || r.Cost != nil }) {
		t.Errorf("no prices, reflect unread: %+v, want 3 rows, no other, no cost", rows)
	}
}

// TestSystemTable: the server's version, this PC's runtime and mode, what
// local_status recorded (reach, backups, update checks) and the index's
// schema and freshness.
func TestSystemTable(t *testing.T) {
	x := newFx(t, fxNow)
	x.server.set(func(d *fakeData) { d.Version = hindsight.Version{APIVersion: "0.5.2"} })
	x.local(index.LocalStatus{ReachLastOK: ago(time.Minute), BreakerFailures: 2, BreakerOpenUntil: fxNow.Add(time.Minute),
		BackupCreatedAt: ago(day), BackupOK: true, UpdateCheckedAt: ago(time.Hour), UpdateNewer: 3, UpdateCheckFailed: true})
	r := x.run("system", x.query(7))
	s := r.Tables.System
	cfg := x.source().Config
	if s.ServerVersion != "0.5.2" || s.Runtime != cfg.RuntimeKind() || s.Mode != cfg.Server.Mode || s.Index.Schema < 1 || s.Index.IngestedAt.IsZero() {
		t.Errorf("system = %+v, want version 0.5.2, runtime %q, mode %q, an index schema", *s, cfg.RuntimeKind(), cfg.Server.Mode)
	}
	if !s.Reach.LastOK.Equal(ago(time.Minute)) || !s.Reach.BreakerOpenUntil.Equal(fxNow.Add(time.Minute)) || s.Reach.BreakerFailures != 2 || !s.Reach.FailingSince.IsZero() {
		t.Errorf("reach = %+v", s.Reach)
	}
	if !s.Backup.CreatedAt.Equal(ago(day)) || !s.Backup.OK || !s.Backup.VerifiedAt.IsZero() || !s.Update.CheckedAt.Equal(ago(time.Hour)) || s.Update.Newer != 3 || !s.Update.CheckFailed {
		t.Errorf("backup %+v update %+v", s.Backup, s.Update)
	}
	raw := string(mustJSON(t, s))
	if strings.Contains(raw, "failing_since") || strings.Contains(raw, "verified_at") {
		t.Errorf("unset times are written: %s", raw)
	}
	if !slices.Equal(slices.Sorted(func(yield func(string) bool) {
		for id := range r.Metrics {
			if !yield(id) {
				return
			}
		}
	}), []string{"doctor.check", "jobs.last_run", "local.backlog", "server.reach"}) {
		t.Errorf("system metrics = %v", r.Metrics)
	}
}
