package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

var (
	codeSpan = regexp.MustCompile(`(?s)<code>.*?</code>`)
	tagSpan  = regexp.MustCompile(`(?s)<[^>]*>`)
)

// textNodes are the pieces of text a reader sees in a page: the text
// between tags, unescaped and trimmed, without the contents of <code>.
func textNodes(page string) []string {
	var out []string
	for _, s := range tagSpan.Split(codeSpan.ReplaceAllString(page, ""), -1) {
		if s = strings.TrimSpace(html.UnescapeString(s)); s != "" {
			out = append(out, strings.Join(strings.Fields(s), " "))
		}
	}
	return out
}

// visibleText is the page's text as a reader sees it: its text nodes
// joined by single spaces; <code> contents and attribute values are gone.
func visibleText(page string) string { return strings.Join(textNodes(page), " ") }

func TestVisibleText(t *testing.T) {
	got := visibleText(`<p class="x" title="hidden">One <code>cmd --flag</code>&amp; two</p>
<a href="/secret">link</a>`)
	if got != "One &amp; two link" && got != "One & two link" {
		t.Errorf("visibleText: %q", got)
	}
	if strings.Contains(got, "cmd") || strings.Contains(got, "hidden") || strings.Contains(got, "secret") {
		t.Errorf("visibleText kept code or an attribute: %q", got)
	}
}

// pageMetrics are the metrics each page shows to everyone, by page.
var pageMetrics = map[string][]string{
	"overview": {"recall.hit_rate", "used.turn_rate", "declare.coverage", "verdict.wrong_rate", "context.per_session", "recall.latency", "pipeline.flow", "bank.size", "bank.growth"},
	"quality":  {"used.turn_rate", "declare.coverage", "recall.hit_rate", "recall.empty_reasons", "search.rate", "search.empty_rate"},
	"cost":     {"context.per_session", "context.per_prompt", "recall.latency", "search.latency", "extraction.tokens", "extraction.cost"},
	"bank":     {"bank.size", "bank.growth", "bank.by_project", "bank.consolidation", "notes.stored", "bank.churn", "stale.never_used", "stale.hidden", "stale.reported"},
	"pipeline": {"pipeline.flow", "retain.outcomes", "local.backlog", "server.queue", "server.extraction_delay", "hooks.failures", "jobs.last_run"},
	"system":   {"doctor.check", "server.reach", "jobs.last_run", "local.backlog"},
}

var devMetrics = []string{"used.share_confident", "used.rank", "used.by_path", "search.rescue_rate", "used.rubber_stamp", "recall.by_lang"}

// apiResult is the data of GET /api/v1/<view>.
func apiResult(t *testing.T, h *harness, view string) metrics.Result {
	t.Helper()
	ck := h.login()
	var env struct {
		Data metrics.Result `json:"data"`
	}
	r := h.do("GET", "/api/v1/"+view, cookieHdr(ck)...)
	if err := json.Unmarshal([]byte(body(t, r)), &env); err != nil || r.StatusCode != 200 {
		t.Fatalf("GET /api/v1/%s: %d, %v", view, r.StatusCode, err)
	}
	return env.Data
}

func TestPagesShowTheViewsNumbers(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	shown := 0
	for page, ids := range pageMetrics {
		path := "/" + page
		if page == "overview" {
			path = "/"
		}
		res := apiResult(t, h, page)
		r, b := pageGet(t, h, ck, path)
		if r.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, r.StatusCode)
		}
		text := visibleText(b)
		for _, id := range ids {
			def, ok := metrics.DefOf(id)
			if !ok {
				t.Fatalf("no metric %s", id)
			}
			if !strings.Contains(text, def.Question) {
				t.Errorf("%s lacks the question of %s: %q", page, id, def.Question)
			}
			m, found := res.Metrics[id]
			want := views.FormatValue(def, m)
			if id == "jobs.last_run" && m.Value != nil { // shown as how long ago
				want = views.Age(int64(*m.Value)) + " ago"
			}
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks the value of %s: %q", page, id, want)
			}
			if found && want != "—" {
				shown++
			}
		}
	}
	if shown == 0 {
		t.Error("no metric has a value in the fixtures: the test checks nothing")
	}
}

