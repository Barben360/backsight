package metrics

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/telemetry"
)

// fullFx is the fixture every page widget reads from: two weeks of
// activity (the 7 days before fxNow and the 7 before them) on every source,
// a server holding stats, series, tags, LLM stats and operations, a doctor
// run, this PC's local status and a ledger with memories to review.
//
// The verdict it gives puts all four axes off ok, each with its reasons:
// Found and Right learning (15 typed prompts this week, under 20; the 105
// machine recalls feed the rates and latencies but are not typed), Cost in
// attention (every recall this week takes 3 s: recall.latency bad at
// once), Flowing in attention (the doctor's metrics-index check fails, a
// hook panicked); 6 attention items in all.
func fullFx(t *testing.T) *fx {
	t.Helper()
	x := newFx(t, fxNow)
	pricesSet(x)
	extractionServer(x)
	prev := fxNow.Add(-7 * day)
	for _, wk := range []struct {
		end    time.Time
		tag    string
		ms     int
		typed  int
		turnsN int
	}{{fxNow, "c", 3000, 15, 25}, {prev, "p", 500, 30, 25}} {
		// machine recalls: half inject, in two languages
		x.prompts("alpha", wk.tag+"-mach", wk.end.Add(-30*time.Minute), 105, func(i int, p *telemetry.Prompt) {
			p.Reason, p.MS, p.Lang = "machine", wk.ms, []string{"en", "fr"}[i%2]
			if i%2 == 0 {
				p.Injected, p.Chars = 1, 400+10*i
			} else {
				p.Empty = "no_candidates"
			}
		})
		// typed prompts: a third inject; a few are skipped
		typed := x.prompts("alpha", wk.tag+"-typed", wk.end.Add(-45*time.Minute), wk.typed, func(i int, p *telemetry.Prompt) {
			p.MS, p.Lang = wk.ms, "en"
			switch {
			case i%3 == 0:
				p.Injected, p.Chars = 2, 1200
			case i%5 == 1:
				p.Decision, p.Reason = "skip", "too_short"
			default:
				p.Empty = "below_floor"
			}
		})
		for i, s := range typed[:6] { // Claude searched memory in 6 typed turns
			x.toolUse(wk.end.Add(-time.Duration(i+1)*time.Hour-40*time.Minute), s, "alpha", 1, "mcp__plugin_backsight_memory__memory_search", false)
		}
		for i := range 110 { // a briefing per session: 110 sessions beside the typed ones
			x.tel(wk.end.Add(-time.Duration(i+1)*time.Hour-50*time.Minute),
				telemetry.Briefing{IDs: []string{"b1"}, Chars: 900 + 100*(i%40), Source: "observations"}, fmt.Sprintf("%s-brief-%d", wk.tag, i), "alpha")
		}
		for i := range 30 {
			x.tel(wk.end.Add(-time.Duration(i+1)*time.Hour-55*time.Minute), telemetry.Tool{Name: "memory_search", MS: 300 + 10*i, Hits: i % 4, OK: true}, wk.tag+"-typed-0", "alpha")
		}
		// declared turns: 15 credit both memories used, 10 report them wrong
		for i := range wk.turnsN {
			at := wk.end.Add(-time.Duration(i+3) * time.Hour)
			s := fmt.Sprintf("%s-turn-%d", wk.tag, i)
			x.turnEvent(at.Add(-time.Second), s, "alpha")
			keys := x.shows(at, s, fxTurn, "", "alpha", "prompt", "", fmt.Sprintf("%s-m%d", wk.tag, i), fmt.Sprintf("%s-n%d", wk.tag, i))
			verdict := "used"
			if i >= 15 {
				verdict = "wrong"
			}
			x.credit(at.Add(30*time.Second), s, "", "alpha", verdict, fmt.Sprintf("%s-m%d", wk.tag, i), fmt.Sprintf("%s-n%d", wk.tag, i))
			x.stop(at.Add(time.Minute), s, "", "declared")
			if i == 0 { // a memory Claude found itself, used
				keys = append(keys, x.shows(at.Add(10*time.Second), s, fxTurn, "", "alpha", "search", "", wk.tag+"-found")...)
				x.credit(at.Add(40*time.Second), s, "", "alpha", "used", wk.tag+"-found")
			}
			if i == wk.turnsN-1 {
				continue // its shows stay unsettled: pending
			}
			out := map[string]string{}
			for k, key := range keys {
				switch {
				case verdict == "used":
					out[key] = feedback.OutcomeCredit
				case k == 0:
					out[key] = feedback.OutcomeMiss
				default:
					out[key] = []string{feedback.OutcomeUnknown, feedback.OutcomeShown, feedback.OutcomeForgotten}[i%3]
				}
			}
			x.settle(at.Add(2*time.Hour), out)
		}
		x.tel(wk.end.Add(-2*time.Hour), telemetry.Credit{Verdict: "used", IDs: 2, OK: true}, wk.tag+"-turn-0", "alpha")
		for i := range 6 {
			x.retained(wk.end.Add(-time.Duration(i+2)*time.Hour), fmt.Sprintf("conversation:%s-doc%d", wk.tag, i), "ok", 800+100*i)
		}
	}
	x.retained(ago(3*time.Hour), "c-doc-failed", "failed", 0)
	x.retained(ago(20*time.Minute), "c-doc-queued", "queued", 0)
	x.tel(ago(5*time.Hour), telemetry.HookFailure{Hook: "prompt", Kind: "panic", MS: 10}, "c-typed-1", "alpha")
	x.tel(ago(5*time.Minute), telemetry.Job{Name: "tick", Status: "done", MS: 40}, "", "")
	x.tel(ago(3*time.Hour), telemetry.Job{Name: "maintain", Status: "done", MS: 900}, "", "")
	x.store(ago(26*time.Hour), "saved", "decision", "alpha")
	x.store(ago(27*time.Hour), "refused", "", "alpha")
	x.retire(ago(30*time.Hour), "forget")
	x.retire(ago(50*time.Hour), "hide")

	l := staleLedger()
	for id, m := range l.Memories {
		m.LastShown = ago(time.Duration(len(id)) * time.Hour)
		l.Memories[id] = m
	}
	x.ledger(l)
	x.local(index.LocalStatus{NotesQueued: 2, NotesOldestAt: ago(time.Hour), ReachLastOK: ago(time.Minute), BreakerFailures: 1,
		BackupCreatedAt: ago(20 * time.Hour), BackupVerifiedAt: ago(19 * time.Hour), BackupOK: true, UpdateCheckedAt: ago(2 * time.Hour), UpdateNewer: 1})
	x.doctor = []DoctorCheck{{ID: "server", Status: "ok", Fix: "backsight doctor"}, {ID: "update", Status: "warn"},
		{ID: "metrics-index", Status: "error", Fix: "backsight stats index rebuild"}}
	x.server.set(func(d *fakeData) {
		d.Version = hindsight.Version{APIVersion: "0.5.2"}
		d.Stats = hindsight.BankStats{NodesByFactType: map[string]int{"world": 900, "experience": 300, "observation": 41},
			TotalDocuments: 52, TotalLinks: 3000, PendingConsolidation: 2, LastConsolidatedAt: ago(4 * time.Hour)}
		d.Tags = []hindsight.TagCount{{Tag: "project:alpha", Count: 120}, {Tag: "project:beta", Count: 7}, {Tag: "kind:decision", Count: 3}}
		for k := range 6 {
			d.Series.Buckets = append(d.Series.Buckets, hindsight.SeriesBucket{Time: utcDay(29 - k), World: int64(3 + k), Experience: 1, Observation: int64(k % 2)})
		}
		d.Ops = append(fakeOps(110, "completed", func(i int) (time.Time, time.Time) {
			u := ago(time.Duration(i+1) * time.Hour)
			return u, u.Add(-time.Duration(5+i%20) * time.Minute)
		}), fakeOps(2, "failed", func(i int) (time.Time, time.Time) {
			return ago(time.Duration(i+1) * day), ago(time.Duration(i+1)*day + time.Hour)
		})...)
		d.Ops = append(d.Ops, fakeOps(1, "pending", func(int) (time.Time, time.Time) { return ago(time.Minute), ago(time.Minute) })...)
	})
	return x
}

