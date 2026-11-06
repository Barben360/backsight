package dashboard

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
)

const ttrID = "pipeline.time_to_recall"

// minMS is minutes as the milliseconds a hop is measured in.
func minMS(m float64) *float64 { return fp(m * 60_000) }

// hopIn is a hop's typical and slowest-1-in-20 times in minutes (0: none)
// and the sessions it was timed on.
type hopIn struct {
	p50, p95 float64
	n        int64
}

// funnelIn is what the metrics package hands the page for time to recall:
// the status it judged, the funnel counts and the four hops (last turn to
// sent, sent to turned into memories, turned into memories to recalled,
// whole way). Nothing here says which state the card is in: the page reads
// it from these.
type funnelIn struct {
	status metrics.Status
	f      map[string]int64
	hops   [4]hopIn
}

func (in funnelIn) apply(r *metrics.Result) {
	give(r, ttrID, func(m *metrics.Metric) {
		m.Status = in.status
		m.Tags = nil
		var hops []metrics.Hop
		for i, h := range in.hops {
			x := metrics.Hop{Name: metrics.HopNames[i], N: h.n}
			if h.p50 > 0 {
				x.P50 = minMS(h.p50)
			}
			if h.p95 > 0 {
				x.P95 = minMS(h.p95)
			}
			hops = append(hops, x)
		}
		m.Value = hops[3].P50
		recalled, sent := in.f["recalled"], in.f["sent"]
		m.Num, m.Den = &recalled, &sent
		extra := map[string]int64{}
		for _, k := range metrics.FunnelKeys {
			extra[k] = in.f[k]
		}
		m.Extra = map[string]any{"funnel": extra, "hops": hops}
	})
}

// The design's four states of the card, as the numbers that make them.
var (
	funnelNormal = funnelIn{status: metrics.StatusOK,
		f:    map[string]int64{"ended": 192, "sent": 188, "waiting_or_failed": 4, "extracted": 184, "never": 4, "recalled": 118, "not_recalled": 66},
		hops: [4]hopIn{{6, 22, 188}, {3, 18, 184}, {1500, 12960, 118}, {1560, 12960, 118}}}
	funnelWatch = funnelIn{status: metrics.StatusWatch,
		f:    map[string]int64{"ended": 192, "sent": 188, "waiting_or_failed": 4, "extracted": 181, "on_server": 7, "recalled": 109, "not_recalled": 72},
		hops: [4]hopIn{{6, 22, 188}, {24, 160, 181}, {1500, 12960, 109}, {1620, 12960, 109}}}
	funnelNever = funnelIn{status: metrics.StatusOK,
		f:    map[string]int64{"ended": 192, "sent": 188, "waiting_or_failed": 4, "extracted": 177, "never": 11, "recalled": 112, "not_recalled": 65},
		hops: [4]hopIn{{6, 22, 188}, {3, 19, 177}, {1500, 12960, 112}, {1560, 12960, 112}}}
	funnelFew = funnelIn{status: metrics.StatusTooFew,
		f:    map[string]int64{"ended": 9, "sent": 9, "extracted": 8, "on_server": 1, "recalled": 4, "not_recalled": 4},
		hops: [4]hopIn{{0, 0, 9}, {0, 0, 8}, {0, 0, 4}, {0, 0, 4}}}
)

func funnelNode(t *testing.T, in funnelIn, edit func(r *metrics.Result)) *node {
	t.Helper()
	n := pageNode(t, "pipeline", false, func(d *PageData) {
		in.apply(d.V["pipeline"])
		if edit != nil {
			edit(d.V["pipeline"])
		}
	})
	return cardOf(t, n, "w-pipeline-time_to_recall")
}

// stageTexts are the label, number and note of each of the four stages.
func stageTexts(c *node) [][3]string {
	var out [][3]string
	for _, s := range c.find("div.funnel div.stage") {
		out = append(out, [3]string{s.first("span.lab").text(), s.first("span.v").text(), s.first("span.s").text()})
	}
	return out
}

// hopTexts are the name, typical time and note of each of the three hops;
// the classes are the hops' own.
func hopTexts(t *testing.T, c *node) (cells [][3]string, classes []string) {
	t.Helper()
	for _, h := range c.find("div.funnel div.hopc") {
		spans := h.find("span")
		if len(spans) < 3 {
			t.Fatalf("hop %q has %d spans", h.text(), len(spans))
		}
		cells = append(cells, [3]string{spans[1].text(), h.first("b").text(), spans[2].text()})
		classes = append(classes, h.attr("class"))
	}
	return
}