func TestOverviewSkeleton(t *testing.T) {
	h := dataHarness(t)
	res := apiResult(t, h, "overview")
	_, b := pageGet(t, h, h.login(), "/")
	text := visibleText(b)
	if res.Verdict == nil {
		t.Fatal("no verdict in the API")
	}
	for _, a := range views.AxisNames {
		word := views.AxisStateWords[res.Verdict.Axes[a.Key].State]
		if word == "" || !strings.Contains(text, a.Name+" "+word) {
			t.Errorf("overview lacks %q followed by %q", a.Name, word)
		}
	}
	for _, want := range []string{"last 7 days", "Needs attention", "Lately", "Memory at a glance"} {
		if !strings.Contains(text, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	for _, id := range []string{"recall.hit_rate", "used.turn_rate", "verdict.wrong_rate", "context.per_session", "recall.latency", "pipeline.flow"} {
		def, _ := metrics.DefOf(id)
		if !strings.Contains(text, def.Question) {
			t.Errorf("overview lacks the tile %s", def.Question)
		}
	}
	if len(res.Tables.Lately) == 0 {
		t.Fatal("no lately rows in the API")
	}
	if got := strings.Count(b, `href="/sessions/`); got != len(res.Tables.Lately) {
		t.Errorf("%d session links, want one per lately row (%d)", got, len(res.Tables.Lately))
	}
}

func TestServerDownMarksServerSections(t *testing.T) {
	old := metrics.ServerTimeout
	metrics.ServerTimeout = 20 * time.Millisecond
	t.Cleanup(func() { metrics.ServerTimeout = old })
	h := dataHarness(t)
	h.srv.set(func(f *fakeServer) { f.down = true })
	ck := h.login()
	r, b := pageGet(t, h, ck, "/bank")
	if r.StatusCode != 200 || !strings.Contains(visibleText(b), "memory server not answering") {
		t.Errorf("/bank: %d, %q", r.StatusCode, visibleText(b))
	}
	// Every metric the server provides says it is not answering, in its own section.
	checked := 0
	for page, ids := range pageMetrics {
		if page == "overview" || page == "system" {
			continue
		}
		_, b := pageGet(t, h, ck, "/"+page)
		for _, id := range ids {
			def, _ := metrics.DefOf(id)
			if def.Source != "server" {
				continue
			}
			sec := sectionOf(b, def.Question)
			if !strings.Contains(visibleText(sec), "memory server not answering") {
				t.Errorf("/%s: the section of %s is not marked: %q", page, id, visibleText(sec))
			}
			checked++
		}
	}
	if checked < 5 {
		t.Errorf("only %d server sections checked", checked)
	}
	// A page whose own views read nothing from the server does not cry wolf.
	_, q := pageGet(t, h, ck, "/quality")
	if strings.Contains(visibleText(q), "not answering") {
		t.Errorf("/quality: %q", visibleText(q))
	}
	res := apiResult(t, h, "overview")
	def, _ := metrics.DefOf("recall.hit_rate")
	m, ok := res.Metrics["recall.hit_rate"]
	want := views.FormatValue(def, m)
	_, o := pageGet(t, h, ck, "/")
	if !ok || want == "—" || !strings.Contains(visibleText(o), want) {
		t.Errorf("/ with the server down: recall.hit_rate %q (in API %v), want it shown", want, ok)
	}
}

func TestDevWidgetsOnlyInDevMode(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	get := func(prefs string) string {
		hdr := ck.Name + "=" + ck.Value
		if prefs != "" {
			hdr += "; " + prefsCookie + "=" + prefs
		}
		r := h.do("GET", "/quality", "Cookie", hdr)
		if r.StatusCode != 200 {
			t.Fatalf("/quality (%s): %d", prefs, r.StatusCode)
		}
		return body(t, r)
	}
	plain, dev := get(""), get("v1.system.1.0")
	for _, id := range devMetrics {
		def, _ := metrics.DefOf(id)
		if strings.Contains(visibleText(plain), def.Question) || strings.Contains(plain, "<code>"+id+"</code>") {
			t.Errorf("without the preference the page shows %s", id)
		}
		if !strings.Contains(visibleText(dev), def.Question) {
			t.Errorf("with the preference the page lacks the question of %s", id)
		}
	}
	if !strings.Contains(dev, "<code>used.rank</code>") {
		t.Error("with the preference the page does not name used.rank")
	}
	if strings.Contains(plain, "<code>recall.hit_rate</code>") || !strings.Contains(dev, "<code>recall.hit_rate</code>") {
		t.Error("the id of a metric shows only with the preference")
	}
}

func TestPagesShowNoJargon(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	// A key that is an everyday word may stand in a sentence; it must
	// not stand alone as a table cell or a line, where it is a code shown raw.
	words := []map[string]string{views.NoDataWords, views.ServerErrorWords, views.OutcomeWords, views.SentWords, views.CheckWords, views.PathWords, views.KindWords,
		views.JobWords, views.JobStatusWords, views.DoctorStatusWords, views.OperationWords, views.RuntimeWords, views.ModeWords}
	var codes []string
	for _, m := range words {
		for k, w := range m {
			// "ok" is also the plain status of a health check or a job.
			if k != "" && k != w && k != "ok" {
				codes = append(codes, k)
			}
		}
	}
	codes = append(codes, "too_few", "cant_judge", "too_early", "recent_weighted", "wilson", "not_filtered", "same_topic", "too_short", "topic_shift", "input_too_large", "invalid_output", "judged_28d", "no_candidates", "below_floor")
	for page := range pageMetrics {
		path := "/" + page
		if page == "overview" {
			path = "/"
		}
		r, b := pageGet(t, h, ck, path)
		if r.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, r.StatusCode)
		}
		nodes := textNodes(b)
		text := strings.Join(nodes, " ")
		for _, d := range metrics.Catalog {
			if strings.Contains(text, d.ID) {
				t.Errorf("%s shows the id %s", page, d.ID)
			}
		}
		for _, k := range codes {
			if strings.ContainsAny(k, "_0123456789") && strings.Contains(text, k) || slices.Contains(nodes, k) {
				t.Errorf("%s shows the code %q", page, k)
			}
		}
	}
}

