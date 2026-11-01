package dashboard

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

const wantCSP = "default-src 'none'; style-src 'self'; img-src 'self' data:; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func TestStatePagesFollowTheDesign(t *testing.T) {
	cases := []struct {
		name    string
		h       func(t *testing.T) *harness
		path    string
		status  int
		h1, sub string
		code    string // the command shown, "" for none
		retry   string // the Try again link's href, "" for none
		icon    string // the status icon's class, "" for none
	}{
		{"building", func(t *testing.T) *harness {
			return newDataHarness(t, withIndex(func(f *fakeIndex) { f.openGate = make(chan struct{}) }), withServer(), withNeedsBuild())
		}, "/", 503, "Preparing the dashboard.", "Building the metrics index from this PC's history. This page refreshes every 2 seconds.", "", "", ""},
		{"updating", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.openErrs = []error{index.ErrUpdateInProgress, index.ErrUpdateInProgress} })
		}, "/quality?days=7", 503, "An update is being applied.", "Try again in a minute.", "", "/quality?days=7", "st-learn"},
		{"updated", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.side = true })
		}, "/", 503, "backsight was updated.", "Stop this dashboard (Ctrl+C in its terminal) and start it again:", "backsight dashboard", "", "st-ok"},
		{"timeout", func(t *testing.T) *harness {
			old := renderTimeout
			renderTimeout = 20 * time.Millisecond
			t.Cleanup(func() { renderTimeout = old })
			h := dataHarness(t)
			h.ix.set(func(f *fakeIndex) { f.block = make(chan struct{}) })
			return h
		}, "/", 503, "The page took too long.", "Try again.", "", "/", "st-watch"},
		{"not found", dataHarness0, "/sessions/zzzzzzzz", 404, "What happened in session zzzzzzzz?", `No session "zzzzzzzz" in the metrics index.`, "", "", ""},
		{"bad request", dataHarness0, "/quality?days=0", 400, views.Questions["quality"], "--days: a whole number from 1 to 365.", "", "", ""},
		{"broken", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.catchUpErr = errors.New("disk exploded at /home/x/secret.db") })
		}, "/", 500, views.Questions["overview"], "The view failed: run backsight doctor.", "", "", "st-watch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := c.h(t)
			r := h.do("GET", c.path, cookieHdr(h.login())...)
			raw := body(t, r)
			doc := parseHTML(t, raw)
			if r.StatusCode != c.status {
				t.Fatalf("status %d, want %d", r.StatusCode, c.status)
			}
			for k, want := range map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store", "Content-Security-Policy": wantCSP} {
				if got := r.Header.Values(k); len(got) != 1 || got[0] != want {
					t.Errorf("%s %q, want %q", k, got, want)
				}
			}
			if got, want := r.Header.Get("Retry-After"), map[bool]string{true: "2", false: ""}[c.name == "building"]; got != want {
				t.Errorf("Retry-After %q, want %q", got, want)
			}
			// The build, update and timeout pages are a centred card; the pages
			// that answer a question keep the frame, whose brand holds both logos.
			solo := c.name != "not found" && c.name != "bad request"
			main, box := doc.first("main#main"), doc.first("main.solo .solo-box")
			if main == nil || solo != (box != nil) || solo != (doc.first("main.solo") != nil) || solo == (doc.first("header.topbar") != nil) {
				t.Fatalf("solo %t: main %v, card %v, topbar %v in %q", solo, main, box, doc.first("header.topbar"), raw)
			}
			if !solo {
				box = doc.first("a.brand")
			}
			if hs := main.find("h1"); len(hs) != 1 || hs[0].text() != c.h1 || hs[0].Parent.attr("class") != "page-head" {
				t.Errorf("h1 %v, want exactly %q in .page-head", hs, c.h1)
			}
			// Kind decides the shape: the framed pages carry their kind.
			wantClass := map[string]string{"not found": "sub state-notfound", "bad request": "sub state-bad"}[c.name]
			if wantClass == "" {
				wantClass = "sub"
			}
			if ps := main.find("p.sub"); len(ps) != 1 || ps[0].attr("class") != wantClass {
				t.Errorf("sentence class %v, want %q", ps, wantClass)
			}
			if ps := main.find("p.sub"); len(ps) != 1 || ps[0].text() != c.sub {
				t.Errorf("sentence %v, want %q", ps, c.sub)
			}
			for _, cl := range []string{"logo-l", "logo-d"} {
				if imgs := box.find("img." + cl); len(imgs) != 1 || imgs[0].attr("alt") != "backsight" || imgs[0].attr("width") != "24" || imgs[0].attr("height") != "24" || imgs[0].attr("src") != assetPath(map[string]string{"logo-l": "logo-light.svg", "logo-d": "logo-dark.svg"}[cl]) {
					t.Errorf("logo %s: %v", cl, imgs)
				}
			}
			if refresh := doc.find(`meta[http-equiv=refresh]`); c.name == "building" {
				if len(refresh) != 1 || refresh[0].attr("content") != "2" {
					t.Errorf("refresh %v, want one with content 2", refresh)
				}
			} else if len(refresh) != 0 {
				t.Errorf("refresh on a page that must not reload: %v", refresh)
			}
			if len(doc.find("progress")) != 0 || len(doc.find(".progress")) != 0 {
				t.Error("a progress bar: no progress figure reaches the dashboard")
			}
			codes := main.find("code")
			if c.code == "" && len(codes) != 0 || c.code != "" && (len(codes) != 1 || codeText(codes[0]) != c.code) {
				t.Errorf("code %v, want %q", codes, c.code)
			}
			if c.code != "" {
				if b := main.find("div.cmd button.copy[hidden]"); len(b) != 1 {
					t.Errorf("copy buttons %v, want one rendered hidden", b)
				}
			}
			links := main.find("a.btn")
			switch {
			case c.retry == "" && len(links) != 0:
				t.Errorf("links %v, want none", links)
			case c.retry != "" && (len(links) != 1 || links[0].attr("href") != c.retry || links[0].text() != "Try again" || links[0].first("span.i.i-refresh") == nil):
				t.Errorf("links %v, want Try again to %q", links, c.retry)
			}
			ico := main.find("span.v-ico")
			switch {
			case c.icon == "" && len(ico) != 0:
				t.Errorf("icon %v, want none", ico)
			case c.icon != "" && (len(ico) != 1 || ico[0].attr("class") != "v-ico "+c.icon || ico[0].attr("aria-hidden") != "true"):
				t.Errorf("icon %v, want %s", ico, c.icon)
			}
			if doc.first("footer.foot") == nil {
				t.Error("the footer is gone")
			}
		})
	}
}

