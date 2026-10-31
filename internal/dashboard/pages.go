package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// viewsHeader is the first line of every template: the views the page
// reads, or "none". The handler computes exactly these.
var viewsHeader = regexp.MustCompile(`\A\{\{/\* views: ([a-z]+(?:, [a-z]+)*|none) \*/\}\}\n`)

// pageSpec is a page: its name, its file and the views it reads.
type pageSpec struct {
	Name, File string
	Views      []string
}

// helpURL is the dashboard's guide on the web, linked from every footer.
const helpURL = "https://github.com/Barben360/backsight/blob/main/docs/dashboard.md"

// The state pages' fixed sentences (the API's are in api.go).
const (
	firstBuildSentence = "Building the metrics index from this PC's history. This page refreshes every 2 seconds."
	updatedPage        = "backsight was updated: restart the dashboard (Ctrl+C, then backsight dashboard)."
	tooSlowPage        = "The page took too long: try again."
)

// declared reads the views header of a template file; ok is false when
// it has none. "none" is no view.
func declared(src []byte) (names []string, ok bool) {
	m := viewsHeader.FindSubmatch(src)
	if m == nil {
		return nil, false
	}
	if string(m[1]) == "none" {
		return nil, true
	}
	return strings.Split(string(m[1]), ", "), true
}

// loadPages parses the pages of fsys afresh at each call: every file with
// a views header other than none is a page, parsed with frame.html. A file
// without the header, an unknown view, a missing frame or a page route
// without a file is an error.
func loadPages(fsys fs.FS) (map[string]*template.Template, map[string]pageSpec, error) {
	files, err := fs.Glob(fsys, "*.html")
	if err != nil {
		return nil, nil, err
	}
	tpls, specs := map[string]*template.Template{}, map[string]pageSpec{}
	for _, file := range files {
		src, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, nil, fmt.Errorf("read template %s: %w", file, err)
		}
		names, ok := declared(src)
		if !ok {
			return nil, nil, fmt.Errorf("template %s: no views header on its first line", file)
		}
		for i, v := range names {
			if !slices.Contains(metrics.Views, v) {
				return nil, nil, fmt.Errorf("template %s: %q is not a view", file, v)
			}
			if slices.Contains(names[:i], v) {
				return nil, nil, fmt.Errorf("template %s: view %q is declared twice", file, v)
			}
		}
		if names == nil {
			continue
		}
		name := strings.TrimSuffix(file, ".html")
		t, err := parseWithFrame(fsys, file)
		if err != nil {
			return nil, nil, err
		}
		tpls[name], specs[name] = t, pageSpec{Name: name, File: file, Views: names}
	}
	for _, r := range routes {
		if _, ok := specs[r.Page]; r.Kind == "page" && !ok {
			return nil, nil, fmt.Errorf("page %s: no template with views", r.Page)
		}
	}
	return tpls, specs, nil
}

// parseWithFrame parses file with the frame's partials: a template set of
// its own, missing map keys refused.
func parseWithFrame(fsys fs.FS, file string) (*template.Template, error) {
	t, err := template.New(file).Funcs(funcs).Option("missingkey=error").ParseFS(fsys, "frame.html", file)
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", file, err)
	}
	return t, nil
}

// pageSet is the parsed templates the server renders with.
type pageSet struct {
	pages map[string]*template.Template
	specs map[string]pageSpec
	state *template.Template
}

// newPageSet parses the dashboard's templates.
func newPageSet() (*pageSet, error) {
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		return nil, err
	}
	state, err := parseWithFrame(Templates(), "state.html")
	if err != nil {
		return nil, err
	}
	return &pageSet{pages: tpls, specs: specs, state: state}, nil
}

// PageData is what a template reads.
type PageData struct {
	Frame Frame
	V     map[string]*metrics.Result // the declared views, by name
	ID    string                     // the route's id, name or date; "" for a list
	Msg   string                     // state, error and not-found pages
	Loc   *time.Location             // the window's zone (tz or this machine's): every time is shown in it
}

// Frame is what every page shows around its content.
type Frame struct {
	Title, Path, Project, Version, HelpURL string // Path: request path and query without code
	Windows, Nav                           []Choice
	AllProjects, ProjectInput              string      // the path without the project filter; the filter's current text
	Keep                                   [][2]string // query pairs the project form carries along
	PrefMenu                               []PrefGroup
	Prefs                                  Prefs
	Scope                                  *metrics.Scope       // the first declared view's; nil on state pages
	ServerRead                             *metrics.ServerState // the server's answer when that view read it, else nil
	Generated                              time.Time
	Reload                                 int // seconds until the browser asks again; 0: never (the first build's page)
}

