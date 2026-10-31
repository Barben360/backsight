package dashboard

import (
	"encoding/json"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// The ids and the project the escaping tests write into the fixture.
const (
	oddMemory  = "a:b"
	oddMemory2 = "m?1/x#y"
	oddProject = "my project"
	oddProj2   = "a/b?c#d"
)

// appendLines appends the lines to the newest month's file of dir named
// prefix, as the producers write them.
func appendLines(t *testing.T, dir, prefix string, lines []map[string]any) {
	t.Helper()
	path := filepath.Join(dir, prefix+indexgen.Now.Format("2006-01")+".jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatal(err)
		}
	}
}

// withOddNames writes sessions, prompts and showings whose project and
// memory ids hold characters a path must escape, and the ledger entries of
// the memories. It follows withIndex.
func withOddNames() harnessOpt {
	return func(h *harness, _ *Config) {
		t := h.t
		at := indexgen.Now.Add(-5 * time.Minute)
		var tel, jour []map[string]any
		for i, p := range []string{oddProject, oddProj2} {
			ts := at.Add(time.Duration(i) * time.Second)
			tel = append(tel, map[string]any{"event": "prompt", "ts": ts.Format(time.RFC3339Nano), "session": "sess-odd-" + strconv.Itoa(i), "project": p,
				"binary_version": "v0.1.17", "decision": "recall", "plen": 100, "above": 1, "injected": 1, "ms": 20, "top": 0.5,
				"ids": []string{oddMemory}, "lang": "en", "n": 1})
		}
		for i, id := range []string{oddMemory, oddMemory2} {
			jour = append(jour, map[string]any{"schema": 1, "seq": 2_001_000 + i, "ts": at.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
				"session": "sess-odd-" + strconv.Itoa(i), "turn": "turn-1", "project": []string{oddProject, oddProj2}[i], "kind": "turn", "n": 1})
			jour = append(jour, map[string]any{"schema": 1, "seq": 2_000_000 + i, "ts": at.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
				"session": "sess-odd-" + strconv.Itoa(i), "turn": "turn-1", "project": []string{oddProject, oddProj2}[i], "kind": "shows", "path": "prompt",
				"items": []map[string]any{{"id": id, "rank": 1, "reranker": 0.5}}})
		}
		appendLines(t, h.dirs.TelemetryDir(), "events-", tel)
		appendLines(t, h.dirs.FeedbackDir(), "journal-", jour)
		ledger := filepath.Join(h.dirs.FeedbackDir(), "ledger.json")
		raw, err := os.ReadFile(ledger)
		if err != nil {
			t.Fatal(err)
		}
		var l struct {
			Schema   int                       `json:"schema"`
			Memories map[string]map[string]any `json:"memories"`
		}
		if err := json.Unmarshal(raw, &l); err != nil {
			t.Fatal(err)
		}
		end := indexgen.End().Format(time.RFC3339)
		for _, id := range []string{oddMemory, oddMemory2} {
			l.Memories[id] = map[string]any{"shown": 3, "credits": 1.0, "credits_at": end, "last_shown": end}
		}
		if raw, err = json.Marshal(l); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ledger, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// apiPath is GET path as the API answers it, its data decoded.
func apiPath(t *testing.T, h *harness, path string) metrics.Result {
	t.Helper()
	var env struct {
		Data metrics.Result `json:"data"`
	}
	r := h.do("GET", path, cookieHdr(h.login())...)
	if err := json.Unmarshal([]byte(body(t, r)), &env); err != nil || r.StatusCode != 200 {
		t.Fatalf("GET %s: %d, %v", path, r.StatusCode, err)
	}
	return env.Data
}

func hrefs(page, rel string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<a href="([^"]*)"(?: rel="`+rel+`")`).FindAllStringSubmatch(page, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestBrowseListsMatchTheAPI(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	for _, l := range []struct {
		path, link string
		rows       func(r metrics.Result) int
	}{
		{"/projects", `href="/projects/`, func(r metrics.Result) int { return len(r.Tables.Projects) }},
		{"/sessions", `href="/sessions/`, func(r metrics.Result) int { return len(r.Tables.Sessions) }},
		{"/memories", `href="/memories/`, func(r metrics.Result) int { return len(r.Tables.Memories) }},
		{"/days", `href="/days/`, func(r metrics.Result) int { return len(r.Tables.Days) }},
	} {
		want := l.rows(apiPath(t, h, "/api/v1"+l.path))
		if want < 4 {
			t.Fatalf("%s: %d rows in the API, the fixture is too small to test paging", l.path, want)
		}
		r, b := pageGet(t, h, ck, l.path)
		if r.StatusCode != 200 {
			t.Fatalf("GET %s: %d", l.path, r.StatusCode)
		}
		if got := strings.Count(b, l.link); got != want {
			t.Errorf("%s: %d row links, the API has %d rows", l.path, got, want)
		}
		// Two rows from the second: a way back to the first page and a way on.
		page := l.path + "?limit=2&offset=1"
		r, b = pageGet(t, h, ck, page)
		if r.StatusCode != 200 {
			t.Fatalf("GET %s: %d", page, r.StatusCode)
		}
		if got := strings.Count(b, l.link); got != 2 {
			t.Errorf("%s: %d row links, want 2", page, got)
		}
		prev, next := hrefs(b, "prev"), hrefs(b, "next")
		if len(prev) != 1 || !strings.Contains(prev[0], "offset=0") || !strings.Contains(prev[0], "limit=2") {
			t.Errorf("%s: previous links %q, want one with offset=0 and limit=2", page, prev)
		}
		if len(next) != 1 || !strings.Contains(next[0], "offset=3") || !strings.Contains(next[0], "limit=2") {
			t.Errorf("%s: next links %q, want one with offset=3 and limit=2", page, next)
		}
		// The first page has no way back.
		_, b = pageGet(t, h, ck, l.path+"?limit=2")
		if got := hrefs(b, "prev"); len(got) != 0 {
			t.Errorf("%s?limit=2: previous links %q on the first page", l.path, got)
		}
	}
}