func dataHarness0(t *testing.T) *harness { return dataHarness(t) }

func TestUnauthorizedPage(t *testing.T) {
	h := newHarness(t)
	r := h.do("GET", "/")
	raw := body(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", r.StatusCode)
	}
	for k, want := range map[string]string{"Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store", "Content-Security-Policy": wantCSP} {
		if got := r.Header.Values(k); len(got) != 1 || got[0] != want {
			t.Errorf("%s %q, want %q", k, got, want)
		}
	}
	if len(r.Cookies()) != 0 {
		t.Errorf("the 401 sets cookies: %v", r.Cookies())
	}
	doc := parseHTML(t, raw)
	main := doc.first("main.solo")
	if main == nil {
		t.Fatalf("no main.solo in %q", raw)
	}
	if hs := main.find("h1"); len(hs) != 1 || hs[0].text() != "This link has expired." {
		t.Errorf("h1 %v", hs)
	}
	ps := main.find("p")
	if len(ps) != 2 || ps[0].text() != "Run this in a terminal on this PC for a new one:" ||
		ps[1].text() != "Each link opens the dashboard once. It only answers on this PC (127.0.0.1) and forgets you after 2 hours idle." || ps[1].attr("class") != "xs muted" {
		t.Errorf("paragraphs %v", ps)
	}
	if codes := main.find("div.cmd code"); len(codes) != 1 || codeText(codes[0]) != "backsight dashboard link" || len(main.find("div.cmd button.copy[hidden]")) != 1 {
		t.Errorf("command %v", codes)
	}
	for _, c := range []struct{ class, file string }{{"logo-l", "logo-light.svg"}, {"logo-d", "logo-dark.svg"}} {
		imgs := main.find("img." + c.class)
		if len(imgs) != 1 || imgs[0].attr("src") != assetPath(c.file) || imgs[0].attr("width") != "64" || imgs[0].attr("height") != "64" || imgs[0].attr("alt") != "backsight" {
			t.Errorf("logo %s: %v", c.class, imgs)
		}
	}
	var sheets, scripts []string
	for _, l := range doc.find("link[rel=stylesheet]") {
		sheets = append(sheets, l.attr("href"))
	}
	for _, s := range doc.find("script") {
		scripts = append(scripts, s.attr("src"))
		if !s.has("defer") {
			t.Errorf("script %v is not deferred", s)
		}
	}
	if len(sheets) != 2 || sheets[0] != assetPath("tokens.css") || sheets[1] != assetPath("dashboard.css") || len(scripts) != 1 || scripts[0] != assetPath("copy.js") {
		t.Errorf("stylesheets %q, scripts %q", sheets, scripts)
	}
	if strings.Contains(raw, "code=") || strings.Contains(raw, h.host()) {
		t.Errorf("the 401 page carries request data: %q", raw)
	}
}

