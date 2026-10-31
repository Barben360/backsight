package metrics

import (
	"context"
	"fmt"
	"math"
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

// The verdict's fixtures. The clock is fxNow (2026-10-30T12:00+01:00), the
// verdict's window the 7 days before it, its previous window the 7 days
// before that. Every case writes out the four axis states it expects.

// assess runs the verdict for q through withTx, as the overview does.
func (x *fx) assess(q Query) assessment {
	x.t.Helper()
	var a assessment
	err := withTx(context.Background(), x.source(), q, x.need, func(e env) error {
		var err error
		a, err = assess(e)
		return err
	})
	if err != nil {
		x.t.Fatal(err)
	}
	return a
}

// axes is a verdict's four states, found right cost flowing.
type axes [4]AxisState

func statesOf(v Verdict) axes {
	var out axes
	for i, name := range axisOrder {
		out[i] = v.Axes[name].State
	}
	return out
}

func wantAxes(t *testing.T, what string, v Verdict, want axes) {
	t.Helper()
	if len(v.Axes) != 4 {
		t.Fatalf("%s: %d axes, want 4", what, len(v.Axes))
	}
	if got := statesOf(v); got != want {
		t.Errorf("%s: axes (found right cost flowing) = %v, want %v", what, got, want)
	}
}

const (
	okay     = AxisOK
	attn     = AxisAttention
	early    = AxisTooEarly
	cant     = AxisCantJudge
	learning = AxisLearning
)

// firstPrompt is a typed prompt 20 days ago: this PC is past learning
// once it has 20 typed prompts in the last 7 days.
func firstPrompt(x *fx) { x.prompts("alpha", "first", fxNow.Add(-20*day), 1, nil) }

// recalls writes n recalls ending at end, one per hour, each in its own
// session, the first inj injecting, each taking ms and adding chars.
func recalls(x *fx, prefix string, end time.Time, n, inj, ms, chars int) {
	x.prompts("alpha", prefix, end, n, func(i int, p *telemetry.Prompt) {
		p.MS, p.Chars = ms, chars
		if i < inj {
			p.Injected = 1
		}
	})
}

// turns writes n declared main-agent turns ending at end, one per hour (3 h
// back first, past the open-turn limit), each showing one memory; verdict
// gives the i-th turn's credit ("" for none).
func turns(x *fx, prefix string, end time.Time, n int, outcome string, verdict func(i int) string) {
	for i := range n {
		v := ""
		if verdict != nil {
			v = verdict(i)
		}
		x.declaredTurn(end.Add(-time.Duration(i+3)*time.Hour), fmt.Sprintf("%s-%d", prefix, i), outcome, v, fmt.Sprintf("%s-m%d", prefix, i))
	}
}

// firstK is a verdict: the first k turns credit a, the rest b.
func firstK(k int, a, b string) func(int) string {
	return func(i int) string {
		if i < k {
			return a
		}
		return b
	}
}

func reasonOf(a Axis, metric string) (Reason, bool) {
	i := slices.IndexFunc(a.Reasons, func(r Reason) bool { return r.Metric == metric })
	if i < 0 {
		return Reason{}, false
	}
	return a.Reasons[i], true
}

func itemsOf(items []Item, metric string) []Item {
	var out []Item
	for _, it := range items {
		if it.Metric == metric {
			out = append(out, it)
		}
	}
	return out
}

func metricsOfItems(items []Item) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Metric)
	}
	return out
}

// TestVerdictIgnoresTheSelectedWindow: the same state judged under a
// 30-day and a 7-day view gives the same verdict, attention and info,
// although the 30-day window holds 30 prompts that got nothing.
func TestVerdictIgnoresTheSelectedWindow(t *testing.T) {
	x := newFx(t, fxNow)
	recalls(x, "old", fxNow.Add(-20*day), 30, 0, 0, 0)
	recalls(x, "cur", fxNow, 25, 25, 0, 0)
	x.local(index.LocalStatus{NotesQueued: 2, NotesOldestAt: ago(2 * time.Hour)})
	x.doctor = []DoctorCheck{{ID: "latency", Status: "warn"}}
	x.ledger(feedback.Ledger{Memories: map[string]feedback.Entry{"n": {Shown: 12}}})

	q7, q30 := x.query(7), x.query(30)
	a7, a30 := x.assess(q7), x.assess(q30)
	wantAxes(t, "7 days", a7.Verdict, axes{okay, early, early, attn})
	if !reflect.DeepEqual(a7.Verdict, a30.Verdict) {
		t.Errorf("verdict under 30 days =\n%+v\nunder 7 days =\n%+v", a30.Verdict, a7.Verdict)
	}
	if !reflect.DeepEqual(a7.Items, a30.Items) || !reflect.DeepEqual(a7.Info, a30.Info) {
		t.Errorf("attention %v / info %v under 30 days, %v / %v under 7", a30.Items, a30.Info, a7.Items, a7.Info)
	}
	if h := a30.Now["recall.hit_rate"]; den0(h) != 25 || num(h) != 25 {
		t.Errorf("hit rate judged = %d/%d, want the last 7 days' 25/25", num(h), den0(h))
	}
	if a30.Verdict.WindowDays != 7 || a30.Verdict.Axes["found"].WindowDays != 7 {
		t.Errorf("window_days = %d (found %d), want 7", a30.Verdict.WindowDays, a30.Verdict.Axes["found"].WindowDays)
	}
}

