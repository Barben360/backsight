package dashboard

import (
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
)

// near fails when got is not within a hundredth of want.
func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.011 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// rowsOf are the cells of each body row of a table, as a reader sees them.
func rowsOf(tbl *node) [][]string {
	var out [][]string
	for _, tr := range tbl.find("tbody tr") {
		var cells []string
		for _, td := range tr.find("td") {
			cells = append(cells, td.text())
		}
		out = append(out, cells)
	}
	return out
}

// tableCaptioned is the table of c whose caption is caption.
func tableCaptioned(t *testing.T, c *node, caption string) *node {
	t.Helper()
	for _, tbl := range c.find("table") {
		if cp := tbl.first("caption"); cp != nil && cp.text() == caption {
			return tbl
		}
	}
	var got []string
	for _, cp := range c.find("caption") {
		got = append(got, cp.text())
	}
	t.Fatalf("no table captioned %q (captions %q)", caption, got)
	return nil
}

// yieldWeek is one weekly cohort as memory.yield hands it over: a complete
// week has its counts (and a share from 20 memories), an incomplete one has
// neither.
type yieldWeek struct {
	monday   string
	complete bool
	num, den int64
	perUsed  float64 // tokens per memory used; 0: none
}

func yieldPoints(ws []yieldWeek) []metrics.Point {
	var out []metrics.Point
	for _, w := range ws {
		p := metrics.Point{Week: w.monday, Source: "server", Complete: &w.complete}
		if w.complete {
			p.Num, p.Den = ip(w.num), ip(w.den)
			if w.den >= 20 {
				p.Value = fp(float64(w.num) / float64(w.den))
			}
			if w.perUsed > 0 {
				p.TokensPerUsed = fp(w.perUsed)
			}
		}
		out = append(out, p)
	}
	return out
}

// the design's thirteen weeks: Aug 4 to Oct 27 2025, as of Oct 30.
var yieldWeeks = []yieldWeek{
	{"2025-08-04", true, 5, 12, 0}, {"2025-08-11", true, 0, 0, 0}, {"2025-08-18", true, 0, 0, 0},
	{"2025-08-25", true, 0, 0, 0}, {"2025-09-01", true, 0, 0, 0},
	{"2025-09-08", true, 13, 52, 15800}, {"2025-09-15", true, 0, 0, 0}, {"2025-09-22", true, 14, 45, 12400},
	{"2025-09-29", false, 0, 0, 0}, {"2025-10-06", false, 0, 0, 0}, {"2025-10-13", false, 0, 0, 0},
	{"2025-10-20", false, 0, 0, 0}, {"2025-10-27", false, 0, 0, 0},
}

// giveYield sets memory.yield to the weeks, its headline being the latest complete one.
func giveYield(r *metrics.Result, ws []yieldWeek) {
	give(r, "memory.yield", func(m *metrics.Metric) {
		m.Series = yieldPoints(ws)
		m.Value, m.Num, m.Den, m.CI = nil, nil, nil, nil
		m.Extra = map[string]any{"tokens": nil, "tokens_per_used": nil, "unresolved": int64(0), "gone": int64(0), "pending": int64(0), "resolved_used": int64(0), "withheld": nil}
		for _, w := range ws {
			if !w.complete {
				continue
			}
			m.Num, m.Den, m.Value = ip(w.num), ip(w.den), nil
			if w.den >= 20 {
				m.Value = fp(float64(w.num) / float64(w.den))
			}
			m.Extra["tokens_per_used"] = nil
			if w.perUsed > 0 {
				m.Extra["tokens_per_used"] = w.perUsed
			}
		}
	})
}