// The feedback-off sentence reads as one sentence: its closing bracket and
// full stop come after the setting's name.
func TestFeedbackOffNoDataRendersItsClosingBracket(t *testing.T) {
	d, _ := metrics.DefOf("recall.hit_rate")
	v := MetricView{ID: d.ID, Def: d, Found: true, M: metrics.Metric{Status: metrics.StatusNoData, Reason: metrics.ReasonFeedbackOff}}
	nd, ok := noDataOf(v, Frame{Window: "last 7 days"}, format.Locale{}, time.UTC)
	if !ok {
		t.Fatal("feedback off has no sentence")
	}
	tpl := template.Must(template.New("probe").Funcs(funcs).Option("missingkey=error").ParseFS(Templates(), "frame.html"))
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "nodata", nd); err != nil {
		t.Fatal(err)
	}
	doc := parseHTML(t, buf.String())
	sentence := doc.first("div.empty span")
	if sentence == nil {
		t.Fatalf("no sentence in %q", buf.String())
	}
	if got, want := codeText(sentence), "Memory feedback is off (feedback.enabled)."; got != want {
		t.Errorf("sentence %q, want %q", got, want)
	}
	if tail := doc.first("div.empty .xs.muted"); tail == nil || tail.text() != "Quality numbers need it. Found, Cost and Flowing still work." {
		t.Errorf("tail %v", tail)
	}
}

// The text page never refreshes: its first-build page says to open the text
// again, and its update page has the same heading as the others.
func TestTextPageStatePagesWords(t *testing.T) {
	path := "/memories/mem-a1b2c3d4/text"
	h := newDataHarness(t, withIndex(func(f *fakeIndex) { f.openGate = make(chan struct{}) }), withServer(), withNeedsBuild())
	r := h.do("GET", path, cookieHdr(h.login())...)
	doc := parseHTML(t, body(t, r))
	if r.StatusCode != 503 || len(doc.find("meta[http-equiv=refresh]")) != 0 {
		t.Errorf("first build: %d, refresh %v", r.StatusCode, doc.find("meta[http-equiv=refresh]"))
	}
	if ps := doc.find("main p.sub"); len(ps) != 1 || ps[0].text() != "Building the metrics index from this PC's history. Open the text again in a few seconds." {
		t.Errorf("first-build sentence %v", ps)
	}
	h = dataHarness(t, func(f *fakeIndex) { f.side = true })
	r = h.do("GET", path, cookieHdr(h.login())...)
	doc = parseHTML(t, body(t, r))
	if hs := doc.find("main h1"); r.StatusCode != 503 || len(hs) != 1 || hs[0].text() != updatedPage || updatedPage != "backsight was updated." {
		t.Errorf("updated: %d, %v", r.StatusCode, hs)
	}
}