// Choice is a link among several; Current marks the one shown.
type Choice struct {
	Label, Href string
	Current     bool
}

// Prefs are the reader's preferences.
type Prefs struct {
	Theme        string
	Dev, Refresh bool
}

// navLinks are the navigation, in the order the top partial slices it:
// the five questions, Browse (three), System, then the developer pair.
var navLinks = []struct{ label, href string }{
	{"Overview", "/"}, {"Quality", "/quality"}, {"Cost", "/cost"}, {"Bank", "/bank"}, {"Pipeline", "/pipeline"},
	{"Projects", "/projects"}, {"Sessions", "/sessions"}, {"Memories", "/memories"},
	{"System", "/system"},
	{"Releases", "/releases"}, {"Definitions", "/defs"},
}

// navFor is the navigation for the request path; the developer pair only
// with dev.
func navFor(path string, dev bool) []Choice {
	path, _, _ = strings.Cut(path, "?")
	links := navLinks
	if !dev {
		links = links[:len(links)-2]
	}
	out := make([]Choice, 0, len(links))
	for _, l := range links {
		cur := path == l.href || l.href != "/" && strings.HasPrefix(path, l.href+"/")
		out = append(out, Choice{Label: l.label, Href: l.href, Current: cur})
	}
	return out
}

// PrefGroup is one setting of the preferences menu and its choices.
type PrefGroup struct {
	Label   string
	Choices []Choice
}

// windowDays are the window choices, shortest first.
var windowDays = []struct {
	label string
	days  int
}{{"24 h", 1}, {"7 d", 7}, {"30 d", 30}, {"90 d", 90}}

// splitPath cuts a path with its query into the path and the raw pairs.
func splitPath(p string) (string, []string) {
	path, q, _ := strings.Cut(p, "?")
	if q == "" {
		return path, nil
	}
	return path, strings.Split(q, "&")
}

// keyOf is the decoded key of a raw pair.
func keyOf(pair string) string {
	k, _, _ := strings.Cut(pair, "=")
	if key, err := url.QueryUnescape(k); err == nil {
		return key
	}
	return k
}