// TestFullFixtureVerdict: the full fixture's verdict is as fullFx says, so
// the widget rows below may ask for every axis's reasons.
func TestFullFixtureVerdict(t *testing.T) {
	x := fullFx(t)
	r := x.run("overview", x.query(7))
	wantAxes(t, "full fixture", *r.Verdict, axes{AxisLearning, AxisLearning, AxisAttention, AxisAttention})
}

// widget is one widget of a page and the field it reads: want is "set"
// (the key exists, its value is not null, and an array or object is not
// empty) or "present" (the key exists; note says why the fixture cannot
// set it). A path's segments are separated by "/" (metric ids hold dots);
// "x[]" fans out over the array x, and the row passes when one of its
// elements does.
type widget struct {
	page, widget, view, path, want, note string
}

// rows is one "set" row per field of the comma-separated fields, each
// under prefix.
func rows(page, w, view, prefix, fields string) []widget {
	var out []widget
	for _, f := range strings.Split(fields, ",") {
		out = append(out, widget{page, w, view, prefix + f, "set", ""})
	}
	return out
}

// pageWidgets is one row per widget field of the six pages (spec §8);
// the browse pages are Task 14's.
func pageWidgets() []widget {
	var ws []widget
	add := func(r ...widget) { ws = append(ws, r...) }
	const set, present = "set", "present"
	// Overview
	for _, ax := range []string{"found", "right", "cost", "flowing"} {
		add(rows("overview", "verdict", "overview", "verdict/axes/"+ax+"/", "state,reasons")...)
	}
	add(widget{"overview", "verdict", "overview", "verdict/window_days", set, ""})
	add(rows("overview", "summary", "overview", "summary/", "prompts_with_memory,turns_with_memory_used,context_chars,tokens_approx,extraction_cost,currency,price_set,extraction_scope")...)
	for _, id := range overviewTiles {
		add(rows("overview", "tile "+id, "overview", "metrics/"+id+"/", "value,den,status,borderline,delta,series,drill")...)
		add(widget{"overview", "tile " + id, "overview", "metrics/" + id + "/tags", present, "[] unless a tag applies; no tag applies to every tile"})
		if d, _ := DefOf(id); d.Unit == "ratio" {
			add(rows("overview", "tile "+id, "overview", "metrics/"+id+"/", "num,ci")...)
		} else {
			add(widget{"overview", "tile " + id, "overview", "metrics/" + id + "/num", present, "a percentile has no numerator: null"})
		}
	}
	add(widget{"overview", "tile used.turn_rate", "overview", "metrics/declare.coverage/value", set, ""})
	add(rows("overview", "attention", "overview", "attention[]/", "metric,severity,axis,text,why,command")...)
	add(widget{"overview", "attention", "overview", "attention_more", set, ""})
	add(rows("overview", "lately", "overview", "tables/lately[]/", "project,last,shown,used,wrong")...)
	add(rows("overview", "memory at a glance", "overview", "metrics/", "bank.size/value,bank.growth/value")...)
	add(rows("overview", "memory at a glance", "overview", "info[]/", "metric,count,command")...)
	// Quality
	add(rows("quality", "used", "quality", "metrics/", "used.turn_rate/series,declare.coverage/series")...)
	add(rows("quality", "outcome mix", "quality", "tables/outcome_mix[]/", "day,used,ignored,not_declared,shown_only,pending")...)
	add(widget{"quality", "outcome mix", "quality", "metrics/used.share_confident/extra/forgotten", set, ""})
	add(rows("quality", "reported", "quality", "tables/reported[]/", "memory_id,verdict,session,turn")...)
	add(rows("quality", "found", "quality", "metrics/", "recall.hit_rate/value,recall.empty_reasons/series,search.rate/value,search.empty_rate/value")...)
	add(rows("quality", "developer", "quality", "metrics/", "used.share_confident/extra,used.rank/extra,used.by_path/extra,recall.by_lang/extra")...)
	add(rows("quality", "developer", "quality", "metrics/", "search.rescue_rate/value,used.rubber_stamp/value")...) // no extra in the catalog: the value
	// Cost
	add(rows("cost", "context", "cost", "metrics/context.per_session/extra/", "histogram,p50,p95")...)
	add(rows("cost", "context", "cost", "metrics/", "context.per_session/series[]/parts,context.per_prompt/value")...)
	add(rows("cost", "speed", "cost", "metrics/", "recall.latency/series[]/p95,search.latency/series")...)
	add(rows("cost", "extraction", "cost", "tables/extraction[]/", "day,operation,input,cached,output,cost")...)
	add(widget{"cost", "extraction", "cost", "metrics/extraction.per_session/value", set, ""})
	// Bank
	add(rows("bank", "size", "bank", "metrics/bank.size/extra/", "world,experience,observation,documents")...)
	add(rows("bank", "growth", "bank", "metrics/", "bank.growth/series[]/parts,bank.consolidation/value,notes.stored/value,bank.churn/series")...)
	add(rows("bank", "by project", "bank", "tables/by_project[]/", "project,memories")...)
	for _, list := range []string{"never_used", "hidden", "reported"} {
		add(rows("bank", "review", "bank", "tables/review/"+list+"[]/", "id,project,shown,last_shown,command")...)
	}
	// Pipeline
	add(rows("pipeline", "flow", "pipeline", "metrics/", "pipeline.flow/series,retain.outcomes/series,local.backlog/extra,server.queue/value,server.extraction_delay/value,hooks.failures/value,jobs.last_run/extra")...)
	add(rows("pipeline", "failed", "pipeline", "tables/failed_operations[]/", "type,age_ms,retries")...)
	add(widget{"pipeline", "jobs", "pipeline", "tables/jobs", set, ""})
	// System
	add(rows("system", "doctor", "system", "tables/doctor[]/", "id,status")...)
	add(widget{"system", "doctor", "system", "tables/doctor[]/fix", present, "a check without a backsight fix has \"\""})
	add(rows("system", "services", "system", "tables/system/", "server_version,runtime,reach,backup,update,index")...)
	add(widget{"system", "jobs", "system", "tables/jobs", set, ""})
	// every page
	for _, v := range pageViews {
		add(rows(v, "scope", v, "scope/", "machines,server,server_ok")...)
		add(rows(v, "window", v, "window/", "since,until,days,tz")...)
		add(rows(v, "freshness", v, "index/", "ingested_at,lag_seconds,partial")...)
		add(widget{v, "freshness", v, "generated_at", set, ""})
	}
	return ws
}