func TestYieldCard(t *testing.T) {
	const cap = "Memories made each week, and how many were used within 28 days"
	card := func(t *testing.T, dev bool, ws []yieldWeek, edit func(m *metrics.Metric)) (*node, *node) {
		t.Helper()
		n := pageNode(t, "cost", dev, func(d *PageData) {
			giveYield(d.V["cost"], ws)
			if edit != nil {
				give(d.V["cost"], "memory.yield", edit)
			}
		})
		return n, cardOf(t, n, "w-memory-yield")
	}

	t.Run("the headline and the card's frame", func(t *testing.T) {
		n, c := card(t, false, yieldWeeks, nil)
		ids := cardIDs(n)
		at := slices.Index(ids, "w-extraction-tokens")
		if at < 0 || at+1 >= len(ids) || ids[at+1] != "w-memory-yield" {
			t.Fatalf("cards %v: the yield is not right after the extraction card", ids)
		}
		if got := c.first("h2").text(); got != "Do new memories turn out useful?" {
			t.Errorf("title %q", got)
		}
		if got := texts(c.find("div.card-head span.chip")); !slices.Equal(got, []string{"this PC", "server"}) {
			t.Errorf("chips %q", got)
		}
		if cli := c.first("details.cli code"); cli == nil || cli.Text != "backsight stats cost --days 30" {
			t.Errorf("CLI %v", cli)
		}
		leads := texts(c.find("div.row.lead > div"))
		want := []string{
			"31 % of the memories made the week of Sep 22, the latest complete week, were used within 28 days (14 of 45)",
			"12.4 k tokens per memory used, that week"}
		if !slices.Equal(leads, want) {
			t.Errorf("leads %q, want %q", leads, want)
		}
	})

	t.Run("the cohort chart: a bar per complete share, a dashed band per week not over", func(t *testing.T) {
		_, c := card(t, false, yieldWeeks, nil)
		if got := len(c.find("figure.chart rect.f-used")); got != 2 { // Sep 8 and Sep 22
			t.Errorf("%d bars, want 2", got)
		}
		if got := len(c.find("div.chart-plot svg rect.inc")); got != 5 {
			t.Errorf("%d dashed bands, want the 5 weeks whose 28 days are not over", got)
		}
		if got := len(c.find("figure.chart svg text.few-t")); got != 1 { // Aug 4 has 12 memories
			t.Errorf("%d too-few notes, want 1", got)
		}
	})

	t.Run("the weeks, newest first, with what each can say", func(t *testing.T) {
		_, c := card(t, false, yieldWeeks, nil)
		got := rowsOf(tableCaptioned(t, c, cap))
		if len(got) < 13 {
			t.Fatalf("%d rows: %q", len(got), got)
		}
		for i, want := range [][]string{
			{"Oct 27", "—", "not complete until Nov 30 (its last day, Nov 2, plus 28 days)"},
			{"Oct 20", "—", "not complete until Nov 23 (its last day, Oct 26, plus 28 days)"},
			{"Sep 29", "—", "not complete until Nov 2 (its last day, Oct 5, plus 28 days)"},
			{"Sep 22", "45", "14", "31 %", "12.4 k"},
			{"Sep 15", "0", "—", "—", "—"},
			{"Sep 8", "52", "13", "25 %", "15.8 k"},
		} {
			j := slices.IndexFunc(got, func(r []string) bool { return r[0] == want[0] })
			if j < 0 || !slices.Equal(got[j], want) {
				t.Errorf("week %s: row %q, want %q", want[0], got[max(j, 0)], want)
			}
			if i == 0 && j != 0 {
				t.Errorf("the newest week is row %d, want the first", j)
			}
		}
		if last := got[len(got)-1]; !slices.Equal(last, []string{"Aug 4", "12", "5", "too few", "—"}) {
			t.Errorf("oldest row %q: fewer than 20 memories show counts, not a share", last)
		}
		if !strings.Contains(c.text(), "A week with fewer than 20 new memories shows its counts, not a share.") {
			t.Errorf("no note on small weeks: %q", c.text())
		}
	})

	t.Run("the headline moves to the latest complete week the metric hands over", func(t *testing.T) {
		ws := slices.Clone(yieldWeeks)
		ws[8] = yieldWeek{"2025-09-29", true, 10, 40, 9100} // Sep 29 now has its answer
		_, c := card(t, false, ws, nil)
		leads := texts(c.find("div.row.lead > div"))
		want := []string{
			"25 % of the memories made the week of Sep 29, the latest complete week, were used within 28 days (10 of 40)",
			"9,100 tokens per memory used, that week"}
		if !slices.Equal(leads, want) {
			t.Errorf("leads %q, want %q", leads, want)
		}
		if got := len(c.find("div.chart-plot svg rect.inc")); got != 4 {
			t.Errorf("%d dashed bands, want 4", got)
		}
	})

	t.Run("no tokens per memory when none was used", func(t *testing.T) {
		ws := slices.Clone(yieldWeeks)
		ws[7] = yieldWeek{"2025-09-22", true, 0, 45, 0}
		_, c := card(t, false, ws, nil)
		leads := texts(c.find("div.row.lead > div"))
		want := "0 % of the memories made the week of Sep 22, the latest complete week, were used within 28 days (0 of 45)"
		if len(leads) != 1 || leads[0] != want {
			t.Errorf("leads %q, want just %q", leads, want)
		}
	})

	t.Run("withheld while the record catches up: counts, no share, no chart", func(t *testing.T) {
		_, c := card(t, false, yieldWeeks, func(m *metrics.Metric) {
			m.Status, m.Reason, m.Value = metrics.StatusNoData, metrics.ReasonRecordCatchingUp, nil
			m.Extra["withheld"] = metrics.ReasonRecordCatchingUp
			m.Tags = []string{"partial"}
			for i := range m.Series {
				m.Series[i].Value = nil
			}
		})
		if got := c.first("div.empty"); got == nil || !strings.Contains(got.text(), "Still reading which memories are new.") {
			t.Errorf("no sentence for a record still catching up: %v", got)
		}
		if c.first("figure.chart") != nil || c.first("div.row.lead") != nil {
			t.Errorf("a chart or headline drawn from withheld shares: %q", c.text())
		}
		rows := rowsOf(tableCaptioned(t, c, cap))
		j := slices.IndexFunc(rows, func(r []string) bool { return r[0] == "Sep 22" })
		if j < 0 || !slices.Equal(rows[j][:3], []string{"Sep 22", "45", "14"}) || rows[j][3] == "31 %" {
			t.Errorf("Sep 22 row %q: counts stay, the share is withheld", rows[j])
		}
	})

	t.Run("no week is old enough", func(t *testing.T) {
		young := []yieldWeek{{"2025-10-20", false, 0, 0, 0}, {"2025-10-27", false, 0, 0, 0}}
		_, c := card(t, false, young, func(m *metrics.Metric) {
			m.Status, m.Reason = metrics.StatusNoData, metrics.ReasonNoCompleteWeek
		})
		if got := c.first("div.empty"); got == nil || !strings.Contains(got.text(), "No week is old enough yet.") {
			t.Errorf("sentence %v", got)
		}
		if c.first("div.row.lead") != nil {
			t.Errorf("a headline without a complete week: %q", c.text())
		}
	})

	t.Run("a week the record does not hold says so", func(t *testing.T) {
		ws := slices.Clone(yieldWeeks)
		_, c := card(t, false, ws, func(m *metrics.Metric) {
			m.Series[6] = metrics.Point{Week: "2025-09-15", Source: "none", Complete: m.Series[6].Complete} // Sep 15: no count at all
		})
		rows := rowsOf(tableCaptioned(t, c, cap))
		j := slices.IndexFunc(rows, func(r []string) bool { return r[0] == "Sep 15" })
		if j < 0 || !slices.Equal(rows[j], []string{"Sep 15", "—", "the new memories of that week were not recorded"}) {
			t.Errorf("Sep 15 row %q", rows[max(j, 0)])
		}
		chartRow := c.first("figure.chart details.tblfb")
		if chartRow == nil || !strings.Contains(chartRow.text(), "Sep 15 — — not recorded") {
			t.Errorf("the chart says something else of Sep 15: %q", chartRow.text())
		}
	})

	t.Run("the card names its metric in developer mode only", func(t *testing.T) {
		_, off := card(t, false, yieldWeeks, nil)
		_, on := card(t, true, yieldWeeks, nil)
		named := func(c *node) bool {
			p := c.first("p.devid")
			return p != nil && hasCode(p, "memory.yield")
		}
		if named(off) || !named(on) {
			t.Errorf("metric named in the card: off %v, on %v", named(off), named(on))
		}
	})
}

