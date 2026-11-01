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

	"github.com/Barben360/backsight/internal/dashboard/format"
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
	// firstBuildTextSentence is the text page's: it never refreshes.
	firstBuildTextSentence = "Building the metrics index from this PC's history. Open the text again in a few seconds."
	updatedPage            = updatedTitle // the heading of the page that says backsight was updated
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
	L     format.Locale              // the browser's language: numbers are written by funcs that take it; English on the 401 page
	P     views.Params               // the page's parameters: its links carry the window and filters
	CLI   string                     // the command that prints the page's first view
	State *StateView                 // the state pages' words; nil elsewhere
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
	Reload                                 int       // seconds until the browser asks again; 0: never (the first build's page)
	RenderedAt                             time.Time // the request's start (Config.Now)
	RenderMS                               int64     // ms from the request's start to just before the template runs (views computed)
	SessionHead                            bool      // the page head says the session's project, span and sent state, not a window
	Window                                 string    // windowPhrase of the page's params and window; "" on the text and state pages
	Catching                               string    // catchingOf the first view's freshness; "" when the history is read
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

// PrefGroup is one setting of the preferences menu and its choices; Hint
// is a line under them, if any.
type PrefGroup struct {
	Label   string
	Choices []Choice
	Hint    string
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
	flag := func(key, onLabel string, on bool) []Choice {
		return []Choice{link("Off", key, "0", !on), link(onLabel, key, "1", on)}
	}
	theme := make([]Choice, 0, len(themes))
	for _, t := range themes {
		theme = append(theme, link(strings.ToUpper(t[:1])+t[1:], "theme", t, p.Theme == t))
	}
	return []PrefGroup{
		{Label: "Theme", Choices: theme},
		{Label: "Developer mode", Choices: flag("dev", "On", p.Dev), Hint: "Adds ranking, learned ranking and release detail."},
		{Label: "Auto-refresh", Choices: flag("refresh", "Every minute", p.Refresh)},
	}
}

// zoneOr is loc, UTC when nil.
func zoneOr(loc *time.Location) *time.Location {
	if loc == nil {
		return time.UTC
	}
	return loc
}

// pageTitle is the question a page asks: its first view's, except for the
// pages about one day, session or memory.
func pageTitle(spec pageSpec, id string) string {
	switch spec.Name {
	case "day":
		return dayTitle(id)
	case "session":
		return "What happened in session " + views.ShortID(id) + "?"
	case "memory":
		return "How has memory " + views.ShortID(id) + " done?"
	}
	return views.Questions[spec.Views[0]]
}

// dayTitle is the question of the page about one day (2026-10-29).
func dayTitle(day string) string {
	d, err := time.Parse("2006-01-02", day)
	if err != nil {
		return "What happened that day?"
	}
	return "What happened on " + d.Format("Monday, Jan 2") + "?"
}

// pageData is the part of a page's data every page has, the text and state
// pages included: the frame with the request's start and the time spent
// since, and the browser's locale. started is the request's start.
func (s *Server) pageData(r *http.Request, title string, prefs Prefs, started time.Time) PageData {
	f := s.frame(r, title, prefs)
	f.RenderedAt, f.RenderMS = started, max(0, s.now().Sub(started).Milliseconds())
	return PageData{Frame: f, Loc: time.UTC, L: localeOf(r)}
}

// withResult sets what the first view's result says about the page: the
// scope, the server's answer, when it was generated, the zone and the
// catch-up sentence; the views being computed, it also sets the time spent.
func (s *Server) withResult(d *PageData, head *metrics.Result) {
	d.Frame.RenderMS = max(0, s.now().Sub(d.Frame.RenderedAt).Milliseconds())
	d.Frame.Scope, d.Frame.ServerRead, d.Frame.Generated = &head.Scope, head.Server, head.GeneratedAt
	d.Frame.Catching = catchingOf(head.Index)
	d.Loc = head.Window.Loc
}

// localeOf is the language the browser asked for, English when none is known.
func localeOf(r *http.Request) format.Locale {
	return format.FromAcceptLanguage(r.Header.Get("Accept-Language"))
}

// catchingOf is the sentence for an index still reading its history; ""
// when it is not.
func catchingOf(fr index.Freshness) string {
	if !fr.Partial {
		return ""
	}
	return "Catching up: " + views.Age(fr.LagSeconds*1000) + " of history still being read"
}

// windowPhrase says the window of a page: the days it spans or, for a
// start or an end given, its first and last day (the end is exclusive).
func windowPhrase(p views.Params, w metrics.Window) string {
	if p.Since == "" && p.Until == "" {
		days := w.Days
		if days == 0 {
			days = p.Days
		}
		if days == 1 {
			return "last 24 h"
		}
		return "last " + strconv.Itoa(days) + " days"
	}
	const f = "Jan 2"
	if !w.Since.IsZero() && !w.Until.IsZero() {
		loc := zoneOr(w.Loc)
		return w.Since.In(loc).Format(f) + " to " + w.Until.In(loc).Add(-time.Nanosecond).Format(f)
	}
	// No resolved window: read the dates as given.
	day := func(s string) (time.Time, bool) {
		if len(s) >= 10 {
			t, err := time.Parse("2006-01-02", s[:10])
			return t, err == nil
		}
		return time.Time{}, false
	}
	from, ok := day(p.Since)
	if !ok {
		return ""
	}
	to, ok := day(p.Until)
	if !ok {
		return "since " + from.Format(f)
	}
	return from.Format(f) + " to " + to.AddDate(0, 0, -1).Format(f)
}