// without is path with its raw query minus the pairs whose key is in drop.
func without(path string, drop ...string) string {
	base, pairs := splitPath(path)
	var kept []string
	for _, p := range pairs {
		if p != "" && !slices.Contains(drop, keyOf(p)) {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return base
	}
	return base + "?" + strings.Join(kept, "&")
}

// windowsFor are the window links of path: days replaces the window, the
// project and the zone are kept. The default window is 7 days.
func windowsFor(path string, q url.Values) []Choice {
	base, pairs := splitPath(path)
	suffix := ""
	seen := map[string]bool{}
	for _, p := range pairs {
		if k := keyOf(p); (k == "project" || k == "tz") && !seen[k] {
			seen[k] = true
			suffix += "&" + p
		}
	}
	cur := q.Get("days")
	if cur == "" {
		cur = "7"
	}
	out := make([]Choice, 0, len(windowDays))
	for _, w := range windowDays {
		d := strconv.Itoa(w.days)
		out = append(out, Choice{Label: w.label, Href: base + "?days=" + d + suffix,
			Current: q.Get("since") == "" && q.Get("until") == "" && cur == d})
	}
	return out
}

// prefMenu is the preferences menu: each choice a /prefs link back to path.
func prefMenu(path string, p Prefs) []PrefGroup {
	link := func(label, key, val string, cur bool) Choice {
		return Choice{Label: label, Href: "/prefs?" + key + "=" + val + "&back=" + url.QueryEscape(path), Current: cur}
	}
	flag := func(key string, on bool) []Choice {
		return []Choice{link("On", key, "1", on), link("Off", key, "0", !on)}
	}
	theme := make([]Choice, 0, len(themes))
	for _, t := range themes {
		theme = append(theme, link(strings.ToUpper(t[:1])+t[1:], "theme", t, p.Theme == t))
	}
	return []PrefGroup{{"Theme", theme}, {"Developer mode", flag("dev", p.Dev)}, {"Auto-refresh", flag("refresh", p.Refresh)}}
}

// frame is the frame of a page about r.
func (s *Server) frame(r *http.Request, title string, p Prefs) Frame {
	path := withoutCode(r.URL)
	q, _ := url.ParseQuery(r.URL.RawQuery) // a garbled query has no pairs to show
	q.Del("code")
	var keep [][2]string
	for _, k := range []string{"days", "since", "until", "tz"} {
		if v := q.Get(k); v != "" {
			keep = append(keep, [2]string{k, v})
		}
	}
	return Frame{
		Title: title, Path: path, Version: s.cfg.Version, HelpURL: helpURL,
		Nav: navFor(path, p.Dev), Prefs: p, Windows: windowsFor(path, q),
		AllProjects: without(path, "project"), ProjectInput: q.Get("project"), Keep: keep,
		PrefMenu: prefMenu(path, p),
	}
}

// Pages is how many page responses were served 200 under a session (the
// text page included; the API, assets, preferences, the 401 and the state
// pages not).
func (s *Server) Pages() int64 { return s.served.Load() }

// servePage answers a page: its views computed, then the template
// executed into a buffer and written whole.
func (s *Server) servePage(spec pageSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		first := spec.Views[0]
		title := views.Questions[first]
		prefs := s.prefsOf(r)
		// A pair net/http cannot parse is refused, not dropped: a garbled
		// filter must not read as no filter.
		q, err := url.ParseQuery(r.URL.RawQuery)
		var p views.Params
		if err != nil {
			err = views.Usage("the query string is malformed")
		} else {
			q.Del("code")
			p, err = views.FromValues(first, q)
		}
		if err != nil {
			s.statePage(w, r, http.StatusBadRequest, title, prefs, sentence(err.Error()), 0)
			return
		}
		if p.DevSet { // the query beats the preference for this request
			prefs.Dev = p.Dev
		} else {
			p.Dev = prefs.Dev
		}
		id := ""
		for _, k := range []string{"name", "id", "date"} {
			if id == "" {
				id = r.PathValue(k)
			}
		}
		data := PageData{V: map[string]*metrics.Result{}, ID: id}
		for _, v := range spec.Views {
			res, st, err := s.data.result(r.Context(), v, p, id)
			if st == stateReady && errors.Is(err, index.ErrUpdateInProgress) {
				st, err = stateUpdating, nil
			}
			switch st {
			case stateBuilding:
				w.Header().Set("Retry-After", "2")
				s.statePage(w, r, http.StatusServiceUnavailable, "backsight", prefs, firstBuildSentence, 2)
				return
			case stateUpdating:
				s.statePage(w, r, http.StatusServiceUnavailable, "backsight", prefs, sentence(views.UpdatingSentence), 0)
				return
			case stateUpdated:
				s.statePage(w, r, http.StatusServiceUnavailable, "backsight", prefs, updatedPage, 0)
				return
			}
			if st == stateBroken && err == nil {
				err = errNoIndex
			}
			if err != nil {
				s.failurePage(w, r, title, prefs, err)
				return
			}
			data.V[v] = &res
		}
		head := data.V[first]
		data.Frame = s.frame(r, title, prefs)
		data.Frame.Scope, data.Frame.ServerRead = &head.Scope, head.Server
		data.Frame.Generated = head.GeneratedAt
		data.Loc = head.Window.Loc
		var buf bytes.Buffer
		if err := s.tpl.pages[spec.Name].ExecuteTemplate(&buf, spec.File, data); err != nil {
			refuse(w, http.StatusInternalServerError, "the page could not be drawn: run backsight doctor")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
		if r.Method == http.MethodGet {
			s.served.Add(1)
		}
	}
}

// failurePage answers a view's failure as the API does, in a page: a
// deadline is the timeout page, an unclassified failure a fixed sentence.
func (s *Server) failurePage(w http.ResponseWriter, r *http.Request, title string, prefs Prefs, err error) {
	if errors.Is(r.Context().Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		s.statePage(w, r, http.StatusServiceUnavailable, title, prefs, tooSlowPage, 0)
		return
	}
	status, reported := apiFailure(err)
	s.statePage(w, r, status, title, prefs, sentence(reported.Error()), 0)
}

// sentence is a reported message as a page shows it: its first letter
// upper-cased and a full stop at the end.
func sentence(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	if last := r[len(r)-1]; last != '.' && last != '!' && last != '?' {
		r = append(r, '.')
	}
	return string(r)
}

// statePage answers status with the state page: a sentence, and for
// reload > 0 a browser that asks again after that many seconds.
func (s *Server) statePage(w http.ResponseWriter, r *http.Request, status int, title string, prefs Prefs, msg string, reload int) {
	f := s.frame(r, title, prefs)
	f.Reload = reload
	var buf bytes.Buffer
	if err := s.tpl.state.ExecuteTemplate(&buf, "state.html", PageData{Frame: f, Msg: msg, Loc: time.UTC}); err != nil {
		refuse(w, http.StatusInternalServerError, "the page could not be drawn: run backsight doctor")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