// recordDays are n days from start, each a point of source: "record" before
// the server's reach (serverFrom), "server" from it, "none" in the gap; the
// parts of a day with a record are given by parts(i).
func recordDays(start time.Time, n, serverFrom, gapFrom, gapTo int, parts func(i int) map[string]int64) []metrics.Point {
	var out []metrics.Point
	for i := range n {
		p := metrics.Point{Day: start.AddDate(0, 0, i).Format("2006-01-02"), Source: "server"}
		switch {
		case i >= gapFrom && i <= gapTo:
			p.Source = "none"
		case i < serverFrom:
			p.Source = "record"
		}
		if p.Source != "none" {
			p.Parts = parts(i)
			var sum int64
			for _, v := range p.Parts {
				sum += v
			}
			p.Value = fp(float64(sum))
		}
		out = append(out, p)
	}
	return out
}

// bandSpan is the x of the left edge and the width of the one gap band
// drawn in the chart's plot.
func bandSpan(t *testing.T, c *node) (x, w float64) {
	t.Helper()
	bands := c.find("figure.chart div.chart-plot rect.gap-band")
	if len(bands) != 1 {
		t.Fatalf("%d gap bands in the plot, want 1", len(bands))
	}
	return num(t, bands[0].attr("x")), num(t, bands[0].attr("width"))
}

