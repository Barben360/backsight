package dashboard

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

// pageNode renders a page on its golden, English, the 30-day window, after
// edit.
func pageNode(t *testing.T, name string, dev bool, edit func(d *PageData)) *node {
	t.Helper()
	return parseHTML(t, render(t, name, func(d *PageData) {
		d.L, d.P = en, views.Params{Days: 30, DaysSet: true}
		d.P.Dev = dev
		d.CLI = "backsight stats " + name + " --days 30"
		d.Frame.Window = "last 30 days"
		d.Frame.Prefs.Dev = dev
		if edit != nil {
			edit(d)
		}
	}))
}

// give sets the metric's number, status and extras.
func give(r *metrics.Result, id string, edit func(m *metrics.Metric)) {
	m := r.Metrics[id]
	m.Status = metrics.StatusOK
	m.Reason = ""
	edit(&m)
	r.Metrics[id] = m
}

func ratePoints(m *metrics.Metric, num, den []int64) {
	for i := range m.Series {
		m.Series[i].Num, m.Series[i].Den = ip(num[i%len(num)]), ip(den[i%len(den)])
		if den[i%len(den)] > 0 {
			m.Series[i].Value = fp(float64(num[i%len(num)]) / float64(den[i%len(den)]))
		} else {
			m.Series[i].Value = nil
		}
	}
}

// richQuality gives every quality metric of the golden a number.
func richQuality(r *metrics.Result) {
	rate := func(id string, v float64, n, d int64) {
		give(r, id, func(m *metrics.Metric) {
			m.Value, m.Num, m.Den, m.CI = fp(v), ip(n), ip(d), &[2]float64{v / 2, min(1, v*1.5)}
			if len(m.Series) == 0 {
				m.Series = slices.Clone(r.Metrics["recall.hit_rate"].Series)
			}
			ratePoints(m, []int64{1, 2, 3}, []int64{4, 5, 6})
		})
	}
	rate("used.turn_rate", 0.62, 31, 50)
	rate("declare.coverage", 0.9, 45, 50)
	rate("recall.hit_rate", 0.28, 189, 676)
	rate("search.rate", 0.24, 24, 100)
	rate("search.empty_rate", 0.13, 13, 100)
	rate("verdict.wrong_rate", 0.04, 3, 71)
	give(r, "recall.empty_reasons", func(m *metrics.Metric) {
		m.Value = fp(9)
		m.Extra = map[string]any{"empty": map[string]int64{"no_candidates": 6, "below_floor": 3}, "skips": map[string]int64{}}
		for i := range m.Series {
			m.Series[i].Parts = map[string]int64{"no_candidates": int64(i), "below_floor": 1}
		}
	})
	r.Tables.Reported = []metrics.ReportedRow{{MemoryID: "mem-aaaa1111bbbb", Verdict: "wrong", Session: "ses-cccc2222dddd", Project: "proj-a", At: r.GeneratedAt.Add(-time.Hour)}}
}

func cardIDs(n *node) []string {
	var out []string
	for _, h := range n.find("h2") {
		if id := h.attr("id"); strings.HasPrefix(id, "w-") {
			out = append(out, id)
		}
	}
	return out
}

func cardOf(t *testing.T, n *node, id string) *node {
	t.Helper()
	c := n.first("section.card[aria-labelledby=" + id + "]")
	if c == nil {
		t.Fatalf("no card %s in %v", id, cardIDs(n))
	}
	return c
}

func TestQualityCards(t *testing.T) {
	want := []string{"w-used-turn_rate", "w-outcomes", "w-reported", "w-recall-hit_rate", "w-recall-empty_reasons", "w-search-rate"}
	t.Run("a card holds one chart or one sentence", func(t *testing.T) {
		for label, edit := range map[string]func(d *PageData){
			"golden": nil,
			"rich":   func(d *PageData) { richQuality(d.V["quality"]) },
		} {
			n := pageNode(t, "quality", false, edit)
			if got := cardIDs(n); !slices.Equal(got, want) {
				t.Fatalf("%s: cards %v, want %v", label, got, want)
			}
			for _, id := range want {
				c := cardOf(t, n, id)
				figs, empties := len(c.find("figure.chart")), len(c.find("div.empty"))
				if id == "w-reported" && label == "rich" { // a table of rows
					if rows := len(c.find("tbody tr")); rows != 1 || figs+empties != 0 {
						t.Errorf("rich reported: %d rows, %d charts, %d sentences", rows, figs, empties)
					}
					continue
				}
				if figs+empties != 1 {
					t.Errorf("%s %s: %d charts and %d sentences", label, id, figs, empties)
				}
				// The golden numbers: the turns have none (0 of 0), the rest do.
				drawn := figs == 1
				if label == "golden" && drawn != (id != "w-used-turn_rate" && id != "w-reported") {
					t.Errorf("golden %s: chart drawn %v", id, drawn)
				}
				if label == "rich" && id != "w-reported" && !drawn {
					t.Errorf("rich %s: no chart: %q", id, c.text())
				}
			}
		}
	})
	t.Run("outcomes", func(t *testing.T) {
		n := pageNode(t, "quality", false, nil)
		c := cardOf(t, n, "w-outcomes")
		var legend []string
		for _, s := range c.find("figure.chart .legend span") {
			legend = append(legend, s.text())
		}
		if w := []string{"used or confirmed", "shown, then ignored", "not declared", "shown only", "pending"}; !slices.Equal(legend, w) {
			t.Errorf("legend %v, want %v", legend, w)
		}
		if len(c.find("rect.seg.f-pending")) == 0 || len(c.find("line.hatch-ln")) == 0 {
			t.Error("the golden has pending memories: no hatched part")
		}
		none := pageNode(t, "quality", false, func(d *PageData) {
			for i := range d.V["quality"].Tables.OutcomeMix {
				d.V["quality"].Tables.OutcomeMix[i].Pending = 0
				d.V["quality"].Tables.OutcomeMix[i].Used = 2
			}
		})
		if got := cardOf(t, none, "w-outcomes").find("rect.seg.f-pending"); len(got) != 0 {
			t.Errorf("no pending memory but %d hatched parts", len(got))
		}
		if got := cardOf(t, none, "w-outcomes").find("line.hatch-ln"); len(got) != 0 {
			t.Errorf("no pending memory but %d hatch lines", len(got))
		}
	})
	t.Run("the ranking card is the developer's", func(t *testing.T) {
		plain := pageNode(t, "quality", false, nil)
		if plain.first("#w-ranking") != nil || len(plain.find("code")) > 0 && hasCode(plain, "used.rank") {
			t.Error("the ranking card shows without developer mode")
		}
		dev := pageNode(t, "quality", true, func(d *PageData) {
			give(d.V["quality"], "used.rank", func(m *metrics.Metric) {
				m.Value = fp(9)
				m.Extra = map[string]any{"histogram": map[string]int64{"1": 6, "2": 2, "unranked": 1}, "hit_at_1": 0.5, "hit_at_3": 0.75}
			})
		})
		c := cardOf(t, dev, "w-ranking")
		if !slices.Contains(cardIDs(dev), "w-ranking") || !hasCode(c, "used.rank") {
			t.Errorf("ranking card: ids %v, used.rank named %v", cardIDs(dev), hasCode(c, "used.rank"))
		}
		if chip := c.first("span.chip-dev"); chip == nil || chip.text() != "developer" {
			t.Errorf("chip %+v", chip)
		}
		if got := len(c.find("div.figs div.fig")); got != 6 {
			t.Errorf("%d figures, want the six developer metrics", got)
		}
		if got := len(c.find("rect.f-seq1")); got != 7 {
			t.Errorf("%d rank bars, want 7", got)
		}
		if !strings.Contains(c.text(), "rank 1 6") {
			t.Errorf("no rank line: %q", c.text())
		}
	})
	t.Run("the outcomes by path, in developer mode", func(t *testing.T) {
		dev := pageNode(t, "quality", true, func(d *PageData) {
			for i := range d.V["quality"].Tables.OutcomeMix {
				d.V["quality"].Tables.OutcomeMix[i].ByPath = map[string]metrics.OutcomeDay{
					"prompt": {Used: 2}, "search": {Pending: 1}}
			}
		})
		c := cardOf(t, dev, "w-outcomes")
		var names []string
		for _, h := range c.find("h3.chart-name") {
			names = append(names, h.text())
		}
		if w := []string{"All paths", "added to the prompt", "Claude's search"}; !slices.Equal(names, w) {
			t.Errorf("chart names %v, want %v", names, w)
		}
		if got := len(c.find("figure.chart")); got != 3 {
			t.Errorf("%d charts", got)
		}
	})
}

func hasCode(n *node, text string) bool {
	for _, c := range n.find("code") {
		if strings.TrimSpace(c.Text) == text {
			return true
		}
	}
	return false
}

