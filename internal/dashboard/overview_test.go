package dashboard

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// renderOverview executes the overview page on r, as the page runs on a
// result: the fixture frame, English, the 7-day window.
func renderOverview(t *testing.T, r *metrics.Result) *node {
	t.Helper()
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	spec := specs["overview"]
	d := fixtureData(t, spec, false)
	d.V["overview"] = r
	d.Loc, d.L = r.Window.Loc, en
	d.P = views.Params{Days: 7}
	d.CLI = "backsight stats overview"
	var buf bytes.Buffer
	if err := tpls["overview"].ExecuteTemplate(&buf, spec.File, d); err != nil {
		t.Fatal(err)
	}
	return parseHTML(t, buf.String())
}

func scenarioNamed(t *testing.T, name string) *metrics.Result {
	t.Helper()
	for _, s := range scenarios {
		if s.Name == name {
			return s.Result()
		}
	}
	t.Fatalf("no scenario %s", name)
	return nil
}

func reasonTexts(rs []ReasonRow) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Text)
	}
	return out
}

func TestBannerRules(t *testing.T) {
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			r := s.Result()
			b := bannerOf(r, en, r.Window.Loc, views.Params{})
			if b.Title != s.Want.Title || b.Class != s.Want.Class {
				t.Errorf("title %q class %q, want %q %q", b.Title, b.Class, s.Want.Title, s.Want.Class)
			}
			if len(b.Axes) != 4 {
				t.Fatalf("%d axes", len(b.Axes))
			}
			for i, a := range b.Axes {
				if got := [3]string{a.Name, a.Word, a.Note}; got != s.Want.Axes[i] {
					t.Errorf("axis %d: %q, want %q", i, got, s.Want.Axes[i])
				}
			}
			if got := reasonTexts(b.Reasons); !slices.Equal(got, s.Want.Reasons) {
				t.Errorf("reasons %q, want %q", got, s.Want.Reasons)
			}
		})
	}
}

func TestBannerClassAndIcon(t *testing.T) {
	for _, c := range []struct{ name, class, icon string }{
		{"healthy", "v-ok", "st-ok"}, {"new", "v-learn", "st-learn"}, {"cantjudge", "v-cant", "st-cant"},
		{"qualitywatch", "v-watch", "st-watch"}, {"broken", "v-bad", "st-bad"},
	} {
		r := scenarioNamed(t, c.name)
		if b := bannerOf(r, en, r.Window.Loc, views.Params{}); b.Class != c.class || b.Icon != c.icon {
			t.Errorf("%s: %s/%s, want %s/%s", c.name, b.Class, b.Icon, c.class, c.icon)
		}
	}
}

