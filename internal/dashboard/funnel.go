package dashboard

import (
	"html/template"
	"strconv"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
)

// FunnelCard is time to recall: the four stages a session goes through and
// the three hops between them, the times of each hop on a chart, and the
// sessions that were sent and never became memories.
type FunnelCard struct {
	Card
	Few    *Alert // too few sessions recalled to time anything; nil when the times are shown
	Stages []FunnelStage
	Hops   []FunnelHop
	Slow   *FunnelSlow // the note of a slow hop in a watch week
	Chart  template.HTML
	Whole  *FunnelWhole
	Never  *NeverTable
}

// FunnelStage is a stage: how many sessions reached it, and what became of
// the others.
type FunnelStage struct{ Label, Value, Note string }

// FunnelHop is the time from one stage to the next: its typical time and
// the slowest 1 in 20, or how many sessions it was timed on. Watch marks the
// hop whose slowest 1 in 20 is over the limit.
type FunnelHop struct {
	Name, Value, Note string
	Watch             bool
}

// FunnelSlow is the sentence of the watch hop: how long 1 in 20 sessions
// took, and the limit.
type FunnelSlow struct{ Took, Limit string }

// FunnelWhole is the whole way, timed on its own sessions.
type FunnelWhole struct{ Typical, Slowest string }

// NeverTable is the sessions sent a day ago or more that gave no memory.
type NeverTable struct {
	Title string
	Rows  []NeverRow
	Queue string // the title of the card that lists failed server tasks
	More  int    // sessions left out of Rows
	Cmd   string
}

// NeverRow is a session that gave no memory and why, in words.
type NeverRow struct {
	ID, Project Ref
	Sent        format.Time
	State       NeverState
}

// NeverState is why a session gave no memory: Word, coloured by Class when
// it is a fault, then Rest.
type NeverState struct{ Word, Class, Rest string }

const (
	neverShown = 10 // rows of the never-extracted table
	neverCmd   = "backsight stats sessions --has never_extracted"
)

// funnelHops are the hops between the stages: the key of the hop in the
// metric and the words that name it.
var funnelHops = [3]struct{ key, name string }{
	{"last_to_sent", "last turn → sent"},
	{"sent_to_extracted", "sent → turned into memories"},
	{"extracted_to_recalled", "first memory recalled"},
}