// TestTwoWindowRule: Found, Right and Cost go to attention on a metric at
// watch or bad in both weeks; only recall.latency at bad counts at once.
// recall.hit_rate watches when the upper bound of 0 of 40 (0.088) is under
// 10%; 14 wrong of 20 has a lower bound of 0.48, over 15%: bad.
func TestTwoWindowRule(t *testing.T) {
	wrong14 := func(x *fx, prevWrong int) {
		recalls(x, "cur", fxNow, 25, 25, 0, 0)
		turns(x, "c", fxNow, 20, "declared", firstK(14, "wrong", "used"))
		turns(x, "p", since7, 20, "declared", firstK(prevWrong, "wrong", "used"))
	}
	for _, c := range []struct {
		name   string
		fill   func(x *fx)
		want   axes
		metric string // the reason and the item of the axis in attention
		text   string
	}{
		{"hit_rate watch this week only", func(x *fx) {
			recalls(x, "cur", fxNow, 40, 0, 0, 0)
			recalls(x, "prev", since7, 40, 40, 0, 0)
		}, axes{okay, early, early, okay}, "", ""},
		{"hit_rate watch both weeks", func(x *fx) {
			recalls(x, "cur", fxNow, 40, 0, 0, 0)
			recalls(x, "prev", since7, 40, 0, 0, 0)
		}, axes{attn, early, early, okay}, "recall.hit_rate", "Prompts that got memory: 0 %."},
		{"wrong_rate bad this week only", func(x *fx) { wrong14(x, 0) }, axes{okay, okay, early, okay}, "", ""},
		{"wrong_rate bad both weeks", func(x *fx) { wrong14(x, 14) }, axes{okay, attn, early, okay},
			"verdict.wrong_rate", "14 memories reported wrong or out of date this week."},
		{"learning comes before attention, the item stays listed", func(x *fx) {
			recalls(x, "cur", fxNow, 10, 10, 0, 0) // 10 typed prompts in 7 days: learning
			turns(x, "c", fxNow, 20, "declared", firstK(14, "wrong", "used"))
			turns(x, "p", since7, 20, "declared", firstK(14, "wrong", "used"))
		}, axes{learning, learning, early, okay}, "verdict.wrong_rate", "14 memories reported wrong or out of date this week."},
		{"latency bad this week only", func(x *fx) {
			recalls(x, "cur", fxNow, 100, 100, 3000, 0)
			recalls(x, "prev", since7, 100, 100, 100, 0)
		}, axes{okay, early, attn, okay}, "recall.latency", "Memory adds 3.0 s to the slowest prompts (limit 2.5 s)."},
		{"latency watch this week only", func(x *fx) {
			recalls(x, "cur", fxNow, 100, 100, 2200, 0)
			recalls(x, "prev", since7, 100, 100, 100, 0)
		}, axes{okay, early, okay, okay}, "", ""},
		{"latency watch both weeks", func(x *fx) {
			recalls(x, "cur", fxNow, 100, 100, 2200, 0)
			recalls(x, "prev", since7, 100, 100, 2200, 0)
		}, axes{okay, early, attn, okay}, "recall.latency", "Memory adds 2.2 s to the slowest prompts (limit 2.5 s)."},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newFx(t, fxNow)
			firstPrompt(x)
			c.fill(x)
			a := x.assess(x.query(7))
			wantAxes(t, c.name, a.Verdict, c.want)
			if c.metric == "" {
				if len(a.Items) != 0 {
					t.Errorf("attention = %v, want none", metricsOfItems(a.Items))
				}
				return
			}
			axis := familyAxis[mustDef(t, c.metric).Family]
			r, ok := reasonOf(a.Verdict.Axes[axis], c.metric)
			if !ok || r.Text != c.text || r.Command != mustDef(t, c.metric).Command {
				t.Errorf("%s reasons = %+v, want %s %q with its command", axis, a.Verdict.Axes[axis].Reasons, c.metric, c.text)
			}
			if got := metricsOfItems(a.Items); !slices.Equal(got, []string{c.metric}) {
				t.Errorf("attention = %v, want [%s]", got, c.metric)
			}
		})
	}
}

// TestFlowingIsImmediate: Flowing reads its error-class metrics and the
// doctor's errors now, without the two-window rule; a doctor warning but
// update is listed without changing the axis.
func TestFlowingIsImmediate(t *testing.T) {
	for _, c := range []struct {
		name  string
		fill  func(x *fx)
		want  AxisState
		items []Item
	}{
		{"one exit failure", func(x *fx) {
			x.tel(ago(time.Hour), telemetry.HookFailure{Hook: "prompt", Kind: "exit", MS: 5}, "s", "alpha")
		}, attn, []Item{{Metric: "hooks.failures", Severity: SeverityWatch, Axis: "flowing", Text: "1 step crashed in the last 24 h.",
			Why: mustDefAction("hooks.failures"), Command: "backsight doctor"}}},
		{"doctor error", func(x *fx) {
			x.doctor = []DoctorCheck{{ID: "metrics-index", Status: "error", Fix: "backsight stats index rebuild"}}
		}, attn, []Item{{Metric: "doctor.check", Severity: SeverityWatch, Axis: "flowing", Text: "Health check «activity index» failed.",
			Why: mustDefAction("doctor.check"), Command: "backsight stats index rebuild"}}},
		{"doctor error on the server alone", func(x *fx) {
			x.doctor = []DoctorCheck{{ID: "server", Status: "error"}}
		}, attn, []Item{{Metric: "doctor.check", Severity: SeverityWatch, Axis: "flowing", Text: "Health check «memory server» failed.",
			Why: mustDefAction("doctor.check"), Command: "backsight doctor"}}},
		{"doctor warning", func(x *fx) {
			x.doctor = []DoctorCheck{{ID: "latency", Status: "warn"}, {ID: "server", Status: "ok"}}
		}, okay, []Item{{Metric: "doctor.check", Severity: SeverityWatch, Axis: "flowing", Text: "Health check «speed» reports a warning.",
			Why: mustDefAction("doctor.check"), Command: "backsight doctor"}}},
		{"update available", func(x *fx) {
			x.doctor = []DoctorCheck{{ID: "update", Status: "warn"}}
		}, okay, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newFx(t, fxNow)
			c.fill(x)
			a := x.assess(x.query(7))
			wantAxes(t, c.name, a.Verdict, axes{early, early, early, c.want})
			if !reflect.DeepEqual(a.Items, c.items) {
				t.Errorf("attention = %+v, want %+v", a.Items, c.items)
			}
			fl := a.Verdict.Axes["flowing"]
			if c.want == okay && len(fl.Reasons) != 0 {
				t.Errorf("flowing ok with reasons %+v, want none", fl.Reasons)
			}
			if c.want == attn && (len(fl.Reasons) != 1 || fl.Reasons[0].Text != c.items[0].Text || fl.Reasons[0].Command != c.items[0].Command) {
				t.Errorf("flowing reasons = %+v, want the item with its command", fl.Reasons)
			}
		})
	}
}

func mustDefAction(id string) string {
	d, _ := DefOf(id)
	return d.Action
}

