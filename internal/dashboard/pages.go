package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Barben360/backsight/internal/dashboard/format"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
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
	HM    string                     // the Overview's calendar colouring (got, used or sessions); "" the default
	// Own is this PC's answer to the page's first view, read only when the
	// Overview, Cost or day page adds up all PCs: the verdict banner and the
	// links that count events are this PC's. Nil everywhere else.
	Own *metrics.Result
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
	Custom                                 *Custom   // the custom window picker; nil on a page without window links
	CustomMenu                             *Custom   // its copy in the Menu
	Machines                               string    // "all" when the URL asks for all PCs: every link and form keeps it; "" this PC
	Switch                                 *Switch   // the scope line's this PC / all PCs switch; nil: the line as shipped
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
	Compact      bool // the compact density
	Keys         bool // keyboard shortcuts on
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
		{Label: "Density", Choices: []Choice{link("Comfortable", "density", "comfortable", !p.Compact), link("Compact", "density", "compact", p.Compact)},
			Hint: "Compact fits more on a screen; touch targets keep their size."},
		{Label: "Keyboard shortcuts", Choices: flag("keys", "On", p.Keys), Hint: "g then a letter opens a page; press ? to list them all."},
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
	// All PCs is a URL parameter like the project filter, never a preference:
	// the forms carry it, the links get it from keepMachines.
	machines := ""
	if q.Get("machines") == "all" {
		machines = "all"
		keep = append(keep, [2]string{"machines", "all"})
	}
	f := Frame{
		Title: title, Path: path, Version: s.cfg.Version, HelpURL: helpURL,
		Nav: navFor(path, p.Dev), Prefs: p, Windows: windowsFor(path, q),
		AllProjects: without(path, "project"), ProjectInput: q.Get("project"), Keep: keep,
		PrefMenu: prefMenu(path, p), Machines: machines,
	}
	// The text page takes no parameter: it offers no window or filter. It
	// never refreshes on its own: each reading of the text is a click. The
	// menu still shows the preference as set.
	if kindOf[r.Pattern] == "text" {
		f.Windows, f.Keep, f.Machines = nil, nil, ""
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
		var redirect, hm string
		var picked bool // the redirect is the Releases picker's comparison
		var refused *Custom
		if err != nil {
			err = views.Usage("the query string is malformed")
		} else {
			q.Del("code")
			// The custom window's from and to become since and until; only
			// the pages that offer window links take them.
			if hasPicker(spec, r) {
				redirect, refused, err = pickWindow(r.URL.EscapedPath(), q, s.now())
				q.Del("from")
				q.Del("to")
			}
			// The Releases picker sends two versions and a measure; the view
			// takes one comparison, so the page redirects to it.
			if spec.Name == "releases" && err == nil && redirect == "" {
				redirect = pickVersions(r.URL.EscapedPath(), q)
				picked = redirect != ""
			}
			// The calendar's colouring is the page's own (Overview, Days): no view takes it.
			if spec.Name == "overview" || spec.Name == "days" {
				hm = q.Get("hm")
				q.Del("hm")
			}
			if err == nil {
				p, err = views.FromValues(first, q)
			}
			// A comparison the view would refuse (the same version twice) is
			// refused here, not redirected to.
			if err == nil && picked {
				if _, qerr := views.Query(first, p, id, s.now()); qerr != nil {
					var oe *output.Error
					if !errors.As(qerr, &oe) || oe.Code != output.Usage { // not a refusal of the request
						s.failurePage(w, r, title, prefs, qerr)
						return
					}
					err = qerr
				}
			}
		}
		if err != nil {
			s.statePage(w, r, http.StatusBadRequest, kindBad, title, prefs, sentence(err.Error()), 0)
			return
		}
		if redirect != "" {
			http.Redirect(w, r, redirect, http.StatusSeeOther)
			return
		}
		if p.DevSet { // the query beats the preference for this request
			prefs.Dev = p.Dev
		} else {
			p.Dev = prefs.Dev
		}
		data := s.pageData(r, title, prefs, started)
		data.V, data.ID, data.P = map[string]*metrics.Result{}, id, p
		data.HM = measureOf(hm).key
		// The page's command is this PC's: the widgets that follow the switch
		// add the flag themselves (headOf, tileHead, the calendar).
		data.CLI = cliFor(first, id, p.ThisPC())
		if spec.Name == "session" || spec.Name == "memory" {
			data.CLI = cliFor(first, id, allTimeParams(p.ThisPC()))
		}
		if spec.Name == "defs" { // the catalog is read whole: its axis links are anchors on one page
			p.Limit, p.Offset = metrics.MaxLimit, 0
			data.P = p
		}
		// The Overview, Cost and the day page on all PCs also read this PC's own answer (for their "This PC's N"
		// links): it is a read of its own, so it runs beside the merged views and not after them.
		ownKind := spec.Name == "overview" || spec.Name == "cost" || spec.Name == "day"
		type ownRead struct {
			res metrics.Result
			st  indexState
			err error
		}
		var ownCh chan ownRead
		if ownKind && p.Machines == "all" {
			ownCh = make(chan ownRead, 1)
			read := ownReadHook
			if read == nil {
				read = s.data.result
			}
			go func() {
				var o ownRead
				defer func() {
					if recover() != nil { // as a view's: the page fails, not the process
						o = ownRead{st: stateReady, err: errViewFailed}
					}
					ownCh <- o
				}()
				o.res, o.st, o.err = read(r.Context(), first, viewParams(spec, first, p.ThisPC()), id)
			}()
		}
		outs := s.readViews(r.Context(), spec, p, id)
		defer func() { // never leave the read running past the request
			if ownCh != nil {
				<-ownCh
			}
		}()
		if bad, ok := firstProblem(outs); ok {
			if s.notReadyPage(w, r, bad.st, prefs) {
				return
			}
			s.failurePage(w, r, title, prefs, bad.err)
			return
		}
		for i, v := range spec.Views {
			data.V[v] = &outs[i].res
		}
		head := data.V[first]
		s.withResult(&data, head)
		// What the figures count, not what the URL asked for: sharing off, or a
		// page that is always this PC's, answers this PC and says so.
		data.P.Machines = head.Scope.Machines
		if ownCh != nil && head.Scope.Machines == "all" { // sharing off answers this PC: the read is not needed
			o := <-ownCh
			ownCh = nil
			if errors.Is(o.err, errViewFailed) {
				s.failurePage(w, r, title, prefs, o.err)
				return
			}
			if o.st == stateReady && o.err == nil {
				data.Own = &o.res
			}
		}
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
			if data.Frame.Windows != nil {
				c := newCustom(data.Frame.Path, q, p, head.Window, s.now())
				if refused != nil {
					c.From, c.To, c.Err, c.FromBad, c.ToBad = refused.From, refused.To, refused.Err, refused.FromBad, refused.ToBad
				}
				data.Frame.Custom, data.Frame.CustomMenu = &c, c.inMenu()
			}
		}
		if kindOf[r.Pattern] != "text" {
			data.Frame.Switch = switchOf(data, head.View)
		}
		body, err := renderPage(s.tpl, spec.Name, data)
		if err != nil {
			refuse(w, http.StatusInternalServerError, "the page could not be drawn: run backsight doctor")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(keepMachines(body, data.Frame.Machines))
		if r.Method == http.MethodGet {
			s.served.Add(1)
		}
	}
}