func TestReportedTable(t *testing.T) {
	n := pageNode(t, "quality", false, func(d *PageData) {
		richQuality(d.V["quality"])
		d.V["quality"].Tables.Reported = append(d.V["quality"].Tables.Reported,
			metrics.ReportedRow{MemoryID: "mem/../x y", Verdict: "outdated", Session: "", Project: "", At: d.V["quality"].GeneratedAt.Add(-48 * time.Hour)})
	})
	c := cardOf(t, n, "w-reported")
	rows := c.find("tbody tr")
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	cells := rows[0].find("td")
	if got := cells[0].first("a.idc"); got == nil || got.attr("href") != "/memories/mem-aaaa1111bbbb" || got.attr("title") != "mem-aaaa1111bbbb" {
		t.Errorf("memory chip %+v", got)
	}
	if cells[1].text() != "reported wrong" || cells[2].text() != "proj-a" {
		t.Errorf("verdict %q, project %q", cells[1].text(), cells[2].text())
	}
	if got := cells[3].first("a.idc"); got == nil || got.attr("href") != "/sessions/ses-cccc2222dddd" {
		t.Errorf("session chip %+v", got)
	}
	if tm := cells[4].first("time"); tm == nil || tm.text() != "1 h ago" {
		t.Errorf("when %+v", tm)
	}
	second := rows[1].find("td")
	if second[1].text() != "out of date" || second[2].text() != dash || second[3].text() != dash {
		t.Errorf("second row %q %q %q", second[1].text(), second[2].text(), second[3].text())
	}
	if got := second[0].first("a.idc").attr("href"); got != "/memories/mem%2F..%2Fx%20y" {
		t.Errorf("hostile memory id href %q", got)
	}
	// Nothing reported: a sentence, no table.
	none := cardOf(t, pageNode(t, "quality", false, nil), "w-reported")
	if none.first("table") != nil || !strings.Contains(none.text(), "Nothing was reported wrong or out of date in the last 30 days.") {
		t.Errorf("empty card: %q", none.text())
	}
}

func TestQualityFeedbackOff(t *testing.T) {
	n := pageNode(t, "quality", false, func(d *PageData) {
		r := d.V["quality"]
		for id, m := range r.Metrics {
			m.Status, m.Reason, m.Value, m.Num, m.Den, m.Series, m.Extra = metrics.StatusNoData, metrics.ReasonFeedbackOff, nil, nil, nil, nil, nil
			r.Metrics[id] = m
		}
		r.Tables.OutcomeMix, r.Tables.Reported = nil, nil
	})
	for _, id := range []string{"w-used-turn_rate", "w-outcomes", "w-reported", "w-recall-hit_rate", "w-recall-empty_reasons", "w-search-rate"} {
		c := cardOf(t, n, id)
		if !strings.Contains(c.text(), "Memory feedback is off") || !hasCode(c, "feedback.enabled") {
			t.Errorf("%s: %q, feedback.enabled named %v", id, c.text(), hasCode(c, "feedback.enabled"))
		}
		if c.first("figure.chart") != nil {
			t.Errorf("%s draws a chart without feedback", id)
		}
	}
}

func TestEmptyReasonsHint(t *testing.T) {
	rich := func(project string) *node {
		return pageNode(t, "quality", false, func(d *PageData) {
			richQuality(d.V["quality"])
			d.P.Project = project
		})
	}
	c := cardOf(t, rich("my proj"), "w-recall-empty_reasons")
	if !strings.Contains(c.text(), "nothing close enough is in memory") || !hasCode(c, "backsight retain backfill --project 'my proj' --plan") {
		t.Errorf("hint: %q", c.text())
	}
	if c := cardOf(t, rich(""), "w-recall-empty_reasons"); !strings.Contains(c.text(), "nothing close enough") || hasCode(c, "backsight retain backfill --plan") || strings.Contains(c.text(), "helps:") {
		t.Errorf("without a project the advice has no command: %q", c.text())
	}
	other := pageNode(t, "quality", false, func(d *PageData) {
		richQuality(d.V["quality"])
		give(d.V["quality"], "recall.empty_reasons", func(m *metrics.Metric) {
			m.Extra = map[string]any{"empty": map[string]int64{"no_candidates": 1, "all_seen": 5}}
		})
	})
	if c := cardOf(t, other, "w-recall-empty_reasons"); strings.Contains(c.text(), "nothing close enough") {
		t.Errorf("advice given when most prompts got nothing for another reason: %q", c.text())
	}
	// An outage is not an empty bank: no backfill advice, and a word of its own.
	outage := pageNode(t, "quality", false, func(d *PageData) {
		richQuality(d.V["quality"])
		d.P.Project = "my proj"
		give(d.V["quality"], "recall.empty_reasons", func(m *metrics.Metric) {
			m.Extra = map[string]any{"empty": map[string]int64{"no_candidates": 1, "error": 5}}
		})
	})
	c = cardOf(t, outage, "w-recall-empty_reasons")
	if strings.Contains(c.text(), "nothing close enough") || hasCode(c, "backsight retain backfill --project 'my proj' --plan") {
		t.Errorf("an outage is advised as an empty bank: %q", c.text())
	}
	if !strings.Contains(c.text(), "could not be searched") {
		t.Errorf("no advice for failed recalls: %q", c.text())
	}
	// The positional colour slice lines up with emptyOrder: each reason
	// keeps its own colour, the failed search the "failed" one.
	var legend, swatches []string
	for _, s := range c.find("figure.chart .legend span") {
		legend = append(legend, s.text())
	}
	for _, i := range c.find("figure.chart .legend i") {
		swatches = append(swatches, i.attr("class"))
	}
	wantLegend := []string{"the search failed", "no candidates", "below the cut-off", "all already seen", "cut short by the size limit", "reason not recorded"}
	wantSwatches := []string{"sw f-failed", "sw f-seq1", "sw f-seq2", "sw f-seq3", "sw f-aside", "sw f-neutral"}
	if !slices.Equal(legend, wantLegend) || !slices.Equal(swatches, wantSwatches) {
		t.Errorf("legend %v with swatches %v, want %v with %v", legend, swatches, wantLegend, wantSwatches)
	}
}

// rich cost data: a histogram, a split, latencies and a day of extraction.
func richCost(r *metrics.Result) {
	bins := make([]map[string]any, 21)
	for i := range bins {
		bins[i] = map[string]any{"lo": int64(i * 1000), "hi": int64((i + 1) * 1000), "n": int64(i % 3)}
	}
	bins[20]["hi"] = nil
	give(r, "context.per_session", func(m *metrics.Metric) {
		m.Value, m.Den = fp(9000), ip(137)
		m.Extra = map[string]any{"p50": 2100.0, "p95": 9000.0, "tokens_p95": int64(2250), "histogram": bins}
		for i := range m.Series {
			m.Series[i].Parts = map[string]int64{"briefing": 100, "prompts": int64(200 + i)}
		}
	})
	give(r, "context.per_prompt", func(m *metrics.Metric) {
		m.Value, m.Den = fp(1500), ip(300)
		m.Extra = map[string]any{"p50": 640.0, "p95": 1500.0}
	})
	for _, id := range []string{"recall.latency", "search.latency"} {
		give(r, id, func(m *metrics.Metric) {
			m.Value, m.Den = fp(1900), ip(200)
			m.Extra = map[string]any{"p50": 420.0, "p95": 1900.0}
			if len(m.Series) == 0 {
				m.Series = slices.Clone(r.Metrics["recall.latency"].Series)
			}
			for i := range m.Series {
				m.Series[i].N, m.Series[i].P50, m.Series[i].P95 = ip(int64(i+1)), fp(400), fp(1000+float64(i)*100)
			}
		})
	}
	give(r, "extraction.tokens", func(m *metrics.Metric) { m.Value = fp(1000) })
	give(r, "extraction.cost", func(m *metrics.Metric) {
		m.Value = fp(186.4)
		m.Extra = map[string]any{"price_set": true, "currency": "USD"}
	})
	give(r, "extraction.per_session", func(m *metrics.Metric) { m.Value = fp(4200) })
	r.Tables.Extraction = []metrics.ExtractionRow{
		{Day: "2026-10-05", Operation: "retain", Input: 100, Cached: 50, Output: 10, Cost: fp(1.5)},
		{Day: "2026-10-05", Operation: "consolidation", Input: 200, Cached: 0, Output: 20},
		{Day: "2026-10-06", Operation: "retain", Input: 300, Cached: 10, Output: 30, Cost: fp(2)},
	}
}

