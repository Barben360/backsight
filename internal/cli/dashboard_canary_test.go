package cli

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/paths"
)

var (
	pageCodeSpan = regexp.MustCompile(`(?s)<code>.*?</code>`)
	pageTagSpan  = regexp.MustCompile(`(?s)<[^>]*>`)
)

// visibleText is the page's text as a reader sees it: the text between
// tags, unescaped and trimmed, joined by single spaces; <code> contents and
// attribute values are gone.
func visibleText(page string) string {
	var out []string
	for _, s := range pageTagSpan.Split(pageCodeSpan.ReplaceAllString(page, ""), -1) {
		if s = strings.TrimSpace(html.UnescapeString(s)); s != "" {
			out = append(out, strings.Join(strings.Fields(s), " "))
		}
	}
	return strings.Join(out, " ")
}

// The texts the server holds for three memories. A memory's text shows on
// its own text page and nowhere else.
const (
	canaryMemText    = "CANARY-MEMORY-TEXT"
	canaryHiddenText = "CANARY-HIDDEN-TEXT"
	canaryThirdText  = "CANARY-THIRD-TEXT"
)

// canaryTexts are the memory ids whose text the server holds, with it.
var canaryTexts = map[string]string{"mem-allowed-2": canaryMemText, ctlMemory: canaryHiddenText, "mem-allowed-3": canaryThirdText}

// crawled is what a dashboard on the canary state answered.
type crawled struct {
	pages  map[string]string // every body by request, the memory text pages excluded
	texts  map[string]string // the memory text pages' bodies by memory id
	stderr string
	link   string // the first link, which holds the login code
	asked  int    // requests made: one body each
}

