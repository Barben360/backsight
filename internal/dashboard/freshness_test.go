package dashboard

import (
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
)

func TestLocaleFromTheRequest(t *testing.T) {
	for header, want := range map[string]string{"de-DE": "de", "fr;q=0.4, ja;q=0.9": "ja", "": "en", "xx": "en"} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Accept-Language", header)
		}
		if got := localeOf(r).Tag; got != want {
			t.Errorf("Accept-Language %q: tag %q, want %q", header, got, want)
		}
	}
}

func TestNumTakesTheLocale(t *testing.T) {
	num := funcs["num"].(func(format.Locale, int64) string)
	if got := num(format.English, 1234567); got != "1,234,567" {
		t.Errorf("English %q", got)
	}
	de := format.FromAcceptLanguage("de")
	if got := num(de, 1234567); got != "1.234.567" {
		t.Errorf("German %q", got)
	}
}

func TestRelAndClockFuncs(t *testing.T) {
	rel := funcs["rel"].(func(time.Time, any, *time.Location) format.Time)
	clock := funcs["clock"].(func(time.Time, any, *time.Location) string)
	now := time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)
	at := now.Add(-12 * time.Minute)
	if got := rel(now, at, nil); got.Text != "12 min ago" || got.Datetime != "2026-10-30T12:48:00Z" {
		t.Errorf("rel of a time %+v", got)
	}
	if got := rel(now, &at, nil); got.Text != "12 min ago" {
		t.Errorf("rel of a pointer %+v", got)
	}
	var none *time.Time
	for _, v := range []any{none, time.Time{}, nil, "x"} {
		if got := rel(now, v, nil); got != (format.Time{Text: "—"}) {
			t.Errorf("rel of %v: %+v", v, got)
		}
		if got := clock(now, v, nil); got != "—" {
			t.Errorf("clock of %v: %q", v, got)
		}
	}
	if got := clock(now, at, nil); got != "12:48" {
		t.Errorf("clock the same day %q", got)
	}
	if got := clock(now, now.Add(-24*time.Hour), nil); got != "Oct 29 13:00" {
		t.Errorf("clock another day %q", got)
	}
}

func TestScopeLineAndFreshness(t *testing.T) {
	h := dataHarness(t)
	ck := cookieHdr(h.login())
	doc := rawPage(t, h, ck, "/")
	scope := doc.first("div.scope")
	if scope == nil || !strings.HasPrefix(scope.text(), "Activity on this PC memory on hindsight.test:8888") || !strings.Contains(scope.text(), "updated") {
		t.Fatalf("scope line %+v", scope)
	}
	if strings.Contains(scope.text(), "not answering") || scope.first("span.chip") != nil {
		t.Errorf("a healthy page alarms: %q", scope.text())
	}
	tm := scope.first("time")
	gen, err := time.Parse(time.RFC3339, tm.attr("datetime"))
	if err != nil || !gen.Equal(h.clock.now()) {
		t.Errorf("updated time %v (%v)", tm.attr("datetime"), err)
	}
	if tm.attr("title") == "" || tm.text() == "" {
		t.Errorf("updated time %+v", tm)
	}
	stale := scope.first("a.chip-stale")
	if stale == nil || stale.attr("href") != "/" || stale.text() != "Updated over 5 min ago. Refresh." {
		t.Errorf("stale chip %+v", stale)
	}
	if got := rawPage(t, h, ck, "/quality?days=30").first("div.scope a.chip-stale").attr("href"); got != "/quality?days=30" {
		t.Errorf("stale chip href %q", got)
	}
	if p := rawPage(t, h, ck, "/quality?project=p1").first("div.scope"); !strings.Contains(p.text(), "project p1") {
		t.Errorf("filtered scope %q", p.text())
	}
	if strings.Contains(scope.text(), "project") {
		t.Errorf("unfiltered scope names a project: %q", scope.text())
	}

	h.srv.mu.Lock()
	h.srv.down = true
	h.srv.mu.Unlock()
	if d := rawPage(t, h, ck, "/system").first("div.scope"); !strings.Contains(d.text(), "not answering") {
		t.Errorf("server down: %q", d.text())
	}
}

func TestScopeLineDownSince(t *testing.T) {
	last := time.Date(2026, 10, 30, 8, 5, 0, 0, time.UTC)
	got := parseHTML(t, render(t, "system", func(d *PageData) {
		d.Frame.Generated = time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)
		d.Frame.ServerRead = &metrics.ServerState{LastOK: &last}
		d.Loc = time.UTC
	})).first("div.scope span.st-watch")
	if got == nil || got.text() != "not answering since 08:05" {
		t.Errorf("down since %+v", got)
	}
}

func TestCatchingSentence(t *testing.T) {
	for _, c := range []struct {
		fr   index.Freshness
		want string
	}{
		{index.Freshness{Partial: true, LagSeconds: 259200}, "Catching up: 3 days of history still being read"},
		{index.Freshness{Partial: true, LagSeconds: 7200}, "Catching up: 2 h of history still being read"},
		{index.Freshness{LagSeconds: 259200}, ""},
	} {
		if got := catchingOf(c.fr); got != c.want {
			t.Errorf("catchingOf(%+v) = %q, want %q", c.fr, got, c.want)
		}
	}
	const sentence = "Catching up: 3 days of history still being read"
	chip := parseHTML(t, render(t, "quality", func(d *PageData) { d.Frame.Catching = sentence })).first("div.scope span.chip")
	if chip == nil || chip.text() != sentence {
		t.Errorf("catching chip %+v", chip)
	}
	if parseHTML(t, render(t, "quality", nil)).first("span.chip") != nil {
		t.Error("a chip without a catch-up")
	}
}