func TestFunnelStates(t *testing.T) {
	const wholeWay = "Whole way, from the end of a session to a memory of it being recalled: typical %s, slowest 1 in 20 %s. Not the sum of the steps: each step is timed on its own sessions."
	stepNames := []string{"last turn → sent", "sent → turned into memories", "first memory recalled"}
	labels := []string{"Sessions ended", "Sent", "Turned into memories", "A memory from it recalled"}

	t.Run("normal", func(t *testing.T) {
		n := pageNode(t, "pipeline", false, func(d *PageData) { funnelNormal.apply(d.V["pipeline"]) })
		ids := cardIDs(n)
		at := slices.Index(ids, "w-server-extraction_delay")
		if at < 0 || at+1 >= len(ids) || ids[at+1] != "w-pipeline-time_to_recall" {
			t.Fatalf("cards %v: the funnel is not right after the extraction delay", ids)
		}
		c := cardOf(t, n, "w-pipeline-time_to_recall")
		if got := c.first("h2").text(); got != "How long until a session's memories come back?" {
			t.Errorf("title %q", got)
		}
		if got := texts(c.find("div.card-head span.chip")); !slices.Equal(got, []string{"this PC", "server"}) {
			t.Errorf("chips %q", got)
		}
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "ok" || !strings.Contains(p.attr("class"), "st-ok") {
			t.Errorf("pill %v", p)
		}
		if cli := c.first("details.cli code"); cli == nil || cli.Text != "backsight stats pipeline --days 30" {
			t.Errorf("CLI %v", cli)
		}
		wantStages := [][3]string{
			{labels[0], "192", "on this PC"}, {labels[1], "188", "4 waiting or failed"},
			{labels[2], "184", "4 never turned into memories"}, {labels[3], "118", "66 not recalled yet"}}
		if got := stageTexts(c); !slices.Equal(got, wantStages) {
			t.Errorf("stages %q, want %q", got, wantStages)
		}
		cells, classes := hopTexts(t, c)
		wantHops := [][3]string{{stepNames[0], "6 min", "1 in 20: 22 min"}, {stepNames[1], "3 min", "1 in 20: 18 min"}, {stepNames[2], "1 d 1 h", "1 in 20: 9 d"}}
		if !slices.Equal(cells, wantHops) {
			t.Errorf("hops %q, want %q", cells, wantHops)
		}
		for i, cl := range classes {
			if strings.Contains(cl, "is-watch") {
				t.Errorf("hop %d is marked watch in a normal week", i)
			}
		}
		if len(c.find("div.funnel span.st-watch")) != 0 || len(c.find("div.state")) != 0 {
			t.Error("a watch word or a state sentence in a normal week")
		}
		if got := len(c.find("div.chart-plot svg circle.rng-p50")); got != 3 {
			t.Errorf("%d typical marks in the chart, want 3", got)
		}
		if got := len(c.find("div.chart-plot svg line.thr-w")); got != 1 {
			t.Errorf("%d watch lines, want 1", got)
		}
		if got := texts(c.find("div.chart-plot svg text")); !slices.Contains(got, "watch 1 h") {
			t.Errorf("chart labels %q: no watch label", got)
		}
		p := c.first("p.small.sub")
		if p == nil || p.text() != fmt.Sprintf(wholeWay, "1 d 2 h", "9 d") {
			t.Errorf("whole way %v", p)
		}
		if got := texts(p.find("b")); !slices.Equal(got, []string{"1 d 2 h", "9 d"}) {
			t.Errorf("whole way figures %q", got)
		}
		if len(c.find("details.never")) != 0 {
			t.Error("a never-extracted table with no row")
		}
		if info := c.first("details.info").text(); !strings.Contains(info, "118 sessions recalled in the window") {
			t.Errorf("based on: %q", info)
		}
	})

	t.Run("watch", func(t *testing.T) {
		c := funnelNode(t, funnelWatch, nil)
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "watch" || !strings.Contains(p.attr("class"), "st-watch") {
			t.Errorf("pill %v", p)
		}
		cells, classes := hopTexts(t, c)
		wantHops := [][3]string{{stepNames[0], "6 min", "1 in 20: 22 min"}, {stepNames[1], "24 min", "1 in 20: 2 h 40 min"}, {stepNames[2], "1 d 1 h", "1 in 20: 9 d"}}
		if !slices.Equal(cells, wantHops) {
			t.Errorf("hops %q, want %q", cells, wantHops)
		}
		for i, cl := range classes {
			if got := slices.Contains(strings.Fields(cl), "is-watch"); got != (i == 1) {
				t.Errorf("hop %d: is-watch %v, want only the sent to turned-into-memories hop", i, got)
			}
		}
		if w := c.find("div.funnel div.hopc span.st-watch"); len(w) != 1 || w[0].text() != "watch" || !slices.Contains(strings.Fields(c.find("div.funnel div.hopc")[1].attr("class")), "is-watch") {
			t.Errorf("watch word %q: want one, on the watch hop", texts(w))
		}
		if got := stageTexts(c)[2]; got != [3]string{labels[2], "181", "7 still on the server"} {
			t.Errorf("stage 3 %q", got)
		}
		st := c.find("div.state")
		if len(st) != 1 || !strings.Contains(st[0].attr("class"), "st-watch") {
			t.Fatalf("%d state sentences %v", len(st), texts(st))
		}
		const want = "Slow to become memory: 1 in 20 sessions took over 2 h 40 min to be turned into memories (watch over 1 h). Usually the server's queue or its model. names the fix."
		if st[0].text() != want {
			t.Errorf("watch note %q, want %q", st[0].text(), want)
		}
		if !hasCode(st[0], "backsight doctor") {
			t.Error("the watch note does not name backsight doctor")
		}
		if p := c.first("p.small.sub"); p == nil || p.text() != fmt.Sprintf(wholeWay, "1 d 3 h", "9 d") {
			t.Errorf("whole way %v", p)
		}
	})

	t.Run("never extracted", func(t *testing.T) {
		c := funnelNode(t, funnelNever, nil)
		if got := stageTexts(c)[2]; got != [3]string{labels[2], "177", "11 never turned into memories"} {
			t.Errorf("stage 3 %q", got)
		}
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "ok" {
			t.Errorf("pill %v: never extracted is no verdict", p)
		}
		_, classes := hopTexts(t, c)
		if slices.ContainsFunc(classes, func(s string) bool { return strings.Contains(s, "is-watch") }) {
			t.Errorf("a watch hop with the status ok: %q", classes)
		}
	})

	t.Run("too few", func(t *testing.T) {
		c := funnelNode(t, funnelFew, nil)
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "too few" || !strings.Contains(p.attr("class"), "st-few") {
			t.Errorf("pill %v", p)
		}
		st := c.find("div.state")
		const want = "Too few to time yet: 4 sessions were recalled in the last 30 days, 20 are needed. The counts below are exact; the times are not shown."
		if len(st) != 1 || st[0].text() != want || !strings.Contains(st[0].attr("class"), "st-few") {
			t.Fatalf("state %v, want %q", texts(st), want)
		}
		wantStages := [][3]string{{labels[0], "9", "on this PC"}, {labels[1], "9", "all sent"},
			{labels[2], "8", "1 still on the server"}, {labels[3], "4", "4 not recalled yet"}}
		if got := stageTexts(c); !slices.Equal(got, wantStages) {
			t.Errorf("stages %q, want %q", got, wantStages)
		}
		cells, classes := hopTexts(t, c)
		wantHops := [][3]string{{stepNames[0], "—", "9 timed, 20 needed"}, {stepNames[1], "—", "8 timed, 20 needed"}, {stepNames[2], "—", "4 timed, 20 needed"}}
		if !slices.Equal(cells, wantHops) {
			t.Errorf("hops %q, want %q", cells, wantHops)
		}
		if slices.ContainsFunc(classes, func(s string) bool { return strings.Contains(s, "is-watch") }) {
			t.Errorf("a watch hop with no time: %q", classes)
		}
		if len(c.find("figure.chart")) != 0 || len(c.find("div.chart-plot")) != 0 || c.first("p.small.sub") != nil {
			t.Error("times drawn or told with too few sessions")
		}
	})

	t.Run("one session", func(t *testing.T) {
		in := funnelFew
		in.f = map[string]int64{"ended": 2, "sent": 2, "extracted": 1, "recalled": 1, "not_recalled": 0, "never": 1}
		c := funnelNode(t, in, nil)
		want := "Too few to time yet: 1 session was recalled in the last 30 days, 20 are needed. The counts below are exact; the times are not shown."
		if st := c.find("div.state"); len(st) != 1 || st[0].text() != want {
			t.Errorf("state %v, want %q", texts(st), want)
		}
		if got := stageTexts(c)[3][2]; got != "all recalled" {
			t.Errorf("stage 4 note %q", got)
		}
	})

	t.Run("typical only", func(t *testing.T) {
		// 30 recalled: the typical times show, the slowest 1 in 20 needs 100.
		in := funnelIn{status: metrics.StatusOK,
			f:    map[string]int64{"ended": 40, "sent": 40, "extracted": 40, "recalled": 30, "not_recalled": 10},
			hops: [4]hopIn{{5, 0, 40}, {4, 0, 40}, {600, 0, 30}, {610, 0, 30}}}
		c := funnelNode(t, in, nil)
		cells, _ := hopTexts(t, c)
		want := [][3]string{{stepNames[0], "5 min", "1 in 20: 100 needed"}, {stepNames[1], "4 min", "1 in 20: 100 needed"}, {stepNames[2], "10 h", "1 in 20: 100 needed"}}
		if !slices.Equal(cells, want) {
			t.Errorf("hops %q, want %q", cells, want)
		}
		if p := c.first("p.small.sub"); p == nil || p.text() != fmt.Sprintf(wholeWay, "10 h 10 min", "—") {
			t.Errorf("whole way %v", p)
		}
	})

	t.Run("no data", func(t *testing.T) {
		c := funnelNode(t, funnelNormal, func(r *metrics.Result) {
			give(r, ttrID, func(m *metrics.Metric) {
				*m = metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonNoEvents}
			})
		})
		if len(c.find("div.funnel")) != 0 || c.first("div.empty") == nil {
			t.Errorf("a funnel on a metric with no data: %q", c.text())
		}
	})
}