// sectionOf is the <section> of an unescaped page whose heading is question.
func sectionOf(page, question string) string {
	for _, sec := range strings.Split(page, "<section>")[1:] {
		if strings.Contains(sec, "<h3>"+question+"</h3>") {
			sec, _, _ = strings.Cut(sec, "</section>")
			return sec
		}
	}
	return ""
}

func TestFreshInstallScopeLineHasNoFalseAlarm(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	for _, p := range []string{"/", "/quality", "/cost", "/pipeline", "/system", "/bank"} {
		if _, b := pageGet(t, h, ck, p); strings.Contains(visibleText(b), "not answering") {
			t.Errorf("%s with a server that answers: %q", p, visibleText(b))
		}
	}
}

// render executes the page on fresh golden data changed by edit.
func render(t *testing.T, name string, edit func(d *PageData)) string {
	t.Helper()
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	spec := specs[name]
	d := fixtureData(t, spec, false)
	if edit != nil {
		edit(&d)
	}
	var buf bytes.Buffer
	if err := tpls[name].ExecuteTemplate(&buf, spec.File, d); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return buf.String()
}

func TestScopeLine(t *testing.T) {
	gen := time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)
	at := gen.Add(-12 * time.Minute)
	for _, c := range []struct {
		label string
		scope metrics.Scope
		read  *metrics.ServerState
		want  []string
		not   []string
	}{
		{"no server", metrics.Scope{}, nil, []string{"no memory server set up"}, []string{"not answering"}},
		{"never recorded", metrics.Scope{Server: "srv:1"}, nil, []string{"memory on srv:1"}, []string{"not answering", "did not answer", "last answered"}},
		{"never recorded and not ok", metrics.Scope{Server: "srv:1", ServerOK: false}, nil, nil, []string{"not answering", "did not answer"}},
		{"recorded failure", metrics.Scope{Server: "srv:1", ServerOKAt: &at}, nil, []string{"did not answer 12 min ago"}, []string{"memory server not answering"}},
		{"recorded answer", metrics.Scope{Server: "srv:1", ServerOK: true, ServerOKAt: &at}, nil, []string{"last answered 12 min ago"}, []string{"not answering"}},
		{"read and failed", metrics.Scope{Server: "srv:1"}, &metrics.ServerState{}, []string{"memory server not answering"}, nil},
		{"read and answered", metrics.Scope{Server: "srv:1", ServerOK: true}, &metrics.ServerState{OK: true}, []string{"memory on srv:1"}, []string{"not answering"}},
	} {
		b := visibleText(render(t, "quality", func(d *PageData) {
			sc := c.scope
			d.Frame.Scope, d.Frame.ServerRead, d.Frame.Generated = &sc, c.read, gen
		}))
		for _, w := range c.want {
			if !strings.Contains(b, w) {
				t.Errorf("%s: lacks %q in %q", c.label, w, b)
			}
		}
		for _, w := range c.not {
			if strings.Contains(b, w) {
				t.Errorf("%s: has %q in %q", c.label, w, b)
			}
		}
	}
}