// spanPhrase is the days a session spans: "Oct 15", or "Oct 15 – Oct 16".
func spanPhrase(start, last time.Time, loc *time.Location) string {
	if start.IsZero() {
		return "all time"
	}
	const f = "Jan 2"
	s := start.In(zoneOr(loc)).Format(f)
	if e := last.In(zoneOr(loc)).Format(f); !last.IsZero() && e != s {
		return s + " – " + e
	}
	return s
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
	f := Frame{
		Title: title, Path: path, Version: s.cfg.Version, HelpURL: helpURL,
		Nav: navFor(path, p.Dev), Prefs: p, Windows: windowsFor(path, q),
		AllProjects: without(path, "project"), ProjectInput: q.Get("project"), Keep: keep,
		PrefMenu: prefMenu(path, p),
	}
	// The text page takes no parameter: it offers no window or filter. It
	// never refreshes on its own: each reading of the text is a click. The
	// menu still shows the preference as set.
	if kindOf[r.Pattern] == "text" {
		f.Windows, f.Keep = nil, nil
		f.Prefs.Refresh = false
	}
	return f
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
		started := s.now()
		id := ""
		for _, k := range []string{"name", "id", "date"} {
			if id == "" {
				id = r.PathValue(k)
			}
		}
		title := pageTitle(spec, id)
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
			s.statePage(w, r, http.StatusBadRequest, kindBad, title, prefs, sentence(err.Error()), 0)
			return
		}
		if p.DevSet { // the query beats the preference for this request
			prefs.Dev = p.Dev
		} else {
			p.Dev = prefs.Dev
		}
		data := s.pageData(r, title, prefs, started)
		data.V, data.ID, data.P = map[string]*metrics.Result{}, id, p
		data.CLI = cliFor(first, id, p)
		if spec.Name == "session" || spec.Name == "memory" {
			data.CLI = cliFor(first, id, allTimeParams(p))
		}
		if spec.Name == "defs" { // the catalog is read whole: its axis links are anchors on one page
			p.Limit, p.Offset = metrics.MaxLimit, 0
			data.P = p
		}
		for _, v := range spec.Views {
			res, st, err := s.data.result(r.Context(), v, p, id)
			if st == stateReady && errors.Is(err, index.ErrUpdateInProgress) {
				st, err = stateUpdating, nil
			}
			if s.notReadyPage(w, r, st, prefs) {
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
		s.withResult(&data, head)
		if kindOf[r.Pattern] != "text" {
			data.Frame.Window = windowPhrase(p, head.Window)
			if spec.Name == "day" {
				// The page is about one day: it states that day and offers no window.
				data.Frame.Windows = nil
				data.Frame.Window = id
				if !head.Window.Since.IsZero() {
					data.Frame.Window = head.Window.Since.In(zoneOr(head.Window.Loc)).Format("2006-01-02")
				}
			}
			if spec.Name == "session" || spec.Name == "memory" {
				// Both pages read the whole history: no window to pick, and none to name.
				data.Frame.Windows = nil
				data.Frame.Window = "all time"
				if spec.Name == "session" {
					// A session has no window: its head says its project,
					// when it ran and how it was sent.
					data.Frame.Window, data.Frame.SessionHead = "", true
				}
			}
		}
		body, err := renderPage(s.tpl, spec.Name, data)
		if err != nil {
			refuse(w, http.StatusInternalServerError, "the page could not be drawn: run backsight doctor")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
		if r.Method == http.MethodGet {
			s.served.Add(1)
		}
	}
}

// renderPage executes the page called name on data into a buffer: the
// whole body or an error, nothing half written.
func renderPage(set *pageSet, name string, data PageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := set.pages[name].ExecuteTemplate(&buf, set.specs[name].File, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// notReadyPage answers a building, updating or updated index with its
// state page; false for any other state, which it leaves to the caller.
func (s *Server) notReadyPage(w http.ResponseWriter, r *http.Request, st indexState, prefs Prefs) bool {
	switch st {
	case stateBuilding:
		w.Header().Set("Retry-After", "2")
		// The text page never refreshes itself: each reading is a click.
		reload, msg := 2, firstBuildSentence
		if kindOf[r.Pattern] == "text" {
			reload, msg = 0, firstBuildTextSentence
		}
		s.statePage(w, r, http.StatusServiceUnavailable, kindBuilding, "backsight", prefs, msg, reload)
	case stateUpdating:
		s.statePage(w, r, http.StatusServiceUnavailable, kindUpdating, "backsight", prefs, "", 0)
	case stateUpdated:
		s.statePage(w, r, http.StatusServiceUnavailable, kindUpdated, "backsight", prefs, "", 0)
	default:
		return false
	}
	return true
}

// failurePage answers a view's failure as the API does, in a page: a
// deadline is the timeout page, an unclassified failure a fixed sentence.
func (s *Server) failurePage(w http.ResponseWriter, r *http.Request, title string, prefs Prefs, err error) {
	if errors.Is(r.Context().Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		s.statePage(w, r, http.StatusServiceUnavailable, kindTimeout, title, prefs, "", 0)
		return
	}
	status, reported := apiFailure(err)
	s.statePage(w, r, status, failureKind(status), title, prefs, sentence(reported.Error()), 0)
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

// statePage answers status with the state page of kind: a sentence, and
// for reload > 0 a browser that asks again after that many seconds.
func (s *Server) statePage(w http.ResponseWriter, r *http.Request, status int, kind, title string, prefs Prefs, msg string, reload int) {
	data := s.pageData(r, title, prefs, s.now())
	data.Frame.Reload, data.Msg = reload, msg
	data.State = stateOf(kind, title, msg, data.Frame.Path)
	var buf bytes.Buffer
	if err := s.tpl.state.ExecuteTemplate(&buf, "state.html", data); err != nil {
		refuse(w, http.StatusInternalServerError, "the page could not be drawn: run backsight doctor")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