// neverRows are three sessions that gave no memory, one of each state, sent
// 5, 3 and 2 days before the result was made.
func neverRows(r *metrics.Result) []metrics.NeverExtractedRow {
	at := r.GeneratedAt
	return []metrics.NeverExtractedRow{
		{Session: "9e04c2b7a1f34d5e8b60c7d2e91f3a45", Project: "project-a", Sent: at.Add(-5 * 24 * time.Hour), State: "failed", Retries: 3},
		{Session: "c1d8e5a0b2f94c61a7d30e58f1b29c64", Project: "backsight", Sent: at.Add(-3 * 24 * time.Hour), State: "no_memory"},
		{Session: "60fa21d9c7e84b35a1f20d96b3e47c18", Project: "project-c", Sent: at.Add(-2 * 24 * time.Hour), State: "none"},
	}
}

func TestNeverExtractedTable(t *testing.T) {
	const cmd = "backsight stats sessions --has never_extracted"
	t.Run("states in words", func(t *testing.T) {
		c := funnelNode(t, funnelNever, func(r *metrics.Result) { r.Tables.NeverExtracted = neverRows(r) })
		d := c.first("details.never")
		if d == nil {
			t.Fatal("no never-extracted table")
		}
		if got := d.first("summary").text(); got != "Sent, never turned into memories (11)" {
			t.Errorf("summary %q", got)
		}
		var rows []string
		for _, tr := range d.find("tbody tr") {
			rows = append(rows, strings.Join(texts(tr.find("td")), " | "))
		}
		want := []string{
			"9e04c2b7 | project-a | 5 d ago | server task failed after 3 tries",
			"c1d8e5a0 | backsight | 3 d ago | finished, no memory made",
			"60fa21d9 | project-c | 2 d ago | no server task found"}
		if !slices.Equal(rows, want) {
			t.Errorf("rows %q, want %q", rows, want)
		}
		if got := texts(d.find("thead th")); !slices.Equal(got, []string{"Session", "Project", "Sent", "On the server"}) {
			t.Errorf("heads %q", got)
		}
		trs := d.find("tbody tr")
		if len(trs) != 3 {
			t.Fatalf("%d rows", len(trs))
		}
		if id := trs[0].first("a.idc"); id == nil || id.attr("href") != "/sessions/9e04c2b7a1f34d5e8b60c7d2e91f3a45" {
			t.Errorf("session chip %v", id)
		}
		if p := trs[0].first("a.trunc"); p == nil || !strings.HasPrefix(p.attr("href"), "/projects/project-a") {
			t.Errorf("project link %v", p)
		}
		if w := trs[0].first("span.st-watch"); w == nil || w.text() != "server task failed" {
			t.Errorf("failed word %v", w)
		}
		if len(trs[1].find("span.st")) != 0 || len(trs[2].find("span.st")) != 0 {
			t.Error("a status word on a state that is not a failure")
		}
		// 11 sessions never became memories, 3 are listed: the note says how to see the other 8.
		note := d.first("p.xs.muted")
		wantNote := "A short session often holds nothing worth keeping: “no memory made” is normal. A failed task is on “Is the server keeping up?”. 8 more: ."
		if note == nil || note.text() != wantNote || !hasCode(note, cmd) {
			t.Errorf("note %v, want %q with the command", note, wantNote)
		}
	})
	t.Run("retries are counted, never worded as an error", func(t *testing.T) {
		const leak = "OSError: connect to 10.0.0.9 refused (token=abc123)"
		c := funnelNode(t, funnelNever, func(r *metrics.Result) {
			at := r.GeneratedAt
			r.Tables.NeverExtracted = []metrics.NeverExtractedRow{
				{Session: "aa000001", Project: "p", Sent: at.Add(-2 * time.Hour), State: "failed", Retries: 1},
				{Session: "aa000002", Project: "p", Sent: at.Add(-2 * time.Hour), State: "failed", Retries: 0},
				{Session: "aa000003", Project: "p", Sent: at.Add(-2 * time.Hour), State: leak},
			}
		})
		var states []string
		for _, tr := range c.find("details.never tbody tr") {
			states = append(states, tr.find("td")[3].text())
		}
		want := []string{"server task failed after 1 try", "server task failed", "not known"}
		if !slices.Equal(states, want) {
			t.Errorf("states %q, want %q", states, want)
		}
		if strings.Contains(c.text(), "OSError") || strings.Contains(c.text(), "abc123") {
			t.Errorf("error text on the card: %q", c.text())
		}
	})
	t.Run("all of them listed", func(t *testing.T) {
		in := funnelNever
		in.f = map[string]int64{"ended": 12, "sent": 12, "extracted": 9, "never": 3, "recalled": 5, "not_recalled": 4}
		in.hops = [4]hopIn{{6, 0, 12}, {3, 0, 9}, {10, 0, 5}, {20, 0, 5}}
		c := funnelNode(t, in, func(r *metrics.Result) { r.Tables.NeverExtracted = neverRows(r) })
		note := c.first("details.never p.xs.muted")
		if note == nil || strings.Contains(note.text(), "more") || !hasCode(note, cmd) {
			t.Errorf("note %v: nothing is left to list, the command is still given", note)
		}
	})
	t.Run("capped at ten rows", func(t *testing.T) {
		in := funnelNever
		in.f = map[string]int64{"ended": 192, "sent": 188, "waiting_or_failed": 4, "extracted": 174, "never": 14, "recalled": 112, "not_recalled": 62}
		c := funnelNode(t, in, func(r *metrics.Result) {
			for i := range 14 {
				r.Tables.NeverExtracted = append(r.Tables.NeverExtracted, metrics.NeverExtractedRow{
					Session: fmt.Sprintf("bb%06d", i), Project: "p", Sent: r.GeneratedAt.Add(-48 * time.Hour), State: "none"})
			}
		})
		if n := len(c.find("details.never tbody tr")); n != 10 {
			t.Errorf("%d rows, want 10", n)
		}
		if got := c.first("details.never summary").text(); got != "Sent, never turned into memories (14)" {
			t.Errorf("summary %q", got)
		}
		if note := c.first("details.never p.xs.muted"); note == nil || !strings.Contains(note.text(), " 4 more: ") {
			t.Errorf("note %v, want 4 more (14 never, 10 listed)", note)
		}
	})
	t.Run("no row, no table", func(t *testing.T) {
		c := funnelNode(t, funnelNever, nil)
		if len(c.find("details.never")) != 0 {
			t.Error("a table with no session in it")
		}
	})
}