func TestPagerLinks(t *testing.T) {
	for _, c := range []struct {
		path       string
		page       metrics.Page
		prev, next string
	}{
		{"/sessions", metrics.Page{Limit: 50, Offset: 0, Total: 120}, "", "/sessions?offset=50"},
		{"/sessions?days=30&offset=50", metrics.Page{Limit: 50, Offset: 50, Total: 120}, "/sessions?days=30&offset=0", "/sessions?days=30&offset=100"},
		{"/sessions?offset=100&has=used", metrics.Page{Limit: 50, Offset: 100, Total: 120}, "/sessions?has=used&offset=50", ""},
		{"/sessions?limit=10&offset=3", metrics.Page{Limit: 10, Offset: 3, Total: 40}, "/sessions?limit=10&offset=0", "/sessions?limit=10&offset=13"},
		{"/sessions", metrics.Page{Limit: 50, Offset: 0, Total: 50}, "", ""},
	} {
		got := pagerOf(c.path, &c.page)
		if got.Prev != c.prev || got.Next != c.next {
			t.Errorf("%s %+v: prev %q next %q, want %q and %q", c.path, c.page, got.Prev, got.Next, c.prev, c.next)
		}
	}
	if got := pagerOf("/x", nil); got != (Pager{}) {
		t.Errorf("no page: %+v", got)
	}
}

func TestFilterLinks(t *testing.T) {
	links := filterLinks("/sessions?days=30&has=wrong&offset=50", "has")
	if len(links) != 1+len(views.HasValues()) {
		t.Fatalf("links %+v", links)
	}
	for _, l := range links {
		if strings.Contains(l.Href, "offset") {
			t.Errorf("%q keeps the offset", l.Href)
		}
		if !strings.Contains(l.Href, "days=30") {
			t.Errorf("%q drops another parameter", l.Href)
		}
		if l.Current != (l.Label == views.HasWords["wrong"]) {
			t.Errorf("%q: current %v", l.Label, l.Current)
		}
	}
	if links[0].Label != "All" || links[0].Href != "/sessions?days=30" {
		t.Errorf("the link that clears the filter: %+v", links[0])
	}
	if links[1].Href != "/sessions?days=30&has="+views.HasValues()[0] {
		t.Errorf("a value link: %q", links[1].Href)
	}
	for _, key := range []string{"sort", "state"} {
		for _, l := range filterLinks("/memories", key)[1:] {
			if strings.Contains(l.Label, "_") {
				t.Errorf("%s link %q shows a stored value as its label", key, l.Label)
			}
		}
	}
}

// section is the part of the page from the heading to the end of its section.
func section(page, heading string) string {
	_, rest, ok := strings.Cut(page, "<h2>"+heading+"</h2>")
	if !ok {
		return ""
	}
	sec, _, _ := strings.Cut(rest, "</section>")
	return sec
}