// TestEveryWidgetHasAField (acceptance): each widget of the six pages
// reads a field of its view's JSON on the full fixture.
func TestEveryWidgetHasAField(t *testing.T) {
	x := fullFx(t)
	views := map[string]any{}
	for _, v := range pageViews {
		views[v] = asJSON(t, x.run(v, x.query(7)))
	}
	for _, w := range pageWidgets() {
		leaves := lookup(views[w.view], strings.Split(w.path, "/"))
		ok := false
		for _, l := range leaves {
			if w.want == "present" || isSet(l) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s / %s: %s %s is not %s (found %d: %v)", w.page, w.widget, w.view, w.path, w.want, len(leaves), leaves)
		}
	}
}

// asJSON is v marshalled and read back as maps and slices.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// lookup is the values at path in v, one per element where a segment ends
// in "[]"; a key missing on the way yields nothing.
func lookup(v any, path []string) []any {
	if len(path) == 0 {
		return []any{v}
	}
	seg, fan := strings.CutSuffix(path[0], "[]")
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	next, ok := m[seg]
	if !ok {
		return nil
	}
	if !fan {
		return lookup(next, path[1:])
	}
	arr, _ := next.([]any)
	var out []any
	for _, el := range arr {
		out = append(out, lookup(el, path[1:])...)
	}
	return out
}

// isSet: not null, and an array or object holds something.
func isSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}
