package dashboard

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestParsePrefs(t *testing.T) {
	def := Prefs{Theme: "system"}
	cases := map[string]Prefs{
		"v1.dark.1.0":                {Theme: "dark", Dev: true},
		"v1.light.0.1":               {Theme: "light", Refresh: true},
		"v2.dark.1.0":                def,
		"v1.blue.0.0":                def,
		"v1.dark.2.0":                def,
		"v1.dark.0.2":                def,
		"v1.dark.0":                  def,
		"v1.dark.0.0.0":              def,
		"v1.dark.1.0.compact.1":      {Theme: "dark", Dev: true, Compact: true, Keys: true},
		"v1.light.0.1.comfortable.0": {Theme: "light", Refresh: true},
		"":                           def,
	}
	for in, want := range cases {
		if got := parsePrefs(in); got != want {
			t.Errorf("parsePrefs(%q) = %+v, want %+v", in, got, want)
		}
	}
	if got := (Prefs{Theme: "light", Dev: true, Refresh: true}).cookie(); got != "v1.light.1.1.comfortable.0" {
		t.Errorf("cookie() = %q", got)
	}
}

// withPrefs is the header of a session plus a preferences cookie.
func withPrefs(ck *http.Cookie, v string) []string {
	return []string{"Cookie", ck.Name + "=" + ck.Value + "; " + prefsCookie + "=" + v}
}

func TestPrefs(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()

	r := h.do("GET", "/prefs?theme=dark&back=/cost", cookieHdr(ck)...)
	const want = "backsight_prefs=v1.dark.0.0.comfortable.0; Path=/; Max-Age=31536000; HttpOnly; SameSite=Strict"
	if r.StatusCode != 303 || r.Header.Get("Location") != "/cost" || r.Header.Get("Set-Cookie") != want {
		t.Errorf("%d %q %q", r.StatusCode, r.Header.Get("Location"), r.Header["Set-Cookie"])
	}

	// The other settings are kept from the request's cookie.
	r = h.do("GET", "/prefs?refresh=1&back=/cost", withPrefs(ck, "v1.light.1.0")...)
	if got := r.Header.Get("Set-Cookie"); !strings.HasPrefix(got, "backsight_prefs=v1.light.1.1.comfortable.0;") {
		t.Errorf("Set-Cookie %q", got)
	}

	for _, c := range []struct{ cookie, want string }{
		{"v1.dark.0.0", `<html data-theme="dark"`},
		{"v1.system.0.1", `<meta http-equiv="refresh" content="60">`},
		{"v1.system.1.0", `Developer <span class="caret" aria-hidden="true">▾</span></summary>`},
	} {
		resp := h.do("GET", "/cost", withPrefs(ck, c.cookie)...)
		if got := body(t, resp); resp.StatusCode != 200 || !strings.Contains(got, c.want) {
			t.Errorf("cookie %s: %d, missing %q", c.cookie, resp.StatusCode, c.want)
		}
	}
	// Without the preferences the page has none of these.
	_, b := pageGet(t, h, ck, "/cost")
	for _, no := range []string{`<meta http-equiv="refresh"`, `Developer <span class="caret"`, `data-theme="dark"`} {
		if strings.Contains(b, no) {
			t.Errorf("default page has %q", no)
		}
	}
}