// TestRootCauseCollapse: a server that does not answer gives Flowing one
// reason for the backlog, the waiting sessions, the queue, the doctor's
// server checks and the reach; watch under 24 h of backlog, bad after.
func TestRootCauseCollapse(t *testing.T) {
	today := func(h, m int) time.Time { return time.Date(2026, 10, 30, h, m, 0, 0, fxParis) }
	down := func(x *fx) {
		x.server.failAll(hindsight.ErrUnreachable)
		x.doctor = []DoctorCheck{{ID: "server", Status: "error"}, {ID: "server-reach", Status: "error"}}
	}
	for _, c := range []struct {
		name   string
		now    time.Time
		fill   func(x *fx)
		root   *Item  // nil: no root cause
		others []Item // the other items
	}{
		{"failing since this morning", fxNow, func(x *fx) {
			x.local(index.LocalStatus{ReachFailingSince: today(9, 12), ReachLastOK: today(9, 10), NotesQueued: 14, NotesOldestAt: fxNow.Add(-3 * time.Hour), SessionsWaiting: 3})
			down(x)
		}, &Item{Severity: SeverityWatch, Text: "Memory server unreachable since 09:12: 14 notes and 3 sessions wait on this PC, nothing is lost."}, nil},
		{"failing since yesterday, backlog 25 h", today(10, 0), func(x *fx) {
			x.local(index.LocalStatus{ReachFailingSince: time.Date(2026, 10, 29, 9, 12, 0, 0, fxParis), NotesQueued: 14,
				NotesOldestAt: today(10, 0).Add(-25 * time.Hour), SessionsWaiting: 3})
			down(x)
		}, &Item{Severity: SeverityBad, Text: "Memory server unreachable since 2026-10-29 09:12: 14 notes and 3 sessions wait on this PC, nothing is lost."}, nil},
		{"breaker open, server answering", fxNow, func(x *fx) {
			x.local(index.LocalStatus{ReachLastOK: today(8, 30), BreakerOpenUntil: fxNow.Add(10 * time.Minute), NotesQueued: 1, NotesOldestAt: fxNow.Add(-10 * time.Minute)})
		}, &Item{Severity: SeverityWatch, Text: "Memory server unreachable since 08:30: 1 note and 0 sessions wait on this PC, nothing is lost."}, nil},
		{"every read unreachable, reach not yet recorded failing", fxNow, func(x *fx) {
			x.local(index.LocalStatus{ReachLastOK: today(11, 0)})
			x.server.failAll(hindsight.ErrUnreachable)
		}, &Item{Severity: SeverityWatch, Text: "Memory server unreachable since 11:00: 0 notes and 0 sessions wait on this PC, nothing is lost."}, nil},
		{"one read failing", fxNow, func(x *fx) {
			x.local(index.LocalStatus{ReachLastOK: today(11, 0)})
			x.server.fail("BankStats", hindsight.ErrUnreachable)
		}, nil, nil},
		{"a failed send stays apart", fxNow, func(x *fx) {
			x.local(index.LocalStatus{ReachFailingSince: today(9, 12), NotesQueued: 14, NotesOldestAt: fxNow.Add(-3 * time.Hour), SessionsWaiting: 3})
			x.retained(ago(time.Hour), "d1", "failed", 100)
			x.retained(ago(2*time.Hour), "d2", "queued", 0)
			down(x)
		}, &Item{Severity: SeverityWatch, Text: "Memory server unreachable since 09:12: 14 notes and 3 sessions wait on this PC, nothing is lost."},
			[]Item{{Metric: "pipeline.flow", Severity: SeverityWatch, Axis: "flowing", Text: "1 session could not be sent; 0 set aside after an error.",
				Why: mustDefAction("pipeline.flow"), Command: "backsight doctor"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newFx(t, c.now)
			c.fill(x)
			a := x.assess(x.query(7))
			fl := a.Verdict.Axes["flowing"]
			if c.root == nil {
				wantAxes(t, c.name, a.Verdict, axes{early, early, early, okay})
				if len(a.Items) != 0 || len(fl.Reasons) != 0 {
					t.Errorf("attention %v, flowing reasons %+v: want none", metricsOfItems(a.Items), fl.Reasons)
				}
				return
			}
			wantAxes(t, c.name, a.Verdict, axes{early, early, early, attn})
			root := *c.root
			root.Metric, root.Axis, root.Why, root.Command = "server.reach", "flowing", mustDefAction("server.reach"), "backsight doctor"
			want := append(slices.Clone(c.others), root) // pipeline.flow sorts before server.reach
			if !reflect.DeepEqual(a.Items, want) {
				t.Errorf("attention =\n%+v\nwant\n%+v", a.Items, want)
			}
			if len(fl.Reasons) != len(want) || fl.Reasons[0] != (Reason{Metric: "server.reach", Severity: root.Severity, Text: root.Text, Command: "backsight doctor"}) {
				t.Errorf("flowing reasons = %+v, want the root cause first, then %d more", fl.Reasons, len(c.others))
			}
		})
	}
}

// TestUnreliableAndCantJudge: 16 declared of 25 turns (upper bound
// 0.7975, under 80%, on 25 turns) → Right can't judge, with one watch item
// for declare.coverage and used.turn_rate tagged unreliable. Learning
// comes before it; it comes before attention.
func TestUnreliableAndCantJudge(t *testing.T) {
	coverage := func(x *fx, prefix string, end time.Time) {
		turns(x, prefix+"d", end, 16, "declared", firstK(16, "used", ""))
		turns(x, prefix+"u", end.Add(-20*time.Hour), 9, "undeclared", nil)
	}
	const text = "Can't judge yet: Claude said what it used on only 64 % of turns."
	for _, c := range []struct {
		name string
		fill func(x *fx)
		want axes
		item bool // the can't-judge item
	}{
		{"can't judge", func(x *fx) {
			firstPrompt(x)
			recalls(x, "cur", fxNow, 25, 25, 0, 0)
			coverage(x, "c", fxNow)
		}, axes{okay, cant, early, okay}, true},
		{"learning first", func(x *fx) {
			x.prompts("alpha", "first", fxNow.Add(-2*day), 1, nil)
			recalls(x, "cur", fxNow, 25, 25, 0, 0)
			coverage(x, "c", fxNow)
		}, axes{learning, learning, early, okay}, false},
		{"before attention: the coverage watched both weeks", func(x *fx) {
			firstPrompt(x)
			recalls(x, "cur", fxNow, 25, 25, 0, 0)
			coverage(x, "c", fxNow)
			coverage(x, "p", since7)
		}, axes{okay, cant, early, okay}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newFx(t, fxNow)
			c.fill(x)
			a := x.assess(x.query(7))
			wantAxes(t, c.name, a.Verdict, c.want)
			if r := a.Now["used.turn_rate"]; !slices.Contains(r.Tags, tagUnreliable) {
				t.Errorf("used.turn_rate tags = %v, want unreliable", r.Tags)
			}
			cov := itemsOf(a.Items, "declare.coverage")
			if !c.item {
				if len(cov) != 0 {
					t.Errorf("coverage items = %+v, want none", cov)
				}
				return
			}
			want := []Item{{Metric: "declare.coverage", Severity: SeverityWatch, Axis: "right", Text: text,
				Why: mustDefAction("declare.coverage"), Command: "backsight stats quality"}}
			if !reflect.DeepEqual(a.Items, want) {
				t.Errorf("attention = %+v, want %+v", a.Items, want)
			}
			rs := a.Verdict.Axes["right"].Reasons
			if len(rs) != 1 || rs[0] != (Reason{Metric: "declare.coverage", Severity: SeverityWatch, Text: text, Command: "backsight stats quality"}) {
				t.Errorf("right reasons = %+v, want the can't-judge sentence once, with its command", rs)
			}
		})
	}
}