// viewOut is the answer for one view of a page.
type viewOut struct {
	res metrics.Result
	st  indexState
	err error
}

// readViews computes the views of a page. A page that reads several asks for
// them together, on separate read transactions: its time is that of the
// slowest, not their sum (the Overview reads the days calendar beside its own
// numbers). The answers come back in the views' order, the caller judging
// them in it.
func (s *Server) readViews(ctx context.Context, spec pageSpec, p views.Params, id string) []viewOut {
	return readViewsWith(ctx, spec, p, id, s.data.result)
}

// viewReader reads one view: data.result, or a stand-in in a test.
type viewReader func(ctx context.Context, view string, p views.Params, id string) (metrics.Result, indexState, error)

// ownReadHook replaces the read of this PC's own answer in a test (nil in the program): set before the server
// starts.
var ownReadHook viewReader

// errViewFailed is what a view that panicked reports: the page fails, not
// the process. The panic's value stays out of it (it may name a path).
var errViewFailed = errors.New("a view of the page could not be read")

func readViewsWith(ctx context.Context, spec pageSpec, p views.Params, id string, read viewReader) []viewOut {
	outs := make([]viewOut, len(spec.Views))
	var wg sync.WaitGroup
	for i, v := range spec.Views {
		q := viewParams(spec, v, p)
		wg.Go(func() {
			defer func() {
				if recover() != nil {
					outs[i] = viewOut{st: stateReady, err: errViewFailed}
				}
			}()
			outs[i].res, outs[i].st, outs[i].err = read(ctx, v, q, id)
		})
	}
	wg.Wait()
	return outs
}