func TestIDPages(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	get := func(path string) string {
		t.Helper()
		r, b := pageGet(t, h, ck, path)
		if r.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, r.StatusCode)
		}
		return b
	}
	// A session with turns.
	var sid string
	var turns int
	for _, s := range apiPath(t, h, "/api/v1/sessions").Tables.Sessions {
		if tab := apiPath(t, h, "/api/v1/sessions/"+url.PathEscape(s.ID)).Tables.Session; tab != nil && len(tab.Turns) > 0 {
			sid, turns = s.ID, len(tab.Turns)
			break
		}
	}
	if sid == "" {
		t.Fatal("no session with turns in the fixture")
	}
	b := get("/sessions/" + url.PathEscape(sid))
	if got := strings.Count(section(b, "Turns"), "<tr>") - 1; got != turns {
		t.Errorf("session %s: %d turn rows, the API has %d", sid, got, turns)
	}
	// A memory.
	mem := apiPath(t, h, "/api/v1/memories").Tables.Memories[0].ID
	b = get("/memories/" + url.PathEscape(mem))
	if !strings.Contains(b, `href="/memories/`+url.PathEscape(mem)+`/text"`) || !strings.Contains(visibleText(b), "Show text") {
		t.Errorf("memory %s: no link to its text", mem)
	}
	if !strings.Contains(b, "<code>backsight forget "+mem+"</code>") {
		t.Errorf("memory %s: no forget command in <code>", mem)
	}
	// A project, a day.
	proj := apiPath(t, h, "/api/v1/projects").Tables.Projects[0].Project
	get("/projects/" + url.PathEscape(proj))
	day := apiPath(t, h, "/api/v1/days").Tables.Days[0].Day
	text := visibleText(get("/days/" + day))
	for _, id := range []string{"recall.hit_rate", "used.turn_rate", "verdict.wrong_rate", "context.per_session", "recall.latency", "pipeline.flow"} {
		def, _ := metrics.DefOf(id)
		if !strings.Contains(text, def.Question) {
			t.Errorf("day %s lacks the tile %q", day, def.Question)
		}
	}
	// The project page shows the same tiles.
	text = visibleText(get("/projects/" + url.PathEscape(proj)))
	for _, id := range []string{"recall.hit_rate", "used.turn_rate", "verdict.wrong_rate", "context.per_session", "recall.latency", "pipeline.flow"} {
		def, _ := metrics.DefOf(id)
		if !strings.Contains(text, def.Question) {
			t.Errorf("project %s lacks the tile %q", proj, def.Question)
		}
	}
}

func TestIDPagesAnswerNotFound(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	for _, p := range []string{"/sessions/nope-nope", "/memories/nope-nope", "/projects/nope-nope", "/days/not-a-day"} {
		r, b := pageGet(t, h, ck, p)
		if r.StatusCode < 400 || r.StatusCode >= 500 {
			t.Errorf("GET %s: %d, want a 4xx", p, r.StatusCode)
		}
		if strings.Contains(b, "<table>") {
			t.Errorf("GET %s shows a table", p)
		}
	}
}

func TestIDLinksAreEscaped(t *testing.T) {
	h := newDataHarness(t, withIndex(), withServer(), withDoctor(), withOddNames())
	ck := h.login()
	for _, c := range []struct{ list, link string }{
		{"/memories?limit=500", "/memories/a:b"},
		{"/memories?limit=500", "/memories/" + url.PathEscape(oddMemory2)},
		{"/projects?limit=500", "/projects/my%20project"},
		{"/projects?limit=500", "/projects/" + url.PathEscape(oddProj2)},
	} {
		r, b := pageGet(t, h, ck, c.list)
		if r.StatusCode != 200 {
			t.Fatalf("GET %s: %d", c.list, r.StatusCode)
		}
		if !strings.Contains(b, `href="`+c.link+`"`) {
			t.Errorf("%s has no link %q", c.list, c.link)
			continue
		}
		if r, _ := pageGet(t, h, ck, c.link); r.StatusCode != 200 {
			t.Errorf("following %s: %d", c.link, r.StatusCode)
		}
	}
	// A session of the odd project links back to it and to its memory.
	_, b := pageGet(t, h, ck, "/sessions/sess-odd-0")
	if !strings.Contains(b, `href="/memories/a:b"`) {
		t.Errorf("the turn rail lacks the link to the memory: %s", section(b, "Turns"))
	}
}