func TestBannerRuleEdges(t *testing.T) {
	// A too-early Right axis: its sentence is the note, the icon is "few".
	r := scenarioNamed(t, "healthy")
	const tooEarly = "Not enough yet: 6 declared turns, 20 needed."
	r.Verdict.Axes["right"] = scAxis(metrics.AxisTooEarly, 7, metrics.Reason{Metric: "used.turn_rate", Severity: metrics.SeverityInfo, Text: tooEarly})
	b := bannerOf(r, en, r.Window.Loc, views.Params{})
	if b.Title != "Right: not enough yet." || b.Class != "v-cant" || b.Icon != "st-few" {
		t.Errorf("too early: %q %s %s", b.Title, b.Class, b.Icon)
	}
	if a := b.Axes[1]; a.Word != "too few" || a.Class != "st-few" || a.Note != tooEarly {
		t.Errorf("too early axis: %+v", a)
	}

	// The root cause bad is v-bad.
	r = scenarioNamed(t, "serverdown")
	r.Verdict.Axes["flowing"].Reasons[0].Severity = metrics.SeverityBad
	b = bannerOf(r, en, r.Window.Loc, views.Params{})
	if b.Class != "v-bad" || b.Icon != "st-bad" || b.Axes[3].Class != "st-bad" || b.Reasons[0].Word != "attention" || b.Reasons[0].Class != "st-bad" {
		t.Errorf("bad root cause: %s %s %+v %+v", b.Class, b.Icon, b.Axes[3], b.Reasons[0])
	}

	// No failing_since: no "since".
	r = scenarioNamed(t, "serverdown")
	r.Metrics["server.reach"] = scValue("server.reach", metrics.StatusWatch, 96, 0, scExtra(map[string]any{"failing_since": nil})).m
	if b = bannerOf(r, en, r.Window.Loc, views.Params{}); b.Title != "Flowing: memory server unreachable, nothing is lost." {
		t.Errorf("no failing_since: %q", b.Title)
	}
	// The time is read in the window's zone, and as a decoded string.
	r = scenarioNamed(t, "serverdown")
	r.Metrics["server.reach"] = scValue("server.reach", metrics.StatusWatch, 96, 0, scExtra(map[string]any{"failing_since": "2026-10-30T08:12:00Z"})).m
	if b = bannerOf(r, en, time.UTC, views.Params{}); b.Title != "Flowing: memory server unreachable since 08:12, nothing is lost." {
		t.Errorf("string failing_since: %q", b.Title)
	}

	// Two reasons on the axis, none of them latency alone.
	r = scenarioNamed(t, "latency")
	r.Verdict.Axes["cost"] = scAxis(metrics.AxisAttention, 7,
		metrics.Reason{Metric: "recall.latency", Severity: metrics.SeverityWatch, Text: "a"},
		metrics.Reason{Metric: "context.per_session", Severity: metrics.SeverityWatch, Text: "b"})
	if b = bannerOf(r, en, r.Window.Loc, views.Params{}); b.Title != "Cost needs attention (2 reasons)." {
		t.Errorf("two reasons: %q", b.Title)
	}
	// Attention wins over can't judge; the first attention axis in order is the headline.
	r = scenarioNamed(t, "cantjudge")
	r.Verdict.Axes["cost"] = scAxis(metrics.AxisAttention, 7, metrics.Reason{Metric: "context.per_session", Severity: metrics.SeverityWatch, Text: "big"})
	b = bannerOf(r, en, r.Window.Loc, views.Params{})
	if b.Title != "Cost needs attention." || !slices.Equal(reasonTexts(b.Reasons), []string{"big", scCantText}) {
		t.Errorf("mixed: %q %q", b.Title, reasonTexts(b.Reasons))
	}
	// At most three reasons.
	r = scenarioNamed(t, "broken")
	fl := r.Verdict.Axes["flowing"]
	fl.Reasons = append(fl.Reasons, metrics.Reason{Metric: "hooks.failures", Severity: metrics.SeverityWatch, Text: "fourth"})
	r.Verdict.Axes["flowing"] = fl
	if b = bannerOf(r, en, r.Window.Loc, views.Params{}); len(b.Reasons) != 3 || b.Title != "Flowing needs attention (4 reasons)." {
		t.Errorf("cap: %d reasons, title %q", len(b.Reasons), b.Title)
	}
	// A learning sentence that does not match the day form is shown whole.
	r = scenarioNamed(t, "new")
	r.Verdict.Axes["found"].Reasons[0].Text = "Learning soon."
	if b = bannerOf(r, en, r.Window.Loc, views.Params{}); b.Axes[0].Note != "Learning soon." {
		t.Errorf("odd learning note: %q", b.Axes[0].Note)
	}
	// A number the note needs is missing: the first reason text, else a dash.
	r = scenarioNamed(t, "qualitywatch")
	delete(r.Metrics, "recall.hit_rate")
	if b = bannerOf(r, en, r.Window.Loc, views.Params{}); b.Axes[0].Note != scFoundText {
		t.Errorf("missing number: %q", b.Axes[0].Note)
	}
	r = scenarioNamed(t, "healthy")
	delete(r.Metrics, "recall.hit_rate")
	if b = bannerOf(r, en, r.Window.Loc, views.Params{}); b.Axes[0].Note != "—" {
		t.Errorf("missing number, no reason: %q", b.Axes[0].Note)
	}
	// Severity bad drops "two weeks running"; no Previous gives the ok note.
	r = scenarioNamed(t, "qualitywatch")
	r.Verdict.Axes["found"].Reasons[0].Severity = metrics.SeverityBad
	if b = bannerOf(r, en, r.Window.Loc, views.Params{}); b.Axes[0].Note != "14 % vs 27 % usual" {
		t.Errorf("bad found: %q", b.Axes[0].Note)
	}
	r = scenarioNamed(t, "qualitywatch")
	m := r.Metrics["recall.hit_rate"]
	m.Previous = nil
	r.Metrics["recall.hit_rate"] = m
	if b = bannerOf(r, en, r.Window.Loc, views.Params{}); b.Axes[0].Note != "14 % of prompts got memory" {
		t.Errorf("no previous: %q", b.Axes[0].Note)
	}
	// No verdict: nothing is claimed.
	if b = bannerOf(&metrics.Result{}, en, nil, views.Params{}); b.Class != "v-cant" || len(b.Axes) != 0 || b.Title == "Memory is working." {
		t.Errorf("no verdict: %+v", b)
	}
}