// firstProblem judges the answers in the views' order and returns the first
// that is not a result: bad.st is then a state that has a page of its own
// (building, updating, updated), or bad.err the failure to tell. So a
// failure of the first view is told even when the second is still building,
// and a building first view hides a failure of the second.
func firstProblem(outs []viewOut) (bad viewOut, ok bool) {
	for _, o := range outs {
		if o.st == stateReady && errors.Is(o.err, index.ErrUpdateInProgress) {
			o.st, o.err = stateUpdating, nil
		}
		switch o.st {
		case stateBuilding, stateUpdating, stateUpdated:
			return viewOut{st: o.st}, true
		}
		if o.st == stateBroken && o.err == nil {
			o.err = errNoIndex
		}
		if o.err != nil {
			return viewOut{st: o.st, err: o.err}, true
		}
	}
	return viewOut{}, false
}

// viewParams are the parameters view is asked with on the page spec. The
// Overview takes only the calendar of the days view, which spans 13 months
// whatever the window: it asks for a window of one day, so that the view does
// not also count the window's days, which the page does not show.
func viewParams(spec pageSpec, view string, p views.Params) views.Params {
	if spec.Name == "overview" && view == "days" {
		p.Days, p.DaysSet, p.Since, p.Until = 1, true, "", ""
	}
	return p
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
	_, _ = w.Write(keepMachines(buf.Bytes(), data.Frame.Machines))
}

// Switch is the scope line's choice between this PC and all PCs on this
// server, and what goes with it: the "Which PCs" list and the chips that say
// what the all-PCs figures leave out. Nil on a page without it (one PC shares
// on the server, or no server is set up): the line is then the shipped one.
type Switch struct {
	ThisHref, AllHref string // the "this PC" half drops machines; the other adds it
	N                 int    // the PCs all PCs counts, this one included
	AllSelected       bool   // all PCs is what the figures count
	Off               bool   // this PC's sharing is off: the second half is a link to Machines
	OffHref           string // (keepMachines adds the parameter a kept link carries)
	OffTitle          string
	NoneTitle         string
	AllNone           bool // the page is always this PC's: the second half is text, not a link
	Host              bool // the server is on this machine: the address says so
	Which             *WhichPCs
	Chips             []PCChip
}

// WhichPCs is the "Which PCs" list: who is counted in all PCs.
type WhichPCs struct {
	Title, Ignored, Fetched, SystemHref string
	Rows                                []PCItem
}

// PCItem is a PC of that list: its short id, what it runs and since when,
// and how fresh its figures are.
type PCItem struct {
	ID, Label, State string
	Stale            bool
}

// PCChip is a note after the scope line: what all PCs leaves out or reads
// from the cache. Href is "" for a note that links nowhere.
type PCChip struct {
	Class, Icon, Text, Title, Href string
}

const (
	machinesHref   = "/system#machines"
	thisOnlyHint   = "Projects, sessions and memories are this PC’s: other PCs send none"
	sharingOffHint = "This PC does not share its activity (feedback.share_ledger is off), so it does not read the other PCs’ either. See Machines on System."
)

// osWords name the operating systems a PC sends (R28).
var osWords = map[string]string{"linux": "Linux", "darwin": "macOS", "windows": "Windows"}

// osWord is the word for os; "" when this PC has not said (no document yet).
func osWord(os string) string {
	if os == "" {
		return ""
	}
	if w, ok := osWords[os]; ok {
		return w
	}
	return "Other"
}

// isLoopbackHost says host:port is this machine's own address.
func isLoopbackHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// addMachines is path with machines=all added (before its fragment).
func addMachines(path string) string {
	path, frag, hasFrag := strings.Cut(without(path, "machines"), "#")
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	path += sep + "machines=all"
	if hasFrag {
		path += "#" + frag
	}
	return path
}