// funnelOf is the time-to-recall card of the pipeline result r.
func (d PageData) funnelOf(r *metrics.Result) FunnelCard {
	l := d.L
	v := d.view(r, timeToRecallID)
	queue := d.view(r, "server.queue")
	h := d.head(v)
	h.Scope = "both" // this PC's record of when the server turned sessions into memories
	c := FunnelCard{Card: Card{Head: h, Dev: d.devIDs(v)}}
	if !v.Found || v.M.Status == metrics.StatusNoData {
		c.Note = d.noData(v)
		return c
	}
	f, _ := extraAs[map[string]int64](v.M, "funnel")
	list, _ := extraAs[[]metrics.Hop](v.M, "hops")
	hops := map[string]metrics.Hop{}
	for _, x := range list {
		hops[x.Name] = x
	}
	whole := hops["whole_way"]
	// Too few: the metric says so, or fewer sessions were recalled than the
	// whole way needs (no typical time). Either way the card tells it and its
	// pill agrees; a watch is the one status that outranks it.
	few := v.M.Status == metrics.StatusTooFew || whole.P50 == nil
	need := int64(v.Def.MinN)
	dur := func(p *float64) string { return chart.Span(*p) }
	watch := v.M.Status == metrics.StatusWatch

	c.Head.Pill = word(v)
	if few && !watch {
		c.Head.Pill = Word{"too few", "st-few"}
	}
	based := l.Int(f["recalled"]) + " sessions recalled in the window; each step on the sessions that reached it"
	if few {
		based = format.Count(l, f["recalled"], "session", "sessions") + " recalled (" + l.Int(need) + " needed)"
	}
	c.Head.Info.What = "For each session sent in the window: from its last turn to its send, to the server turning it into memories, to the first time one of those memories was shown."
	c.Head.Info.Limits = "Watch when the slowest 1 in 20 takes over " + chartLimit(v) + " from sent to turned into memories."
	c.Head.Info.BasedOn, c.Head.Info.Smallest = based, ""

	// The stages: how many sessions reached each, and what became of the rest.
	sub := func(parts ...string) string {
		var out string
		for _, p := range parts {
			if p == "" {
				continue
			}
			if out != "" {
				out += " · "
			}
			out += p
		}
		return out
	}
	count := func(n int64, one string) string {
		if n == 0 {
			return ""
		}
		return l.Int(n) + " " + one
	}
	or := func(s, empty string) string {
		if s == "" {
			return empty
		}
		return s
	}
	c.Stages = []FunnelStage{
		{"Sessions ended", l.Int(f["ended"]), "on this PC"},
		{"Sent", l.Int(f["sent"]), or(count(f["waiting_or_failed"], "waiting or failed"), "all sent")},
		{"Turned into memories", l.Int(f["extracted"]), or(sub(count(f["never"], "never turned into memories"),
			count(f["on_server"], "still on the server"), count(f["before_record"], "sent before this was recorded")), "all turned into memories")},
		{"A memory from it recalled", l.Int(f["recalled"]), or(count(f["not_recalled"], "not recalled yet"), "all recalled")},
	}

	for i, hp := range funnelHops {
		hop := hops[hp.key]
		x := FunnelHop{Name: hp.name, Watch: watch && i == 1}
		switch {
		case x.Watch && hop.P50 != nil && hop.P95 != nil: // a watch outranks too few for the hop it names
			x.Value, x.Note = dur(hop.P50), "1 in 20: "+dur(hop.P95)
		case few || hop.P50 == nil:
			x.Value, x.Note = dash, l.Int(hop.N)+" timed"
			if hop.N < need {
				x.Note += ", " + l.Int(need) + " needed"
			}
		case hop.P95 == nil:
			x.Value, x.Note = dur(hop.P50), "1 in 20: "+l.Int(metrics.MinForP95)+" needed"
		default:
			x.Value, x.Note = dur(hop.P50), "1 in 20: "+dur(hop.P95)
		}
		c.Hops = append(c.Hops, x)
	}

	if s := hops["sent_to_extracted"]; watch && s.P95 != nil {
		c.Slow = &FunnelSlow{Took: dur(s.P95), Limit: chartLimit(v)}
	}
	if few {
		c.Few = &Alert{Class: "st-few", Icon: "i-few", Lead: "Too few to time yet:", Rest: format.Count(l, f["recalled"], "session was", "sessions were") +
			" recalled in the " + d.frame().Window + ", " + l.Int(need) + " are needed. The counts below are exact; the times are not shown."}
	} else {
		rng := chart.Ranges{Label: "Time for each step from session to recalled memory, typical and slowest 1 in 20, on a logarithmic time scale",
			Empty: "No step has enough sessions to time yet.", More: d.more()}
		if w := v.Def.Absolute.Watch; w != nil {
			rng.WatchMS = *w
		}
		for i, hp := range funnelHops {
			hop := hops[hp.key]
			rng.Hops = append(rng.Hops, chart.Hop{Name: funnelHops[i].name, P50: hop.P50, P95: hop.P95, N: hop.N})
		}
		c.Chart = rng.HTML()
		slowest := dash
		if whole.P95 != nil {
			slowest = dur(whole.P95)
		}
		c.Whole = &FunnelWhole{Typical: dur(whole.P50), Slowest: slowest}
	}

	c.Never = d.neverTable(r, f["never"], queue.Def.Question)
	return c
}

// chartLimit is the watch limit of time to recall as the chart writes it.
func chartLimit(v MetricView) string {
	if w := v.Def.Absolute.Watch; w != nil {
		return chart.Span(*w)
	}
	return dash
}

// neverTable is the sessions that were sent and never turned into memories:
// nil when the result lists none. queue is the title of the card that lists
// the failed server tasks.
func (d PageData) neverTable(r *metrics.Result, never int64, queue string) *NeverTable {
	rows := r.Tables.NeverExtracted
	if len(rows) == 0 {
		return nil
	}
	l := d.L
	t := &NeverTable{Queue: queue, Cmd: neverCmd}
	total := max(never, int64(len(rows)))
	t.Title = "Sent, never turned into memories (" + l.Int(total) + ")"
	for _, x := range rows[:min(len(rows), neverShown)] {
		t.Rows = append(t.Rows, NeverRow{ID: idLink("/sessions/", x.Session), Project: d.projectLink(x.Project),
			Sent: format.Rel(r.GeneratedAt, x.Sent, d.Loc), State: neverState(x)})
	}
	t.More = int(total) - len(t.Rows)
	return t
}

// neverState says why a sent session gave no memory. Only a state the
// dashboard knows is worded; nothing the server said is shown.
func neverState(x metrics.NeverExtractedRow) NeverState {
	switch x.State {
	case "failed":
		s := NeverState{Word: "server task failed", Class: "st-watch"}
		if x.Retries > 0 {
			s.Rest = " after " + strconv.Itoa(x.Retries) + " " + map[bool]string{true: "try", false: "tries"}[x.Retries == 1]
		}
		return s
	case "no_memory":
		return NeverState{Word: "finished, no memory made"}
	case "none":
		return NeverState{Word: "no server task found"}
	}
	return NeverState{Word: "not known"}
}