func TestCostCards(t *testing.T) {
	n := pageNode(t, "cost", false, func(d *PageData) { richCost(d.V["cost"]) })
	want := []string{"w-context-per_session", "w-context-split", "w-recall-latency", "w-extraction-tokens", "w-memory-yield"}
	if got := cardIDs(n); !slices.Equal(got, want) {
		t.Fatalf("cards %v, want %v", got, want)
	}
	t.Run("the context histogram", func(t *testing.T) {
		c := cardOf(t, n, "w-context-per_session")
		if got := len(c.find("rect.f-seq1")); got != 21 {
			t.Errorf("%d bars, want the histogram's 21", got)
		}
		var marks []string
		for _, m := range c.find("text.lbl") {
			marks = append(marks, m.text())
		}
		if w := []string{"typical 2,100 chars", "high 9,000 chars"}; !slices.Equal(marks, w) {
			t.Errorf("markers %v, want %v", marks, w)
		}
		if !strings.Contains(c.text(), "≈ 2,250 tokens") || !strings.Contains(c.text(), "typical 2,100 chars, high 9,000 chars") {
			t.Errorf("lead and lines: %q", c.text())
		}
		if c.first("div.row.lead") == nil {
			t.Error("no headline number")
		}
	})
	t.Run("where the context is added", func(t *testing.T) {
		c := cardOf(t, n, "w-context-split")
		var parts []string
		for _, s := range c.find("figure.chart .legend span") {
			parts = append(parts, s.text())
		}
		if w := []string{"at session start", "before prompts"}; !slices.Equal(parts, w) {
			t.Errorf("parts %v, want %v", parts, w)
		}
		if len(c.find("rect.seg.f-seq1")) == 0 || len(c.find("rect.seg.f-seq2")) == 0 {
			t.Error("a part is not drawn")
		}
	})
	t.Run("the strip of key figures", func(t *testing.T) {
		type fig struct{ lab, v, sub string }
		var got []fig
		for _, f := range n.find("main div.figs div.fig") {
			got = append(got, fig{f.first("span.lab").text(), f.first("span.v").text(), f.first("span.s").text()})
		}
		want := []fig{
			{"Context added per session", "9,000 chars", "high · typical 2,100 chars · ≈ 2,250 tokens"},
			{"Context added per prompt that got memory", "1,500 chars", "high · typical 640 chars"},
			{"Time memory adds to a prompt", "1.9 s", "slowest 1 in 20 · typical 420 ms"},
			{"Time of a memory search", "1.9 s", "slowest 1 in 20 · typical 420 ms"},
			{"What the server's model spent (cost)", "$186.40", "server, all PCs · last 30 days"}}
		if !slices.Equal(got, want) {
			t.Errorf("strip %q, want %q", got, want)
		}
		// The strip precedes the cards.
		if st := n.first("div.figs"); st.Parent.Name != "main" || len(n.find("div.figs")) != 1 {
			t.Error("strip placement")
		}
	})
	t.Run("latency", func(t *testing.T) {
		c := cardOf(t, n, "w-recall-latency")
		if c.first("line.thr") == nil {
			t.Error("no limit line")
		}
		var limit string
		for _, x := range c.find("text.thr-t") {
			limit = x.text()
		}
		if limit != "limit 2.5 s" {
			t.Errorf("limit label %q", limit)
		}
		if got := len(c.find("polyline.s-latency")) + len(c.find("polyline.s-search-latency")); got != 2 {
			t.Errorf("%d lines, want one per metric", got)
		}
		if !strings.Contains(c.text(), "backsight doctor") && !hasCode(c, "backsight doctor") {
			t.Error("no advice for a slow memory")
		}
	})
	t.Run("extraction", func(t *testing.T) {
		c := cardOf(t, n, "w-extraction-tokens")
		var table *node
		for _, tb := range c.find("table.tbl") {
			if strings.HasPrefix(tb.text(), "Server task") {
				table = tb
			}
		}
		if table == nil {
			t.Fatal("no extraction table")
		}
		var rows []string
		for _, r := range table.find("tbody tr") {
			rows = append(rows, r.text())
		}
		want := []string{"saving notes 400 60 40 $3.50", "summarising 200 0 20 —", "all tasks 600 60 60 $186.40"}
		if !slices.Equal(rows, want) {
			t.Errorf("rows %q, want %q", rows, want)
		}
		if table.first("tr.total") == nil {
			t.Error("no total row")
		}
		// The days by task stay in the chart's own table: one day-by-task table only.
		if got := len(c.find("details.tblfb table")); got != 1 {
			t.Errorf("%d chart tables", got)
		}
		if c.first("figure.chart") == nil {
			t.Error("no chart of the tokens")
		}
	})
	t.Run("developer", func(t *testing.T) {
		dev := pageNode(t, "cost", true, func(d *PageData) { richCost(d.V["cost"]) })
		c := cardOf(t, dev, "w-extraction-per_session")
		if c.first("div.fig") == nil || !hasCode(c, "extraction.per_session") {
			t.Errorf("per-session card: %q", c.text())
		}
		if n.first("#w-extraction-per_session") != nil || n.first("#w-context-per_prompt") != nil {
			t.Error("the per-session card shows without developer mode")
		}
	})
	t.Run("a card without numbers says why", func(t *testing.T) {
		g := pageNode(t, "cost", false, nil)
		for _, id := range want {
			c := cardOf(t, g, id)
			if c.first("figure.chart") != nil || c.first("div.empty") == nil {
				t.Errorf("%s: a chart or no sentence without a number", id)
			}
		}
	})
}

func TestLinesOfMetrics(t *testing.T) {
	view := func(id string, series ...metrics.Point) MetricView {
		v, _ := metricOf(&metrics.Result{Metrics: map[string]metrics.Metric{id: {Status: metrics.StatusOK, Value: fp(0.5), Series: series}}}, id)
		return v
	}
	pt := func(day string, num, den int64) metrics.Point {
		return metrics.Point{Day: day, Value: fp(float64(num) / float64(den)), Num: ip(num), Den: ip(den)}
	}
	used := view("used.turn_rate", pt("2026-10-05", 2, 13), pt("2026-10-06", 1, 4))
	titles := func(h string) []string {
		var out []string
		for _, r := range parseHTML(t, h).find("rect.hit") {
			out = append(out, r.first("title").text())
		}
		return out
	}
	got := titles(string(linesOf(en, "Used", "", true, used)))
	if len(got) != 2 || !strings.HasPrefix(got[0], "Oct 5 · Used: 15 %") || !strings.HasSuffix(got[0], "turns: 13") || !strings.HasSuffix(got[1], "turns: 4") {
		t.Errorf("rate titles %q", got)
	}
	if !strings.Contains(string(linesOf(en, "Used", "", true, used)), ">100 %</text>") {
		t.Error("a rate is not drawn from 0 to 100 %")
	}
	if !strings.Contains(string(linesOf(en, "Used", "", true, used)), `class="band`) {
		t.Error("a rate has no interval band")
	}
	// The latency: the slowest 1 in 20 of the day, on a scale in time, not in percent.
	lat := view("recall.latency",
		metrics.Point{Day: "2026-10-05", N: ip(7), P50: fp(800), P95: fp(2300)}, metrics.Point{Day: "2026-10-06", N: ip(5), P50: fp(700), P95: fp(900)})
	out := string(latencyOf(en, "", lat))
	if got := titles(out); len(got) != 2 || !strings.Contains(got[0], "slowest 1 in 20: 2.3 s") || !strings.HasSuffix(got[0], "prompts: 7") {
		t.Errorf("latency titles %q", got)
	}
	if strings.Contains(out, "%") || strings.Contains(out, `class="band`) {
		t.Errorf("the latency axis is scaled as a rate or has a band: %.300s", out)
	}
	// A non-rate line is scaled to its own values.
	valued := view("recall.latency", metrics.Point{Day: "2026-10-05", Value: fp(2300), N: ip(7)}, metrics.Point{Day: "2026-10-06", Value: fp(900), N: ip(5)})
	scaled := string(linesOf(en, "Time", "", false, valued))
	if strings.Contains(scaled, "%") || strings.Contains(scaled, `class="band`) || !strings.Contains(scaled, ">2.5 s</text>") {
		t.Errorf("a non-rate line drawn as a rate (its top should be 2.5 s): %.300s", scaled)
	}
	// A metric with no days draws no line and says so.
	if got := string(linesOf(en, "Used", "", true, view("used.turn_rate"))); !strings.Contains(got, `class="empty"`) {
		t.Errorf("no days: %q", got)
	}
}

func TestDailyChartsAndTheFirstPartialDay(t *testing.T) {
	first := metrics.Point{Day: "2026-10-04", Value: fp(0.5), Num: ip(1), Den: ip(2), Partial: true}
	second := metrics.Point{Day: "2026-10-05", Value: fp(0.5), Num: ip(5), Den: ip(10)}
	v, _ := metricOf(&metrics.Result{Metrics: map[string]metrics.Metric{"recall.hit_rate": {Value: fp(0.5), Series: []metrics.Point{first, second}}}}, "recall.hit_rate")
	// A rate keeps the cut-short day: it is a value per event, and the faint bars show how few.
	if got := string(linesOf(en, "x", "", true, v)); !strings.Contains(got, "Oct 4 · Found") {
		t.Error("the line chart dropped the first, partial day")
	}
	// A count would dip: it is left out.
	parts := metrics.Metric{Value: fp(1), Series: []metrics.Point{
		{Day: "2026-10-04", Partial: true, Parts: map[string]int64{"briefing": 1, "prompts": 1}},
		{Day: "2026-10-05", Parts: map[string]int64{"briefing": 100, "prompts": 100}}}}
	pv, _ := metricOf(&metrics.Result{Metrics: map[string]metrics.Metric{"context.per_session": parts}}, "context.per_session")
	if got := string(partsOf(en, "x", "", pv, splitOrder, splitWords, []string{"f-seq1", "f-seq2"}, "")); strings.Contains(got, "Oct 4") || !strings.Contains(got, "Oct 5") {
		t.Errorf("parts chart: %.400s", got)
	}
	out := string(outcomesOf(en, "", []metrics.OutcomeDay{{Day: "2026-10-04", Used: 1, Partial: true}, {Day: "2026-10-05", Used: 9}}))
	if strings.Contains(out, "Oct 4") || !strings.Contains(out, "Oct 5") {
		t.Errorf("outcomes chart: %.400s", out)
	}
	// A window of one bucket keeps it.
	if out := string(outcomesOf(en, "", []metrics.OutcomeDay{{Day: "2026-10-05", Used: 9, Partial: true}})); !strings.Contains(out, "Oct 5") {
		t.Error("the only bucket was dropped")
	}
}

func TestXLabels(t *testing.T) {
	for key, want := range map[string]string{
		"2026-10-05": "Oct 5", "2026-10-05T15:00+02:00": "15:00", "2026-W40": "2026-W40", "": "",
	} {
		if got := xName(key); got != want {
			t.Errorf("xName(%q) = %q, want %q", key, got, want)
		}
	}
	hourly := metrics.Metric{Value: fp(0.5), Series: []metrics.Point{
		{Hour: "2026-10-05T14:00+02:00", Value: fp(0.5), Num: ip(1), Den: ip(2)}, {Hour: "2026-10-05T15:00+02:00", Value: fp(0.5), Num: ip(1), Den: ip(2)}}}
	v, _ := metricOf(&metrics.Result{Metrics: map[string]metrics.Metric{"recall.hit_rate": hourly}}, "recall.hit_rate")
	if got := string(linesOf(en, "x", "", true, v)); !strings.Contains(got, "14:00 · Found") || !strings.Contains(got, "15:00 · Found") {
		t.Errorf("hourly labels: %.500s", got)
	}
}