// switchOf is the switch of the page d: nil when the scope line is the
// shipped one. It is shown when other PCs share (N > 1), when the URL asks
// for all PCs (a kept link keeps working and says what it counts) and, with
// a memory server set up, when this PC's sharing is off (to say why all PCs
// is not offered).
func switchOf(d PageData, view string) *Switch {
	f, sc := d.Frame, d.Frame.Scope
	if sc == nil || sc.PCs == nil {
		return nil
	}
	pcs, asked := sc.PCs, f.Machines == "all"
	if pcs.Sharing && pcs.Count <= 1 && !asked || !pcs.Sharing && sc.Server == "" && !asked {
		return nil
	}
	sw := &Switch{
		ThisHref: without(f.Path, "machines"), AllHref: addMachines(f.Path), N: pcs.Count,
		AllSelected: sc.Machines == "all", Off: !pcs.Sharing, OffHref: machinesHref, OffTitle: sharingOffHint,
		Host: isLoopbackHost(sc.Server), AllNone: metrics.ThisOnlyView(view) && pcs.Sharing, NoneTitle: thisOnlyHint,
	}
	if !sw.AllSelected {
		return sw
	}
	sw.Which = whichPCs(d, pcs)
	sw.Chips = pcChips(d, pcs)
	return sw
}

// shortDay is a UTC date as "Oct 21", with the year when it is not this one's.
func shortDay(day string, now time.Time) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	if t.Year() != now.Year() {
		return t.Format("Jan 2, 2006")
	}
	return t.Format("Jan 2")
}

// sentAgo is how long ago a PC sent its document, as views.Age writes it.
func sentAgo(sent, now time.Time) string {
	return views.Age(max(0, now.Sub(sent).Milliseconds()))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return strconv.Itoa(n) + " " + one
	}
	return strconv.Itoa(n) + " " + many
}

// serverSilent says the memory server did not answer the page's read, or
// the last one this PC recorded.
func serverSilent(f Frame) bool {
	if f.ServerRead != nil {
		return !f.ServerRead.OK
	}
	return f.Scope != nil && f.Scope.ServerOKAt != nil && !f.Scope.ServerOK
}

func whichPCs(d PageData, pcs *metrics.PCs) *WhichPCs {
	now := d.Frame.Generated
	w := &WhichPCs{Title: "Counted in “all PCs” (" + strconv.Itoa(pcs.Count) + ")", SystemHref: machinesHref}
	for _, r := range pcs.List {
		it := PCItem{ID: r.Machine, Stale: r.Stale}
		parts := []string{}
		if w := osWord(r.OS); w != "" {
			parts = append(parts, w)
		}
		if r.FirstSeen != "" {
			parts = append(parts, "since "+shortDay(r.FirstSeen, now))
		}
		switch {
		case r.This:
			this := "this PC"
			if isLoopbackHost(d.Frame.Scope.Server) {
				this += ", holds the memory server"
			}
			parts = append(parts, this)
			it.State = "its own figures, live"
		case r.SentAt == nil: // a counted PC always has a send time
			it.State = "figures not read yet"
		case r.Stale && r.Until != nil:
			it.State = "sent " + sentAgo(*r.SentAt, now) + " ago: its figures stop on " + shortDay(*r.Until, now)
		default:
			it.State = "sent " + sentAgo(*r.SentAt, now) + " ago"
			if !r.Active {
				it.State += " · no activity in this window"
			}
		}
		it.Label = strings.Join(parts, " · ")
		w.Rows = append(w.Rows, it)
	}
	if pcs.Ignored > 0 {
		if pcs.Ignored == 1 {
			w.Ignored = "1 document did not pass the checks and is left out. What it holds is not shown."
		} else {
			w.Ignored = strconv.Itoa(pcs.Ignored) + " documents did not pass the checks and are left out. What they hold is not shown."
		}
	}
	switch {
	case pcs.FetchedAt == nil:
		w.Fetched = "Other PCs’ figures have not been fetched yet; they are fetched every 6 h."
	case serverSilent(d.Frame):
		w.Fetched = "Fetched at " + format.Clock(now, *pcs.FetchedAt, d.Loc) + ": the memory server is not answering."
		if since := serverSince(d.Frame); !since.IsZero() {
			w.Fetched = "Fetched at " + format.Clock(now, *pcs.FetchedAt, d.Loc) + ": the memory server has not answered since " + format.Clock(now, since, d.Loc) + "."
		}
	default:
		w.Fetched = "Other PCs’ figures were fetched " + views.Age(max(0, now.Sub(*pcs.FetchedAt).Milliseconds())) + " ago; they are fetched every 6 h."
	}
	return w
}

// serverSince is when the server last answered; zero when unknown.
func serverSince(f Frame) time.Time {
	switch {
	case f.ServerRead != nil && f.ServerRead.LastOK != nil:
		return *f.ServerRead.LastOK
	case f.ServerRead == nil && f.Scope != nil && f.Scope.ServerOKAt != nil:
		return *f.Scope.ServerOKAt
	}
	return time.Time{}
}