func TestRecordBands(t *testing.T) {
	start := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC) // 90 days, to Oct 5
	const (
		n          = 90
		serverFrom = 60 // the server keeps the last 30 days
		gapFrom    = 13 // Jul 21 to Jul 23
		gapTo      = 15
	)
	day := func(i int) string { return start.AddDate(0, 0, i).Format("Jan 2") }
	kinds := func(i int) map[string]int64 {
		return map[string]int64{"observation": 1, "world": int64(2 + i%3), "experience": int64(3 + i%4)}
	}
	tokens := func(i int) map[string]int64 { return map[string]int64{"retain": 200, "consolidation": 40} }

	costData := func(d *PageData) {
		pts := recordDays(start, n, serverFrom, gapFrom, gapTo, tokens)
		give(d.V["cost"], "extraction.tokens", func(m *metrics.Metric) {
			m.Value, m.Series, m.Extra = fp(1000), pts, map[string]any{"record_from": "2026-07-08"}
		})
		d.V["cost"].Tables.Extraction = nil
		for _, p := range pts {
			if p.Source == "none" {
				continue
			}
			d.V["cost"].Tables.Extraction = append(d.V["cost"].Tables.Extraction,
				metrics.ExtractionRow{Day: p.Day, Operation: "retain", Input: 150, Output: 50},
				metrics.ExtractionRow{Day: p.Day, Operation: "consolidation", Input: 30, Output: 10})
		}
	}
	bankData := func(d *PageData) {
		richBank(d.V["bank"])
		pts := recordDays(start, n, serverFrom, gapFrom, gapTo, kinds)
		give(d.V["bank"], "bank.growth", func(m *metrics.Metric) {
			m.Value, m.Series, m.Extra = fp(11), pts, map[string]any{"record_from": "2026-07-08", "world": int64(1), "experience": int64(1), "observation": int64(1)}
		})
	}

	for _, pg := range []struct {
		page, card string
		edit       func(d *PageData)
	}{{"cost", "w-extraction-tokens", costData}, {"bank", "w-bank-growth", bankData}} {
		t.Run(pg.page, func(t *testing.T) {
			c := cardOf(t, pageNode(t, pg.page, false, pg.edit), pg.card)
			hits := c.find("figure.chart div.chart-plot rect.hit")
			if len(hits) != n {
				t.Fatalf("%d days drawn, want %d", len(hits), n)
			}
			// the band covers the three days of the gap and no other.
			x, w := bandSpan(t, c)
			left := num(t, hits[gapFrom].attr("x"))
			right := num(t, hits[gapTo].attr("x")) + num(t, hits[gapTo].attr("width"))
			near(t, "band left edge", x, left)
			near(t, "band right edge", x+w, right)
			// the source changes where the server's days begin.
			if got := len(c.find("figure.chart div.chart-plot line.src-mk")); got != 1 {
				t.Errorf("%d source marks, want 1", got)
			}
			if !strings.Contains(c.text(), "kept by the server (30 days)") || !strings.Contains(c.text(), "from this PC's daily record") {
				t.Errorf("source labels missing: %q", c.text())
			}
			if !strings.Contains(c.text(), "no record: the daily upkeep could not reach the server") {
				t.Errorf("no legend for the gap: %q", c.text())
			}
			// the table says each day outside the server's reach, newest first.
			tbl := tableCaptioned(t, c, "Days that do not come from the server")
			rows := rowsOf(tbl)
			if len(rows) != serverFrom {
				t.Fatalf("%d rows, want the %d days before the server's reach", len(rows), serverFrom)
			}
			if !slices.Equal(rows[0], []string{day(serverFrom - 1), "this PC's record"}) {
				t.Errorf("first row %q", rows[0])
			}
			gapRows := 0
			for _, r := range rows {
				if r[1] == "no record (server not reached)" {
					gapRows++
				}
			}
			if gapRows != 3 {
				t.Errorf("%d rows say no record, want 3", gapRows)
			}
			for i := gapTo; i >= gapFrom; i-- {
				want := []string{day(i), "no record (server not reached)"}
				if j := slices.IndexFunc(rows, func(r []string) bool { return r[0] == want[0] }); j < 0 || !slices.Equal(rows[j], want) {
					t.Errorf("day %s: row %q, want %q", day(i), rows[max(j, 0)], want)
				}
			}
		})
	}

	t.Run("the notes under each chart", func(t *testing.T) {
		cost := cardOf(t, pageNode(t, "cost", false, costData), "w-extraction-tokens")
		if !strings.Contains(cost.text(), "Same source and same hole as “Is memory still growing?” on Bank: both read the daily record this PC keeps for the days the server no longer holds.") {
			t.Errorf("cost note: %q", cost.text())
		}
		bank := cardOf(t, pageNode(t, "bank", false, bankData), "w-bank-growth")
		// the text of a reader leaves out what is in <code>
		want := "Jul 21 to Jul 23: ran but the memory server did not answer, so those days were not recorded. Nothing is missing from memory itself; only this chart has a hole."
		if !strings.Contains(bank.text(), want) || !hasCode(bank, "jobs maintain") {
			t.Errorf("bank note: %q, want %q", bank.text(), want)
		}
	})

	t.Run("days before the record began are no outage", func(t *testing.T) {
		c := cardOf(t, pageNode(t, "bank", false, func(d *PageData) {
			bankData(d)
			give(d.V["bank"], "bank.growth", func(m *metrics.Metric) {
				m.Extra["record_from"] = "2026-07-12"
				for i := 0; i < 4; i++ { // Jul 8 to Jul 11: before the record
					m.Series[i] = metrics.Point{Day: m.Series[i].Day, Source: "none"}
				}
			})
		}), "w-bank-growth")
		if !strings.Contains(c.text(), "Jul 21 to Jul 23: ran but the memory server did not answer") {
			t.Errorf("the real gap is not told: %q", c.text())
		}
		if strings.Contains(c.text(), "Jul 8 to Jul 11:") {
			t.Errorf("days before the record began are called an outage: %q", c.text())
		}
		if !strings.Contains(c.text(), "Before Jul 12 this PC had not started its daily record.") {
			t.Errorf("no word on the days before the record: %q", c.text())
		}
	})

	t.Run("a window inside the server's reach draws none of it", func(t *testing.T) {
		for page, card := range map[string]string{"cost": "w-extraction-tokens", "bank": "w-bank-growth"} {
			c := cardOf(t, pageNode(t, page, false, func(d *PageData) {
				if page == "bank" {
					richBank(d.V["bank"])
				} else {
					richCost(d.V["cost"])
				}
			}), card)
			for _, sel := range []string{"rect.gap-band", "line.src-mk"} {
				if len(c.find(sel)) != 0 {
					t.Errorf("%s: %s drawn without any day outside the server's reach", page, sel)
				}
			}
			if len(c.find("table[aria-label]")) != 0 || strings.Contains(c.text(), "server not reached") || strings.Contains(c.text(), "Same source") {
				t.Errorf("%s: a record note without a record: %q", page, c.text())
			}
		}
	})

	t.Run("a clock far from UTC does not move the days", func(t *testing.T) {
		c := cardOf(t, pageNode(t, "bank", false, func(d *PageData) {
			d.Loc = time.FixedZone("Tokyo", 9*3600)
			bankData(d)
		}), "w-bank-growth")
		if !strings.Contains(c.text(), "Jul 21 to Jul 23: ran but the memory server did not answer") {
			t.Errorf("days moved: %q", c.text())
		}
	})
}