func TestMetricPartial(t *testing.T) {
	tpls, _, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	run := func(v MetricView) string {
		var buf bytes.Buffer
		if err := tpls["overview"].ExecuteTemplate(&buf, "metric", v); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	one := int64(12)
	val, delta := 4000.0, 500.0
	ctx, _ := metrics.DefOf("context.per_session")
	m := metrics.Metric{Value: &val, Den: &one, Status: metrics.StatusOK, Unit: "chars", Tags: []string{"not_filtered"},
		CI: &[2]float64{3000, 5000}, Delta: &delta, Previous: &val, Drill: "backsight stats cost --days 7",
		Series: []metrics.Point{{}, {}, {}}}
	v := MetricView{ID: ctx.ID, Def: ctx, M: m, Found: true}
	full := run(v)
	text := visibleText(full)
	for _, want := range []string{ctx.Question, views.FormatValue(ctx, m), "n = 12", "interval " + views.IntervalText(ctx, m), views.DeltaText(ctx, m), "this PC", "not filtered", "3 days"} {
		if want == "" || !strings.Contains(text, want) {
			t.Errorf("metric lacks %q in %q", want, text)
		}
	}
	if !strings.Contains(full, "<details><summary>CLI</summary><code>backsight stats cost --days 7</code></details>") {
		t.Errorf("the CLI disclosure: %s", full)
	}
	if strings.Contains(full, "<code>"+ctx.ID+"</code>") {
		t.Error("the id shows without the developer preference")
	}
	v.ShowID = true
	if !strings.Contains(run(v), "<code>"+ctx.ID+"</code>") {
		t.Error("the id does not show with the developer preference")
	}
	// A server metric, weekly series, nothing optional.
	bank, _ := metrics.DefOf("bank.size")
	bare := visibleText(run(MetricView{ID: bank.ID, Def: bank, M: metrics.Metric{Status: metrics.StatusOK}, Found: true}))
	for _, not := range []string{"n =", "interval", "this PC", "not filtered", "days", "CLI"} {
		if strings.Contains(bare, not) {
			t.Errorf("a bare server metric shows %q: %q", not, bare)
		}
	}
	if !strings.Contains(bare, "server") {
		t.Errorf("no server chip: %q", bare)
	}
	// The reason a number is missing shows once.
	nd := visibleText(run(MetricView{ID: bank.ID, Def: bank, Found: true,
		M: metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonServerDown}}))
	if got := strings.Count(nd, views.NoDataWords[metrics.ReasonServerDown]); got != 1 {
		t.Errorf("the no-data reason shows %d times: %q", got, nd)
	}
	// A ratio's interval is in its value, once.
	hit, _ := metrics.DefOf("recall.hit_rate")
	rv := 0.15
	rm := metrics.Metric{Value: &rv, Status: metrics.StatusOK, CI: &[2]float64{0.1, 0.2}}
	if got := strings.Count(visibleText(run(MetricView{ID: hit.ID, Def: hit, M: rm, Found: true})), "10–20"); got != 1 {
		t.Errorf("a ratio's interval shows %d times", got)
	}
}