// doctorHistoryRows are nine checks over 30 days of October with four days
// the upkeep did not run: three that changed and six that never left ok.
func doctorHistoryRows() []metrics.DoctorHistoryRow {
	noRun := map[int]bool{5: true, 6: true, 18: true, 25: true}
	status := func(i, d int) string {
		switch {
		case noRun[d]:
			return "none"
		case i == 0 && d >= 20, i == 2 && d >= 27, i == 1 && d == 12:
			return "watch"
		case i == 1 && d == 11:
			return "bad"
		}
		return "ok"
	}
	ids := []string{"backup", "server", "update", "config", "disk", "jobs", "locks", "notes", "recall"}
	var rows []metrics.DoctorHistoryRow
	for i, id := range ids {
		row := metrics.DoctorHistoryRow{Check: id}
		prev := ""
		for d := range 30 {
			st := status(i, d)
			row.Days = append(row.Days, metrics.DoctorDay{Day: fmt.Sprintf("2026-10-%02d", d+1), Status: st})
			switch st {
			case "ok":
				row.OK++
			case "watch":
				row.Watch++
			case "bad":
				row.Bad++
			}
			if st != "none" {
				if prev != "" && prev != st {
					row.LastChange = &metrics.DoctorChange{Day: fmt.Sprintf("2026-10-%02d", d+1), From: prev, To: st}
				}
				prev, row.Now = st, st
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func giveDoctorHistory(r *metrics.Result, rows []metrics.DoctorHistoryRow, runs, without int64) {
	r.Tables.DoctorHistory = rows
	give(r, "doctor.history", func(m *metrics.Metric) {
		m.Value, m.Num, m.Den = fp(float64(runs)), ip(runs), ip(runs)
		m.Extra = map[string]any{"days_without": without}
	})
}

func TestDoctorHistoryCard(t *testing.T) {
	n := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		giveDoctorHistory(d.V["system"], doctorHistoryRows(), 26, 4)
	})
	ids := cardIDs(n)
	at := slices.Index(ids, "w-doctor-check")
	if at < 0 || at+1 >= len(ids) || ids[at+1] != "w-doctor-history" {
		t.Fatalf("cards %v: the history is not right after the health checks", ids)
	}
	c := cardOf(t, n, "w-doctor-history")
	if got := c.first("h2").text(); got != "Have the health checks passed lately?" {
		t.Errorf("title %q", got)
	}
	if got := texts(c.find("div.card-head span.chip")); !slices.Equal(got, []string{"this PC"}) {
		t.Errorf("chips %q", got)
	}
	if cli := c.first("details.cli code"); cli == nil || cli.Text != "backsight stats system --days 30" {
		t.Errorf("CLI %v", cli)
	}
	t.Run("the strip", func(t *testing.T) {
		svg := c.first("figure.chart svg")
		if svg == nil {
			t.Fatal("no strip")
		}
		// 9 checks x 30 days: 36 days without a run, and the others by the day's result.
		count := func(class string) int { return len(svg.find("path." + class)) }
		for class, want := range map[string]int{"norun": 36, "f-st-watch": 9 + 1 + 3, "f-st-bad": 1, "f-st-ok": 270 - 36 - 13 - 1} {
			if got := count(class); got != want {
				t.Errorf("%d marks of %s, want %d", got, class, want)
			}
		}
		if got := len(svg.find("text.lbl")); got != 9 {
			t.Errorf("%d check names, want 9", got)
		}
		if got := texts(c.find("div.legend span")); !slices.Equal(got, []string{"ok", "watch", "attention", "no upkeep run that day"}) {
			t.Errorf("legend %q", got)
		}
	})
	t.Run("the table folds the checks that never left ok", func(t *testing.T) {
		var rows []string
		for _, tr := range c.find("details.tblfb tbody tr") {
			rows = append(rows, strings.Join(texts(tr.find("td")), " | "))
		}
		want := []string{
			"backup | watch | 17 | 9 | 0 | 4 | Oct 21: ok → watch",
			"memory server | ok | 24 | 1 | 1 | 4 | Oct 14: watch → ok",
			"update | watch | 23 | 3 | 0 | 4 | Oct 28: ok → watch",
			"6 other checks | ok | 26 each | 0 | 0 | 4 each | —"}
		if !slices.Equal(rows, want) {
			t.Errorf("rows\n%q\nwant\n%q", rows, want)
		}
	})
	t.Run("the runs sentence", func(t *testing.T) {
		p := c.first("details.tblfb p.tbl-more")
		if p == nil || p.text() != "26 runs in 30 days; 4 days without a run." {
			t.Errorf("sentence %v", p)
		}
	})
	t.Run("one run, one day without", func(t *testing.T) {
		n := pageNode(t, "system", false, func(d *PageData) {
			richSystem(d.V["system"])
			rows := doctorHistoryRows()[:1]
			rows[0].Days = rows[0].Days[:2]
			giveDoctorHistory(d.V["system"], rows, 1, 1)
		})
		p := cardOf(t, n, "w-doctor-history").first("details.tblfb p.tbl-more")
		if p == nil || p.text() != "1 run in 2 days; 1 day without a run." {
			t.Errorf("sentence %v", p)
		}
	})
	t.Run("a lone quiet check is listed, not folded", func(t *testing.T) {
		n := pageNode(t, "system", false, func(d *PageData) {
			richSystem(d.V["system"])
			giveDoctorHistory(d.V["system"], doctorHistoryRows()[:4], 26, 4)
		})
		var names []string
		for _, tr := range cardOf(t, n, "w-doctor-history").find("details.tblfb tbody tr") {
			names = append(names, tr.find("td")[0].text())
		}
		if want := []string{"backup", "memory server", "update", "settings"}; !slices.Equal(names, want) {
			t.Errorf("rows %q, want %q", names, want)
		}
	})
	t.Run("every check ok", func(t *testing.T) {
		n := pageNode(t, "system", false, func(d *PageData) {
			richSystem(d.V["system"])
			giveDoctorHistory(d.V["system"], doctorHistoryRows()[3:], 26, 4)
		})
		var names []string
		for _, tr := range cardOf(t, n, "w-doctor-history").find("details.tblfb tbody tr") {
			names = append(names, tr.find("td")[0].text())
		}
		if want := []string{"6 checks"}; !slices.Equal(names, want) {
			t.Errorf("rows %q, want %q", names, want)
		}
	})
	t.Run("no run", func(t *testing.T) {
		n := pageNode(t, "system", false, func(d *PageData) {
			richSystem(d.V["system"])
			d.V["system"].Tables.DoctorHistory = nil
			give(d.V["system"], "doctor.history", func(m *metrics.Metric) {
				*m = metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonNoEvents}
			})
		})
		c := cardOf(t, n, "w-doctor-history")
		if len(c.find("figure.chart")) != 0 || c.first("div.empty") == nil {
			t.Errorf("a strip with no run: %q", c.text())
		}
	})
}

// scanOf gives the system page the last scan: count found, age days ago,
// taking ms.
func scanOf(r *metrics.Result, count, age, ms int64, tags ...string) {
	give(r, "secrets.suspects", func(m *metrics.Metric) {
		m.Value, m.Tags = fp(float64(count)), tags
		if count > 0 {
			m.Status = metrics.StatusWatch
		}
		m.Extra = map[string]any{"scanned_at": r.GeneratedAt.Add(-time.Duration(age) * 24 * time.Hour), "age_days": age,
			"live": count, "forgotten": int64(0), "pages": int64(3), "ms": ms}
	})
}

func secretsNode(t *testing.T, edit func(r *metrics.Result)) *node {
	t.Helper()
	n := pageNode(t, "system", false, func(d *PageData) {
		richSystem(d.V["system"])
		edit(d.V["system"])
	})
	ids := cardIDs(n)
	if at := slices.Index(ids, "w-doctor-history"); at < 0 || at+1 >= len(ids) || ids[at+1] != "w-secrets-suspects" {
		t.Fatalf("cards %v: the secrets card is not right after the history", ids)
	}
	return cardOf(t, n, "w-secrets-suspects")
}

func TestSecretsCardStates(t *testing.T) {
	const cmd = "backsight memory scan"
	pill := func(t *testing.T, c *node) (string, string) {
		t.Helper()
		p := c.first("div.card-head span.pill")
		if p == nil {
			t.Fatal("no pill")
		}
		return p.text(), p.attr("class")
	}
	t.Run("found", func(t *testing.T) {
		c := secretsNode(t, func(r *metrics.Result) { scanOf(r, 2, 2, 41_000) })
		if c.first("h2").text() != "Could memory hold a secret?" {
			t.Errorf("title %q", c.first("h2").text())
		}
		if got := texts(c.find("div.card-head span.chip")); !slices.Equal(got, []string{"server"}) {
			t.Errorf("chips %q", got)
		}
		if w, cl := pill(t, c); w != "watch" || !strings.Contains(cl, "st-watch") {
			t.Errorf("pill %q %q", w, cl)
		}
		wantFigs := [][3]string{{"Possible secrets", "2", "found by the last scan"}, {"Last scan", "2 d ago", "took 41 s"}}
		if got := figsOf(c); !slices.Equal(got, wantFigs) {
			t.Errorf("figures %q, want %q", got, wantFigs)
		}
		if tm := c.first("div.fig time"); tm == nil || tm.attr("datetime") == "" || tm.attr("title") == "" {
			t.Errorf("last scan time %v", tm)
		}
		if p := c.first("p.small.sub"); p == nil || p.text() != "The scan lists them in your terminal, with what to do for each. The dashboard never shows which memories they are." {
			t.Errorf("sentence %v", p)
		}
		if cmds := c.find("div.cmd code"); len(cmds) != 1 || cmds[0].Text != cmd {
			t.Errorf("commands %v", texts(cmds))
		}
		info := c.first("details.info")
		if dts, dds := info.find("dt"), info.find("dd"); len(dts) != len(dds) || !slices.Contains(texts(dts), "Privacy") ||
			dds[slices.Index(texts(dts), "Privacy")].text() != "Only the count and the date are kept: never the text, never which memories." {
			t.Errorf("popover rows %q / %q", texts(dts), texts(dds))
		}
		if !slices.Contains(texts(info.find("dd")), "Watch on Flowing as soon as one is found.") || !strings.Contains(info.text(), "Possible secrets in memory") {
			t.Errorf("popover %q", info.text())
		}
	})
	t.Run("found, scan old", func(t *testing.T) {
		c := secretsNode(t, func(r *metrics.Result) { scanOf(r, 1, 41, 9_000, "old_scan") })
		if w, _ := pill(t, c); w != "watch" {
			t.Errorf("pill %q: an old scan does not change the status", w)
		}
		if got := figsOf(c); len(got) != 2 || got[0][1] != "1" || got[1][2] != "took 9 s" {
			t.Errorf("figures %q", got)
		}
	})
	t.Run("none found", func(t *testing.T) {
		c := secretsNode(t, func(r *metrics.Result) { scanOf(r, 0, 2, 41_000) })
		if w, cl := pill(t, c); w != "ok" || !strings.Contains(cl, "st-ok") {
			t.Errorf("pill %q %q", w, cl)
		}
		if p := c.first("p.small"); p == nil || p.text() != "None found by the last scan, 2 d ago (41 s)." {
			t.Errorf("sentence %v", p)
		}
		if len(c.find("div.cmd")) != 0 || len(c.find("div.fig")) != 0 {
			t.Error("a command or figures on a clean scan")
		}
	})
	t.Run("no scan yet", func(t *testing.T) {
		c := secretsNode(t, func(r *metrics.Result) {
			give(r, "secrets.suspects", func(m *metrics.Metric) {
				*m = metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonNoEvents}
			})
		})
		if w, cl := pill(t, c); w != "no scan yet" || !strings.Contains(cl, "st-few") {
			t.Errorf("pill %q %q", w, cl)
		}
		if p := c.first("p.small"); p == nil || p.text() != "Memory has never been scanned on this PC." {
			t.Errorf("sentence %v", p)
		}
		if cmds := c.find("div.cmd code"); len(cmds) != 1 || cmds[0].Text != cmd {
			t.Errorf("commands %v", texts(cmds))
		}
	})
	t.Run("old scan", func(t *testing.T) {
		c := secretsNode(t, func(r *metrics.Result) { scanOf(r, 0, 41, 41_000, "old_scan") })
		if w, cl := pill(t, c); w != "old scan" || !strings.Contains(cl, "st-few") {
			t.Errorf("pill %q %q", w, cl)
		}
		if p := c.first("p.small"); p == nil || p.text() != "None found, but the last scan is 41 days old: memories added since are not checked." {
			t.Errorf("sentence %v", p)
		}
		if cmds := c.find("div.cmd code"); len(cmds) != 1 || cmds[0].Text != cmd {
			t.Errorf("commands %v", texts(cmds))
		}
	})
	t.Run("a scan of today", func(t *testing.T) {
		c := secretsNode(t, func(r *metrics.Result) { scanOf(r, 0, 0, 800) })
		if p := c.first("p.small"); p == nil || !strings.HasPrefix(p.text(), "None found by the last scan, ") || !strings.HasSuffix(p.text(), " (800 ms).") {
			t.Errorf("sentence %v", p)
		}
	})
}

