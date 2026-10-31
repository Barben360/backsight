package dashboard

import (
	"encoding/json"
	"html"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

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
	words := []map[string]string{views.NoDataWords, views.ServerErrorWords, views.OutcomeWords, views.SentWords, views.CheckWords, views.PathWords, views.KindWords}
	var codes []string
	for _, m := range words {
		for k, w := range m {
			// "ok" is also the plain status of a health check or a job.
			if k != "" && k != w && k != "ok" {
				codes = append(codes, k)
			}
		}
	}
	codes = append(codes, "too_few", "cant_judge", "too_early", "recent_weighted", "wilson", "not_filtered", "judged_28d", "no_candidates", "below_floor")
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
