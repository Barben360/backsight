package dashboard

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// prefsCookie is the preferences cookie's name; prefsMaxAge is one year.
const (
	prefsCookie = "backsight_prefs"
	prefsMaxAge = 31536000
)

// themes are the theme choices, in the order the menu shows them.
var themes = []string{"system", "light", "dark"}

// parsePrefs reads a cookie value (v1.<theme>.<dev>.<refresh>); anything
// else is the defaults.
func parsePrefs(v string) Prefs {
	def := Prefs{Theme: "system"}
	parts := strings.Split(v, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return def
	}
	theme, dev, refresh := parts[1], parts[2], parts[3]
	known := false
	for _, t := range themes {
		known = known || t == theme
	}
	if !known || (dev != "0" && dev != "1") || (refresh != "0" && refresh != "1") {
		return def
	}
	return Prefs{Theme: theme, Dev: dev == "1", Refresh: refresh == "1"}
}

// bit is a preference flag as its cookie digit.
func bit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// cookie is the cookie value that parsePrefs reads back.
func (p Prefs) cookie() string {
	return "v1." + p.Theme + "." + bit(p.Dev) + "." + bit(p.Refresh)
}

// prefsOf is the reader's preferences, from the cookie.
func (s *Server) prefsOf(r *http.Request) Prefs {
	c, err := r.Cookie(prefsCookie)
	if err != nil {
		return Prefs{Theme: "system"}
	}
	return parsePrefs(c.Value)
}

// servePrefs changes one preference and sends the reader back: exactly
// one of theme, dev and refresh besides back, back a local path.
func (s *Server) servePrefs(w http.ResponseWriter, r *http.Request) {
	prefs := s.prefsOf(r)
	// Only a navigation sets a preference: not a HEAD, not a prefetch.
	if r.Method != http.MethodGet || prefetched(r.Header) {
		s.statePage(w, r, http.StatusBadRequest, kindBad, "backsight", prefs, "A preference changes only when its link is opened.", 0)
		return
	}
	bad := func(msg string) { s.statePage(w, r, http.StatusBadRequest, kindBad, "backsight", prefs, msg, 0) }
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		bad("The query string is malformed.")
		return
	}
	back, ok := "", false
	if len(q["back"]) == 1 {
		back, ok = localBack(q.Get("back"))
	}
	if !ok {
		bad("The page to go back to is not valid.")
		return
	}
	q.Del("back")
	if len(q) != 1 {
		bad("Change one preference at a time.")
		return
	}
	for k, vs := range q {
		v := ""
		if len(vs) == 1 {
			v = vs[0]
		}
		switch {
		case k == "theme" && (v == "system" || v == "light" || v == "dark"):
			prefs.Theme = v
		case k == "dev" && (v == "0" || v == "1"):
			prefs.Dev = v == "1"
		case k == "refresh" && (v == "0" || v == "1"):
			prefs.Refresh = v == "1"
		default:
			bad("That preference or value is not valid.")
			return
		}
	}
	w.Header().Add("Set-Cookie", prefsCookie+"="+prefs.cookie()+"; Path=/; Max-Age="+strconv.Itoa(prefsMaxAge)+"; HttpOnly; SameSite=Strict")
	w.Header().Set("Location", back)
	w.WriteHeader(http.StatusSeeOther)
}