func TestBannerHead(t *testing.T) {
	r := scenarioNamed(t, "healthy")
	b := bannerOf(r, en, r.Window.Loc, views.Params{Project: "my proj", Days: 30, DaysSet: true})
	if b.Head.ID != "w-verdict" || b.Head.Scope != "machine" || b.Head.CLI != "backsight stats overview --days 7 --project 'my proj'" {
		t.Errorf("head: %+v", b.Head)
	}
	if b.Head.Info.Href != "/defs" || b.Head.Info.HrefText != "All definitions" || len(b.Head.Info.Paras) != 2 {
		t.Errorf("info: %+v", b.Head.Info)
	}
}

func TestBannerMarkup(t *testing.T) {
	n := renderOverview(t, scenarioNamed(t, "healthy"))
	sec := n.first("section.verdict.v-ok")
	if sec == nil || sec.attr("aria-labelledby") != "w-verdict" {
		t.Fatalf("section: %v", sec)
	}
	if h := sec.first("h2.v-title"); h == nil || h.attr("id") != "w-verdict" || h.text() != "Memory is working. (last 7 days)" {
		t.Errorf("title: %v", h)
	}
	if sec.first(".v-ico.st-ok") == nil {
		t.Error("no icon")
	}
	if sum := sec.first("details.info summary"); sum == nil || sum.attr("aria-label") != "How the verdict is judged" {
		t.Fatalf("info summary: %v", sum)
	}
	pop := sec.first(".pop")
	for _, want := range []string{"Always the last 7 days against the 7 before", "An axis turns to attention when a number stays at watch two weeks in a row"} {
		if !strings.Contains(pop.text(), want) {
			t.Errorf("popover lacks %q: %q", want, pop.text())
		}
	}
	if a := sec.first(".pop a[href=/defs]"); a == nil || a.text() != "All definitions" {
		t.Errorf("definitions link: %v", a)
	}
	if c := sec.first("details.cli code"); c == nil || codeText(c) != "backsight stats overview --days 7" {
		t.Errorf("cli: %v", c)
	}
	lis := sec.find("ul.axes li.axis")
	if len(lis) != 4 {
		t.Fatalf("%d axes", len(lis))
	}
	if lis[0].first(".axis-name").text() != "Found" || lis[0].first(".st.st-ok").text() != "ok" || lis[0].first(".axis-note").text() != "28 % of prompts got memory" {
		t.Errorf("first axis: %v", lis[0])
	}
	if sec.first("ul.reasons") != nil {
		t.Error("reasons on a healthy banner")
	}

	// A project filter reaches the CLI.
	n = renderOverview(t, scenarioNamed(t, "broken"))
	rs := n.find("section.verdict.v-bad ul.reasons li.reason")
	if len(rs) != 3 {
		t.Fatalf("%d reasons", len(rs))
	}
	if p := rs[0].first(".pill.st.st-bad"); p == nil || p.text() != "attention" || rs[0].first("p").text() != scFailedText {
		t.Errorf("reason 0: %v", rs[0])
	}
	if p := rs[2].first(".pill.st.st-watch"); p == nil || p.text() != "watch" {
		t.Errorf("reason 2: %v", rs[2])
	}
	if c := rs[0].first(".cmd code"); c == nil || codeText(c) != "backsight retain retry-quarantine" || !rs[0].first("button.copy").has("hidden") {
		t.Errorf("reason command: %v", c)
	}
}

