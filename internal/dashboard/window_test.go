package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

func TestCustomWindowRedirects(t *testing.T) {
	h := dataHarness(t)
	hdr := cookieHdr(h.login())
	r := h.do("GET", "/quality?project=p1&from=2026-10-01&to=2026-10-07", hdr...)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/quality?project=p1&since=2026-10-01&until=2026-10-08" {
		t.Errorf("%d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// The window keys and the offset are dropped, the others kept.
	r = h.do("GET", "/cost?days=7&offset=50&tz=UTC&since=2026-01-01&from=2026-10-30&to=2026-10-30", hdr...)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/cost?tz=UTC&since=2026-10-30&until=2026-10-31" {
		t.Errorf("days dropped: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}

// pickerOf is the desktop picker of path's page, which must answer 200.
func pickerOf(t *testing.T, h *harness, hdr []string, path string) (*node, *node) {
	t.Helper()
	doc := rawPage(t, h, hdr, path)
	var desk *node
	for _, n := range doc.find("nav.seg") {
		if n.attr("aria-label") == "Window" {
			desk = n.first("details.wpick")
		}
	}
	if desk == nil {
		t.Fatalf("%s: no details.wpick in the window links", path)
	}
	return doc, desk
}

func TestCustomWindowRefusals(t *testing.T) {
	h := dataHarness(t)
	hdr := cookieHdr(h.login())
	cases := map[string]string{
		"from=2026-10-07&to=2026-10-01":         "“To” (Oct 1) is before “From” (Oct 7). Pick a “To” on or after Oct 7.",
		"from=2006-10-01&to=2026-10-07":         "This PC keeps 13 months: pick Oct 1, 2025 or later.",
		"from=2025-09-30&to=2026-10-07":         "This PC keeps 13 months: pick Oct 1, 2025 or later.",
		"from=2026-10-01&to=2026-10-31":         "Oct 31 is in the future: pick today or earlier.",
		"from=2026-02-30&to=2026-10-07":         "Pick both days.",
		"from=2026-10-01&to=":                   "Pick both days.",
		"from=2026-10-01":                       "Pick both days.",
		"to=2026-10-01":                         "Pick both days.",
		"from=2026-10-01&to=2026-10-07T10:00":   "Pick both days.",
		"from=2026-10-01&to=2026-10-07%2010:00": "Pick both days.",
	}
	for q, want := range cases {
		r := h.do("GET", "/quality?project=p1&"+q, hdr...)
		raw := body(t, r)
		if r.StatusCode != 200 {
			t.Errorf("%s: %d, want the page", q, r.StatusCode)
			continue
		}
		doc := parseHTML(t, raw)
		var desk *node
		for _, n := range doc.find("nav.seg") {
			if n.attr("aria-label") == "Window" {
				desk = n.first("details.wpick")
			}
		}
		if desk == nil || !desk.has("open") {
			t.Errorf("%s: the picker is not open", q)
			continue
		}
		e := desk.first("p.ferr")
		if e == nil || e.attr("id") != "werr" || e.attr("role") != "alert" || strings.TrimSpace(e.text()) != want {
			t.Errorf("%s: error %v, want %q", q, e, want)
			if e != nil {
				t.Logf("got %q", e.text())
			}
			continue
		}
		bad := 0
		for _, in := range desk.find("input[type=date]") {
			if in.attr("aria-describedby") != "werr" {
				t.Errorf("%s: %s not described by the error", q, in.attr("name"))
			}
			if in.attr("aria-invalid") == "true" {
				bad++
			}
		}
		if bad == 0 {
			t.Errorf("%s: no field aria-invalid", q)
		}
		// The previous window: the 7-day link is current.
		if got := current(doc, `nav.seg`, "true"); len(got) == 0 || got[0] != "7 d" {
			t.Errorf("%s: window links current %v, want 7 d", q, got)
		}
	}
	// The dates are kept.
	_, desk := pickerOf(t, h, hdr, "/quality?from=2026-10-07&to=2026-10-01")
	for _, in := range desk.find("input[type=date]") {
		want := map[string]string{"from": "2026-10-07", "to": "2026-10-01"}[in.attr("name")]
		if in.attr("value") != want {
			t.Errorf("field %s = %q, want %q", in.attr("name"), in.attr("value"), want)
		}
	}
	// A key given twice: the usage page.
	for _, q := range []string{"from=2026-10-01&from=2026-10-02&to=2026-10-07", "from=2026-10-01&to=2026-10-07&to=2026-10-08"} {
		if r := h.do("GET", "/quality?"+q, hdr...); r.StatusCode != 400 {
			t.Errorf("%s: %d, want 400", q, r.StatusCode)
		}
	}
	// Hostile input: never a 500, a redirect always to the page itself.
	for _, q := range []string{
		"from=2026-10-01&to=2026-10-07&project=%2F%2Fevil.example",
		"from=%00&to=%ff", "from=99999-01-01&to=99999-01-02", "from=-2026-10-01&to=2026-10-07",
		"from=2026-10-01&to=2026-10-07&days=7", "from=2026-10-01&to=2026-10-07&nope=1",
	} {
		r := h.do("GET", "/cost?"+q, hdr...)
		if r.StatusCode >= 500 {
			t.Errorf("%s: %d", q, r.StatusCode)
		}
		if loc := r.Header.Get("Location"); r.StatusCode == 303 && !strings.HasPrefix(loc, "/cost?") {
			t.Errorf("%s: redirect to %q", q, loc)
		}
	}
	if r := h.do("GET", "/cost?from=2026-10-01&to=2026-10-07&nope=1", hdr...); r.StatusCode != 400 {
		t.Errorf("an unknown key: %d, want 400", r.StatusCode)
	}
}

func TestCustomWindowSummary(t *testing.T) {
	h := dataHarness(t)
	hdr := cookieHdr(h.login())
	_, desk := pickerOf(t, h, hdr, "/quality")
	if s := desk.first("summary"); s == nil || !strings.HasPrefix(s.text(), "Custom") || s.attr("aria-label") != "Custom window: pick the days" || strings.Contains(desk.attr("class"), "is-current") {
		t.Errorf("default summary: %v class %q", s, desk.attr("class"))
	}
	doc, desk := pickerOf(t, h, hdr, "/quality?since=2026-10-01&until=2026-10-08")
	s := desk.first("summary")
	if s == nil || !strings.HasPrefix(s.text(), "Oct 1 – Oct 7") || s.attr("aria-label") != "Oct 1 – Oct 7, custom window, change it" {
		t.Errorf("applied summary: %v", s)
	}
	if !strings.Contains(" "+desk.attr("class")+" ", " is-current ") {
		t.Errorf("applied: class %q, want is-current", desk.attr("class"))
	}
	if ph := doc.first("div.page-head span.muted"); ph == nil || ph.text() != "window: Oct 1 to Oct 7" {
		t.Errorf("page head %v", ph)
	}
	if got := current(doc, "nav.seg", "true"); len(got) != 0 {
		t.Errorf("window links current %v, want none", got)
	}
	// Not midnights: the plain summary.
	_, desk = pickerOf(t, h, hdr, "/quality?since=2026-10-01T10:00Z&until=2026-10-08T00:00Z&tz=Europe/Paris")
	if s := desk.first("summary"); s == nil || !strings.HasPrefix(s.text(), "Custom") || strings.Contains(desk.attr("class"), "is-current") {
		t.Errorf("not midnights: %v", s)
	}
}

func TestPickerWorksWithoutScripts(t *testing.T) {
	h := dataHarness(t)
	hdr := cookieHdr(h.login())
	_, desk := pickerOf(t, h, hdr, "/quality?project=p1&dev=1&days=30&offset=50")
	form := desk.first("form")
	if form == nil || form.attr("method") != "get" || form.attr("action") != "/quality" {
		t.Fatalf("form %v", form)
	}
	dates := form.find("input[type=date]")
	if len(dates) != 2 {
		t.Fatalf("%d date fields, want 2", len(dates))
	}
	for _, in := range form.find("label input[type=date]") {
		if in.attr("min") != "2025-10-01" || in.attr("max") != "2026-10-30" || in.has("aria-describedby") {
			t.Errorf("%s: min %q max %q describedby %q", in.attr("name"), in.attr("min"), in.attr("max"), in.attr("aria-describedby"))
		}
	}
	if n := len(form.find("label input[type=date]")); n != 2 {
		t.Errorf("%d date fields in a label, want 2", n)
	}
	if b := form.first("button"); b == nil || b.attr("type") != "submit" {
		t.Errorf("no submit button: %v", b)
	}
	hidden := map[string]string{}
	for _, in := range form.find("input[type=hidden]") {
		hidden[in.attr("name")] = in.attr("value")
	}
	if len(hidden) != 2 || hidden["project"] != "p1" || hidden["dev"] != "1" {
		t.Errorf("hidden inputs %v, want project and dev", hidden)
	}
	if hint := form.first("p.fhint"); hint == nil || hint.text() != "Both days are included. Up to 13 months back, the history this PC keeps." {
		t.Errorf("hint %v", hint)
	}
}

func TestPickerInTheMenu(t *testing.T) {
	h := dataHarness(t)
	hdr := cookieHdr(h.login())
	r := h.do("GET", "/quality?from=2026-10-07&to=2026-10-01", hdr...)
	raw := body(t, r)
	doc := parseHTML(t, raw)
	menu := doc.first("details.navmenu")
	if menu == nil {
		t.Fatal("no Menu")
	}
	mp := menu.first("details.wpick")
	if mp == nil || mp.first("form") == nil || len(mp.find("label input[type=date]")) != 2 || mp.first("button") == nil {
		t.Fatalf("the Menu holds no picker form: %v", mp)
	}
	e := mp.first("p.ferr")
	if e == nil || e.attr("id") != "werr-m" || e.attr("role") != "alert" {
		t.Errorf("Menu error %v", e)
	}
	for _, in := range mp.find("input[type=date]") {
		if in.attr("aria-describedby") != "werr-m" {
			t.Errorf("Menu field %s described by %q", in.attr("name"), in.attr("aria-describedby"))
		}
	}
	ids := map[string]int{}
	for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(raw, -1) {
		ids[m[1]]++
	}
	for id, n := range ids {
		if n > 1 {
			t.Errorf("id %q used %d times", id, n)
		}
	}
	if ids["werr"] != 1 || ids["werr-m"] != 1 {
		t.Errorf("ids werr %d werr-m %d, want 1 each", ids["werr"], ids["werr-m"])
	}
}