func TestStaleChipIsCSSOnly(t *testing.T) {
	b, err := assetFS.ReadFile("assets/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	// The chip starts hidden and a zero-duration animation, delayed 300 s,
	// ends it visible: dropping either leaves it always or never shown.
	hidden := regexp.MustCompile(`\.chip-stale\s*\{[^}]*visibility:\s*hidden;[^}]*animation:\s*stale-show 0s linear 300s forwards;[^}]*\}`)
	if !hidden.MatchString(css) {
		t.Error("the stale chip is not hidden at first with the delayed animation")
	}
	if !regexp.MustCompile(`@keyframes stale-show\s*\{\s*to\s*\{\s*visibility:\s*visible;\s*\}\s*\}`).MatchString(css) {
		t.Error("the keyframe does not end in visibility: visible")
	}
	decl := regexp.MustCompile(`(?m)(?:^|[;{\s])(animation|transition)[\w-]*\s*:`)
	if n := len(decl.FindAllString(css, -1)); n != 1 {
		t.Errorf("%d animation or transition declarations, want only the stale chip's", n)
	}
	if strings.Contains(css, "transition") {
		t.Error("a transition in the stylesheet")
	}
}

func TestCatchingChipComesFromTheIndex(t *testing.T) {
	h := dataHarness(t)
	ck := cookieHdr(h.login())
	if c := rawPage(t, h, ck, "/").first("div.scope span.chip"); c != nil {
		t.Errorf("a healthy index shows %q", c.text())
	}
	h.ix.set(func(f *fakeIndex) { f.fresh = &index.Freshness{Partial: true, LagSeconds: 259200} })
	c := rawPage(t, h, ck, "/quality").first("div.scope span.chip")
	if c == nil || c.text() != "Catching up: 3 days of history still being read" {
		t.Errorf("catching chip %+v", c)
	}
}

// probe replaces the page (and the state page) with templates that write a
// number (French groups by spaces, which text() folds) in the request's locale, through the real handlers.
func probe(t *testing.T, h *harness, page string) {
	t.Helper()
	frame, err := fs.ReadFile(Templates(), "frame.html")
	if err != nil {
		t.Fatal(err)
	}
	spec := h.s.tpl.specs[page]
	body := "{{template \"head\" .}}{{template \"top\" .}}<p id=\"probe\">{{num .L 1234567}}</p>{{template \"foot\" .}}"
	mem := fstest.MapFS{
		"frame.html": {Data: frame},
		spec.File:    {Data: []byte("{{/* views: " + strings.Join(spec.Views, ", ") + " */}}\n" + body)},
		"state.html": {Data: []byte("{{/* views: none */}}\n" + body)},
	}
	if h.s.tpl.pages[page], err = parseWithFrame(mem, spec.File); err != nil {
		t.Fatal(err)
	}
	if h.s.tpl.state, err = parseWithFrame(mem, "state.html"); err != nil {
		t.Fatal(err)
	}
}

func TestLocaleReachesEveryPage(t *testing.T) {
	h, _, text := textHarness(t, "the text")
	probe(t, h, "quality")
	probe(t, h, textPageName)
	ck := h.login()
	for _, c := range []struct{ path, lang, want string }{
		{"/quality", "fr", "1 234 567"},
		{"/quality", "de-DE", "1.234.567"},
		{"/quality", "", "1,234,567"},
		{"/quality?days=0", "fr", "1 234 567"}, // a state page
		{"/quality?days=0", "", "1,234,567"},
		{text, "de", "1.234.567"},
		{text, "", "1,234,567"},
	} {
		hdr := cookieHdr(ck)
		if c.lang != "" {
			hdr = append(hdr, "Accept-Language", c.lang)
		}
		doc := parseHTML(t, body(t, h.do("GET", c.path, hdr...))) // a state page is a 400
		if p := doc.first("p#probe"); p == nil || p.text() != c.want {
			t.Errorf("%s with %q: %+v, want %q", c.path, c.lang, p, c.want)
		}
	}
}

func TestTextPageHasTheFreshnessOfAPage(t *testing.T) {
	var reads atomic.Int64
	h, _, text := textHarness(t, "the text", func(h *harness, c *Config) {
		base := c.Now
		c.Now = func() time.Time { return base().Add(time.Duration(reads.Add(1)*5) * time.Millisecond) }
	})
	doc := rawPage(t, h, cookieHdr(h.login()), text)
	scope := doc.first("div.scope")
	if scope == nil || !strings.Contains(scope.text(), "updated just now") || scope.first("time") == nil {
		t.Errorf("scope line %+v", scope)
	}
	foot := doc.first("footer.foot")
	ms := regexp.MustCompile(`Rendered at \d\d:\d\d in (\d+) ms$`).FindStringSubmatch(foot.first("span time").Parent.text())
	if ms == nil {
		t.Fatalf("footer %q: no render time", foot.text())
	}
	if n, _ := strconv.Atoi(ms[1]); n <= 0 || n%5 != 0 {
		t.Errorf("rendered in %d ms", n)
	}
}

func TestUpdatedTimeIsTheGeneratedInstant(t *testing.T) {
	gen := time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)
	tm := parseHTML(t, render(t, "quality", func(d *PageData) {
		d.Loc = time.UTC
		d.Frame.Generated, d.Frame.RenderedAt = gen, gen.Add(7*time.Minute)
	})).first("div.scope time")
	if tm == nil || tm.attr("datetime") != "2026-10-30T13:00:00Z" || tm.attr("title") != "2026-10-30 13:00 UTC" || tm.text() != "7 min ago" {
		t.Errorf("updated time %+v", tm)
	}
}