func TestDeveloperPagesInTheMenu(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	with := func(prefs, path string) (int, string) {
		hdr := ck.Name + "=" + ck.Value
		if prefs != "" {
			hdr += "; " + prefsCookie + "=" + prefs
		}
		r := h.do("GET", path, "Cookie", hdr)
		return r.StatusCode, html.UnescapeString(body(t, r))
	}
	for _, prefs := range []string{"", "v1.system.1.0"} {
		for _, p := range []string{"/releases", "/defs?limit=500"} {
			if code, _ := with(prefs, p); code != 200 {
				t.Errorf("GET %s with preference %q: %d", p, prefs, code)
			}
		}
		_, b := with(prefs, "/projects")
		for _, link := range []string{`href="/releases"`, `href="/defs"`} {
			if got := strings.Contains(b, link); got != (prefs != "") {
				t.Errorf("preference %q: menu has %s: %v", prefs, link, got)
			}
		}
	}
	_, b := with("", "/defs?limit=500")
	for _, d := range metrics.Catalog {
		if got := strings.Count(b, "<code>"+d.ID+"</code>"); got != 1 {
			t.Errorf("/defs lists %s %d times", d.ID, got)
		}
	}
	if got := strings.Count(section(b, "Definitions"), "<tr>") - 1; got != len(metrics.Catalog) {
		t.Errorf("/defs has %d rows, the catalog %d", got, len(metrics.Catalog))
	}
}

func TestReleasesListsVersionsAndSettings(t *testing.T) {
	h := dataHarness(t)
	res := apiPath(t, h, "/api/v1/releases")
	if len(res.Tables.Releases) == 0 || len(res.Tables.Settings) == 0 {
		t.Fatalf("the fixture has %d releases, %d settings", len(res.Tables.Releases), len(res.Tables.Settings))
	}
	_, b := pageGet(t, h, h.login(), "/releases")
	text := visibleText(b)
	for _, r := range res.Tables.Releases {
		if !strings.Contains(text, r.Version) {
			t.Errorf("/releases lacks the version %s", r.Version)
		}
		for _, id := range metrics.ReleaseFlagship {
			def, _ := metrics.DefOf(id)
			if want := views.FormatValue(def, r.Flagship[id]); want != "—" && !strings.Contains(text, want) {
				t.Errorf("version %s lacks %s = %q", r.Version, id, want)
			}
		}
	}
	for _, w := range []string{res.Tables.Note, "feedback.boost_weight"} {
		if !strings.Contains(text, w) {
			t.Errorf("/releases lacks %q", w)
		}
	}
	if !strings.Contains(b, "<code>backsight feedback replay --set") {
		t.Error("/releases lacks the replay pointer in <code>")
	}
}

func TestProjectPageWithoutMemories(t *testing.T) {
	for _, none := range []bool{true, false} {
		b := render(t, "project", func(d *PageData) {
			p := d.V["projects"].Tables.Project
			p.NoMemories, p.Command = none, "backsight import --project zz"
		})
		got := strings.Contains(visibleText(b), "This project has no memories yet. Import its past sessions:")
		if got != none {
			t.Errorf("no memories %v: sentence shown %v", none, got)
		}
		if has := strings.Contains(b, "<code>backsight import --project zz</code>"); has != none {
			t.Errorf("no memories %v: command in <code> %v", none, has)
		}
	}
}

func TestProjectNamesAreClippedWithTheFullNameOnHover(t *testing.T) {
	long := strings.Repeat("p", 30)
	b := render(t, "projects", func(d *PageData) { d.V["projects"].Tables.Projects[0].Project = long })
	if !strings.Contains(b, `title="`+long+`">`+views.Clip(long)+`</a>`) || strings.Contains(visibleText(b), long) {
		t.Errorf("the long name is not clipped with its full name in title")
	}
	id := strings.Repeat("a", 30)
	b = render(t, "memories", func(d *PageData) { d.V["memories"].Tables.Memories[0].ID = id })
	if !strings.Contains(b, `title="`+id+`">`+id[:8]+`</a>`) || strings.Contains(visibleText(b), id) {
		t.Errorf("the long id is not cut to 8 characters with its full id in title")
	}
}