func TestExtractionOf(t *testing.T) {
	rows := []metrics.ExtractionRow{
		{Day: "2026-10-05", Operation: "retain", Input: 100, Output: 1}, {Day: "2026-10-05", Operation: "consolidation", Input: 10},
		{Day: "2026-10-05", Operation: "refresh_mental_model", Input: 20}, {Day: "2026-10-05", Operation: "batch_retain", Input: 400},
		{Day: "2026-10-05", Operation: "other", Input: 8}, {Day: "2026-10-06", Operation: "retain", Input: 7},
	}
	out := parseHTML(t, string(extractionOf(en, "", rows)))
	var legend []string
	for _, s := range out.find(".legend span") {
		legend = append(legend, s.text())
	}
	if w := []string{"saving notes", "summarising", "refreshing summaries", "other"}; !slices.Equal(legend, w) {
		t.Fatalf("legend %v, want %v", legend, w)
	}
	for i, class := range []string{"f-seq1", "f-seq2", "f-seq3", "f-neutral"} {
		if i3 := out.find(".legend i." + class); len(i3) != 1 {
			t.Errorf("part %d lacks class %s", i, class)
		}
	}
	// "other" holds the fourth task and the server's own "other": 400 + 8.
	var firstDay []string
	for _, td := range out.find("details.tblfb tbody tr")[0].find("td") {
		firstDay = append(firstDay, td.text())
	}
	if w := []string{"Oct 5", "101 tokens", "10 tokens", "20 tokens", "408 tokens"}; !slices.Equal(firstDay, w) {
		t.Errorf("first day %v, want %v", firstDay, w)
	}
}

// R29: the histogram is the metrics package's own value in process and a
// decoded map in a golden; both draw the same.
func TestHistogramReadsBothShapes(t *testing.T) {
	h := dataHarness(t)
	res, st, err := h.s.data.result(t.Context(), "cost", views.Params{Limit: 50}, "")
	if err != nil || st != stateReady {
		t.Fatalf("cost: state %d, %v", st, err)
	}
	m := res.Metrics["context.per_session"]
	hist, ok := m.Extra["histogram"]
	if _, isGeneric := hist.([]map[string]any); !ok || isGeneric {
		t.Fatalf("the metrics package no longer returns its own histogram type: %T", hist)
	}
	m.Value = fp(2100) // markers need the percentiles: give them as the metrics package does, typed
	m.Extra["p50"], m.Extra["p95"] = fp(1500), fp(2500)
	inProcess := MetricView{ID: "context.per_session", Def: mustDef(t, "context.per_session"), M: m, Found: true}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var decoded metrics.Metric
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, isMap := decoded.Extra["histogram"].([]any); !isMap {
		t.Fatalf("decoded histogram is %T", decoded.Extra["histogram"])
	}
	a, b := string(contextHistOf(en, "", inProcess)), string(contextHistOf(en, "", MetricView{ID: inProcess.ID, Def: inProcess.Def, M: decoded, Found: true}))
	if a != b {
		t.Errorf("the two shapes draw differently:\n%.600s\n%.600s", a, b)
	}
	if got := len(parseHTML(t, a).find("rect.f-seq1")); got < 20 || !strings.Contains(a, "typical 1.5 k chars") && !strings.Contains(a, "typical 1,500 chars") {
		t.Errorf("the histogram is not drawn (%d bars): %.400s", got, a)
	}
	// An absent percentile (a nil pointer in process, null decoded) draws no marker.
	m.Extra["p50"] = (*float64)(nil)
	inProcess.M = m
	if out := string(contextHistOf(en, "", inProcess)); strings.Contains(out, "typical") {
		t.Errorf("a marker for a missing percentile: %.400s", out)
	}
}

func mustDef(t *testing.T, id string) metrics.Def {
	t.Helper()
	d, ok := metrics.DefOf(id)
	if !ok {
		t.Fatalf("no metric %s", id)
	}
	return d
}

// polylineClasses are the classes of the card's lines.
func polylineClasses(c *node) []string {
	var out []string
	for _, p := range c.find("polyline") {
		out = append(out, p.attr("class"))
	}
	return out
}

func TestQualityLeadsAndLegends(t *testing.T) {
	n := pageNode(t, "quality", false, func(d *PageData) { richQuality(d.V["quality"]) })
	legend := func(c *node) []string {
		var out []string
		for _, s := range c.find("figure.chart .legend span") {
			out = append(out, s.text())
		}
		return out
	}
	leads := func(c *node) []string {
		var out []string
		for _, l := range c.find("div.row.lead div") {
			out = append(out, l.text())
		}
		return out
	}
	used := cardOf(t, n, "w-used-turn_rate")
	if w := []string{"Used", "Said what it used", "interval (95 %)", "faint bars: turns"}; !slices.Equal(legend(used), w) {
		t.Errorf("used legend %q, want %q", legend(used), w)
	}
	if got := leads(used); len(got) != 2 || !strings.HasPrefix(got[0], "62 % ") || !strings.HasSuffix(got[0], "used by Claude") ||
		!strings.HasPrefix(got[1], "90 % ") || !strings.HasSuffix(got[1], "said what it used") {
		t.Errorf("used leads %q", got)
	}
	// The second line of a pair is dashed; each line has its own class.
	if got := polylineClasses(used); !slices.Equal(got, []string{"ln s-used", "ln s-coverage ln-2"}) {
		t.Errorf("used lines %q", got)
	}
	search := cardOf(t, n, "w-search-rate")
	if w := []string{"Claude searched", "Searches that found nothing", "interval (95 %)", "faint bars: prompts"}; !slices.Equal(legend(search), w) {
		t.Errorf("search legend %q, want %q", legend(search), w)
	}
	if got := leads(search); len(got) != 2 || !strings.HasPrefix(got[0], "24 % ") || !strings.HasPrefix(got[1], "13 % ") || !strings.HasSuffix(got[1], "found nothing") {
		t.Errorf("search leads %q", got)
	}
	if got := polylineClasses(search); !slices.Equal(got, []string{"ln s-search", "ln s-empty ln-2"}) {
		t.Errorf("search lines %q", got)
	}
	if got := polylineClasses(cardOf(t, n, "w-recall-hit_rate")); !slices.Equal(got, []string{"ln s-hit"}) {
		t.Errorf("hit line %q", got)
	}
	if c := cardOf(t, n, "w-reported"); c.first("a[href=/bank]") == nil {
		t.Error("no way to review the reported memories")
	}
}

func TestOutcomeLegendClasses(t *testing.T) {
	out := parseHTML(t, string(outcomesOf(en, "", []metrics.OutcomeDay{{Day: "2026-10-05", Used: 1, Ignored: 1, NotDeclared: 1, ShownOnly: 1, Pending: 1}})))
	var got []string
	for _, i := range out.find(".legend i") {
		got = append(got, i.attr("class"))
	}
	if w := []string{"sw f-used", "sw f-ignored", "sw f-undeclared", "sw f-shown", "sw f-pending"}; !slices.Equal(got, w) {
		t.Errorf("legend swatches %q, want %q", got, w)
	}
}

func TestOutcomesFoot(t *testing.T) {
	n := pageNode(t, "quality", false, nil)
	var total int64
	for _, r := range goldenResult(t, "quality").Tables.OutcomeMix {
		total += r.Used + r.Ignored + r.NotDeclared + r.ShownOnly + r.Pending
	}
	if want := format3(total) + " shown"; !strings.Contains(cardOf(t, n, "w-outcomes").text(), want) {
		t.Errorf("outcomes foot lacks %q: %q", want, cardOf(t, n, "w-outcomes").text())
	}
	one := pageNode(t, "quality", false, func(d *PageData) {
		d.V["quality"].Tables.OutcomeMix = []metrics.OutcomeDay{{Day: "2026-10-05", Used: 1}}
	})
	if !strings.Contains(cardOf(t, one, "w-outcomes").text(), "1 memory shown") {
		t.Errorf("singular: %q", cardOf(t, one, "w-outcomes").text())
	}
}

func format3(n int64) string {
	if n == 1 {
		return "1 memory"
	}
	return en.Int(n) + " memories"
}

func TestRankingFollowsTheNoDataReason(t *testing.T) {
	n := pageNode(t, "quality", true, func(d *PageData) {
		for id, m := range d.V["quality"].Metrics {
			m.Status, m.Reason, m.Value, m.Num, m.Den, m.Series, m.Extra = metrics.StatusNoData, metrics.ReasonFeedbackOff, nil, nil, nil, nil, nil
			d.V["quality"].Metrics[id] = m
		}
		d.V["quality"].Tables.OutcomeMix = nil
	})
	c := cardOf(t, n, "w-ranking")
	if !strings.Contains(c.text(), "Memory feedback is off") || !hasCode(c, "feedback.enabled") {
		t.Errorf("ranking card: %q", c.text())
	}
	if strings.Contains(c.text(), "No turn used a ranked memory") || c.first("div.fig") != nil || c.first("figure.chart") != nil {
		t.Errorf("ranking card draws on no data: %q", c.text())
	}
	if !hasCode(c, "used.rank") {
		t.Error("the card names no metric in developer mode")
	}
}