func TestSecretsAttentionOnOverview(t *testing.T) {
	r := scenarioNamed(t, "healthy")
	r.Attention = []metrics.Item{{Metric: "secrets.suspects", Severity: metrics.SeverityWatch, Axis: "flowing",
		Text:    "Possible secrets in memory: run backsight memory scan.",
		Why:     "The last scan, 2 days ago, found 2 memories that look like a key, token or password. Memory is shown to Claude in later sessions; the scan says which ones and how to forget them.",
		Command: "backsight memory scan"}}
	c := renderOverview(t, r).first("section.card[aria-labelledby=w-attention]")
	if c == nil {
		t.Fatal("no attention card")
	}
	lis := c.find("li.att-item")
	if len(lis) != 1 {
		t.Fatalf("%d items", len(lis))
	}
	li := lis[0]
	if w := li.first("span.pill"); w.text() != "watch" || !strings.Contains(w.attr("class"), "st-watch") {
		t.Errorf("pill %v", w)
	}
	if got := li.first("p").text(); got != r.Attention[0].Text {
		t.Errorf("text %q, want %q", got, r.Attention[0].Text)
	}
	if cmds := li.find("div.cmd code"); len(cmds) != 1 || cmds[0].Text != "backsight memory scan" {
		t.Errorf("command %v", texts(cmds))
	}
	if why := li.first("details.why div"); why == nil || why.text() != r.Attention[0].Why {
		t.Errorf("why %v", why)
	}
}
