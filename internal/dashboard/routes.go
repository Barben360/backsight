package dashboard

import (
	"net/http"

	"github.com/Barben360/backsight/internal/views"
)

// route is one of the dashboard's routes: a ServeMux pattern (GET also
// matches HEAD, which the text page refuses), a sample path tests request,
// its kind (page, text, prefs, api or asset) and, for a page, its name.
type route struct{ Pattern, Sample, Kind, Page string }

// routes is every route the dashboard serves.
var routes = []route{
	{"GET /{$}", "/", "page", "overview"},
	{"GET /quality", "/quality", "page", "quality"},
	{"GET /cost", "/cost", "page", "cost"},
	{"GET /bank", "/bank", "page", "bank"},
	{"GET /pipeline", "/pipeline", "page", "pipeline"},
	{"GET /system", "/system", "page", "system"},
	{"GET /releases", "/releases", "page", "releases"},
	{"GET /defs", "/defs", "page", "defs"},
	{"GET /projects", "/projects", "page", "projects"},
	{"GET /projects/{name}", "/projects/backsight", "page", "project"},
	{"GET /sessions", "/sessions", "page", "sessions"},
	{"GET /sessions/{id}", "/sessions/sess-a1b2c3d4", "page", "session"},
	{"GET /memories", "/memories", "page", "memories"},
	{"GET /memories/{id}", "/memories/mem-a1b2c3d4", "page", "memory"},
	{"GET /days", "/days", "page", "days"},
	{"GET /days/{date}", "/days/2026-10-30", "page", "day"},
	{"GET /memories/{id}/text", "/memories/mem-a1b2c3d4/text", "text", ""},
	{"GET /prefs", "/prefs", "prefs", ""},
	{"GET /api/v1/{view}", "/api/v1/overview", "api", ""},
	{"GET /api/v1/{view}/{id}", "/api/v1/sessions/sess-a1b2c3d4", "api", ""},
	{"GET /assets/{file}", assetPath("logo-light.svg"), "asset", ""},
}

// routes is the mux of every route, behind the guard.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Pattern, s.handler(r))
	}
	// Any other path under /api/ is the API's own 404, not the mux's.
	mux.HandleFunc("GET /api/", func(w http.ResponseWriter, _ *http.Request) {
		apiError(w, "stats", http.StatusNotFound, views.Usage(noPageSentence))
	})
	return mux
}

// kindOf maps each route's pattern to its kind.
var kindOf = func() map[string]string {
	m := map[string]string{}
	for _, r := range routes {
		m[r.Pattern] = r.Kind
	}
	return m
}()

// handler is the handler of r's kind.
func (s *Server) handler(r route) http.Handler {
	switch r.Kind {
	case "asset":
		return http.HandlerFunc(serveAsset)
	case "prefs":
		return http.HandlerFunc(s.servePrefs)
	case "api":
		return http.HandlerFunc(s.serveAPI)
	case "text":
		return http.HandlerFunc(s.serveText)
	case "page":
		return s.servePage(s.tpl.specs[r.Page])
	default:
		panic("route " + r.Pattern + ": unknown kind " + r.Kind)
	}
}
