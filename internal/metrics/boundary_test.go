package metrics

import (
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The windows are half-open, [Since, Until): an event at exactly Since is
// the window's, one at exactly Until the next window's (else it would count
// in two adjacent windows). Each family's windowed reads are pinned here on
// a fixed past window, so that nothing "as of now" gets in the way.

// edgeWindow is a 7-day window ending two days before fxNow, off the
// hour so that no day or hour bucket starts on its edges.
func edgeWindow(t *testing.T) Query {
	t.Helper()
	since := time.Date(2026, 10, 20, 10, 17, 0, 0, fxParis)
	w, err := NewWindow(fxNow, 0, since, since.Add(7*day), fxParis, "Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	return Query{Window: w, Machines: "this"}
}

// edgeCase is one windowed read: write puts one event at at; count reads
// what the window counted of it from the family's metrics.
type edgeCase struct {
	name  string
	ids   []string
	write func(x *fx, at time.Time)
	count func(ms map[string]Metric) int64
}

// denOf is a metric's n, valOf its value (0 when absent).
func denOf(id string) func(map[string]Metric) int64 {
	return func(ms map[string]Metric) int64 { return den0(ms[id]) }
}

func valOf(id string) func(map[string]Metric) int64 {
	return func(ms map[string]Metric) int64 {
		if v := ms[id].Value; v != nil {
			return int64(*v)
		}
		return 0
	}
}

// runEdges checks each case: the event at Since counts 1, the one at Until
// (a millisecond-exact instant) counts 0.
func runEdges(t *testing.T, cases []edgeCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, e := range []struct {
				where string
				want  int64
			}{{"at Since", 1}, {"at Until", 0}} {
				x := newFx(t, fxNow)
				x.local(index.LocalStatus{})
				q := edgeWindow(t)
				at := q.Window.Since
				if e.want == 0 {
					at = q.Window.Until
				}
				c.write(x, at)
				if got := c.count(x.metrics(c.ids, q)); got != e.want {
					t.Errorf("%s: counted %d, want %d", e.where, got, e.want)
				}
			}
		})
	}
}

func TestWindowEdgesReach(t *testing.T) {
	prompt := func(x *fx, at time.Time) {
		x.tel(at, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Injected: 1}, "s", "alpha")
	}
	runEdges(t, []edgeCase{
		{"prompts", reachIDs, prompt, denOf("recall.hit_rate")},
		{"memory_search calls", reachIDs, func(x *fx, at time.Time) {
			x.tel(at, telemetry.Tool{Name: "memory_search", OK: true, MS: 40}, "s", "alpha")
		}, denOf("search.empty_rate")},
		{"a search in the prompt's turn", reachIDs, func(x *fx, at time.Time) {
			x.tel(at, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80}, "s", "alpha")
			x.toolUse(at, "s", "alpha", 1, "mcp__plugin_backsight_memory__memory_search", false)
		}, func(ms map[string]Metric) int64 {
			if n := ms["search.rate"].Num; n != nil {
				return *n
			}
			return 0
		}},
	})
}

func TestWindowEdgesQuality(t *testing.T) {
	runEdges(t, []edgeCase{
		{"turns by their show", qualityIDs, func(x *fx, at time.Time) {
			x.declaredTurn(at, "s", "declared", "used", "m1")
		}, denOf("declare.coverage")},
		{"wrong rate by show time", qualityIDs, func(x *fx, at time.Time) {
			x.declaredTurn(at, "s", "declared", "wrong", "m1")
		}, denOf("verdict.wrong_rate")},
		{"a credit at the show's instant", qualityIDs, func(x *fx, at time.Time) {
			x.shows(at, "s", fxTurn, "", "alpha", "prompt", "", "m1")
			x.credit(at, "s", "", "alpha", "wrong", "m1")
		}, denOf("verdict.wrong_rate")},
		{"settled shows by show time", qualityIDs, func(x *fx, at time.Time) {
			keys := x.shows(at, "s", fxTurn, "", "alpha", "prompt", "", "m1")
			x.settle(at.Add(time.Hour), map[string]string{keys[0]: feedback.OutcomeCredit})
		}, denOf("used.share_confident")},
		{"shows not settled yet", qualityIDs, func(x *fx, at time.Time) {
			x.shows(at, "s", fxTurn, "", "alpha", "prompt", "", "m1")
		}, func(ms map[string]Metric) int64 {
			n, _ := ms["used.share_confident"].Extra["pending"].(int64)
			return n
		}},
	})
}