func TestSummaryLine(t *testing.T) {
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			r := s.Result()
			n := renderOverview(t, r)
			p := n.first("p.summary")
			if p == nil || p.text() != s.Want.Summary {
				t.Fatalf("summary %v, want %q", p, s.Want.Summary)
			}
			if bs := p.find("b"); len(bs) != 4 {
				t.Errorf("%d bold figures", len(bs))
			}
			if got := summaryOf(r, en); got.Prompts == "" || got.Scope == "" {
				t.Errorf("summaryOf: %+v", got)
			}
		})
	}
	// Priced but the server did not answer: unknown, and the sentence says why.
	r := scenarioNamed(t, "serverdown")
	if got := summaryOf(r, en); got.Cost != "unknown" || got.Scope != "(the memory server is not answering)." {
		t.Errorf("serverdown: %+v", got)
	}
	r = scenarioNamed(t, "healthy")
	if got := summaryOf(r, en); got.Cost != "$43.62" || got.Scope != "(server, all PCs)." || got.Tokens != "17 k" {
		t.Errorf("healthy: %+v", got)
	}
	// The price is not set: that is said, not "unknown", server or not.
	r = scenarioNamed(t, "serverdown")
	r.Summary.PriceSet = false
	if got := summaryOf(r, en); got.Cost != "price not set" {
		t.Errorf("price not set: %+v", got)
	}
	// Without a summary the line is not drawn.
	if n := renderOverview(t, &metrics.Result{Window: metrics.Window{Loc: time.UTC}}); n.first("p.summary") != nil {
		t.Error("a summary line without a summary")
	}
}