func TestPrefsBack(t *testing.T) {
	h := dataHarness(t)
	ck := cookieHdr(h.login())
	for _, back := range []string{"//evil.example", "https://evil.example/", `/\evil.example`, "evil", "", `/a\b`, "/%0d%0aSet-Cookie:x", "/a//b"} {
		path := "/prefs?theme=dark"
		if back != "" {
			path += "&back=" + strings.ReplaceAll(back, `\`, "%5C")
		}
		r := h.do("GET", path, ck...)
		if r.StatusCode != 400 || len(r.Header.Values("Set-Cookie")) != 0 || r.Header.Get("Location") != "" {
			t.Errorf("back %q: %d %v", back, r.StatusCode, r.Header)
		}
	}
	for _, back := range []string{"/", "/sessions?has=used"} {
		r := h.do("GET", "/prefs?theme=dark&back="+strings.NewReplacer("?", "%3F", "=", "%3D").Replace(back), ck...)
		if r.StatusCode != 303 || r.Header.Get("Location") != back {
			t.Errorf("back %q: %d %q", back, r.StatusCode, r.Header.Get("Location"))
		}
	}
}

func TestPrefsValues(t *testing.T) {
	h := dataHarness(t)
	ck := cookieHdr(h.login())
	for _, q := range []string{"theme=blue", "theme=dark&dev=1", "size=2", "dev=2", "theme=dark&theme=light", "back=/"} {
		r := h.do("GET", "/prefs?"+q+"&back=/cost", ck...)
		if q == "back=/" {
			r = h.do("GET", "/prefs?back=/cost", ck...)
		}
		if r.StatusCode != 400 || len(r.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("%s: %d %v", q, r.StatusCode, r.Header["Set-Cookie"])
		}
	}
	if r := h.do("GET", "/prefs?theme=dark&back=/cost"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no session: %d", r.StatusCode)
	}
}

func TestPrefsOnlyOnNavigation(t *testing.T) {
	h := dataHarness(t)
	ck := cookieHdr(h.login())
	for name, hdr := range map[string][]string{
		"Sec-Purpose": {"Sec-Purpose", "prefetch"}, "Purpose": {"Purpose", "prefetch"},
		"X-Moz": {"X-Moz", "prefetch"}, "X-Purpose": {"X-Purpose", "preview"},
	} {
		r := h.do("GET", "/prefs?theme=dark&back=/cost", append(append([]string{}, ck...), hdr...)...)
		if r.StatusCode == 303 || len(r.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("%s: %d %v", name, r.StatusCode, r.Header["Set-Cookie"])
		}
	}
	r := h.do("HEAD", "/prefs?theme=dark&back=/cost", ck...)
	if r.StatusCode == 303 || len(r.Header.Values("Set-Cookie")) != 0 {
		t.Errorf("HEAD: %d %v", r.StatusCode, r.Header["Set-Cookie"])
	}
}

func TestPrefsCookieSixFields(t *testing.T) {
	if got := parsePrefs("v1.dark.1.0.compact.1"); got != (Prefs{Theme: "dark", Dev: true, Compact: true, Keys: true}) {
		t.Errorf("six fields: %+v", got)
	}
	// A block 5 cookie keeps its four settings; the new two take their defaults.
	if got := parsePrefs("v1.dark.1.0"); got != (Prefs{Theme: "dark", Dev: true}) {
		t.Errorf("four fields: %+v", got)
	}
	for _, bad := range []string{"v1.dark.1.0.huge.1", "v1.dark.1.0.compact.2", "v1.dark.1.0.compact", "v1.dark.1.0.compact.1.x", "garbage", "v1.dark.1.0.1.1"} {
		if got := parsePrefs(bad); got != (Prefs{Theme: "system"}) {
			t.Errorf("%q: %+v, want defaults", bad, got)
		}
	}
	if got := (Prefs{Theme: "light", Compact: true, Keys: true}).cookie(); got != "v1.light.0.0.compact.1" {
		t.Errorf("cookie() = %q", got)
	}
	// The next save of a four-field cookie writes six.
	h := dataHarness(t)
	ck := h.login()
	r := h.do("GET", "/prefs?theme=light&back=/cost", withPrefs(ck, "v1.dark.1.0")...)
	if got := r.Header.Get("Set-Cookie"); !strings.HasPrefix(got, "backsight_prefs=v1.light.1.0.comfortable.0;") {
		t.Errorf("Set-Cookie %q", got)
	}
}

func TestPrefsDensityAndKeys(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	r := h.do("GET", "/prefs?density=compact&back=/quality", cookieHdr(ck)...)
	if r.StatusCode != 303 || r.Header.Get("Location") != "/quality" ||
		!strings.HasPrefix(r.Header.Get("Set-Cookie"), "backsight_prefs=v1.system.0.0.compact.0;") {
		t.Errorf("density: %d %q %q", r.StatusCode, r.Header.Get("Location"), r.Header["Set-Cookie"])
	}
	// The other settings are kept.
	r = h.do("GET", "/prefs?keys=1&back=/cost", withPrefs(ck, "v1.dark.1.1.compact.0")...)
	if got := r.Header.Get("Set-Cookie"); !strings.HasPrefix(got, "backsight_prefs=v1.dark.1.1.compact.1;") {
		t.Errorf("keys: %q", got)
	}
	for _, q := range []string{"density=huge", "density=", "keys=2", "keys=on", "density=compact&keys=1"} {
		r := h.do("GET", "/prefs?"+q+"&back=/cost", cookieHdr(ck)...)
		if r.StatusCode != 400 || len(r.Header.Values("Set-Cookie")) != 0 {
			t.Errorf("%s: %d %v", q, r.StatusCode, r.Header["Set-Cookie"])
		}
	}
}

// prefGroup is the text of the links under a Preferences group label that
// carry aria-current, and all the group's link texts.
func prefGroup(t *testing.T, doc *node, label string) (current, all []string) {
	t.Helper()
	for _, pop := range doc.find("div.menu-pop") {
		in := false
		for _, c := range pop.Kids {
			if c.attr("class") == "grp" {
				in = c.text() == label
				continue
			}
			if c.Name == "hr" {
				in = false
			}
			if in && c.Name == "a" {
				all = append(all, strings.TrimPrefix(c.text(), "✓"))
				if c.attr("aria-current") == "true" {
					current = append(current, strings.TrimPrefix(c.text(), "✓"))
				}
			}
		}
	}
	return
}

func TestCompactPageClass(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	for _, path := range []string{"/", "/sessions"} {
		for _, c := range []struct {
			cookie  string
			compact bool
			cur     string
		}{{"v1.system.0.0.comfortable.0", false, "Comfortable"}, {"v1.system.0.0.compact.1", true, "Compact"}, {"v1.system.0.0", false, "Comfortable"}} {
			resp := h.do("GET", path, withPrefs(ck, c.cookie)...)
			doc := parseHTML(t, body(t, resp))
			if got := doc.first("div.page") != nil && len(doc.find("div.page.d-compact")) == 1; got != c.compact {
				t.Errorf("%s %s: d-compact %v, want %v", path, c.cookie, got, c.compact)
			}
			cur, all := prefGroup(t, doc, "Density")
			if strings.Join(all, ",") != "Comfortable,Compact" || strings.Join(cur, ",") != c.cur {
				t.Errorf("%s %s: density links %v current %v", path, c.cookie, all, cur)
			}
			if !strings.Contains(body(t, h.do("GET", path, withPrefs(ck, c.cookie)...)), "Compact fits more on a screen; touch targets keep their size.") {
				t.Errorf("%s: density hint missing", path)
			}
		}
	}
}

func TestKeysAttribute(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	on := parseHTML(t, body(t, h.do("GET", "/cost", withPrefs(ck, "v1.system.0.0.comfortable.1")...)))
	if b := on.first("body"); b == nil || b.attr("data-keys") != "1" {
		t.Errorf("keys on: body %+v", b)
	}
	cur, all := prefGroup(t, on, "Keyboard shortcuts")
	if strings.Join(all, ",") != "Off,On" || strings.Join(cur, ",") != "On" {
		t.Errorf("keys menu %v current %v", all, cur)
	}
	for _, c := range []string{"v1.system.0.0.comfortable.0", "v1.system.0.0"} {
		off := parseHTML(t, body(t, h.do("GET", "/cost", withPrefs(ck, c)...)))
		if b := off.first("body"); b == nil || b.attr("data-keys") != "" {
			t.Errorf("%s: data-keys %q", c, b.attr("data-keys"))
		}
		if cur, _ := prefGroup(t, off, "Keyboard shortcuts"); strings.Join(cur, ",") != "Off" {
			t.Errorf("%s: current %v", c, cur)
		}
	}
	if !strings.Contains(body(t, h.do("GET", "/cost", cookieHdr(ck)...)), "g then a letter opens a page; press ? to list them all.") {
		t.Error("keys hint missing")
	}
}

// All PCs is a URL parameter, never a preference (A2): a link back to a page
// that carries it keeps it in the URL, the cookie holds nothing about it, and
// the next page without the parameter is this PC's.
func TestPrefsNeverKeepMachines(t *testing.T) {
	old := dataSpec
	dataSpec.OtherPCs = 3
	t.Cleanup(func() { dataSpec = old })
	h := dataHarness(t)
	ck := h.login()
	r := h.do("GET", "/prefs?theme=dark&back="+url.QueryEscape("/cost?machines=all"), cookieHdr(ck)...)
	const want = "backsight_prefs=v1.dark.0.0.comfortable.0; Path=/; Max-Age=31536000; HttpOnly; SameSite=Strict"
	if r.StatusCode != 303 || r.Header.Get("Set-Cookie") != want {
		t.Fatalf("%d, cookie %q, want 303 and %q", r.StatusCode, r.Header["Set-Cookie"], want)
	}
	if loc := r.Header.Get("Location"); loc != "/cost?machines=all" {
		t.Errorf("Location %q, the page to come back to keeps its parameter", loc)
	}
	// The preferences links on an all-PCs page are the page's own, with the
	// parameter inside back, and never a machines key of their own.
	doc := rawPage(t, h, cookieHdr(ck), "/cost?machines=all")
	for _, a := range doc.find("header a") {
		href := a.attr("href")
		if !strings.HasPrefix(href, "/prefs?") {
			continue
		}
		u, err := url.Parse(href)
		if err != nil || u.Query().Has("machines") || u.Query().Get("back") != "/cost?machines=all" {
			t.Errorf("preferences link %q: machines key %v, back %q", href, u.Query().Has("machines"), u.Query().Get("back"))
		}
	}
	// With the cookie just set, the next page without the parameter is this PC's.
	hdr := []string{"Cookie", ck.Name + "=" + ck.Value + "; backsight_prefs=v1.dark.0.0.comfortable.0"}
	doc = rawPage(t, h, hdr, "/")
	sw := doc.first(".scopebar .mscope")
	if sw == nil {
		t.Fatal("no switch")
	}
	if got := current(sw, "nav.mseg", "true"); !slices.Equal(got, []string{"this PC"}) || sw.first("details.pcs") != nil {
		t.Errorf("next page: selected %q, list %v; want this PC and no list", got, sw.first("details.pcs"))
	}
}