// raw are values the stored data holds that a reader must never see as they are.
var raw = []string{"outdated", "batch_retain", "maintain", "partial", "warn", "docker", "remote", "bank-config", "tick"}

func hasWord(text, w string) bool {
	return regexp.MustCompile(`(^|[^A-Za-z_-])` + regexp.QuoteMeta(w) + `($|[^A-Za-z_-])`).MatchString(text)
}

func TestTablesShowWordsNotStoredValues(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cost := 0.5
	review := func(id string) []metrics.ReviewRow {
		return []metrics.ReviewRow{{ID: id, Project: "proj-" + id[:3], Shown: 21, LastShown: &now, Command: "backsight forget " + id}}
	}
	pages := map[string]func(d *PageData){
		"quality": func(d *PageData) {
			t := &d.V["quality"].Tables
			t.Reported = []metrics.ReportedRow{{MemoryID: "mem-aaaa1111bbbb", Verdict: "outdated", Session: "ses-cccc2222dddd", At: now}}
			t.OutcomeMix = []metrics.OutcomeDay{{Day: "2026-10-30", Used: 11, Ignored: 12, NotDeclared: 13, ShownOnly: 14, Pending: 15}}
		},
		"cost": func(d *PageData) {
			d.V["cost"].Tables.Extraction = []metrics.ExtractionRow{{Day: "2026-10-30", Operation: "batch_retain", Input: 101, Cached: 102, Output: 103, Cost: &cost}}
		},
		"bank": func(d *PageData) {
			d.V["bank"].Tables.Review = &metrics.Review{NeverUsed: review("mem-n1111111"), Hidden: review("mem-h2222222"), Reported: review("mem-r3333333")}
		},
		"pipeline": func(d *PageData) {
			t := &d.V["pipeline"].Tables
			t.FailedOperations = []metrics.OperationRow{{Type: "batch_retain", AgeMS: 3_600_000, Retries: 4}}
			t.Jobs = []metrics.JobRow{{Name: "maintain", Status: "partial", LastAt: now, MS: 90, AgeMS: 60_000}}
			t.Doctor = []metrics.DoctorRow{{ID: "bank-config", Status: "warn", Fix: "backsight doctor --fix"}}
		},
		"system": func(d *PageData) {
			t := &d.V["system"].Tables
			t.Jobs = []metrics.JobRow{{Name: "tick", Status: "partial", LastAt: now, MS: 91, AgeMS: 60_000}}
			t.Doctor = []metrics.DoctorRow{{ID: "bank-config", Status: "warn", Fix: "backsight doctor --fix"}}
			t.System = &metrics.SystemInfo{ServerVersion: "1.2.3", Runtime: "docker", Mode: "remote"}
		},
	}
	want := map[string][]string{
		"quality":  {"out of date", `<td>11</td>`, `<td>12</td>`, `<td>13</td>`, `<td>14</td>`, `<td>15</td>`, `href="/memories/mem-aaaa1111bbbb"`, `href="/sessions/ses-cccc2222dddd"`, "mem-aaaa"},
		"cost":     {"saving notes in batches", `<td>101</td>`, `<td>102</td>`, `<td>103</td>`, "0.50"},
		"bank":     {"mem-n111", "mem-h222", "mem-r333", "<code>backsight forget mem-n1111111</code>", "<code>backsight forget mem-h2222222</code>", "<code>backsight forget mem-r3333333</code>", `<td>21</td>`},
		"pipeline": {"saving notes in batches", `<td>4</td>`, "2026-10-30 13:00 CET", "1 h", "daily maintenance", "partly done", "memory settings", "warning", "<code>backsight doctor --fix</code>"},
		"system":   {"regular upkeep", "partly done", "warning", "version 1.2.3, running in Docker, mode on another machine"},
	}
	for name, edit := range pages {
		b := render(t, name, func(d *PageData) { edit(d) })
		text := visibleText(b)
		for _, w := range want[name] {
			if !strings.Contains(b, w) && !strings.Contains(text, w) {
				t.Errorf("%s lacks %q", name, w)
			}
		}
		for _, r := range raw {
			if hasWord(text, r) {
				t.Errorf("%s shows the stored value %q: %q", name, r, text)
			}
		}
	}
}

