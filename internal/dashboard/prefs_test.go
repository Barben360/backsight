package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

func TestParsePrefs(t *testing.T) {
	def := Prefs{Theme: "system"}
	cases := map[string]Prefs{
		"v1.dark.1.0":   {Theme: "dark", Dev: true},
		"v1.light.0.1":  {Theme: "light", Refresh: true},
		"v2.dark.1.0":   def,
		"v1.blue.0.0":   def,
		"v1.dark.2.0":   def,
		"v1.dark.0.2":   def,
		"v1.dark.0":     def,
		"v1.dark.0.0.0": def,
		"":              def,
	}
	for in, want := range cases {
		if got := parsePrefs(in); got != want {
			t.Errorf("parsePrefs(%q) = %+v, want %+v", in, got, want)
		}
	}
	if got := (Prefs{Theme: "light", Dev: true, Refresh: true}).cookie(); got != "v1.light.1.1" {
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
	const want = "backsight_prefs=v1.dark.0.0; Path=/; Max-Age=31536000; HttpOnly; SameSite=Strict"
	if r.StatusCode != 303 || r.Header.Get("Location") != "/cost" || r.Header.Get("Set-Cookie") != want {
		t.Errorf("%d %q %q", r.StatusCode, r.Header.Get("Location"), r.Header["Set-Cookie"])
	}

	// The other settings are kept from the request's cookie.
	r = h.do("GET", "/prefs?refresh=1&back=/cost", withPrefs(ck, "v1.light.1.0")...)
	if got := r.Header.Get("Set-Cookie"); !strings.HasPrefix(got, "backsight_prefs=v1.light.1.1;") {
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