func TestBrowseTablesShowWordsNotStoredValues(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	one := int64(1)
	chip := func(path, outcome string) metrics.ShowChip {
		return metrics.ShowChip{MemoryID: "mem-chip-" + path, Path: path, Outcome: outcome, Rank: &one}
	}
	sessions := func(tabs *metrics.Tables, sent string) {
		for i := range tabs.Sessions {
			tabs.Sessions[i].Sent = sent
		}
	}
	type page struct {
		name string
		edit func(d *PageData)
		want []string
		not  []string
	}
	notStored := []string{"confident_miss", "never_used", "quarantined", "same_topic", "too_short", "below_floor", "unhide", "no_candidates", "undeclared", "outdated", "batch_retain"}
	for _, p := range []page{
		{"sessions", func(d *PageData) { sessions(&d.V["sessions"].Tables, "quarantined") }, []string{"set aside after an error"}, notStored},
		{"session", func(d *PageData) {
			s := d.V["sessions"].Tables.Session
			s.Totals.Sent = "retry"
			s.Turns = []metrics.TurnRow{
				{N: &one, At: now, Decision: "skip", Reason: "same_topic", Check: "undeclared"},
				{N: &one, At: now, Decision: "recall", Empty: "below_floor", Declared: []string{"outdated", "none"},
					Shows: []metrics.ShowChip{chip("gate", "confident_miss"), chip("failure", "unknown"), chip("briefing", "credit")}, Check: "skipped"},
			}
		}, []string{"retrying", "no memory looked up", "same topic as before", "didn't say", "below the cut-off", "out of date, none used", "before a question",
			"shown, then ignored", "after an error", "not declared", "at session start", "used by Claude", "check skipped", "looked for memory"}, notStored},
		{"memories", func(d *PageData) {
			for i := range d.V["memories"].Tables.Memories {
				d.V["memories"].Tables.Memories[i].State = "never_used"
			}
		}, []string{"often shown, never used", "recent-weighted counts"}, notStored},
		{"memory", func(d *PageData) {
			m := d.V["memories"].Tables.Memory
			m.Kind, m.ServerState = "world", "active"
			m.Row.State = "wrong"
			m.Shows = []metrics.ShowMark{{At: now, Path: "search", Outcome: "confident_miss", Session: "sess-1", Rank: &one}}
			m.Retirements = []metrics.RetireMark{{At: now, Kind: "unhide"}, {At: now, Kind: "hide"}}
		}, []string{"facts about you and your work", "available", "reported wrong", "Claude's search", "shown, then ignored", "restored", "hidden"}, notStored},
		{"day", func(d *PageData) { sessions(&metrics.Tables{Sessions: d.V["days"].Tables.Day.Sessions}, "failed") }, []string{"failed"}, notStored},
		{"project", func(d *PageData) {
			for i := range d.V["projects"].Tables.Project.Sessions {
				d.V["projects"].Tables.Project.Sessions[i].Sent = "queued"
			}
			d.V["projects"].Tables.Project.NeverUsed = []metrics.ReviewRow{{ID: "mem-never-0001", Shown: 12, Command: "backsight forget mem-never-0001"}}
		}, []string{"waiting", "mem-neve", "<code>backsight forget mem-never-0001</code>"}, notStored},
	} {
		b := render(t, p.name, p.edit)
		text := visibleText(b)
		for _, w := range p.want {
			if !strings.Contains(b, w) && !strings.Contains(text, w) {
				t.Errorf("%s lacks %q in %q", p.name, w, text)
			}
		}
		for _, r := range p.not {
			if hasWord(text, r) {
				t.Errorf("%s shows the stored value %q: %q", p.name, r, text)
			}
		}
	}
}

// browsePaths are the browse pages: the four lists, a filtered and an
// ordered list, and the first project, session, memory and day of the lists.
func browsePaths(t *testing.T, h *harness) []string {
	t.Helper()
	return []string{"/projects", "/sessions", "/memories", "/days", "/sessions?has=used", "/memories?state=never_used&sort=rate",
		"/projects/" + url.PathEscape(apiPath(t, h, "/api/v1/projects").Tables.Projects[0].Project),
		"/sessions/" + url.PathEscape(apiPath(t, h, "/api/v1/sessions").Tables.Sessions[0].ID),
		"/memories/" + url.PathEscape(apiPath(t, h, "/api/v1/memories").Tables.Memories[0].ID),
		"/days/" + apiPath(t, h, "/api/v1/days").Tables.Days[0].Day}
}

func TestBrowsePagesDoNotCryWolfAboutTheServer(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	mem := apiPath(t, h, "/api/v1/memories").Tables.Memories[0].ID
	for _, p := range []string{"/projects", "/sessions", "/memories", "/days", "/releases", "/defs", "/memories/" + url.PathEscape(mem)} {
		if _, b := pageGet(t, h, ck, p); strings.Contains(visibleText(b), "not answering") {
			t.Errorf("%s with a server that answers: %q", p, visibleText(b))
		}
	}
}
