package dashboard

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

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
		if w := []string{"used by Claude", "shown, then ignored", "not declared", "shown only", "pending"}; !slices.Equal(legend, w) {
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
	want := []string{"w-context-per_session", "w-context-split", "w-recall-latency", "w-extraction-tokens"}
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
