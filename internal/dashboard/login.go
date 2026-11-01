package dashboard

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/output"
)

// idleLimit is how long the dashboard serves without a signed-in request
// (or a login) before it stops; idleCheck, how often it looks.
var (
	idleLimit = 2 * time.Hour
	idleCheck = time.Minute
)

// StopIdle: nobody signed in used the dashboard for idleLimit.
const StopIdle StopReason = "idle"

// defaultIdleTicks ticks every idleCheck; idleTicks is Serve's ticker:
// tests send the ticks themselves.
func defaultIdleTicks() (<-chan time.Time, func()) {
	t := time.NewTicker(idleCheck)
	return t.C, t.Stop
}

var idleTicks = defaultIdleTicks

// idleChecked, when set (tests), hears each idle check's verdict once the
// check is over, so a test moves its clock only between checks.
var idleChecked func(stop bool)

// noSession is the API's answer to a request without a valid session.
const noSession = "no valid session: run backsight dashboard link and open the link it prints"

// Link is a login link: what the dashboard prints at start.
type Link struct {
	URL       string    `json:"url"`
	Port      int       `json:"port"`
	ExpiresAt time.Time `json:"expires_at"`
}

// FirstLink is the link of the code minted as Start's last step; its
// ExpiresAt is in UTC.
func (s *Server) FirstLink() Link { return s.link }

// cookieName is the session cookie's name. Browsers do not scope cookies
// by port: the port in the name keeps two dashboards' sessions apart.
func cookieName(port int) string { return "backsight_dash_" + strconv.Itoa(port) }

// localBack accepts target as a redirect on this origin: one leading
// "/", no "//" in the path (the query may hold one), no "\", no control
// character, no scheme, host or user. A query is kept, a fragment dropped.
func localBack(target string) (string, bool) {
	target, _, _ = strings.Cut(target, "#")
	path, _, _ := strings.Cut(target, "?")
	if !strings.HasPrefix(target, "/") || strings.Contains(path, "//") || strings.ContainsRune(target, '\\') {
		return "", false
	}
	if hasControl(target) {
		return "", false
	}
	u, err := url.Parse(target)
	if err != nil || !localURL(u) {
		return "", false
	}
	return target, true
}

// hasControl: s holds a control character (url.Parse refuses them too; this
// does not rely on it).
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// localURL: u is a path and query only, no other site.
func localURL(u *url.URL) bool {
	return u.Scheme == "" && u.Host == "" && u.User == nil && u.Opaque == ""
}

// navigation: Sec-Fetch-Mode, when present, says the request is a
// navigation. Absent (older browsers), as for Sec-Fetch-Site, it passes.
func navigation(h http.Header) bool {
	mode, ok := h["Sec-Fetch-Mode"]
	return !ok || len(mode) == 1 && mode[0] == "navigate"
}

// prefetched: the request is a browser's prefetch or preview, not a
// navigation: it must not spend a login code.
func prefetched(h http.Header) bool {
	return hasToken(h, "Sec-Purpose", "prefetch") || hasToken(h, "Purpose", "prefetch") ||
		hasToken(h, "X-Moz", "prefetch") || hasToken(h, "X-Purpose", "preview")
}

// hasToken: a value of header name lists token, its parameters aside.
func hasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, item := range strings.Split(v, ",") {
			item, _, _ = strings.Cut(item, ";")
			if strings.EqualFold(strings.TrimSpace(item), token) {
				return true
			}
		}
	}
	return false
}

// login answers a navigation to a page with a code. An accepted code
// starts a session; a refused one, with a valid session, only drops the
// code from the address; without one it is the 401 page. Either redirect
// goes to the same local path and query, without the code.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	back, ok := localBack(withoutCode(r.URL))
	if !ok {
		back = "/"
	}
	err := ErrBadCode
	if codes := r.URL.Query()["code"]; len(codes) == 1 {
		err = AcceptCode(s.cfg.Dirs, codes[0], s.now())
	}
	if err != nil {
		if !s.signedIn(r) {
			s.unauthorized(w, r)
			return
		}
	} else {
		secret, err := s.sessions.create()
		if err != nil {
			refuse(w, http.StatusInternalServerError, "could not start a session: try a fresh link")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: cookieName(s.port), Value: secret, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
	}
	s.touch()
	w.Header().Set("Location", back)
	w.WriteHeader(http.StatusSeeOther)
}

// withoutCode is u's escaped path and query, every code parameter
// dropped and the others kept as sent.
func withoutCode(u *url.URL) string {
	var kept []string
	for _, p := range strings.Split(u.RawQuery, "&") {
		k, _, _ := strings.Cut(p, "=")
		if key, err := url.QueryUnescape(k); p == "" || err == nil && key == "code" {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) == 0 {
		return u.EscapedPath()
	}
	return u.EscapedPath() + "?" + strings.Join(kept, "&")
}

// signedIn: one of the request's cookies of this dashboard's name is a
// live session's secret. The Authorization header is never read.
func (s *Server) signedIn(r *http.Request) bool {
	name := cookieName(s.port)
	for _, c := range r.Cookies() {
		if c.Name == name && lowerHex(c.Value, 64) && s.sessions.valid(c.Value) {
			return true
		}
	}
	return false
}

// unauthorized answers 401: the JSON envelope under /api/, the page
// elsewhere.
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = output.WriteJSON(w, "dashboard", nil, &output.Error{Code: output.NeedsUser, Message: noSession})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(unauthorizedPage)
}

// touch records a signed-in request or an accepted login: the idle clock
// restarts. It moves forward only, so a clock stepped back does not bring
// the stop closer.
func (s *Server) touch() {
	now := s.now().Round(0)
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	if now.After(s.lastTouch) {
		s.lastTouch = now
	}
}

// idle: no signed-in request came for more than idleLimit.
func (s *Server) idle() bool {
	now := s.now().Round(0)
	s.idleMu.Lock()
	defer s.idleMu.Unlock()
	return now.Sub(s.lastTouch) > idleLimit
}