func TestOverviewAttentionAndExtras(t *testing.T) {
	b := render(t, "overview", func(d *PageData) {
		r := d.V["overview"]
		r.Attention = []metrics.Item{{Text: "Something is off", Why: "Because of a thing", Command: "backsight doctor"}}
		r.AttentionMore = 3
	})
	for _, w := range []string{`<a href="/system">3 more</a>`, "<details><summary>Why</summary><p>Because of a thing</p></details>", "<code>backsight doctor</code>"} {
		if !strings.Contains(b, w) {
			t.Errorf("overview lacks %q", w)
		}
	}
}

func TestCostDeveloperGate(t *testing.T) {
	def, _ := metrics.DefOf("extraction.per_session")
	for _, dev := range []bool{false, true} {
		b := render(t, "cost", func(d *PageData) { d.Frame.Prefs.Dev = dev })
		if got := strings.Contains(visibleText(b), def.Question); got != dev {
			t.Errorf("cost with dev %v shows extraction.per_session: %v", dev, got)
		}
	}
}

func TestSystemBlockIsShown(t *testing.T) {
	b := visibleText(render(t, "system", nil))
	for _, w := range []string{"Memory server", "Reach", "Backup", "Updates", "Activity index"} {
		if !strings.Contains(b, w) {
			t.Errorf("system lacks %q", w)
		}
	}
}

func TestExtraBreakdownsAreShown(t *testing.T) {
	set := func(r *metrics.Result, id string, extra map[string]any) {
		m := r.Metrics[id]
		m.Status = metrics.StatusOK
		m.Extra = extra
		r.Metrics[id] = m
	}
	pathStat := func(num, den int64, v float64) map[string]any {
		return map[string]any{"num": num, "den": den, "value": v}
	}
	check := func(page string, edit func(d *PageData), want ...string) {
		t.Helper()
		text := visibleText(render(t, page, edit))
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s lacks %q in %q", page, w, text)
			}
		}
	}
	check("quality", func(d *PageData) {
		r := d.V["quality"]
		set(r, "recall.empty_reasons", map[string]any{"empty": map[string]int64{"no_candidates": 4}, "skips": map[string]int64{"gap": 2}})
		set(r, "used.by_path", map[string]any{"by_path": map[string]any{"prompt": pathStat(3, 5, 0.6), "search": pathStat(1, 2, 0.5)}})
		set(r, "recall.by_lang", map[string]any{"by_lang": map[string]any{"fr": pathStat(1, 4, 0.25), "unknown": pathStat(2, 2, 1)}})
		set(r, "used.rank", map[string]any{"hit_at_1": 0.5, "hit_at_3": 0.75, "histogram": map[string]int64{"1": 6, "2": 2, "unranked": 1}})
	}, "why: no candidates 4", "added to the prompt: 3 of 5 (60 %)", "Claude's search: 1 of 2 (50 %)", "fr: 1 of 4 (25 %)",
		"language not detected: 2 of 2 (100 %)", "the best-ranked memory was used in 50 % of turns", "one of the first three was used in 75 % of turns",
		"rank 1 6, rank 2 2", "not ranked 1")
	check("bank", func(d *PageData) {
		set(d.V["bank"], "bank.growth", map[string]any{"world": int64(5), "experience": int64(2), "observation": int64(1), "days_covered": 30})
	}, "added over the period: 5 facts about you and your work, 2 facts from sessions, 1 summaries", "the server keeps only 30 days of history")
	check("pipeline", func(d *PageData) {
		r := d.V["pipeline"]
		set(r, "retain.outcomes", map[string]any{"by_status": map[string]int64{"ok": 7, "failed": 2, "queued": 0}})
		set(r, "local.backlog", map[string]any{"notes_queued": int64(2), "sessions_waiting": int64(1), "quarantined": int64(0), "oldest_ms": int64(3_600_000)})
	}, "by result: failed 2, sent 7", "2 notes and 1 session wait on this PC")
	check("cost", func(d *PageData) {
		set(d.V["cost"], "context.per_session", map[string]any{"p50": 1200.0, "p95": 9000.0})
	}, "typical 1,200 chars, high 9,000 chars")
}

