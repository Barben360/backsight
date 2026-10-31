package dashboard

import (
	"net/http"
	"strconv"
	"strings"
)

// csp is every response's Content-Security-Policy: the page loads nothing
// but its own origin's styles, images and scripts.
const csp = "default-src 'none'; style-src 'self'; img-src 'self' data:; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders sets the headers every response carries. Only a served
// asset (immutable) may be cached; no CORS header is ever set.
func securityHeaders(h http.Header, immutable bool) {
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	if immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-store")
	}
}

// guard is the outermost handler. It sets the security headers, then
// checks, each before any work: the Host (421), the method (405), the
// Fetch Metadata site (403); it serves /assets/ and hands the rest to
// next.
func (s *Server) guard(next http.Handler) http.Handler {
	port := strconv.Itoa(s.port)
	hosts := [2]string{"127.0.0.1:" + port, "localhost:" + port}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w.Header(), false)
		// A DNS-rebound name, another port (an ssh -L to a different
		// local port) or no Host at all: not this dashboard's origin.
		if r.Host != hosts[0] && r.Host != hosts[1] {
			refuse(w, http.StatusMisdirectedRequest, "misdirected request")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			refuse(w, http.StatusMethodNotAllowed, "only GET and HEAD")
			return
		}
		// Absent in older browsers; present, only a navigation typed or
		// from this origin passes (another local port is same-site).
		if site, ok := r.Header["Sec-Fetch-Site"]; ok && (len(site) != 1 || site[0] != "same-origin" && site[0] != "none") {
			refuse(w, http.StatusForbidden, "this page opens only from its own links or the address bar")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			serveAsset(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refuse answers one plain-text line.
func refuse(w http.ResponseWriter, code int, line string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(line + "\n"))
}