// TestLearningDoesNotHideFlowing: first prompt 2 days ago, 12 prompts, a
// failed send → Found and Right learn, Flowing is in attention, Cost is
// judged (too early on 12 sessions, not learning).
func TestLearningDoesNotHideFlowing(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "first", fxNow.Add(-2*day), 1, nil)
	recalls(x, "cur", fxNow, 11, 11, 0, 0)
	x.retained(ago(time.Hour), "d1", "failed", 100)
	a := x.assess(x.query(7))
	wantAxes(t, "learning", a.Verdict, axes{learning, learning, early, attn})
	if !a.Verdict.Learning {
		t.Error("verdict.learning = false, want true")
	}
	const text = "Learning: day 3 of about 7, 12 prompts so far."
	for _, name := range []string{"found", "right"} {
		if rs := a.Verdict.Axes[name].Reasons; len(rs) != 1 || rs[0].Text != text || rs[0].Severity != SeverityInfo {
			t.Errorf("%s reasons = %+v, want %q", name, rs, text)
		}
	}
	if rs := a.Verdict.Axes["cost"].Reasons; len(rs) != 1 || rs[0].Text != "Not enough yet: 12 sessions, 100 needed." {
		t.Errorf("cost reasons = %+v", rs)
	}
	if got := metricsOfItems(a.Items); !slices.Equal(got, []string{"pipeline.flow"}) {
		t.Errorf("attention = %v, want [pipeline.flow]", got)
	}
}

// TestLearningThresholds: Found and Right learn until 7 days have passed
// since the first typed prompt and the last 7 days hold 20 typed prompts
// (machine prompts do not count); the day shown is at most the 7th.
func TestLearningThresholds(t *testing.T) {
	machine := func(_ int, p *telemetry.Prompt) { p.Decision, p.Reason = "skip", "machine" }
	for _, c := range []struct {
		name string
		fill func(x *fx)
		text string // "" when not learning
	}{
		{"7 days, 20 prompts", func(x *fx) {
			x.tel(fxNow.Add(-7*day), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Injected: 1}, "first", "alpha")
			recalls(x, "cur", fxNow, 19, 19, 0, 0)
		}, ""},
		{"a minute short of 7 days", func(x *fx) {
			x.tel(fxNow.Add(-7*day+time.Minute), telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, Injected: 1}, "first", "alpha")
			recalls(x, "cur", fxNow, 19, 19, 0, 0)
		}, "Learning: day 7 of about 7, 20 prompts so far."},
		{"20 days, 19 typed prompts and 5 machine ones", func(x *fx) {
			firstPrompt(x)
			recalls(x, "cur", fxNow, 19, 19, 0, 0)
			x.prompts("alpha", "mach", fxNow, 5, machine)
		}, "Learning: day 7 of about 7, 19 prompts so far."},
		{"20 days, 20 prompts", func(x *fx) {
			firstPrompt(x)
			recalls(x, "cur", fxNow, 20, 20, 0, 0)
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newFx(t, fxNow)
			c.fill(x)
			a := x.assess(x.query(7))
			want := axes{okay, early, early, okay}
			if c.text != "" {
				want = axes{learning, learning, early, okay}
			}
			wantAxes(t, c.name, a.Verdict, want)
			if a.Verdict.Learning != (c.text != "") {
				t.Errorf("verdict.learning = %v", a.Verdict.Learning)
			}
			if rs := a.Verdict.Axes["found"].Reasons; c.text != "" && (len(rs) != 1 || rs[0].Text != c.text) {
				t.Errorf("found reasons = %+v, want %q", rs, c.text)
			}
		})
	}
}