func TestOriginCard(t *testing.T) {
	tokyo := time.FixedZone("Tokyo", 9*3600)
	// Oct 12 18:31 UTC is Oct 13 03:31 in Tokyo: the card writes the reader's day.
	ended := time.Date(2026, 10, 12, 18, 31, 0, 0, time.UTC)
	origin := func(o *metrics.Origin) *node {
		return pageNode(t, "memory", false, func(d *PageData) {
			d.Loc = tokyo
			d.V["memories"].Tables.Memory.Origin = o
		})
	}
	at := func(t time.Time) *time.Time { return &t }
	card := func(t *testing.T, o *metrics.Origin) *node {
		t.Helper()
		return cardOf(t, origin(o), "w-memory-origin")
	}
	str := func(s string) *string { return &s }

	t.Run("this PC", func(t *testing.T) {
		o := &metrics.Origin{State: metrics.OriginThisPC, DocKind: "session", Session: "9e04c2b7-0000-4000-8000-000000000001", Project: "project-a",
			Ended: at(ended), Sent: at(ended.Add(6 * time.Minute)), Extracted: at(ended.Add(9 * time.Minute)),
			FirstShown: at(ended.Add(9*time.Minute + 25*time.Hour))}
		c := card(t, o)
		if got := c.first("h2").text(); got != "Where did it come from?" {
			t.Errorf("title %q", got)
		}
		if got := texts(c.find("span.chip")); !slices.Equal(got, []string{"this PC"}) {
			t.Errorf("chips %q", got)
		}
		var rows [][2]string
		for _, dt := range c.find("dl dt") {
			rows = append(rows, [2]string{dt.text()})
		}
		for i, dd := range c.find("dl dd") {
			rows[i][1] = dd.text()
		}
		want := [][2]string{
			{"Session", "9e04c2b7 · project-a"},
			{"Ended", "Oct 13, sent 6 min later"},
			{"Became memory", "3 min after it was sent"},
			{"First shown", "Oct 14, 1 d 1 h later"},
		}
		if !slices.Equal(rows, want) {
			t.Errorf("rows %q, want %q", rows, want)
		}
		if a := c.first("dd a.idc"); a == nil || a.attr("href") != "/sessions/9e04c2b7-0000-4000-8000-000000000001" {
			t.Errorf("session link %v", a)
		}
		if tm := c.first("dd time"); tm == nil || tm.attr("title") != "2026-10-13 03:31" {
			t.Errorf("time element %v", tm)
		}
		if c.first("p.xs") != nil {
			t.Errorf("a never-shown note on a memory that was shown: %q", c.text())
		}
	})

	t.Run("this PC, never shown", func(t *testing.T) {
		o := &metrics.Origin{State: metrics.OriginThisPC, DocKind: "session", Session: "c1d8e5a0", Project: "backsight",
			Ended: at(ended), Sent: at(ended.Add(4 * time.Minute)), Extracted: at(ended.Add(6 * time.Minute))}
		c := card(t, o)
		var first string
		for i, dt := range c.find("dl dt") {
			if dt.text() == "First shown" {
				first = c.find("dl dd")[i].text()
			}
		}
		if first != "not yet" {
			t.Errorf("first shown %q, want not yet", first)
		}
		if got := c.first("p.xs"); got == nil || got.text() != "Never shown so far: it waits for a prompt on its topic. Its counts and its shows timeline are empty." {
			t.Errorf("note %v", got)
		}
	})

	t.Run("another PC", func(t *testing.T) {
		c := card(t, &metrics.Origin{State: metrics.OriginOtherPC, DocKind: "session", Session: "5aa0", Created: str("2026-10-09")})
		if got := texts(c.find("span.chip")); !slices.Equal(got, []string{"server"}) {
			t.Errorf("chips %q", got)
		}
		if got := c.first("p").text(); got != "From a session on another PC (made Oct 9). Its turns are on that PC's dashboard." {
			t.Errorf("sentence %q", got)
		}
		if c.first("dl") != nil {
			t.Error("a session list for a session of another PC")
		}
		// a date, not an instant: no zone moves it.
		c = card(t, &metrics.Origin{State: metrics.OriginOtherPC, DocKind: "session", Session: "5aa0", Created: str("2026-10-09")})
		if !strings.Contains(c.text(), "(made Oct 9)") {
			t.Errorf("zone moved the day: %q", c.text())
		}
	})

	t.Run("another PC, creation day not known", func(t *testing.T) {
		c := card(t, &metrics.Origin{State: metrics.OriginOtherPC, DocKind: "session", Session: "5aa0"})
		if got := c.first("p").text(); got != "From a session on another PC. Its turns are on that PC's dashboard." {
			t.Errorf("sentence %q", got)
		}
	})

	t.Run("no session", func(t *testing.T) {
		c := card(t, &metrics.Origin{State: metrics.OriginNoSession, DocKind: "note"})
		if got := c.first("p").text(); got != "Stored on its own: it did not come from a session." {
			t.Errorf("sentence %q", got)
		}
		if got := len(c.find("span.chip")); got != 0 {
			t.Errorf("%d chips on a memory with no session", got)
		}
	})

	t.Run("not known yet", func(t *testing.T) {
		c := card(t, &metrics.Origin{State: metrics.OriginUnknown})
		if got := c.first("p").text(); got != "Not known yet: the next daily upkeep looks it up on the server." {
			t.Errorf("sentence %q", got)
		}
	})

	t.Run("under the header, before the rest", func(t *testing.T) {
		page := render(t, "memory", func(d *PageData) {
			d.Loc = tokyo
			d.V["memories"].Tables.Memory.Origin = &metrics.Origin{State: metrics.OriginUnknown}
		})
		meta, origin, text := strings.Index(page, `class="row small muted memory-meta"`), strings.Index(page, `id="w-memory-origin"`), strings.Index(page, `id="w-memory-text"`)
		if meta < 0 || origin < 0 || text < 0 || meta >= origin || origin >= text {
			t.Errorf("order: header %d, origin %d, text card %d", meta, origin, text)
		}
	})

	t.Run("a view without an origin draws no card", func(t *testing.T) {
		n := origin(nil)
		if n.first("#w-memory-origin") != nil {
			t.Error("origin card without an origin")
		}
	})

	t.Run("never a document id", func(t *testing.T) {
		page := render(t, "memory", func(d *PageData) {
			d.V["memories"].Tables.Memory.Origin = &metrics.Origin{State: metrics.OriginNoSession, DocKind: "import"}
		})
		if strings.Contains(page, "import:") {
			t.Errorf("a document id on the page")
		}
	})
}