// longen gives every series n days from 2026-01-01.
func longen(r *metrics.Result, n int) {
	day := func(i int) string {
		return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
	}
	for id, m := range r.Metrics {
		if len(m.Series) == 0 {
			continue
		}
		tpl := m.Series[len(m.Series)-1]
		m.Series = make([]metrics.Point, n)
		for i := range m.Series {
			m.Series[i] = tpl
			m.Series[i].Day, m.Series[i].Partial = day(i), false
		}
		r.Metrics[id] = m
	}
	if len(r.Tables.OutcomeMix) > 0 {
		tpl := r.Tables.OutcomeMix[len(r.Tables.OutcomeMix)-1]
		r.Tables.OutcomeMix = make([]metrics.OutcomeDay, n)
		for i := range r.Tables.OutcomeMix {
			r.Tables.OutcomeMix[i] = tpl
			r.Tables.OutcomeMix[i].Day, r.Tables.OutcomeMix[i].Partial = day(i), false
		}
	}
}

// Every chart tells where the rows it caps can be had: the page's command with --json.
func TestChartsNameTheCommandUnderACappedTable(t *testing.T) {
	check := func(name string, wantCharts int, edit func(d *PageData)) {
		n := pageNode(t, name, false, edit)
		more := 0
		for _, fig := range n.find("figure.chart") {
			if p := fig.first("p.tbl-more"); p != nil {
				more++
				if !hasCode(p, "backsight stats "+name+" --days 30 --json") {
					t.Errorf("%s: capped table points at %q", name, p.text())
				}
			}
		}
		if more != wantCharts {
			t.Errorf("%s: %d charts with a capped table, want %d", name, more, wantCharts)
		}
	}
	check("quality", 5, func(d *PageData) { richQuality(d.V["quality"]); longen(d.V["quality"], 130) })
	check("cost", 3, func(d *PageData) {
		richCost(d.V["cost"])
		longen(d.V["cost"], 130)
		d.V["cost"].Tables.Extraction = nil
		for i := 0; i < 130; i++ {
			d.V["cost"].Tables.Extraction = append(d.V["cost"].Tables.Extraction,
				metrics.ExtractionRow{Day: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02"), Operation: "retain", Input: 1})
		}
	})
}

func TestReportedHostileLinks(t *testing.T) {
	n := pageNode(t, "quality", false, func(d *PageData) {
		d.V["quality"].Tables.Reported = []metrics.ReportedRow{{MemoryID: "m<1>", Verdict: "wrong", Session: `ses/../y"z`, Project: "p/../q r?x=1", At: d.V["quality"].GeneratedAt}}
	})
	row := cardOf(t, n, "w-reported").first("tbody tr")
	cells := row.find("td")
	if got := cells[2].first("a").attr("href"); got != "/projects/p%2F..%2Fq%20r%3Fx=1" {
		t.Errorf("project href %q", got)
	}
	if got := cells[3].first("a").attr("href"); got != "/sessions/ses%2F..%2Fy%22z" {
		t.Errorf("session href %q", got)
	}
	if got := cells[0].first("a").attr("href"); got != "/memories/m%3C1%3E" {
		t.Errorf("memory href %q", got)
	}
}

func TestExtractionTableIsCappedAndTotalled(t *testing.T) {
	n := pageNode(t, "cost", false, func(d *PageData) {
		richCost(d.V["cost"])
		d.V["cost"].Tables.Extraction = nil
		for i := 0; i < 130; i++ {
			d.V["cost"].Tables.Extraction = append(d.V["cost"].Tables.Extraction,
				metrics.ExtractionRow{Day: "2026-10-05", Operation: "task" + en.Int(int64(i)), Input: 2, Output: 1})
		}
	})
	c := cardOf(t, n, "w-extraction-tokens")
	var table *node
	for _, tb := range c.find("table.tbl") {
		if strings.HasPrefix(tb.text(), "Server task") {
			table = tb
		}
	}
	if got := len(table.find("tbody tr.total")); got != 1 {
		t.Fatalf("%d total rows", got)
	}
	if got := len(table.find("tbody tr")); got != 121 {
		t.Errorf("%d rows, want 120 tasks and the total", got)
	}
	if !strings.Contains(table.first("tr.total").text(), "all tasks 260 0 130") {
		t.Errorf("total over all 130 tasks: %q", table.first("tr.total").text())
	}
	if p := c.first("p.tbl-more"); p == nil || !strings.Contains(p.text(), "Showing 120 of 130 tasks") {
		t.Errorf("cap note %+v", p)
	}
	if got := len(table.find("th.r")); got != 4 {
		t.Errorf("%d right-aligned headers", got)
	}
	if n.first("main a[href=/pipeline]") == nil {
		t.Error("no link to the crashed steps")
	}
}

func TestLatencyLimitBelongsToRecall(t *testing.T) {
	pt := func(day string, p95 float64) metrics.Point {
		return metrics.Point{Day: day, N: ip(3), P95: fp(p95)}
	}
	mk := func(id string) MetricView {
		v, _ := metricOf(&metrics.Result{Metrics: map[string]metrics.Metric{id: {Value: fp(1), Series: []metrics.Point{pt("2026-10-05", 2300), pt("2026-10-06", 900)}}}}, id)
		return v
	}
	only := parseHTML(t, string(latencyOf(en, "", mk("search.latency"))))
	if only.first("line.thr") != nil {
		t.Error("recall's limit drawn over the search line alone")
	}
	if got := only.first("rect.hit").first("title").text(); !strings.HasSuffix(got, "searches: 3") {
		t.Errorf("bars of a search chart: %q", got)
	}
	both := parseHTML(t, string(latencyOf(en, "", mk("search.latency"), mk("recall.latency"))))
	if both.first("line.thr") == nil {
		t.Error("recall's limit missing")
	}
	// Only the percentile of a day is missing: the day's value stands in.
	v, _ := metricOf(&metrics.Result{Metrics: map[string]metrics.Metric{"recall.latency": {Value: fp(1), Series: []metrics.Point{
		{Day: "2026-10-05", Value: fp(1500)}, {Day: "2026-10-06", Value: fp(800)}}}}}, "recall.latency")
	if got := string(latencyOf(en, "", v)); !strings.Contains(got, "slowest 1 in 20: 1.5 s") {
		t.Errorf("value fallback: %.300s", got)
	}
}

func TestLongLatencyLinesMergeByMax(t *testing.T) {
	// The merged positions' sentences are what is read: ask the chart for them past its hover limit.
	oldHover := chart.HoverMax
	chart.HoverMax = 1 << 30
	t.Cleanup(func() { chart.HoverMax = oldHover })
	var ps []metrics.Point
	for i := 0; i < 1000; i++ {
		v := 100.0
		if i%2 == 1 {
			v = 900
		}
		ps = append(ps, metrics.Point{Day: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02"), Value: fp(v), N: ip(1)})
	}
	v, _ := metricOf(&metrics.Result{Metrics: map[string]metrics.Metric{"recall.latency": {Value: fp(1), Series: ps}}}, "recall.latency")
	title := parseHTML(t, string(linesOf(en, "x", "", false, v))).first("rect.hit").first("title").text()
	if !strings.Contains(title, "900 ms") {
		t.Errorf("merged positions are not the slowest of them: %q", title)
	}
}

func TestHistogramLabelsAndEmptyOther(t *testing.T) {
	n := pageNode(t, "cost", false, func(d *PageData) { richCost(d.V["cost"]) })
	bars := cardOf(t, n, "w-context-per_session").find("rect.f-seq1")
	first, last := bars[0].first("title").text(), bars[len(bars)-1].first("title").text()
	if !strings.HasPrefix(first, "0–1 k: ") || !strings.HasPrefix(last, "20 k+: ") {
		t.Errorf("bin labels %q … %q", first, last)
	}
	only := parseHTML(t, string(extractionOf(en, "", []metrics.ExtractionRow{{Day: "2026-10-05", Operation: "other", Input: 5}})))
	var legend []string
	for _, s := range only.find(".legend span") {
		legend = append(legend, s.text())
	}
	if !slices.Equal(legend, []string{"other"}) {
		t.Errorf("a server task named other: legend %q", legend)
	}
}

func TestOutcomePathsAreInPathOrder(t *testing.T) {
	rows := []metrics.OutcomeDay{{Day: "2026-10-05", ByPath: map[string]metrics.OutcomeDay{
		"search": {Used: 1}, "prompt": {Used: 1}, "gate": {Used: 1}, "briefing": {Used: 1}, "read": {Used: 1}}}}
	want := []string{"at session start", "before a question", "added to the prompt", "opened by Claude", "Claude's search"}
	for i := 0; i < 30; i++ {
		var got []string
		for _, c := range outcomePaths(en, "", rows) {
			got = append(got, c.Name)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("run %d: %q, want %q", i, got, want)
		}
	}
}

// costDay is the day i (0 is Oct 1) of the design's 30-day window.
func costDay(i int) string {
	return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
}

// allPCsCost is the Cost view of B8Cost added up over the four PCs: 1,468 prompts
// on three PCs, the slowest 1 in 20 between 1.0 and 2.0 s (2.0 and 2.5 s on Oct 13
// to 15), this PC's exact values as the design draws them, the context as an
// average of 2,300 characters over 311 sessions beside this PC's high value of
// 3,600, and the By PC table of the design.
func allPCsCost(r *metrics.Result) {
	richCost(r)
	r.GeneratedAt = scopeNow
	r.Scope.Machines, r.Scope.PCs = "all", b8PCs(3)
	give(r, "context.per_session", func(m *metrics.Metric) {
		m.Value, m.Den, m.Num = fp(2300), ip(311), ip(715300)
		m.Tags = []string{"status_this_pc", "average"}
		m.Extra["p50"], m.Extra["p95"], m.Extra["average"] = 2100.0, 3600.0, 2300.0
	})
	own := []float64{1500, 1600, 1400, 1500, 1700, 1600, 1500, 1400, 1600, 1700, 1500, 1600, 1900, 1900, 1800, 1600, 1500, 1400, 1500, 1600, 1700, 1600, 1500, 1500, 1400, 1600, 1700, 1500, 1500, 1600}
	give(r, "recall.latency", func(m *metrics.Metric) {
		m.Value, m.Den = fp(2000), ip(1468)
		m.Tags = []string{"approximate"}
		m.Extra = map[string]any{"p50": 840.0, "p95": 1600.0, "range_ms": rangeMS(1000, i64p(2000)), "p50_range_ms": []any{500.0, 1000.0}}
		m.Series = nil
		for i := range own {
			lo, hi := 1000.0, 2000.0
			if i >= 12 && i <= 14 {
				lo, hi = 2000, 2500
			}
			m.Series = append(m.Series, metrics.Point{Day: costDay(i), N: ip(int64(41 + i)), Lo: fp(lo), Hi: fp(hi), ThisP95: fp(own[i])})
		}
	})
	r.Tables.ByPC = []metrics.ByPCRow{
		{Machine: "3f9c2a17", This: true, OS: "linux", Prompts: 676, P95: fp(1600), P50: fp(840), Until: ptrS("2026-10-29"), Active: true},
		{Machine: "a07be4d1", OS: "darwin", Prompts: 612, P95RangeMS: rangeMS(2000, i64p(2500)), P50RangeMS: rangeMS(500, i64p(1000)), Until: ptrS("2026-10-29"), Active: true},
		{Machine: "c84e1f39", OS: "linux", Prompts: 180, P95RangeMS: rangeMS(1000, i64p(2000)), P50RangeMS: rangeMS(500, i64p(1000)), Until: ptrS("2026-10-21"), Stale: true, Active: true},
		{Machine: "5d21c8e0", OS: "windows", Prompts: 0, Until: ptrS("2026-10-29")},
	}
}

// thisPCOwn is this PC's own answer to the Cost view: 676 recall prompts.
func thisPCOwn() *metrics.Result {
	r := &metrics.Result{Metrics: map[string]metrics.Metric{"recall.latency": {Den: ip(676)}}}
	return r
}

func allPCsCostNode(t *testing.T, edit func(d *PageData)) *node {
	t.Helper()
	return pageNode(t, "cost", false, func(d *PageData) {
		allPCsCost(d.V["cost"])
		d.P.Machines = "all"
		d.CLI = "backsight stats cost --days 30"
		d.Own = thisPCOwn()
		if edit != nil {
			edit(d)
		}
	})
}

func TestCostLatencyCardAllPCs(t *testing.T) {
	n := allPCsCostNode(t, nil)
	c := cardOf(t, n, "w-recall-latency")
	if h := c.first("h2").text(); h != "How long does memory make a prompt wait?" {
		t.Errorf("head %q", h)
	}
	chip := c.first("span.chip")
	if chip == nil || chip.text() != "all PCs (4)" || chip.attr("title") != "Added up from the 4 PCs that share their activity on this server" {
		t.Errorf("chip %v", chip)
	}
	if c.first("span.chip .i-pcs") == nil || len(c.find("span.chip")) != 1 {
		t.Error("the chip is not the one all PCs chip")
	}
	// The ⓘ says what, the limits and what the figure rests on.
	info := c.first("details.info .pop")
	if info == nil {
		t.Fatal("no ⓘ")
	}
	var dts, dds []string
	for _, x := range info.find("dt") {
		dts = append(dts, x.text())
	}
	for _, x := range info.find("dd") {
		dds = append(dds, x.text())
	}
	if !slices.Equal(dts, []string{"What", "Limits", "Based on"}) {
		t.Errorf("ⓘ terms %v", dts)
	}
	if len(dds) != 3 || !strings.HasPrefix(dds[0], "Per day, the slowest 1 in 20 prompts across the PCs.") ||
		dds[1] != "Watch over 2.0 s, attention over 2.5 s. Both are bucket edges, so the status is exact." || dds[2] != "1,468 prompts on 3 PCs" {
		t.Errorf("ⓘ text %q", dds)
	}
	if got := c.first("details.cli code").text(); got != "backsight stats cost --days 30 --machines all" {
		t.Errorf("CLI %q", got)
	}
	// The chart: a bar and a ring per day, the limits, the design's legend.
	fig := c.first("figure.chart")
	if len(fig.find("rect.bkt")) != 30 || len(fig.find("circle.bkt-own[cx]")) != 30 || len(fig.find("rect.hit")) != 30 {
		t.Errorf("%d bars, %d rings, %d hover rects", len(fig.find("rect.bkt")), len(fig.find("circle.bkt-own[cx]")), len(fig.find("rect.hit")))
	}
	if fig.first("line.thr-w") == nil || fig.first("line.thr") == nil {
		t.Error("the watch or limit line is missing")
	}
	var legend []string
	for _, s := range fig.find("div.legend span") {
		legend = append(legend, s.text())
	}
	if w := []string{"slowest 1 in 20, all PCs: between two bucket edges", "this PC alone (exact)", "watch 2.0 s", "limit 2.5 s"}; !slices.Equal(legend, w) {
		t.Errorf("legend %q, want %q", legend, w)
	}
	if hit := fig.find("rect.hit")[12].first("title").text(); hit != "Oct 13: 53 prompts, all PCs; slowest 1 in 20 between 2.0 and 2.5 s; this PC alone 1.9 s" {
		t.Errorf("hover %q", hit)
	}
	// The table of the chart: the design's four columns, a row per day.
	tbl := fig.first("details.tblfb table.tbl")
	var head []string
	for _, th := range tbl.find("thead th") {
		head = append(head, th.text())
	}
	if w := []string{"Day", "Prompts, all PCs", "Slowest 1 in 20, all PCs", "This PC alone"}; !slices.Equal(head, w) {
		t.Errorf("columns %q, want %q", head, w)
	}
	rows := tbl.find("tbody tr")
	if len(rows) != 30 || rows[0].text() != "Oct 1 41 1.0–2.0 s 1.5 s" || rows[12].text() != "Oct 13 53 2.0–2.5 s 1.9 s" {
		t.Errorf("%d rows, first %q, Oct 13 %q", len(rows), rows[0].text(), rows[12].text())
	}
	// Nothing of this PC's old card: no search line, no lead, no advice.
	if c.first("div.lead") != nil || len(c.find("polyline")) != 0 || hasCode(c, "backsight doctor") {
		t.Error("the card keeps the old lines, lead or advice")
	}
}

func TestByPCDisclosure(t *testing.T) {
	n := allPCsCostNode(t, nil)
	c := cardOf(t, n, "w-recall-latency")
	by := c.find("figure.chart > details.tblfb")
	if len(by) != 2 || by[1].first("summary").text() != "By PC" {
		t.Fatalf("%d disclosures in the figure", len(by))
	}
	tbl := by[1].first("table.tbl.mach")
	var head []string
	for _, th := range tbl.find("thead th") {
		head = append(head, th.text())
	}
	if w := []string{"PC", "Prompts", "Slowest 1 in 20", "Typical"}; !slices.Equal(head, w) {
		t.Errorf("columns %q", head)
	}
	type row struct{ id, who, prompts, slow, typical string }
	var got []row
	for _, tr := range tbl.find("tbody tr") {
		td := tr.find("td")
		id := td[0].first("span.idc").text()
		got = append(got, row{id, strings.TrimPrefix(td[0].text(), id), td[1].text(), td[2].text(), td[3].text()})
	}
	want := []row{
		{"3f9c2a17", "Linux · this PC", "676", "1.6 s", "840 ms"},
		{"a07be4d1", "macOS", "612", "2.0–2.5 s", "500 ms–1.0 s"},
		{"c84e1f39", "Linux · up to Oct 21", "180", "1.0–2.0 s", "500 ms–1.0 s"},
		{"5d21c8e0", "Windows", "0", "no activity in this window", "—"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows %q\nwant %q", got, want)
	}
	idle := tbl.find("tbody tr")[3].find("td")
	if idle[2].attr("class") != "muted" || idle[3].attr("class") != "muted" {
		t.Error("an idle PC's cells are not muted")
	}
	// The link names this PC's own prompts, and the foot says why others have none.
	foot := c.first("div.card-foot")
	if a := foot.first("a"); a == nil || a.text() != "This PC’s 676 prompts →" {
		t.Errorf("link %v", a)
	}
	if m := foot.first("span.muted"); m == nil || m.text() != "Other PCs send counts per bucket, not prompts: their events stay on them." {
		t.Errorf("foot note %v", m)
	}
	// Without this PC's own answer the link does not invent a count.
	n2 := allPCsCostNode(t, func(d *PageData) { d.Own = nil })
	if a := cardOf(t, n2, "w-recall-latency").first("div.card-foot a"); a == nil || a.text() != "This PC’s events →" {
		t.Errorf("link without own: %v", a)
	}
	// Hostile text in a row never reaches the page as markup.
	h := allPCsCostNode(t, func(d *PageData) { d.V["cost"].Tables.ByPC[1].Machine = `<i>x</i>` })
	if got := h.find("table.mach td span.idc")[1].text(); got != "<i>x</i>" {
		t.Errorf("id %q", got)
	}
}

func TestCostCardsThatStayThisPC(t *testing.T) {
	n := allPCsCostNode(t, nil)
	// The context histogram: chip this PC, the command with no flag, the design's sentence with the all PCs average.
	c := cardOf(t, n, "w-context-per_session")
	if chip := c.first("span.chip"); chip == nil || chip.text() != "this PC" {
		t.Errorf("context chip %v", chip)
	}
	if got := c.first("details.cli code").text(); got != "backsight stats cost --days 30" {
		t.Errorf("context CLI %q", got)
	}
	st := c.first("div.state.st-few")
	if st == nil || st.text() != "This chart is this PC’s: other PCs send a total and a count, not sizes. All PCs: 2,300 characters per session on average, over 311 sessions." {
		t.Errorf("state %v", st)
	}
	if b := st.first("b"); b == nil || b.text() != "2,300" {
		t.Errorf("the average is not in bold: %v", b)
	}
	// The card's own number is this PC's high value, not the average.
	if lead := c.first("div.lead").text(); !strings.Contains(lead, "3,600 chars") || strings.Contains(lead, "2,300") {
		t.Errorf("lead %q", lead)
	}
	// Where the context is added, and the server's cost: this PC / server chips and the design's lines.
	for id, want := range map[string][2]string{
		"w-context-split":     {"this PC", "This PC’s only, with all PCs too: other PCs do not send where context was added."},
		"w-extraction-tokens": {"server", "The server’s own figures, already every PC’s: the same with this PC or all PCs."},
	} {
		c := cardOf(t, n, id)
		if chip := c.first("span.chip"); chip == nil || chip.text() != want[0] {
			t.Errorf("%s chip %v", id, chip)
		}
		if p := c.first("p.xs.muted"); p == nil || p.text() != want[1] {
			t.Errorf("%s line %v", id, p)
		}
	}
	// The server's cost is unchanged: the same figure and table with this PC or all PCs.
	table := func(n *node) string {
		return cardOf(t, n, "w-extraction-tokens").first("table.tbl").text()
	}
	this := pageNode(t, "cost", false, func(d *PageData) { richCost(d.V["cost"]) })
	if a, b := table(n), table(this); a != b || a == "" {
		t.Errorf("the server's table differs: %q vs %q", a, b)
	}
}

func TestCostStripAllPCs(t *testing.T) {
	n := allPCsCostNode(t, nil)
	type fig struct{ lab, icon, v, sub string }
	var got []fig
	for _, f := range n.find("main div.figs div.fig") {
		icon := ""
		if i := f.first("span.lab span.i"); i != nil {
			icon = strings.TrimPrefix(strings.Fields(i.attr("class"))[2], "i-")
		}
		got = append(got, fig{f.first("span.lab").text(), icon, f.first("span.v").text(), f.first("span.s").text()})
	}
	want := []fig{
		{"Context added per session", "pcs", "2,300 chars", "average, all PCs · this PC’s high value 3,600"},
		{"Context added per prompt that got memory", "pc", "1,500 chars", "high · typical 640 chars · this PC"},
		{"Time memory adds to a prompt", "pcs", "1.0–2.0 s", "approximate · slowest 1 in 20, all PCs"},
		{"Time of a memory search", "pc", "1.9 s", "slowest 1 in 20 · this PC"},
		{"What the server's model spent (cost)", "server", "$186.40", "server, all PCs · last 30 days"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("strip %q\nwant %q", got, want)
	}
	if f := n.find("main div.figs div.fig")[2].first("span.s span.approx"); f == nil || f.text() != "approximate" {
		t.Error("the word approximate is not set apart")
	}
}

// With this PC selected, /cost is what it was: none of the all PCs markup.
func TestCostThisPCIsUnchanged(t *testing.T) {
	render := func(rich bool, edit func(d *PageData)) string {
		return render(t, "cost", func(d *PageData) {
			d.L, d.P = en, views.Params{Days: 30, DaysSet: true}
			d.CLI = "backsight stats cost --days 30"
			d.Frame.Window, d.Frame.Prefs.Dev = "last 30 days", false
			if rich {
				richCost(d.V["cost"])
			}
			if edit != nil {
				edit(d)
			}
		})
	}
	plain := render(true, nil)
	// A this-PC result that carries the scope of the PCs, and an own answer, draws the same bytes.
	same := render(true, func(d *PageData) {
		d.V["cost"].Scope.PCs = b8PCs(3)
		d.Own = thisPCOwn()
	})
	if plain != same {
		t.Error("this PC's page changes when the PCs are only in the scope")
	}
	for _, s := range []string{"all PCs (", "bkt", "By PC", "i-pcs", `class="approx"`, `class="state`, "Other PCs send counts", "with all PCs too"} {
		if strings.Contains(plain, s) {
			t.Errorf("this PC's page carries %q", s)
		}
	}
	// The main of this PC's page is the bytes block 7 drew (hashes taken from the page before block 8).
	// To re-pin after a change of this PC's page that is meant (a word, a class), run
	//
	//	go test ./internal/dashboard/ -run TestCostThisPCIsUnchanged
	//
	// and copy the "sha256 <got>" each failure prints into the matching pin below
	// (the first is the rich page, the second the empty one). A change nobody meant is
	// the bug: read the page's diff first, as this PC's page must not move with block 8.
	pin := func(body, want string) {
		t.Helper()
		i, j := strings.Index(body, "<main"), strings.Index(body, "</main>")
		if i < 0 || j < i {
			t.Fatal("no main")
		}
		if sum := fmt.Sprintf("%x", sha256.Sum256([]byte(body[i:j+len("</main>")]))); sum != want {
			t.Errorf("the main of this PC's page changed: sha256 %s, want %s", sum, want)
		}
	}
	pin(plain, "55b9c03126f9366c8fdcc9fd847fe5ee9f07c7ad3a5ee4bc35372850458a8bd9")
	pin(render(false, nil), "4ef72f7269615a3f05c94cd28782f65e9b65d6a1d035bc42bdc02864b75ae7c0")
	// The latency card of this PC's page is still two lines and the limit.
	n := parseHTML(t, plain)
	if got := len(cardOf(t, n, "w-recall-latency").find("polyline")); got != 2 {
		t.Errorf("%d lines", got)
	}
}

// A window drawn by the hour keeps this PC's series: the card draws lines, keeps the PCs below and says so.
func TestCostLatencyByTheHour(t *testing.T) {
	n := allPCsCostNode(t, func(d *PageData) {
		m := d.V["cost"].Metrics["recall.latency"]
		for i := range m.Series {
			m.Series[i].Lo, m.Series[i].Hi, m.Series[i].ThisP95 = nil, nil, nil
		}
		d.V["cost"].Metrics["recall.latency"] = m
	})
	c := cardOf(t, n, "w-recall-latency")
	if len(c.find("rect.bkt")) != 0 || len(c.find("polyline")) == 0 {
		t.Error("lines expected when no day has a bucket")
	}
	if c.first("table.mach") == nil {
		t.Error("the PCs one by one are gone")
	}
	if p := c.first("p.xs.muted"); p == nil || !strings.Contains(p.text(), "this PC’s figures") {
		t.Errorf("note %v", p)
	}
}

// The day page on all PCs names this PC's own counts in its merged tiles; without
// this PC's own answer (a project page never has one) they say "events".
func TestDayTilesNameThisPCsEvents(t *testing.T) {
	day := func(own *metrics.Result) map[string]string {
		n := pageNode(t, "day", false, func(d *PageData) {
			r := d.V["days"]
			r.Scope.Machines, r.Scope.PCs = "all", b8PCs(3)
			d.P.Machines = "all"
			d.Own = own
		})
		links := map[string]string{}
		for _, tile := range n.find("article.tile") {
			links[tile.first("h3").text()] = tile.first("div.tile-foot a").text()
		}
		return links
	}
	golden := goldenResult(t, "day-page")
	with := day(golden)
	found := golden.Metrics["recall.hit_rate"]
	if found.Den == nil {
		t.Fatal("the golden day has no prompts to count")
	}
	want := "This PC’s " + en.Int(*found.Den) + " prompts →"
	if *found.Den == 1 {
		want = "This PC’s 1 prompt →"
	}
	if with["Found"] != want {
		t.Errorf("Found link with own %q, want %q (all %v)", with["Found"], want, with)
	}
	if without := day(nil); without["Found"] != "This PC’s events →" {
		t.Errorf("Found link without own %q", without["Found"])
	}
}

// The longest window on all PCs, with eight other PCs: the page stays under the size cap, no chart over
// the marks cap, and the latency chart keeps its bars (one a day) without hover sentences.
func TestCostAllPCsAtTheLongestWindow(t *testing.T) {
	body := render(t, "cost", func(d *PageData) {
		d.L, d.P = en, views.Params{Days: 395, DaysSet: true, Machines: "all"}
		d.CLI = "backsight stats cost --days 395"
		d.Frame.Window, d.Frame.Prefs.Dev = "last 395 days", false
		d.Own = thisPCOwn()
		r := d.V["cost"]
		allPCsCost(r)
		longen(r, 395)
		pcs := b8PCs(9)
		pcs.Count = 9
		r.Scope.PCs = pcs
		for i := 0; i < 8; i++ {
			r.Tables.ByPC = append(r.Tables.ByPC, metrics.ByPCRow{Machine: fmt.Sprintf("%08x", 0xa0b1c2d3+i), OS: "linux", Prompts: int64(100 + i),
				P95RangeMS: rangeMS(1000, i64p(2000)), P50RangeMS: rangeMS(250, i64p(500)), Until: ptrS("2026-10-29"), Active: true})
		}
	})
	if len(body) > sizeCap {
		t.Errorf("the page is %d bytes, cap %d", len(body), sizeCap)
	}
	t.Logf("page: %d bytes", len(body))
	n := parseHTML(t, body)
	for i, f := range n.find("figure.chart") {
		if marks := len(f.find("title")); marks > chart.MaxMarks {
			t.Errorf("chart %d holds %d marks, cap %d", i, marks, chart.MaxMarks)
		}
	}
	fig := cardOf(t, n, "w-recall-latency").first("figure.chart")
	if bars, rings := len(fig.find("rect.bkt")), len(fig.find("circle.bkt-own[cx]")); bars != 395 || rings != 395 {
		t.Errorf("%d bars and %d rings, want one of each a day", bars, rings)
	}
	if len(fig.find("rect.hit")) != 0 || len(fig.first("details.tblfb table").find("tbody tr")) != chart.MaxTableRows {
		t.Error("a long window has no hover rects and a capped table")
	}
	if rows := len(fig.find("table.mach tbody tr")); rows != 12 {
		t.Errorf("%d By PC rows, want 12 (the design's 4 and 8 more)", rows)
	}
	t.Logf("latency chart: %d titles, %d elements drawn", len(fig.find("title")), len(fig.find("rect"))+len(fig.find("circle"))+len(fig.find("line")))
}

// On a real index with three other PCs, /cost and the day page on all PCs read this PC's own answer for
// their links ("This PC’s N prompts →"), as the Overview does; the By PC rows start with this PC.
func TestAllPCsPagesReadThisPCsOwnAnswer(t *testing.T) {
	h := othersHarness(t)
	hdr := cookieHdr(h.login())
	cost := rawPage(t, h, hdr, "/cost?machines=all")
	c := cardOf(t, cost, "w-recall-latency")
	if chip := c.first("span.chip"); chip == nil || chip.text() != "all PCs (4)" {
		t.Fatalf("chip %v", chip)
	}
	rows := c.find("table.mach tbody tr")
	if len(rows) != 4 || !strings.HasSuffix(rows[0].find("td")[0].text(), "this PC") {
		t.Fatalf("%d By PC rows, the first %q", len(rows), rows[0].text())
	}
	// This PC alone, its own count: the same one the page with this PC selected rests on.
	this := rawPage(t, h, hdr, "/cost?machines=this")
	based := cardOf(t, this, "w-recall-latency").first("details.info .pop").text()
	m := regexp.MustCompile(`Based on ([\d,]+) prompts`).FindStringSubmatch(based)
	if m == nil {
		t.Fatalf("no count in the ⓘ of this PC's card: %q", based)
	}
	if link := c.first("div.card-foot a"); link == nil || link.text() != "This PC’s "+m[1]+" prompts →" {
		t.Errorf("link %v, want %s prompts", link, m[1])
	}
	days := rawPage(t, h, hdr, "/days?machines=all")
	var href string
	for _, a := range days.find("a.hm-c") {
		if strings.HasPrefix(a.attr("href"), "/days/20") {
			href = a.attr("href")
			break
		}
	}
	if href == "" {
		t.Fatal("no day on the calendar")
	}
	if !strings.Contains(href, "machines=all") {
		t.Fatalf("the day link drops the switch: %s", href)
	}
	day := rawPage(t, h, hdr, href)
	found := false
	for _, tile := range day.find("article.tile") {
		if a := tile.first("div.tile-foot a"); a != nil && strings.HasPrefix(a.text(), "This PC’s ") {
			found = found || tile.first("h3").text() == "Found"
			if strings.Contains(a.text(), "events") && tile.first("h3").text() == "Found" {
				t.Errorf("the day's Found link has no count: %q", a.text())
			}
		}
	}
	if !found {
		t.Error("the day page's merged tiles do not name this PC's events")
	}
}

// This PC before it has a machine id is drawn as Which PCs draws it, a dash
// that says why, not an empty chip.
func TestByPCWithoutAMachineIDDrawsNoEmptyChip(t *testing.T) {
	n := allPCsCostNode(t, func(d *PageData) { d.V["cost"].Tables.ByPC[0].Machine = "" })
	tbl := cardOf(t, n, "w-recall-latency").find("table.tbl.mach")[0]
	first := tbl.find("tbody tr")[0].first("span.idc")
	if first == nil || first.text() != "—" || first.attr("title") != "This PC has no machine id yet" {
		t.Errorf("first id chip %v", first)
	}
	for _, id := range tbl.find("span.idc") {
		if strings.TrimSpace(id.text()) == "" {
			t.Errorf("an empty id chip: %v", id)
		}
	}
}

// A bucket reads the same in the hover, the table and the CLI, the open one
// included ("over 5.0 s", never "over 5 s").
func TestBucketWordsAreTheTablesAndTheCLIs(t *testing.T) {
	i := func(v int64) *int64 { return &v }
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		lo      float64
		hi      *float64
		between string
	}{
		{1000, f(2000), "between 1.0 and 2.0 s"},
		{250, f(500), "between 250 and 500 ms"},
		{5000, nil, "over 5.0 s"},
	} {
		between, cell := bucketWordsIn(format.Locale{})(c.lo, c.hi)
		var hi *int64
		if c.hi != nil {
			hi = i(int64(*c.hi))
		}
		if want := views.RangeText(int64(c.lo), hi); cell != want {
			t.Errorf("cell %q, want the CLI's %q", cell, want)
		}
		if between != c.between {
			t.Errorf("hover %q, want %q", between, c.between)
		}
	}
}

// Under a comma locale the buckets keep the page's marks, the open one and the
// shared unit included, in the tile, the chart's words and the By PC table:
// "over 5,0 s", never "over 5.0 s" beside "limit 2,5 s".
func TestRangeTextsFollowThePagesLocale(t *testing.T) {
	fr := format.FromAcceptLanguage("fr")
	if got := fr.Range(500, i64p(1000)); got != "500 ms–1,0 s" {
		t.Errorf("range %q", got)
	}
	if got := fr.Range(5000, nil); got != "over 5,0 s" {
		t.Errorf("open range %q", got)
	}
	if got := fr.Range(1000, i64p(2000)); got != "1,0–2,0 s" {
		t.Errorf("shared unit %q", got)
	}
	if got, en := fr.Range(250, i64p(500)), (format.Locale{}).Range(250, i64p(500)); got != "250–500 ms" || got != en {
		t.Errorf("ms range %q, English %q", got, en)
	}
	f := func(v float64) *float64 { return &v }
	between, cell := bucketWordsIn(fr)(1000, f(2000))
	if between != "between 1,0 and 2,0 s" || cell != "1,0–2,0 s" {
		t.Errorf("chart words %q, %q", between, cell)
	}

	// The overview with all PCs, in French: the Speed tile and its typical range.
	own := scenarioNamed(t, "healthy")
	r := allPCsOverview(t)
	speedAs(r, metrics.StatusBad, 5000, 2000, 5000, nil, []any{500.0, 1000.0})
	var tl Tile
	for _, g := range tilesOf(r, allPCsPage, fr, r.Window.Loc, false, own) {
		for _, x := range g.Tiles {
			if x.Name == "Speed" {
				tl = x
			}
		}
	}
	if tl.Big != "over 5,0" || tl.Unit != "s" || !strings.Contains(tl.Sub, "typical 500 ms–1,0 s") || !strings.Contains(tl.Sub, "limit 2,5 s") {
		t.Errorf("Speed tile %q %q, sub %q", tl.Big, tl.Unit, tl.Sub)
	}
	art := renderAllPCsIn(t, fr, r, own, nil, allPCsPage).find("article.tile")[4]
	if big := art.first("span.big").text(); big != "over 5,0" {
		t.Errorf("drawn value %q", big)
	}
	if !strings.Contains(art.text(), "typical 500 ms–1,0 s") {
		t.Errorf("drawn tile %q", art.text())
	}
}

// The Cost page in French, all PCs: the card's figure, the By PC table and the
// detail line write their buckets with the comma, as "limit 2,5 s" does.
func TestCostPageBucketsFollowTheLocale(t *testing.T) {
	fr := format.FromAcceptLanguage("fr")
	n := allPCsCostNode(t, func(d *PageData) { d.L = fr })
	var figs []string
	for _, f := range n.find("div.fig") {
		figs = append(figs, f.text())
	}
	if all := strings.Join(figs, "|"); !strings.Contains(all, "1,0–2,0 s") || strings.Contains(all, "1.0") {
		t.Errorf("Speed figure not in the locale: %q", all)
	}
	c := cardOf(t, n, "w-recall-latency")
	var cells []string
	for _, td := range c.find("table.tbl.mach td") {
		cells = append(cells, td.text())
	}
	if all := strings.Join(cells, "|"); !strings.Contains(all, "2,0–2,5 s|500 ms–1,0 s") || strings.Contains(all, "1.0") {
		t.Errorf("By PC table not in the locale: %q", all)
	}
	// The detail line under a Speed card that is not merged away (the PCs are
	// not counted here) reads the same typical range.
	line := func(l format.Locale) string {
		n := allPCsCostNode(t, func(d *PageData) { d.L = l; d.V["cost"].Scope.PCs = nil })
		return cardOf(t, n, "w-recall-latency").text()
	}
	if txt := line(fr); !strings.Contains(txt, "all PCs: typical 500 ms–1,0 s") {
		t.Errorf("detail line not in the locale: %q", txt)
	}
	if txt := line(format.Locale{}); !strings.Contains(txt, "all PCs: typical 500 ms–1.0 s") {
		t.Errorf("English detail line: %q", txt)
	}
}

// The copy-summary line for Speed on all PCs writes its bucket in the locale.
func TestSummaryLineBucketsFollowTheLocale(t *testing.T) {
	r := allPCsOverview(t)
	speedAs(r, metrics.StatusBad, 5000, 2000, 2000, i64p(5000), []any{500.0, 1000.0})
	if got := mergedCostLine(r, format.FromAcceptLanguage("fr"), "Speed"); got != "slowest 1 in 20 between 2,0 and 5,0 s (approximate), all PCs" {
		t.Errorf("line %q", got)
	}
	if got := mergedCostLine(r, format.Locale{}, "Speed"); got != "slowest 1 in 20 between 2.0 and 5.0 s (approximate), all PCs" {
		t.Errorf("English line %q", got)
	}
}