// TestRightFallsBackTo28Days: Right, too early on 7 days, is judged on the
// last 28 against the 28 before, says so and tags its metrics; still too
// early on 28, it names the 28-day counts. Relative thresholds there use
// the four 7-day windows before the 28 days, when two of them qualify.
func TestRightFallsBackTo28Days(t *testing.T) {
	week8 := fxNow.Add(-8 * day)    // inside the 28 days, before the 7
	day29 := fxNow.Add(-29 * day)   // the 28 days before, and the first 7-day baseline
	day36 := fxNow.Add(-36 * day)   // the second baseline
	used := firstK(100, "used", "") // every turn credits used
	for _, c := range []struct {
		name  string
		fill  func(x *fx)
		want  axes
		check func(t *testing.T, a assessment)
	}{
		{"judged on 28 days", func(x *fx) {
			turns(x, "c", fxNow, 6, "declared", used)
			turns(x, "o", week8, 24, "declared", used)
		}, axes{okay, okay, early, okay}, func(t *testing.T, a assessment) {
			r := a.Verdict.Axes["right"]
			if r.WindowDays != 28 || len(r.Reasons) != 1 || r.Reasons[0] != (Reason{Metric: "used.turn_rate", Severity: SeverityInfo, Text: "Judged on the last 28 days: too few turns in 7."}) {
				t.Errorf("right = %+v, want 28 days with the info reason", r)
			}
			for _, id := range axisMetrics["right"] {
				if !slices.Contains(a.Right28[id].Tags, tagJudged28d) || slices.Contains(a.Now[id].Tags, tagJudged28d) {
					t.Errorf("%s: 28-day tags %v, 7-day tags %v; want judged_28d on the 28-day one only", id, a.Right28[id].Tags, a.Now[id].Tags)
				}
			}
			if d := den0(a.Right28["used.turn_rate"]); d != 30 {
				t.Errorf("28-day declared turns = %d, want 30", d)
			}
		}},
		{"too early on 28 days too", func(x *fx) {
			turns(x, "c", fxNow, 6, "declared", used)
			turns(x, "o", week8, 9, "declared", used)
		}, axes{okay, early, early, okay}, func(t *testing.T, a assessment) {
			r := a.Verdict.Axes["right"]
			if r.WindowDays != 28 || len(r.Reasons) != 1 || r.Reasons[0].Text != "Not enough yet: 15 declared turns, 20 needed." || a.Right28 != nil {
				t.Errorf("right = %+v (28-day metrics %v), want too early on 15 declared turns", r, a.Right28 != nil)
			}
		}},
		{"bad on 28 days and the 28 before", func(x *fx) {
			turns(x, "c", fxNow, 6, "declared", firstK(2, "wrong", "used"))
			turns(x, "o", week8, 24, "declared", firstK(8, "wrong", "used"))
			turns(x, "p", day29, 25, "declared", firstK(10, "wrong", "used"))
		}, axes{okay, attn, early, okay}, func(t *testing.T, a assessment) {
			r := a.Verdict.Axes["right"]
			want := Reason{Metric: "verdict.wrong_rate", Severity: SeverityBad, Text: "10 memories reported wrong or out of date in the last 28 days.", Command: "backsight memory stale"}
			if r.WindowDays != 28 || len(r.Reasons) != 2 || r.Reasons[0] != want || r.Reasons[1].Text != judged28Sentence {
				t.Errorf("right = %+v, want 28 days, %+v then the info reason", r, want)
			}
			if got := metricsOfItems(a.Items); !slices.Equal(got, []string{"verdict.wrong_rate"}) {
				t.Errorf("attention = %v", got)
			}
		}},
		{"bad on 28 days only", func(x *fx) {
			turns(x, "c", fxNow, 6, "declared", firstK(2, "wrong", "used"))
			turns(x, "o", week8, 24, "declared", firstK(8, "wrong", "used"))
			turns(x, "p", day29, 25, "declared", used)
		}, axes{okay, okay, early, okay}, func(t *testing.T, a assessment) {
			if a.Right28["verdict.wrong_rate"].Status != StatusBad || len(a.Items) != 0 {
				t.Errorf("wrong rate %s, attention %v: want bad on 28 days and no item", a.Right28["verdict.wrong_rate"].Status, metricsOfItems(a.Items))
			}
		}},
		{"one 7-day baseline qualifies: no relative status", func(x *fx) {
			turns(x, "c", fxNow, 6, "declared", firstK(1, "used", "none"))
			turns(x, "o", week8, 24, "declared", firstK(5, "used", "none"))
			turns(x, "b1", day29, 20, "declared", used)
		}, axes{okay, okay, early, okay}, func(t *testing.T, a assessment) {
			if m := a.Right28["used.turn_rate"]; m.Status != StatusOK || num(m) != 6 || den0(m) != 30 {
				t.Errorf("used.turn_rate on 28 days = %d/%d %s, want 6/30 ok", num(m), den0(m), m.Status)
			}
		}},
		{"two 7-day baselines qualify: watch", func(x *fx) {
			turns(x, "c", fxNow, 6, "declared", firstK(1, "used", "none"))
			turns(x, "o", week8, 24, "declared", firstK(5, "used", "none"))
			turns(x, "b1", day29, 20, "declared", used)
			turns(x, "b2", day36, 20, "declared", used)
		}, axes{okay, okay, early, okay}, func(t *testing.T, a assessment) {
			if m := a.Right28["used.turn_rate"]; m.Status != StatusWatch {
				t.Errorf("used.turn_rate on 28 days = %s, want watch (6/30 against a median of 1)", m.Status)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newFx(t, fxNow)
			firstPrompt(x)
			recalls(x, "cur", fxNow, 25, 25, 0, 0)
			c.fill(x)
			a := x.assess(x.query(7))
			wantAxes(t, c.name, a.Verdict, c.want)
			if !a.ran28 {
				t.Error("the 28-day figures were not computed")
			}
			c.check(t, a)
		})
	}
}

// TestVerdictWithoutActivity: an axis with nothing to read is too early,
// with its reason's sentence, never learning; Flowing is still judged.
func TestVerdictWithoutActivity(t *testing.T) {
	const (
		serverOnly = "This PC runs the server only: activity is recorded on the PCs that use it."
		feedOff    = "Memory feedback is off (`feedback.enabled`)."
		nothing    = "Nothing yet: memory starts recording at your next Claude Code prompt."
	)
	for _, c := range []struct {
		name  string
		fill  func(x *fx)
		want  axes
		texts [3]string // found, right, cost; "" when judged
	}{
		{"server-only host", func(x *fx) {
			x.cfg = func(c *config.Config) { c.Server.Role = config.RoleServer }
			recalls(x, "cur", fxNow, 12, 12, 0, 0)
		}, axes{early, early, early, okay}, [3]string{serverOnly, serverOnly, serverOnly}},
		{"feedback off, 300 prompts", func(x *fx) {
			x.cfg = func(c *config.Config) { c.Feedback.Enabled = false }
			recalls(x, "p", fxNow, 300, 300, 0, 0)
		}, axes{okay, early, okay, okay}, [3]string{"", feedOff, ""}},
		{"fresh install", func(*fx) {}, axes{early, early, early, okay}, [3]string{nothing, nothing, nothing}},
		{"feedback off on a young install", func(x *fx) {
			x.cfg = func(c *config.Config) { c.Feedback.Enabled = false }
			x.prompts("alpha", "first", fxNow.Add(-2*day), 1, nil)
			recalls(x, "cur", fxNow, 11, 11, 0, 0)
		}, axes{learning, early, early, okay}, [3]string{"Learning: day 3 of about 7, 12 prompts so far.", feedOff, ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newFx(t, fxNow)
			c.fill(x)
			a := x.assess(x.query(7))
			wantAxes(t, c.name, a.Verdict, c.want)
			if a.Verdict.Learning != (c.want[0] == learning) {
				t.Errorf("verdict.learning = %v", a.Verdict.Learning)
			}
			if a.ran28 || a.Right28 != nil {
				t.Errorf("the 28-day fallback ran (%v) with nothing to read", a.ran28)
			}
			for i, name := range axisOrder[:3] {
				rs := a.Verdict.Axes[name].Reasons
				if c.texts[i] == "" {
					continue
				}
				if len(rs) != 1 || rs[0].Text != c.texts[i] || rs[0].Severity != SeverityInfo || a.Verdict.Axes[name].WindowDays != 7 {
					t.Errorf("%s = %+v, want %q", name, a.Verdict.Axes[name], c.texts[i])
				}
			}
		})
	}
}

// TestAttentionOrderAndCap: 7 items, 2 bad and 5 watch over every axis:
// the first 5 listed, bad first, then found, right, cost, flowing, then by
// metric; attention_more 2; each axis's banner holds at most 3.
func TestAttentionOrderAndCap(t *testing.T) {
	x := newFx(t, fxNow)
	firstPrompt(x)
	recalls(x, "cur", fxNow, 120, 0, 3000, 10000)  // floor watch, latency bad, context watch
	recalls(x, "prev", since7, 110, 0, 100, 10000) // floor watch, context watch
	turns(x, "c", fxNow, 20, "declared", firstK(4, "wrong", "used"))
	turns(x, "p", since7, 20, "declared", firstK(4, "wrong", "used"))
	for i := range 10 {
		x.tel(ago(time.Duration(i+1)*time.Minute), telemetry.HookFailure{Hook: "prompt", Kind: "killed", MS: 5}, "s", "alpha")
	}
	x.local(index.LocalStatus{NotesQueued: 1, NotesOldestAt: ago(2 * time.Hour)})
	x.doctor = []DoctorCheck{{ID: "latency", Status: "warn"}}

	var v Verdict
	var items []Item
	var more int
	err := withTx(context.Background(), x.source(), x.query(7), x.need, func(e env) error {
		var err error
		v, items, more, err = verdict(e)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	wantAxes(t, "seven items", v, axes{attn, attn, attn, attn})
	type key struct {
		metric string
		sev    Severity
		axis   string
	}
	var got []key
	for _, it := range items {
		got = append(got, key{it.Metric, it.Severity, it.Axis})
	}
	want := []key{{"recall.latency", SeverityBad, "cost"}, {"hooks.failures", SeverityBad, "flowing"},
		{"recall.hit_rate", SeverityWatch, "found"}, {"verdict.wrong_rate", SeverityWatch, "right"}, {"context.per_session", SeverityWatch, "cost"}}
	if !slices.Equal(got, want) || more != 2 {
		t.Errorf("attention = %v (+%d), want %v (+2)", got, more, want)
	}
	if it := items[4]; it.Text != "Memory adds more context than its budget: 10,000 characters per session." {
		t.Errorf("context text = %q", it.Text)
	}
	var fl []string
	for _, r := range v.Axes["flowing"].Reasons {
		fl = append(fl, r.Metric)
	}
	if !slices.Equal(fl, []string{"hooks.failures", "local.backlog", "doctor.check"}) {
		t.Errorf("flowing reasons = %v, want the 3 flowing items, bad first, the doctor's warning last", fl)
	}
}

// TestDoctorErrorLeadsFlowing: four warnings the doctor ran before its one
// error: the error (the only reason Flowing is in attention) is Flowing's
// first reason and the first doctor item of the attention list; the
// warnings follow it, so the axis's cap of 3 keeps two of them and the
// list's cap of 5 the four.
func TestDoctorErrorLeadsFlowing(t *testing.T) {
	x := newFx(t, fxNow)
	x.doctor = []DoctorCheck{{ID: "backup", Status: "warn"}, {ID: "latency", Status: "warn"}, {ID: "transcripts", Status: "warn"},
		{ID: "noise", Status: "warn"}, {ID: "metrics-index", Status: "error"}}
	var v Verdict
	var items []Item
	err := withTx(context.Background(), x.source(), x.query(7), x.need, func(e env) error {
		var err error
		v, items, _, err = verdict(e)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	fl := v.Axes["flowing"]
	if fl.State != AxisAttention || len(fl.Reasons) != 3 || fl.Reasons[0].Text != "Health check «activity index» failed." {
		t.Errorf("flowing = %+v, want attention led by the failed check", fl)
	}
	var doctor []string
	for _, it := range items {
		if it.Metric == "doctor.check" {
			doctor = append(doctor, it.Text)
		}
	}
	if len(doctor) != 5 || doctor[0] != "Health check «activity index» failed." || !strings.HasSuffix(doctor[1], "reports a warning.") {
		t.Errorf("doctor items = %q, want the failed check first, then the warnings", doctor)
	}
}

// TestProjectVerdictUsesItsHistory: under a project filter the verdict
// reads that project's prompts only: alpha (25 prompts over 10 days, 22 in
// the last 7) is judged; beta, whose first prompt was 25 h ago, learns,
// while the whole PC (gamma's 300 prompts) does not.
func TestProjectVerdictUsesItsHistory(t *testing.T) {
	x := newFx(t, fxNow)
	x.prompts("alpha", "a", fxNow, 22, injecting(22))
	x.prompts("alpha", "a-old", fxNow.Add(-8*day), 3, injecting(3))
	x.prompts("beta", "b", fxNow, 25, injecting(25))
	x.prompts("gamma", "g", fxNow, 300, nil)
	for _, c := range []struct {
		project string
		want    axes
		found   string // found's reason, "" for none
		hits    [2]int64
	}{
		{"alpha", axes{okay, early, early, okay}, "", [2]int64{22, 22}},
		{"beta", axes{learning, learning, early, okay}, "Learning: day 2 of about 7, 25 prompts so far.", [2]int64{25, 25}},
		{"", axes{okay, early, okay, okay}, "", [2]int64{47, 215}},
	} {
		q := x.query(7)
		q.Project = c.project
		a := x.assess(q)
		wantAxes(t, "project "+c.project, a.Verdict, c.want)
		if h := a.Now["recall.hit_rate"]; num(h) != c.hits[0] || den0(h) != c.hits[1] {
			t.Errorf("project %q: hit rate %d/%d, want %d/%d", c.project, num(h), den0(h), c.hits[0], c.hits[1])
		}
		rs := a.Verdict.Axes["found"].Reasons
		if (c.found == "" && len(rs) != 0) || (c.found != "" && (len(rs) != 1 || rs[0].Text != c.found)) {
			t.Errorf("project %q: found reasons %+v, want %q", c.project, rs, c.found)
		}
	}
}

// TestInfoItems: 7 often shown never used, none hidden, 2 reported: the
// two counts above zero are info items, never attention.
func TestInfoItems(t *testing.T) {
	x := newFx(t, fxNow)
	l := feedback.Ledger{Memories: map[string]feedback.Entry{"r-1": {Shown: 2, Wrong: 1}, "r-2": {Shown: 3, Wrong: 2}}}
	for i := range 7 {
		l.Memories[fmt.Sprintf("n-%d", i)] = feedback.Entry{Shown: 12}
	}
	x.ledger(l)
	a := x.assess(x.query(7))
	want := []InfoItem{{Metric: "stale.never_used", Count: 7, Command: "backsight memory stale"}, {Metric: "stale.reported", Count: 2, Command: "backsight memory stale"}}
	if !reflect.DeepEqual(a.Info, want) {
		t.Errorf("info = %+v, want %+v", a.Info, want)
	}
	for _, it := range a.Items {
		if strings.HasPrefix(it.Metric, "stale.") {
			t.Errorf("attention holds %s", it.Metric)
		}
	}
	y := newFx(t, fxNow)
	if b := y.assess(y.query(7)); b.Info == nil || len(b.Info) != 0 {
		t.Errorf("no ledger: info = %#v, want an empty list", b.Info)
	}
}

// TestSummary: TestHitRate's telemetry (12 prompts got memory, 5,000
// characters each) with 4 briefings of 3,000 characters, TestUsedTurnRate's
// journal (12 turns used memory), and 400,000 input tokens priced 1 USD a
// million: 60,000 + 12,000 = 72,000 characters, 18,000 tokens, 0.40 USD.
// Without prices (none set, a model the price table does not know) the
// cost is null and price_set false.
func TestSummary(t *testing.T) {
	ids := []string{"recall.hit_rate", "used.turn_rate", "context.per_session", "extraction.cost"}
	fill := func(x *fx) {
		hitRatePrompts(x, 5000)
		for i := range 4 {
			x.tel(ago(time.Duration(i+1)*time.Hour), telemetry.Briefing{Chars: 3000, Source: "raw"}, fmt.Sprintf("cur-%d", i), "alpha")
		}
		x.tel(since7.Add(-time.Hour), telemetry.Briefing{Chars: 99999, Source: "raw"}, "before", "alpha") // before the window
		usedTurnRateJournal(x)
		x.server.set(func(d *fakeData) {
			d.LLM = map[string][]hindsight.LLMBucket{"": {{Time: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), Input: 400_000}}}
		})
	}
	x := newFx(t, fxNow)
	x.cfg = func(c *config.Config) { c.LLM.PriceInput, c.LLM.Currency = 1, "USD" }
	fill(x)
	s := summary(x.metrics(ids, x.query(7)))
	if s.ExtractionCost == nil || math.Abs(*s.ExtractionCost-0.40) > 1e-9 {
		t.Fatalf("extraction cost = %v, want 0.40", s.ExtractionCost)
	}
	s.ExtractionCost = nil
	want := Summary{PromptsWithMemory: 12, TurnsWithMemoryUsed: 12, ContextChars: 72000, TokensApprox: 18000,
		Currency: "USD", PriceSet: true, ExtractionScope: "server"}
	if s != want {
		t.Errorf("summary = %+v, want %+v", s, want)
	}

	y := newFx(t, fxNow)
	y.cfg = func(c *config.Config) { c.LLM.Model = "no-such-model" }
	fill(y)
	s = summary(y.metrics(ids, y.query(7)))
	if s.ExtractionCost != nil || s.PriceSet || s.ContextChars != 72000 || s.ExtractionScope != "server" {
		t.Errorf("without prices: %+v, want a null cost, price_set false", s)
	}
	if empty := summary(map[string]Metric{}); empty != (Summary{ExtractionScope: "server"}) {
		t.Errorf("no metrics: %+v", empty)
	}
}

// The attention sentences of the counted metrics, for TestCopyFormats.
func hookText(n int64) string {
	return itemText(mustDefOf("hooks.failures"), Metric{Extra: map[string]any{"last_24h": map[string]int64{"exit": n}}}, 7, 0, false)
}

func flowText(failed, setAside int64) string {
	return itemText(mustDefOf("pipeline.flow"), Metric{Extra: map[string]any{"failed": failed, "set_aside": setAside}}, 7, 0, false)
}

func wrongText(n int64, days int) string {
	return itemText(mustDefOf("verdict.wrong_rate"), Metric{Num: &n}, days, 0, false)
}

func mustDefOf(id string) Def {
	d, ok := DefOf(id)
	if !ok {
		panic(id)
	}
	return d
}

// TestCopyFormats pins the numbers and the plurals in the sentences.
func TestCopyFormats(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{grouped(0), "0"}, {grouped(999), "999"}, {grouped(1000), "1,000"}, {grouped(72000), "72,000"},
		{grouped(1234567), "1,234,567"}, {grouped(-4500), "-4,500"},
		{percent(0.64), "64"}, {percent(0.6449), "64"}, {percent(0.6451), "65"},
		{seconds(2660), "2.7"}, {seconds(3000), "3.0"},
		{formatValue("ms", 45_000), "45.0 s"}, {formatValue("ms", 1_800_000), "30 min"},
		{formatValue("ms", 7_200_000), "2 h"}, {formatValue("ms", 3*86_400_000), "3 days"},
		{formatValue("ratio", 0.031), "3 %"}, {formatValue("count", 6), "6"}, {formatValue("money", 0.4), "0.40"},
		{clock(time.Date(2026, 10, 30, 0, 5, 0, 0, fxParis), fxNow, fxParis), "00:05"},
		{clock(time.Date(2026, 10, 29, 23, 55, 0, 0, fxParis), fxNow, fxParis), "2026-10-29 23:55"},
		{clock(time.Date(2026, 10, 29, 23, 5, 0, 0, time.UTC), fxNow, fxParis), "00:05"}, // 00:05 in Paris
		{count(1, "step", "steps"), "1 step"}, {count(2, "step", "steps"), "2 steps"}, {count(1000, "step", "steps"), "1,000 steps"},
		{count(0, "note", "notes"), "0 notes"},
		{learningSentence(2, 1), "Learning: day 2 of about 7, 1 prompt so far."},
		{tooEarlySentence(1, countUnits["used.turn_rate"], 20), "Not enough yet: 1 declared turn, 20 needed."},
		{tooEarlySentence(2, countUnits["context.per_session"], 100), "Not enough yet: 2 sessions, 100 needed."},
		{hookText(1), "1 step crashed in the last 24 h."}, {hookText(2), "2 steps crashed in the last 24 h."},
		{flowText(1, 0), "1 session could not be sent; 0 set aside after an error."},
		{flowText(2, 1), "2 sessions could not be sent; 1 set aside after an error."},
		{wrongText(1, 7), "1 memory reported wrong or out of date this week."},
		{wrongText(2, 28), "2 memories reported wrong or out of date in the last 28 days."},
		{cantJudgeSentence(0.64), "Can't judge yet: Claude said what it used on only 64 % of turns."},
		{cantJudgeSentence(0.7966), "Can't judge yet: Claude said what it used on only 79 % of turns."},
		{cantJudgeSentence(0.29), "Can't judge yet: Claude said what it used on only 29 % of turns."},
		{rootCauseSentence(time.Time{}, fxNow, fxParis, 2, 1), "Memory server unreachable: 2 notes and 1 session wait on this PC, nothing is lost."},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// TestVerdictFiguresReused: when the view's query is the verdict's, its
// window the last 7 days (built up to a minute before now), the verdict
// judges that very window and its figures can stand for the view's; any
// other window, another filter on the same window, or a developer metric
// the verdict does not compute, cannot.
func TestVerdictFiguresReused(t *testing.T) {
	x := newFx(t, fxNow)
	firstPrompt(x)
	recalls(x, "cur", fxNow, 25, 25, 0, 0)
	ids := []string{"recall.hit_rate", "pipeline.flow", "bank.size", "stale.hidden"}
	built := func(before time.Duration, days int) Query {
		w, err := NewWindow(fxNow.Add(-before), days, time.Time{}, time.Time{}, fxParis, "Europe/Paris")
		if err != nil {
			t.Fatal(err)
		}
		return Query{Window: w, Machines: "this"}
	}
	q := built(30*time.Second, 7)
	a := x.assess(q)
	if !a.Window.Since.Equal(q.Window.Since) || !a.Window.Until.Equal(q.Window.Until) {
		t.Errorf("verdict window %v..%v, want the view's %v..%v", a.Window.Since, a.Window.Until, q.Window.Since, q.Window.Until)
	}
	m, ok := a.metricsFor(q, ids)
	if !ok || len(m) != len(ids) || !reflect.DeepEqual(m["recall.hit_rate"], a.Now["recall.hit_rate"]) {
		t.Errorf("reuse = %v (%d metrics), want the verdict's own", ok, len(m))
	}
	if direct := x.metrics(ids, q)["recall.hit_rate"]; !reflect.DeepEqual(direct, m["recall.hit_rate"]) {
		t.Errorf("reused hit rate\n%+v\ncomputed for the view\n%+v", m["recall.hit_rate"], direct)
	}
	if _, ok := a.metricsFor(q, []string{"recall.by_lang"}); ok {
		t.Error("reused a developer metric the verdict does not compute")
	}
	for _, c := range []struct {
		name string
		edit func(*Query)
	}{
		{"another project", func(q *Query) { q.Project = "alpha" }},
		{"other machines", func(q *Query) { q.Machines = "all" }},
		{"a path", func(q *Query) { q.Path = "search" }},
		{"developer mode", func(q *Query) { q.Dev = true }},
	} {
		other := q
		c.edit(&other)
		if _, ok := a.metricsFor(other, ids); ok {
			t.Errorf("%s, same window: figures reused", c.name)
		}
	}
	for _, c := range []struct {
		name string
		q    Query
	}{{"30 days", built(0, 30)}, {"built 2 minutes ago", built(2*time.Minute, 7)}} {
		a := x.assess(c.q)
		if !a.Window.Until.Equal(fxNow) {
			t.Errorf("%s: verdict ends at %v, want now", c.name, a.Window.Until)
		}
		if _, ok := a.metricsFor(c.q, ids); ok {
			t.Errorf("%s: figures reused for another window", c.name)
		}
	}
}

// TestHitRateSentenceFollowsTheStepThatFired: "fewer than usual" only when
// the comparison with before fired (upper bound under 0.7 × the median);
// an absolute alarm above the usual level reads the label sentence.
func TestHitRateSentenceFollowsTheStepThatFired(t *testing.T) {
	d, _ := DefOf("recall.hit_rate")
	f := func(v float64) *float64 { return &v }
	absolute := Metric{Value: f(0.03), CI: &[2]float64{0.01, 0.08}} // under 10 %, above the 2 % usual
	if got := itemText(d, absolute, 7, 0.02, true); got != "Prompts that got memory: 3 %." {
		t.Errorf("absolute alarm: %q", got)
	}
	relative := Metric{Value: f(0.2), CI: &[2]float64{0.15, 0.25}} // 0.25 < 0.7 × 50 %
	if got := itemText(d, relative, 7, 0.5, true); got != "Fewer prompts get memory than usual: 20 % vs 50 % before." {
		t.Errorf("relative alarm: %q", got)
	}
}