// giveDevBank sets the developer metrics of the bank page.
func giveDevBank(r *metrics.Result) {
	give(r, "bank.active_share", func(m *metrics.Metric) {
		m.Value, m.Num, m.Den, m.CI = fp(412.0/1214), ip(412), ip(1214), &[2]float64{0.31, 0.37}
		m.Tags, m.Extra = nil, map[string]any{"unresolved": int64(0), "gone": int64(0)}
	})
	give(r, "shows.concentration", func(m *metrics.Metric) {
		m.Value, m.Num, m.Den, m.CI = fp(0.62), nil, nil, nil
		m.Extra = map[string]any{"gini": 0.62, "top10_share": 0.31, "shows": int64(1906)}
	})
	r.Tables.TopShown = []metrics.TopShownRow{
		{Memory: "e5a17c90-1111-4000-8000-000000000001", Project: "backsight", Shown: 96, Share: 0.0504, Used: 4},
		{Memory: "0c3b9f42-1111-4000-8000-000000000002", Project: "backsight", Shown: 81, Share: 0.0426, Used: 0},
		{Memory: "7f3a9c21-1111-4000-8000-000000000003", Project: "project-d-shared-frontend-tools", Shown: 74, Share: 0.0388, Used: 6},
	}
}

func devCard(t *testing.T, page, id string, edit func(d *PageData)) *node {
	t.Helper()
	return cardOf(t, pageNode(t, page, true, edit), id)
}

func TestDevWidgetsBank(t *testing.T) {
	t.Run("how much of the memory ever comes up", func(t *testing.T) {
		c := devCard(t, "bank", "w-bank-active_share", func(d *PageData) {
			richBank(d.V["bank"])
			giveDevBank(d.V["bank"])
			// 23:30 UTC on Sep 30 is Oct 1 in Paris: the card names the reader's day.
			d.V["bank"].Window.Since = time.Date(2026, 9, 30, 23, 30, 0, 0, time.UTC)
			d.CLI = cliFor("bank", "", d.P) // as the handler sets it: the developer flag is part of the command
		})
		if got := c.first("h2").text(); got != "How much of the memory ever comes up?" {
			t.Errorf("title %q", got)
		}
		if got := texts(c.find("div.card-head span.chip")); !slices.Equal(got, []string{"this PC", "developer"}) {
			t.Errorf("chips %q", got)
		}
		if cli := c.first("details.cli code"); cli == nil || cli.Text != "backsight stats bank --days 30 --dev" {
			t.Errorf("CLI %v", cli)
		}
		if !hasCode(c, "bank.active_share") {
			t.Error("the metric id is not named")
		}
		var figs [][4]string
		for _, f := range c.find("div.fig") {
			figs = append(figs, [4]string{f.first("span.lab").text(), f.first("span.v").text(), f.first("span.s").text()})
		}
		want := [][4]string{
			{"Shown at least once", "34 %", "412 of the 1,214 memories there were on Oct 1"},
			{"Never shown in the window", "802", "not a fault: most memory waits for its topic"},
		}
		if !slices.Equal(figs, want) {
			t.Errorf("figures %q, want %q", figs, want)
		}
		if !strings.Contains(c.text(), "Memories made during the window are left out of both counts.") {
			t.Errorf("no note: %q", c.text())
		}
	})

	t.Run("a few memories taking all the shows", func(t *testing.T) {
		c := devCard(t, "bank", "w-shows-concentration", func(d *PageData) { richBank(d.V["bank"]); giveDevBank(d.V["bank"]) })
		if got := c.first("h2").text(); got != "Do a few memories take all the shows?" {
			t.Errorf("title %q", got)
		}
		if cli := c.first("details.cli code"); cli == nil || cli.Text != "backsight stats bank --days 30" {
			t.Errorf("CLI %v", cli)
		}
		if got := c.first("div.row.lead > div").text(); got != "0.62 Gini of shows per memory shown (0: all shown alike, 1: one memory takes every show)" {
			t.Errorf("lead %q", got)
		}
		if got := c.first("p.sub").text(); got != "The 3 most shown memories took 31 % of the 1,906 shows." {
			t.Errorf("sentence %q", got)
		}
		got := rowsOf(c.first("table"))
		want := [][]string{
			{"1", "e5a17c90", "backsight", "96", "5.0 %", "4"},
			{"2", "0c3b9f42", "backsight", "81", "4.3 %", "0"},
			{"3", "7f3a9c21", "project-d-shared-frontend-tools", "74", "3.9 %", "6"},
		}
		if len(got) < 3 {
			t.Fatalf("rows %q", got)
		}
		for i := range want {
			// the project name is clipped on screen: compare its start.
			if got[i][0] != want[i][0] || got[i][1] != want[i][1] || got[i][3] != want[i][3] || got[i][4] != want[i][4] || got[i][5] != want[i][5] ||
				!strings.HasPrefix(want[i][2], strings.TrimSuffix(got[i][2], "…")) {
				t.Errorf("row %d: %q, want %q", i+1, got[i], want[i])
			}
		}
		if a := c.first("tbody a.idc"); a == nil || a.attr("href") != "/memories/e5a17c90-1111-4000-8000-000000000001" {
			t.Errorf("memory link %v", a)
		}
		if a := c.first("a[href='/memories?sort=shown']"); a == nil || a.text() != "all, by shows →" {
			t.Errorf("no link to the whole list: %v", a)
		}
	})

	t.Run("only in developer mode", func(t *testing.T) {
		n := pageNode(t, "bank", false, func(d *PageData) { richBank(d.V["bank"]); giveDevBank(d.V["bank"]) })
		for _, id := range []string{"w-bank-active_share", "w-shows-concentration"} {
			if n.first("#"+id) != nil {
				t.Errorf("%s drawn without developer mode", id)
			}
		}
		if strings.Contains(n.text(), "Gini") || strings.Contains(n.text(), "most shown memories took") {
			t.Errorf("developer words on the plain page")
		}
	})

	t.Run("unresolved memories are said, not hidden", func(t *testing.T) {
		c := devCard(t, "bank", "w-bank-active_share", func(d *PageData) {
			richBank(d.V["bank"])
			giveDevBank(d.V["bank"])
			give(d.V["bank"], "bank.active_share", func(m *metrics.Metric) {
				m.Tags, m.Extra["unresolved"] = []string{"partial"}, int64(3)
			})
		})
		want := "3 memories shown are not in the record of new memories yet, so counted as older."
		if !strings.Contains(c.text(), want) {
			t.Errorf("text %q lacks %q", c.text(), want)
		}
	})

	t.Run("no number says why", func(t *testing.T) {
		c := devCard(t, "bank", "w-bank-active_share", func(d *PageData) {
			richBank(d.V["bank"])
			give(d.V["bank"], "bank.active_share", func(m *metrics.Metric) {
				m.Value, m.Num, m.Den = nil, nil, nil
				m.Status, m.Reason = metrics.StatusNoData, metrics.ReasonServerDown
			})
		})
		if c.first("div.empty") == nil || !strings.Contains(c.text(), "Memory server not answering") || c.first("div.fig") != nil {
			t.Errorf("card %q", c.text())
		}
	})
}