// canaryCrawl serves the canary state of the stats views (every source
// carries canaries; the server holds a memory's text, an operation's error
// message and a doctor check whose summary is a canary), asks the dashboard
// for every page route in each form of its id, dev off and on, every API
// route, and the 401, state and 404 pages, then stops it.
func canaryCrawl(t *testing.T) crawled {
	t.Helper()
	home := canaryState(t)
	port := fakeHindsight(t)
	cfgPath := pathsOf(home).ConfigFile()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(raw, []byte("\n[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	for id, text := range canaryTexts {
		memories[id] = `{"id":"` + id + `","text":"` + text + `","type":"world","tags":["project:` + ctlProject + `"]}`
	}
	projectEntities[ctlProject] = "[]"
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	fakeOperations = []string{
		`{"id":"op1","operation_type":"retain","status":"failed","document_id":"doc1","created_at":"` + at + `","updated_at":"` + at + `","retry_count":2,"error_message":"CANARY-OPERROR"}`,
		`{"id":"op2","operation_type":"retain","status":"completed","document_id":"doc2","created_at":"` + at + `","updated_at":"` + at + `","retry_count":0,"error_message":"CANARY-OPERROR"}`,
	}
	oldDoctor := doctorRun
	doctorRun = func(context.Context, doctor.Env) doctor.Report {
		return doctor.Report{Checks: []doctor.Check{{ID: "server", Status: doctor.Warn, Summary: "CANARY-DOCTOR", Detail: "CANARY-DOCTOR", Fix: "backsight doctor"}}}
	}
	t.Cleanup(func() { doctorRun = oldDoctor })

	buildIndex(t)
	day := dayWithPrompts(t)
	base, d, first, stderr, _, stop := serveDashboard(t)
	c := dashLogin(t, base, first)
	got := crawled{pages: map[string]string{}, texts: map[string]string{}, link: first}

	// every id form: the sessions, memories, project and day of the canary
	// state, the metric of the definitions
	ids := map[string][]string{
		"sessions": {"s1"},
		"memories": {ctlMemory, "mem-allowed-2", "mem-allowed-3"},
		"projects": {ctlProject},
		"days":     {day},
		"defs":     {"recall.hit_rate"},
	}
	pages := []string{"/", "/quality", "/cost", "/bank", "/pipeline", "/system", "/releases", "/defs",
		"/projects", "/sessions", "/memories", "/days"}
	for _, view := range []string{"projects", "sessions", "memories", "days"} {
		for _, id := range ids[view] {
			pages = append(pages, "/"+view+"/"+id)
		}
	}
	get := func(path string, want int) {
		t.Helper()
		status, body := dashGet(t, base, c, path)
		if status != want {
			t.Errorf("GET %s: %d, want %d: %.200s", path, status, want, body)
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("GET %s: an empty body", path)
		}
		got.pages[path] = body
		got.asked++
	}
	for _, dev := range []string{"0", "1"} {
		q := "?days=30&tz=UTC&dev=" + dev
		for _, p := range pages {
			get(p+q, http.StatusOK)
		}
		for _, view := range metrics.Views {
			get("/api/v1/"+view+q, http.StatusOK)
			for _, id := range ids[view] {
				get("/api/v1/"+view+"/"+id+q, http.StatusOK)
			}
		}
	}
	get("/api/v1/memories?sort=rate&state=hidden&tz=UTC", http.StatusOK)

	// the pages that stand for no data: not signed in, a refused query, an
	// id that names nothing, a path that is no page
	if status, body := dashGet(t, base, nil, "/"); status != http.StatusUnauthorized {
		t.Errorf("GET / without a session: %d", status)
	} else {
		got.pages["401"] = body
		got.asked++
	}
	get("/?days=0", http.StatusBadRequest)
	get("/sessions/zzzzzzzzzz?tz=UTC", http.StatusNotFound)
	get("/sessions/s?tz=UTC", http.StatusNotFound)
	get("/memories/zzzzzzzzzz?tz=UTC", http.StatusNotFound)
	get("/projects/nope?tz=UTC", http.StatusNotFound)
	get("/days/2026-13-01?tz=UTC", http.StatusNotFound)
	get("/nope", http.StatusNotFound)
	get("/api/v1/nope", http.StatusNotFound)
	get("/api/v1/sessions/zzzzzzzzzz?tz=UTC", http.StatusNotFound)
	get("/api/v1/overview?days=0", http.StatusBadRequest)

	// the one place a text is shown: each memory's own page, the hidden
	// one's and one the journal never hid included
	for id := range canaryTexts {
		status, body := dashGet(t, base, c, "/memories/"+id+"/text")
		if status != http.StatusOK {
			t.Errorf("the text page of %s: %d: %.200s", id, status, body)
		}
		got.texts[id] = body
	}

	// the preferences and the assets are static: they answer, and hold none
	if status, body := dashGet(t, base, c, "/prefs?theme=dark&back=%2F"); status != http.StatusSeeOther {
		t.Errorf("/prefs: %d, want 303: %.200s", status, body)
	} else {
		got.pages["/prefs"] = body + "\n"
		got.asked++
	}
	_, overview := dashGet(t, base, c, "/?tz=UTC")
	seen := map[string]bool{}
	for _, asset := range regexp.MustCompile(`/assets/[A-Za-z0-9._-]+`).FindAllString(overview, -1) {
		if !seen[asset] {
			seen[asset] = true
			get(asset, http.StatusOK)
		}
	}
	// the stylesheets and the script, whatever the logos: every asset the
	// dashboard embeds is linked from the overview, and was read above
	for _, base := range []string{"tokens.", "dashboard.", "copy."} {
		found := false
		for a := range seen {
			found = found || strings.HasPrefix(a, "/assets/"+base)
		}
		if !found {
			t.Errorf("the overview links no %s asset", base)
		}
	}

	stop()
	got.stderr = stderr.String()
	crawlStatePages(t, d, &got)
	return got
}

// crawlStatePages starts the dashboard again on the canary state with its
// index in each state that stands for the views, and keeps the pages and
// the API answers: an update being applied (the index needs a migration
// while an update is uncommitted), a first build under way (held until the
// dashboard stops), an index that a newer backsight changed, and an index
// that cannot be opened, whose error quotes a canary and a path.
func crawlStatePages(t *testing.T, d paths.Dirs, got *crawled) {
	t.Helper()
	oldStart := startServer
	t.Cleanup(func() { startServer = oldStart })
	for _, mode := range []struct {
		name   string
		status int
		prep   func()
		open   func(ctx context.Context) (dashboard.Index, error)
	}{
		{"updating", http.StatusServiceUnavailable, func() { needsMigration(t, d) }, nil},
		{"building", http.StatusServiceUnavailable, func() {
			if err := os.Remove(filepath.Join(d.IndexDir(), index.FileName)); err != nil {
				t.Fatal(err)
			}
		}, func(ctx context.Context) (dashboard.Index, error) {
			<-ctx.Done() // the first build is held until the dashboard stops
			return nil, ctx.Err()
		}},
		{"updated", http.StatusServiceUnavailable, func() { needsMigration(t, d) },
			func(context.Context) (dashboard.Index, error) {
				return nil, fmt.Errorf("CANARY-ERROR %s: %w", d.IndexDir(), index.ErrSchemaChanged)
			}},
		{"unreadable", http.StatusServiceUnavailable, func() { needsMigration(t, d) },
			func(context.Context) (dashboard.Index, error) {
				return nil, fmt.Errorf("CANARY-ERROR open %s: boom", filepath.Join(d.IndexDir(), index.FileName))
			}},
	} {
		mode.prep()
		if mode.open != nil {
			open := mode.open
			startServer = func(ctx context.Context, cfg dashboard.Config) (*dashboard.Server, error) {
				cfg.OpenIndex = open
				return oldStart(ctx, cfg)
			}
		} else {
			startServer = oldStart
		}
		base, _, first, _, _, stop := serveDashboard(t)
		c := dashLogin(t, base, first)
		for _, p := range []string{"/", "/sessions/s1", "/memories/mem-allowed-2/text", "/api/v1/overview"} {
			status, body := dashGet(t, base, c, p)
			if status != mode.status || strings.TrimSpace(body) == "" {
				t.Errorf("%s: GET %s: %d, want %d: %.200s", mode.name, p, status, mode.status, body)
			}
			got.pages[mode.name+" "+p] = body
			got.asked++
		}
		stop()
	}
}

// Free text of every source stays out of every page and every API answer
// of the dashboard (acceptance): the canary state of the stats views, the
// server's memory text, operation error and doctor summary, each page route
// in each form of its id, dev off and on, each API route, and the 401, state
// and 404 pages hold no canary; the memory's text page holds the text and no
// other canary (the positive control); the dashboard's stderr holds no canary
// and no login code.
func TestDashboardHoldsNoCanary(t *testing.T) {
	got := canaryCrawl(t)
	if got.asked < 80 || len(got.pages) != got.asked {
		t.Fatalf("%d requests, %d recorded: the scan misses most of the dashboard", got.asked, len(got.pages))
	}
	all := append(append([]string{}, canaries...), "CANARY-DOCTOR", "CANARY-OPERROR", "CANARY-HIDDEN-TEXT", "CANARY-THIRD-TEXT", "CANARY-MEMORY-TEXT", "CANARY")
	for path, body := range got.pages {
		for _, c := range all {
			if strings.Contains(body, c) {
				t.Errorf("%q in the answer to %s", c, path)
			}
		}
	}
	// the positive control: each text page holds its own memory's text and
	// no other canary, not the hidden one's note either
	for id, text := range canaryTexts {
		body := got.texts[id]
		if !strings.Contains(body, text) {
			t.Errorf("the text of %s did not come back on its page: %.300s", id, body)
		}
		for _, c := range all {
			if strings.Contains(strings.ReplaceAll(body, text, ""), c) {
				t.Errorf("%q on the text page of %s", c, id)
			}
		}
	}
	for _, c := range all {
		if strings.Contains(got.stderr, c) {
			t.Errorf("%q on stderr", c)
		}
	}
	code := got.link[strings.Index(got.link, "code=")+len("code="):]
	if code = strings.TrimSpace(code); len(code) != 32 || strings.Contains(got.stderr, code) {
		t.Errorf("the login code %q (a 32-character code is expected) is on stderr: %q", code, got.stderr)
	}
	// the failed operation and the doctor reached the pages as counts and
	// ids, so the scan above had something to hold back
	api := got.pages["/api/v1/pipeline?days=30&tz=UTC&dev=0"]
	if !strings.Contains(api, `"retries": 2`) {
		t.Errorf("the failed operation did not reach the pipeline view: %.300s", api)
	}
	if sys := got.pages["/api/v1/system?days=30&tz=UTC&dev=0"]; !strings.Contains(sys, `"backsight doctor"`) {
		t.Errorf("the doctor check did not reach the system view: %.300s", sys)
	}
}

// dayWithPrompts is a day of the canary state that holds a prompt, read from
// the fixture's own days view, not from the clock.
func dayWithPrompts(t *testing.T) string {
	t.Helper()
	v := runView(t, "stats", "days", "--tz", "UTC", "--days", "30")
	rows, _ := at(v.env.Data, "tables", "days").([]any)
	for _, r := range rows {
		if n, _ := at(r, "prompts").(float64); n > 0 {
			return at(r, "day").(string)
		}
	}
	t.Fatalf("no day with a prompt: %s", v.raw)
	return ""
}