func TestWindowEdgesCost(t *testing.T) {
	runEdges(t, []edgeCase{
		{"sessions' context", costIDs, func(x *fx, at time.Time) {
			x.tel(at, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Injected: 1, Chars: 300, MS: 200}, "s", "alpha")
		}, denOf("context.per_session")},
		{"sessions' briefings", costIDs, func(x *fx, at time.Time) {
			x.tel(at, telemetry.Briefing{IDs: []string{"m1"}, Chars: 900, Source: "observations"}, "s", "alpha")
		}, denOf("context.per_session")},
		{"prompts' context", costIDs, func(x *fx, at time.Time) {
			x.tel(at, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Injected: 1, Chars: 300, MS: 200}, "s", "alpha")
		}, denOf("context.per_prompt")},
		{"recall latency", costIDs, func(x *fx, at time.Time) {
			x.tel(at, telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, MS: 200}, "s", "alpha")
		}, denOf("recall.latency")},
		{"search latency", costIDs, func(x *fx, at time.Time) {
			x.tel(at, telemetry.Tool{Name: "memory_search", OK: true, MS: 40}, "s", "alpha")
		}, denOf("search.latency")},
	})
}

func TestWindowEdgesBank(t *testing.T) {
	runEdges(t, []edgeCase{
		{"retirements", bankIDs, func(x *fx, at time.Time) { x.retire(at, "forget") }, valOf("bank.churn")},
		{"notes stored", bankIDs, func(x *fx, at time.Time) { x.store(at, "saved", "decision", "alpha") }, valOf("notes.stored")},
	})
}

func TestWindowEdgesPipeline(t *testing.T) {
	runEdges(t, []edgeCase{
		{"documents sent", pipelineIDs, func(x *fx, at time.Time) { x.retained(at, "d1", "ok", 100) }, denOf("pipeline.flow")},
		{"prompts the breaker skipped", pipelineIDs, func(x *fx, at time.Time) {
			x.tel(at, telemetry.Prompt{N: 1, Decision: "skip", Reason: "breaker"}, "s", "alpha")
		}, valOf("server.reach")},
		{"hook failures", pipelineIDs, func(x *fx, at time.Time) {
			x.tel(at, telemetry.HookFailure{Hook: "prompt", Kind: "exit", MS: 5}, "s", "alpha")
		}, valOf("hooks.failures")},
	})
}

// TestDayBucketEdges: a day is [local midnight, next local midnight): a
// failure at exactly midnight Paris on 10-22 is that day's, one a
// millisecond before is 10-21's.
func TestDayBucketEdges(t *testing.T) {
	midnight := time.Date(2026, 10, 22, 0, 0, 0, 0, fxParis)
	for _, c := range []struct {
		at  time.Time
		day string
	}{{midnight, "2026-10-22"}, {midnight.Add(-time.Millisecond), "2026-10-21"}} {
		x := newFx(t, fxNow)
		x.tel(c.at, telemetry.HookFailure{Hook: "prompt", Kind: "exit", MS: 5}, "s", "alpha")
		m := x.metrics(pipelineIDs, edgeWindow(t))["hooks.failures"]
		var days []string
		for _, p := range m.Series {
			if p.Value != nil && *p.Value > 0 {
				days = append(days, p.Day)
			}
		}
		if len(days) != 1 || days[0] != c.day {
			t.Errorf("failure at %v: on days %v, want %s", c.at, days, c.day)
		}
	}
}

// TestHookFailuresLast24hEdges: hooks.failures is judged on [now - 24 h,
// now): a failure at exactly now - 24 h counts, one at now does not.
func TestHookFailuresLast24hEdges(t *testing.T) {
	for _, c := range []struct {
		at   time.Time
		want int64
	}{{fxNow.Add(-day), 1}, {fxNow, 0}} {
		x := newFx(t, fxNow)
		x.tel(c.at, telemetry.HookFailure{Hook: "prompt", Kind: "exit", MS: 5}, "s", "alpha")
		x.prompts("alpha", "p", fxNow, 1, nil) // something recorded either way
		m := x.metrics(pipelineIDs, x.query(7))["hooks.failures"]
		last, _ := m.Extra["last_24h"].(map[string]int64)
		if got := last["exit"]; got != c.want {
			t.Errorf("failure at %v: last_24h %v, want exit %d", c.at, last, c.want)
		}
	}
}