// the language test of recall.by_lang as the metric hands it over.
func giveLangTest(r *metrics.Result, status metrics.Status) {
	give(r, "recall.by_lang", func(m *metrics.Metric) {
		m.Status = status
		m.Extra = map[string]any{"by_lang": map[string]any{
			"en": map[string]any{"num": 84, "den": 271, "value": 84.0 / 271, "ci": []float64{0.26, 0.37}},
			"fr": map[string]any{"num": 11, "den": 84, "value": 11.0 / 84, "ci": []float64{0.07, 0.22}},
			"de": map[string]any{"num": 3, "den": 9, "value": 3.0 / 9, "ci": []float64{0.1, 0.7}},
		}, "test": map[string]any{
			"reference": "en", "reference_num": 84, "reference_den": 271,
			"gaps":     []map[string]any{{"lang": "fr", "num": 11, "den": 84, "value": 11.0 / 84, "gap": 0.18, "interval": []float64{0.08, 0.27}}},
			"left_out": []map[string]any{{"lang": "de", "num": 3, "den": 9}},
		}}
	})
}

func giveDevQuality(r *metrics.Result) {
	give(r, "injection.contradicted", func(m *metrics.Metric) {
		m.Value, m.Num, m.Den, m.CI, m.Status = fp(5.0/167), ip(5), ip(167), &[2]float64{0.01, 0.07}, metrics.StatusOK
	})
	give(r, "boost.set_changed", func(m *metrics.Metric) {
		m.Value, m.Num, m.Den, m.Status = fp(30.0/167), ip(30), ip(167), metrics.StatusOK
		m.Extra = map[string]any{"prompts": int64(120), "prompts_set_changed": int64(20), "searches": int64(47), "searches_set_changed": int64(10)}
	})
	giveLangTest(r, metrics.StatusWatch)
}

