package dashboard

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// The text page's name, title and refusals.
const (
	textPageName = "memory_text"
	textTitle    = "Memory text"
	notOpened    = "the memory text shows only in a page you open, from its link or the address bar"
	textGetOnly  = "the memory text opens only with GET"
	textNoParams = "the memory text page takes no parameters"
)

// serveText answers /memories/{id}/text, the one page that shows a
// memory's text. The guard checked the session cookie (never a bearer) and
// Sec-Fetch-Site; a Sec-Fetch-Dest, when sent, must be a document, and a
// prefetch, a HEAD or any query is refused before any work. The text is
// read live from the server, outside the result cache and without a
// catch-up (the index only resolves the id), and is escaped in the page.
func (s *Server) serveText(w http.ResponseWriter, r *http.Request) {
	// An <img>, an <iframe>, a fetch() or a prefetch is not the page opened.
	if dest, ok := r.Header["Sec-Fetch-Dest"]; ok && (len(dest) != 1 || dest[0] != "document") || prefetched(r.Header) {
		refuse(w, http.StatusForbidden, notOpened)
		return
	}
	// A HEAD would read the text for nothing, and tell its length.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		refuse(w, http.StatusMethodNotAllowed, textGetOnly)
		return
	}
	prefs := s.prefsOf(r)
	if r.URL.RawQuery != "" {
		s.statePage(w, r, http.StatusBadRequest, textTitle, prefs, sentence(textNoParams), 0)
		return
	}
	ctx := r.Context()
	if st, err := s.data.check(ctx); st != stateReady {
		if !s.notReadyPage(w, r, st, prefs) {
			if err == nil {
				err = errNoIndex
			}
			s.failurePage(w, r, textTitle, prefs, err)
		}
		return
	}
	id := r.PathValue("id")
	if strings.TrimSpace(id) == "" { // the view would read it as no id at all
		s.failurePage(w, r, textTitle, prefs, metrics.NotFoundError{Kind: "memory", ID: id})
		return
	}
	q, err := views.Query("memories", views.Params{Limit: 50, Text: true}, id, s.now())
	if err != nil {
		s.statePage(w, r, http.StatusBadRequest, textTitle, prefs, sentence(err.Error()), 0)
		return
	}
	// Never s.data.result: the result cache's key ignores Text.
	res, err := metrics.Memories(ctx, s.data.base(), q)
	if err != nil {
		s.failurePage(w, r, textTitle, prefs, err)
		return
	}
	data := PageData{V: map[string]*metrics.Result{"memories": &res}, ID: id, Loc: res.Window.Loc}
	data.Frame = s.frame(r, textTitle, prefs)
	data.Frame.Scope, data.Frame.ServerRead, data.Frame.Generated = &res.Scope, res.Server, res.GeneratedAt
	var buf bytes.Buffer
	if err := s.tpl.pages[textPageName].ExecuteTemplate(&buf, textPageName+".html", data); err != nil {
		refuse(w, http.StatusInternalServerError, "the page could not be drawn: run backsight doctor")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
	s.served.Add(1)
}
