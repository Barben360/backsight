package dashboard

import "net/http"

// route is one of the dashboard's routes: a ServeMux pattern (GET also
// matches HEAD), a sample path tests request, its kind (page, text,
// prefs, api or asset) and, for a page, its name.
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
	case "page":
		return http.HandlerFunc(placeholderPage)
	default:
		return http.HandlerFunc(notBuilt)
	}
}

// placeholderPage stands for a page until its renderer exists.
func placeholderPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><title>backsight</title></head>" +
		"<body><p>This page is not built yet.</p></body></html>\n"))
}

// notBuilt stands for a route whose handler does not exist yet.
func notBuilt(w http.ResponseWriter, _ *http.Request) {
	refuse(w, http.StatusNotImplemented, "not built yet")
}