func TestDevWidgetsQuality(t *testing.T) {
	t.Run("corrections after memory", func(t *testing.T) {
		c := devCard(t, "quality", "w-injection-contradicted", func(d *PageData) { giveDevQuality(d.V["quality"]) })
		if got := c.first("h2").text(); got != "Does the user correct Claude after memory is shown?" {
			t.Errorf("title %q", got)
		}
		if got := texts(c.find("div.card-head span.chip")); !slices.Equal(got, []string{"developer"}) {
			t.Errorf("chips %q: the design draws the developer chip alone", got)
		}
		f := c.first("div.fig")
		if f == nil || f.first("span.lab").text() != "Next prompt looked like a correction" || f.first("span.v").text() != "3 %" ||
			f.first("span.s").text() != "5 of 167 prompts that got memory" {
			t.Errorf("figure %v", f)
		}
		if !strings.Contains(c.text(), "The pattern that spots a correction has never been measured for precision, so this never gets a status. The sessions:") ||
			!hasCode(c, "backsight stats sessions --has contradicted") {
			t.Errorf("hint %q", c.text())
		}
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "info only" || !strings.Contains(p.attr("class"), "st-info") {
			t.Errorf("pill %v", p)
		}
	})

	t.Run("the learned ranking", func(t *testing.T) {
		c := devCard(t, "quality", "w-boost-set_changed", func(d *PageData) { giveDevQuality(d.V["quality"]) })
		if got := c.first("h2").text(); got != "How often does the learned ranking change what is shown?" {
			t.Errorf("title %q", got)
		}
		f := c.first("div.fig")
		if f == nil || f.first("span.lab").text() != "Recalls it changed" || f.first("span.v").text() != "18 %" ||
			f.first("span.s").text() != "30 of 167 recalls showed a different set" {
			t.Errorf("figure %v", f)
		}
		if !strings.Contains(c.text(), "What another weight would do:") || !hasCode(c, BoostReplayCommand) {
			t.Errorf("no replay advice: %q", c.text())
		}
	})

	t.Run("the language test", func(t *testing.T) {
		c := devCard(t, "quality", "w-language-test", func(d *PageData) { giveDevQuality(d.V["quality"]) })
		if got := c.first("h2").text(); got != "Does memory show up as often in every language?" {
			t.Errorf("title %q", got)
		}
		if p := c.first("div.card-head span.pill"); p == nil || p.text() != "watch" || !strings.Contains(p.attr("class"), "st-watch") {
			t.Errorf("pill %v", p)
		}
		got := rowsOf(c.first("table"))
		want := [][]string{
			{"English", "31 % (26–37)", "271"},
			{"French", "13 % (7–22)", "84"},
			{"German", "3 of 9", "9"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rows %q, want %q", got, want)
		}
		wantLine := "watch French prompts get memory 18 pts less often than English (8 to 27 pts): a gap over 15 pts whose interval excludes 0."
		var lines []string
		for _, p := range c.find("p.small") {
			lines = append(lines, p.text())
		}
		if !slices.Contains(lines, wantLine) {
			t.Errorf("lines %q, want %q", lines, wantLine)
		}
		if !strings.Contains(c.text(), "Languages with fewer than 20 prompts show counts and are left out of the test.") {
			t.Errorf("no note on small languages: %q", c.text())
		}
	})

	t.Run("a gap inside the limit is no watch line", func(t *testing.T) {
		c := devCard(t, "quality", "w-language-test", func(d *PageData) {
			giveDevQuality(d.V["quality"])
			giveLangTest(d.V["quality"], metrics.StatusOK)
			m := d.V["quality"].Metrics["recall.by_lang"]
			g := m.Extra["test"].(map[string]any)
			g["gaps"] = []map[string]any{{"lang": "fr", "num": 30, "den": 84, "value": 30.0 / 84, "gap": 0.05, "interval": []float64{-0.04, 0.14}}}
			g["left_out"] = []map[string]any{}
		})
		if strings.Contains(c.text(), "a gap over 15 pts") {
			t.Errorf("a watch line for a gap of 5 pts: %q", c.text())
		}
		if c.first("div.card-head span.pill") != nil {
			t.Errorf("a watch pill without a watch")
		}
		if strings.Contains(c.text(), "left out of the test") {
			t.Errorf("a note on small languages when none is small: %q", c.text())
		}
	})

	t.Run("the page's watch line follows the metric's rule", func(t *testing.T) {
		// a gap of 15 pts exactly is not over the limit; one just past it with an interval that excludes 0 is.
		for gap, want := range map[float64]bool{0.15: false, 0.16: true} {
			c := devCard(t, "quality", "w-language-test", func(d *PageData) {
				giveDevQuality(d.V["quality"])
				m := d.V["quality"].Metrics["recall.by_lang"]
				m.Extra["test"].(map[string]any)["gaps"] = []map[string]any{{"lang": "fr", "num": 11, "den": 84, "gap": gap, "interval": []float64{gap - 0.05, gap + 0.05}}}
			})
			if got := strings.Contains(c.text(), "a gap over 15 pts"); got != want {
				t.Errorf("gap %v: watch line %v, want %v", gap, got, want)
			}
			if metrics.LangBehind(gap, gap-0.05, gap+0.05) != want {
				t.Errorf("gap %v: the metric reads otherwise", gap)
			}
		}
	})

	t.Run("a language ahead of the reference reads more often", func(t *testing.T) {
		c := devCard(t, "quality", "w-language-test", func(d *PageData) {
			giveDevQuality(d.V["quality"])
			m := d.V["quality"].Metrics["recall.by_lang"]
			m.Extra["test"].(map[string]any)["gaps"] = []map[string]any{{"lang": "fr", "num": 60, "den": 84, "value": 60.0 / 84, "gap": -0.2, "interval": []float64{-0.31, -0.09}}}
		})
		if !strings.Contains(c.text(), "French prompts get memory 20 pts more often than English (9 to 31 pts)") {
			t.Errorf("text %q", c.text())
		}
	})

	t.Run("only in developer mode", func(t *testing.T) {
		n := pageNode(t, "quality", false, func(d *PageData) { giveDevQuality(d.V["quality"]) })
		for _, id := range []string{"w-injection-contradicted", "w-boost-set_changed", "w-language-test"} {
			if n.first("#"+id) != nil {
				t.Errorf("%s drawn without developer mode", id)
			}
		}
		for _, w := range []string{"Recalls it changed", "Next prompt looked like a correction", "two-proportion", "Prompt language"} {
			if strings.Contains(n.text(), w) {
				t.Errorf("developer text %q on the plain page", w)
			}
		}
	})
}