// pcChips are the notes all PCs adds to the scope line, in order: the
// project filter that spares the other PCs, the PCs that went quiet, the
// documents left out and the cache read while the server is silent.
func pcChips(d PageData, pcs *metrics.PCs) []PCChip {
	now, sc := d.Frame.Generated, d.Frame.Scope
	var chips []PCChip
	if sc.OthersUnfiltered {
		chips = append(chips, PCChip{Class: "chip chip-nf", Text: "other PCs: not filtered",
			Title: "The project filter applies to this PC only: other PCs send no project names, so their figures are counted whole."})
	}
	if pcs.Stale > 0 {
		var quiet []metrics.PCRow
		var oldest *time.Time // the longest silence
		for _, r := range pcs.List {
			if r.Stale {
				quiet = append(quiet, r)
				if r.SentAt != nil && (oldest == nil || r.SentAt.Before(*oldest)) {
					oldest = r.SentAt
				}
			}
		}
		since := "over 2 days"
		if oldest != nil {
			since = sentAgo(*oldest, now)
		}
		// chip st-watch, not chip-stale: the stylesheet hides chip-stale for the
		// first 300 s (it is the "page is old" chip), which would hide a standing note.
		c := PCChip{Class: "chip st-watch", Icon: "i-clock", Href: machinesHref,
			Text: plural(pcs.Stale, "PC", "PCs") + ": nothing sent for " + since}
		if len(quiet) == 1 && quiet[0].Until != nil {
			c.Title = quiet[0].Machine + " last sent its figures " + since + " ago: its days after " + shortDay(*quiet[0].Until, now) + " are missing."
		} else {
			c.Title = "Their days since they last sent are missing from all PCs."
		}
		chips = append(chips, c)
	}
	if pcs.Ignored > 0 {
		text := "1 document ignored"
		if pcs.Ignored > 1 {
			text = strconv.Itoa(pcs.Ignored) + " documents ignored"
		}
		chips = append(chips, PCChip{Class: "chip", Icon: "i-info", Text: text, Href: machinesHref,
			Title: "A document from another PC did not pass the checks and is left out. Its content is not shown."})
	}
	if pcs.FetchedAt != nil && serverSilent(d.Frame) {
		chips = append(chips, PCChip{Class: "chip st-watch", Icon: "i-clock",
			Text: "other PCs as fetched at " + format.Clock(now, *pcs.FetchedAt, d.Loc)})
	}
	return chips
}

// machinesLink matches the opening tag of a link; its href is the one kept.
var (
	anchorTag  = regexp.MustCompile(`<a\s[^>]*>`)
	hrefAttr   = regexp.MustCompile(`\shref="(/[^"]*)"`)
	machinesIn = regexp.MustCompile(`[?&;]machines=|&amp;machines=`)
)

// isTextRoute says href is a memory's text page, which takes no parameter at
// all (it answers 400 to any): its links never carry machines.
func isTextRoute(href string) bool {
	path, _, _ := strings.Cut(href, "#")
	path, _, _ = strings.Cut(path, "?")
	return strings.HasPrefix(path, "/memories/") && strings.HasSuffix(path, "/text")
}

// keepMachines is the page body with machines=all on every link inside the
// dashboard but the preferences (which take the page to come back to), the
// assets, the memory text pages (no parameters) and the "this PC" half of the switch, which drops it. A link goes
// where the reader is: the forms carry the parameter as a hidden input. Every
// page with a reader asking for all PCs goes through here, so no link is
// forgotten when a page gains one.
func keepMachines(body []byte, machines string) []byte {
	if machines != "all" {
		return body
	}
	return anchorTag.ReplaceAllFunc(body, func(tag []byte) []byte {
		if bytes.Contains(tag, []byte(`data-machines="this"`)) {
			return tag
		}
		return hrefAttr.ReplaceAllFunc(tag, func(attr []byte) []byte {
			m := hrefAttr.FindSubmatch(attr)
			href := string(m[1])
			if strings.HasPrefix(href, "//") || strings.HasPrefix(href, "/prefs") || strings.HasPrefix(href, "/assets/") || isTextRoute(href) || machinesIn.MatchString(href) {
				return attr
			}
			path, frag, hasFrag := strings.Cut(href, "#")
			sep := "?"
			if strings.Contains(path, "?") {
				sep = "&amp;"
			}
			path += sep + "machines=all"
			if hasFrag {
				path += "#" + frag
			}
			return []byte(` href="` + path + `"`)
		})
	})
}
