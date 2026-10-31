package cli

import (
	"context"
	"html"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/metrics"
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

// canaryMemText is the one text a page may hold: the memory's own page
// shows it on request.
const canaryMemText = "CANARY-MEMORY-TEXT"

// crawled is what a dashboard on the canary state answered.
type crawled struct {
	pages  map[string]string // every body by request path, memory text excluded
	text   string            // the body of the memory text page
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
	memories["mem-allowed-2"] = `{"id":"mem-allowed-2","text":"` + canaryMemText + `","type":"world","tags":["project:` + ctlProject + `"]}`
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
	base, _, first, stderr, _, stop := serveDashboard(t)
	c := dashLogin(t, base, first)
	got := crawled{pages: map[string]string{}, link: first}

	// every id form: the sessions, memories, project and day of the canary
	// state, the metric of the definitions
	day := time.Now().UTC().Format("2006-01-02")
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
	get("/days/2026-13-01?tz=UTC", http.StatusBadRequest)
	get("/nope", http.StatusNotFound)
	get("/api/v1/nope", http.StatusNotFound)
	get("/api/v1/sessions/zzzzzzzzzz?tz=UTC", http.StatusNotFound)
	get("/api/v1/overview?days=0", http.StatusBadRequest)

	// the one place the text is shown
	status, body := dashGet(t, base, c, "/memories/mem-allowed-2/text")
	if status != http.StatusOK {
		t.Errorf("the memory text page: %d: %.200s", status, body)
	}
	got.text = body

	stop()
	got.stderr = stderr.String()
	return got
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
		t.Fatalf("%d requests, %d distinct bodies: the scan misses most of the dashboard", got.asked, len(got.pages))
	}
	all := append(append([]string{}, canaries...), "CANARY-DOCTOR", "CANARY-OPERROR", "CANARY")
	for path, body := range got.pages {
		for _, c := range all {
			if strings.Contains(body, c) {
				t.Errorf("%q in the answer to %s", c, path)
			}
		}
	}
	if !strings.Contains(got.text, canaryMemText) {
		t.Errorf("the memory text did not come back on its page: %.300s", got.text)
	}
	for _, c := range all {
		if strings.Contains(strings.ReplaceAll(got.text, canaryMemText, ""), c) {
			t.Errorf("%q on the memory text page", c)
		}
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