func TestRealBreakdownsAreShown(t *testing.T) {
	h := dataHarness(t)
	res, st, err := h.s.data.result(context.Background(), "quality", views.Params{Limit: 50, Dev: true}, "")
	if err != nil || st != stateReady {
		t.Fatalf("quality: state %d, %v", st, err)
	}
	def, _ := metrics.DefOf("recall.by_lang")
	lines := views.ExtraLines(def, res.Metrics["recall.by_lang"])
	if len(lines) == 0 {
		t.Fatal("no language lines from the computed metric: the shape of langStat changed")
	}
	for _, l := range lines {
		if !regexp.MustCompile(`: \d+ of \d+ \(\d+ %\)$`).MatchString(l) {
			t.Errorf("line %q", l)
		}
	}
}

func TestSkipReasonsAndHookKindsAreWordsOnPages(t *testing.T) {
	skips := map[string]int64{}
	for _, r := range indexgen.PromptReasons {
		skips[r] = 1
	}
	kinds := map[string]int64{}
	rawNames := append(slices.Clone(indexgen.PromptReasons), "panic", "fatal", "timeout", "killed", "exit", "invalid_output", "input_too_large", "spawn")
	for _, k := range rawNames[len(indexgen.PromptReasons):] {
		kinds[k] = 1
	}
	set := func(r *metrics.Result, id string, extra map[string]any) {
		m := r.Metrics[id]
		m.Status = metrics.StatusOK
		m.Extra = extra
		r.Metrics[id] = m
	}
	for page, edit := range map[string]func(*PageData){
		"quality": func(d *PageData) {
			set(d.V["quality"], "recall.empty_reasons", map[string]any{"empty": map[string]int64{}, "skips": skips})
		},
		"overview": func(d *PageData) {
			set(d.V["overview"], "recall.empty_reasons", map[string]any{"empty": map[string]int64{}, "skips": skips})
		},
		"pipeline": func(d *PageData) {
			set(d.V["pipeline"], "hooks.failures", map[string]any{"by_kind": kinds, "last_24h": kinds})
		},
	} {
		text := visibleText(render(t, page, edit))
		if !strings.Contains(text, "skipped") && page != "pipeline" || !strings.Contains(text, "in the last 24 h") && page == "pipeline" {
			if page == "overview" {
				continue // the overview shows no such metric
			}
			t.Errorf("%s shows no breakdown: %q", page, text)
		}
		for _, r := range rawNames {
			if hasWord(text, r) {
				t.Errorf("%s shows the stored name %q", page, r)
			}
		}
	}
}