func TestAttentionCard(t *testing.T) {
	n := renderOverview(t, scenarioNamed(t, "broken"))
	card := n.first("section.card[aria-labelledby=w-attention]")
	if card == nil {
		t.Fatal("no attention card")
	}
	if h := card.first("h2#w-attention"); h == nil || h.text() != "What should I do?" {
		t.Errorf("heading: %v", h)
	}
	if card.first(".card-head details.info") != nil {
		t.Error("an empty ⓘ on a card that has none")
	}
	if c := card.first(".card-head .muted"); c == nil || c.text() != "3 items" {
		t.Errorf("count: %v", c)
	}
	if c := card.first("details.cli code"); c == nil || codeText(c) != "backsight stats overview" {
		t.Errorf("cli: %v", c)
	}
	items := card.find("ul.att li.att-item")
	if len(items) != 3 {
		t.Fatalf("%d items", len(items))
	}
	for i, want := range [][2]string{{"attention", "st-bad"}, {"attention", "st-bad"}, {"watch", "st-watch"}} {
		it := items[i]
		if p := it.first(".pill.st." + want[1]); p == nil || p.text() != want[0] {
			t.Errorf("item %d pill: %v", i, p)
		}
		if b := it.first("button.copy"); b == nil || !b.has("hidden") {
			t.Errorf("item %d copy button: %v", i, b)
		}
		if d := it.first("details.why"); d == nil || d.first("summary").text() != "Why this matters" || len(d.text()) <= len("Why this matters") {
			t.Errorf("item %d why: %v", i, d)
		}
	}
	if items[0].first("p").text() != scFailedText || items[2].first("p").text() != scCrashedText {
		t.Error("item texts")
	}
	if card.first(".empty") != nil || card.first("a[href=/system]") != nil {
		t.Error("empty box or more link on a full list")
	}

	// One item reads "1 item".
	n = renderOverview(t, scenarioNamed(t, "cantjudge"))
	if c := n.first("section.card[aria-labelledby=w-attention] .card-head .muted"); c == nil || c.text() != "1 item" {
		t.Errorf("count of one: %v", c)
	}

	for _, c := range []struct{ scenario, calm string }{
		{"healthy", "Everything is within its usual range. Nothing to do."},
		{"new", "Memory is still learning: the verdict firms up after about a week of prompts."},
	} {
		n = renderOverview(t, scenarioNamed(t, c.scenario))
		e := n.first("section.card[aria-labelledby=w-attention] .empty")
		if e == nil || e.first(".st.st-ok").text() != "Nothing needs attention." || e.first(".small").text() != c.calm {
			t.Errorf("%s: %v", c.scenario, e)
		}
		if n.first("section.card[aria-labelledby=w-attention] ul.att") != nil {
			t.Errorf("%s: a list on an empty card", c.scenario)
		}
	}

	r := scenarioNamed(t, "broken")
	r.AttentionMore = 2
	n = renderOverview(t, r)
	if a := n.first("section.card[aria-labelledby=w-attention] a[href=/system]"); a == nil || a.text() != "2 more on the System page →" {
		t.Errorf("more link: %v", a)
	}
}

func TestAttentionOf(t *testing.T) {
	r := scenarioNamed(t, "broken")
	c := attentionOf(r, en)
	if c.Count != "3 items" || len(c.Items) != 3 || len(c.Whys) != 3 || c.Calm != "" || c.More != "" {
		t.Errorf("broken: %+v", c)
	}
	if c.Items[1].Command != "backsight doctor" || c.Whys[1] == "" {
		t.Errorf("item 1: %+v %q", c.Items[1], c.Whys[1])
	}
}

func TestExtraAs(t *testing.T) {
	at := time.Date(2026, 10, 30, 9, 12, 0, 0, time.UTC)
	for label, extra := range map[string]map[string]any{
		"in process":  {"failing_since": at},
		"decoded":     {"failing_since": at.Format(time.RFC3339)},
		"after JSON":  jsonRoundTrip(t, map[string]any{"failing_since": at}),
		"a pointer":   {"failing_since": &at},
		"zone offset": {"failing_since": at.In(time.FixedZone("x", 3600)).Format(time.RFC3339)},
	} {
		got, ok := extraAs[time.Time](metrics.Metric{Extra: extra}, "failing_since")
		if !ok || !got.Equal(at) {
			t.Errorf("%s: %v %v, want %v", label, got, ok, at)
		}
	}
	for label, extra := range map[string]map[string]any{
		"missing key":   {"other": 1},
		"nil map":       nil,
		"null":          {"failing_since": nil},
		"another shape": {"failing_since": map[string]any{"a": 1}},
		"not a time":    {"failing_since": "yesterday"},
	} {
		if got, ok := extraAs[time.Time](metrics.Metric{Extra: extra}, "failing_since"); ok {
			t.Errorf("%s: %v, want absent", label, got)
		}
	}
	// Counts: a typed int64 and a decoded float64 are the same number.
	for label, v := range map[string]any{"int64": int64(3), "float64": float64(3)} {
		if n, ok := extraAs[int64](metrics.Metric{Extra: map[string]any{"failed": v}}, "failed"); !ok || n != 3 {
			t.Errorf("%s: %d %v", label, n, ok)
		}
	}
	if _, ok := extraAs[int64](metrics.Metric{Extra: map[string]any{"failed": "three"}}, "failed"); ok {
		t.Error("a string read as a count")
	}
}

func jsonRoundTrip(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
