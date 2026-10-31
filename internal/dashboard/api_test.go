package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index"
	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/views"
)

// apiGet is the status and decoded body of GET path under a session.
func apiGet(t *testing.T, h *harness, ck *http.Cookie, path string) (*http.Response, map[string]any) {
	t.Helper()
	r := h.do("GET", path, cookieHdr(ck)...)
	if ct := r.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("GET %s: Content-Type %q", path, ct)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body(t, r)), &m); err != nil {
		t.Fatalf("GET %s: body is not JSON: %v", path, err)
	}
	return r, m
}

// path digs into a decoded body: keys joined by dots.
func dig(t *testing.T, m map[string]any, path string) any {
	t.Helper()
	var v any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s: %q is not an object", path, k)
		}
		v = mm[k]
	}
	return v
}

// wantError checks an error envelope.
func wantError(t *testing.T, r *http.Response, m map[string]any, status int, command string, code int, msg string) {
	t.Helper()
	if r.StatusCode != status {
		t.Errorf("status %d, want %d", r.StatusCode, status)
	}
	if m["ok"] != false || m["command"] != command || m["schema_version"] != float64(1) {
		t.Errorf("envelope %v, want ok false, command %q, schema_version 1", m, command)
	}
	if _, has := m["data"]; has {
		t.Errorf("an error carries data: %v", m["data"])
	}
	if got := dig(t, m, "error.code"); got != float64(code) {
		t.Errorf("error.code %v, want %d", got, code)
	}
	if got := dig(t, m, "error.message"); got != msg {
		t.Errorf("error.message %q, want %q", got, msg)
	}
}

// direct is what `backsight stats <view> [<id>] --json` prints for p: the
// view run on the dashboard's source, written by the CLI's writer.
func direct(t *testing.T, h *harness, view string, p views.Params, id string) string {
	t.Helper()
	if st, err := h.s.data.check(context.Background()); st != stateReady || err != nil {
		t.Fatalf("check: %d, %v", st, err)
	}
	src, err := h.s.data.source(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	q, err := views.Query(view, p, id, h.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	res, err := metrics.Run(context.Background(), view, src, q)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := output.WriteJSON(&buf, "stats "+view, res, nil); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestAPIServesTheView(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	r, got := apiGet(t, h, ck, "/api/v1/overview")
	if r.StatusCode != 200 || got["command"] != "stats overview" || got["ok"] != true {
		t.Fatalf("status %d, envelope %v", r.StatusCode, got)
	}
	if dig(t, got, "data.view") != "overview" || dig(t, got, "data.view_schema") != float64(1) {
		t.Errorf("view %v schema %v", dig(t, got, "data.view"), dig(t, got, "data.view_schema"))
	}

	// The same view run directly, printed by the CLI's writer: the same bytes.
	if want := direct(t, h, "overview", views.Params{Limit: metrics.DefaultLimit}, ""); body(t, r) != want {
		t.Errorf("the API differs from the view run directly:\n got %s\nwant %s", body(t, r), want)
	}

	// HEAD: the headers of the GET, no body.
	hd := h.do("HEAD", "/api/v1/overview", cookieHdr(ck)...)
	if hd.StatusCode != 200 || hd.Header.Get("Content-Type") != "application/json; charset=utf-8" || body(t, hd) != "" {
		t.Errorf("HEAD: %d, %q, body %q", hd.StatusCode, hd.Header.Get("Content-Type"), body(t, hd))
	}
}

func TestAPIParams(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	_, m := apiGet(t, h, ck, "/api/v1/overview?days=30")
	if got := dig(t, m, "data.window.days"); got != float64(30) {
		t.Errorf("days=30: window.days %v", got)
	}
	_, m = apiGet(t, h, ck, "/api/v1/overview?since=2026-10-01&until=2026-10-08&tz=UTC")
	if s, u := dig(t, m, "data.window.since"), dig(t, m, "data.window.until"); s != "2026-10-01T00:00:00Z" || u != "2026-10-08T00:00:00Z" {
		t.Errorf("window %v to %v", s, u)
	}
	r, m := apiGet(t, h, ck, "/api/v1/overview?limit=501")
	wantError(t, r, m, 400, "stats overview", 2, "--limit: a whole number from 1 to 500")

	for _, q := range []string{"?days=3;limit=0", "?days=%zz", "?project=%"} {
		r, m := apiGet(t, h, ck, "/api/v1/overview"+q)
		wantError(t, r, m, 400, "stats overview", 2, "the query string is malformed")
	}
	for _, q := range []string{"?days=3&days=4", "?limit=5&limit=6", "?project=a&project=b"} {
		r, m := apiGet(t, h, ck, "/api/v1/overview"+q)
		if r.StatusCode != 400 || dig(t, m, "error.code") != float64(2) || !strings.HasPrefix(fmt.Sprint(dig(t, m, "error.message")), "give ") {
			t.Errorf("%s: %d %v, want 400 'give <key> once'", q, r.StatusCode, m["error"])
		}
	}

	mem := "/api/v1/memories/mem-a1b2c3d4"
	for path, param := range map[string]string{
		"/api/v1/overview?has=used":     "has",
		"/api/v1/overview?foo=1":        "foo",
		mem + "?text=1":                 "text",
		"/api/v1/overview?machines=all": "machines",
	} {
		r, m := apiGet(t, h, ck, path)
		if r.StatusCode != 400 || dig(t, m, "error.code") != float64(2) || !strings.Contains(fmt.Sprint(dig(t, m, "error.message")), `"`+param+`"`) {
			t.Errorf("%s: %d %v, want 400 naming %q", path, r.StatusCode, m["error"], param)
		}
	}
}

// TestAPIParamsReachTheView: each parameter the API reads changes the
// view as it does on the command line (the same bytes as the view run
// with that parameter), and differs from the view without it.
func TestAPIParamsReachTheView(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	def := views.Params{Limit: metrics.DefaultLimit}
	for _, c := range []struct {
		view, query string
		p           func(*views.Params)
	}{
		{"overview", "project=proj-3", func(p *views.Params) { p.Project = "proj-3" }},
		{"overview", "path=briefing", func(p *views.Params) { p.Path = "briefing" }},
		{"overview", "dev=1", func(p *views.Params) { p.Dev, p.DevSet = true, true }},
		{"sessions", "has=memory", func(p *views.Params) { p.Has = "memory" }},
		{"sessions", "offset=2&limit=3", func(p *views.Params) { p.Offset, p.Limit = 2, 3 }},
		{"memories", "sort=rate", func(p *views.Params) { p.Sort = "rate" }},
		{"memories", "state=never_used", func(p *views.Params) { p.State = "never_used" }},
	} {
		p := def
		c.p(&p)
		got := body(t, h.do("GET", "/api/v1/"+c.view+"?"+c.query, cookieHdr(ck)...))
		if want := direct(t, h, c.view, p, ""); got != want {
			t.Errorf("%s?%s: differs from the view run with %+v", c.view, c.query, p)
		}
		// path and dev reach the view but the generated data has nothing they filter.
		if got == direct(t, h, c.view, def, "") && c.p != nil && !strings.HasPrefix(c.query, "path=") && !strings.HasPrefix(c.query, "dev=") {
			t.Errorf("%s?%s: the parameter changed nothing", c.view, c.query)
		}
	}
}

func TestAPINotFound(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	list := "one of overview, quality, cost, bank, pipeline, system, releases, projects, sessions, memories, days, defs"
	for _, v := range []string{"nope", "index"} {
		r, m := apiGet(t, h, ck, "/api/v1/"+v)
		wantError(t, r, m, 404, "stats "+v, 2, fmt.Sprintf("unknown view %q: %s", v, list))
	}
	r, m := apiGet(t, h, ck, "/api/v1/sessions/zzzzzzzz")
	wantError(t, r, m, 404, "stats sessions", 2, `no session "zzzzzzzz" in the metrics index`)
	r, m = apiGet(t, h, ck, "/api/v1/overview/x")
	wantError(t, r, m, 404, "stats overview", 2, "no such page")
}

func TestAPIStates(t *testing.T) {
	const building = "the metrics index is being built: try again in a few seconds"
	cases := []struct {
		name string
		h    func(t *testing.T) *harness
		msg  string
	}{
		{"building", func(t *testing.T) *harness {
			return newDataHarness(t, withIndex(func(f *fakeIndex) { f.openGate = make(chan struct{}) }), withServer(), withNeedsBuild()) // the build waits for the gate until the test ends
		}, building},
		{"updating", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.openErrs = []error{index.ErrUpdateInProgress, index.ErrUpdateInProgress} })
		}, views.UpdatingSentence},
		{"updated", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.side = true })
		}, "backsight was updated: restart the dashboard (Ctrl+C, then backsight dashboard)"},
		{"broken", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.openErrs = []error{index.ErrUpdateInProgress, errors.New("boom /secret/path")} }) // Start meets the first
		}, ""},
		{"catch-up meets an update", func(t *testing.T) *harness {
			return dataHarness(t, func(f *fakeIndex) { f.catchUpErr = index.ErrUpdateInProgress })
		}, views.UpdatingSentence},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := c.h(t)
			r, m := apiGet(t, h, h.login(), "/api/v1/overview")
			if r.StatusCode != 503 || m["ok"] != false || m["command"] != "stats overview" {
				t.Fatalf("%d %v, want 503 and ok false", r.StatusCode, m)
			}
			msg := fmt.Sprint(dig(t, m, "error.message"))
			if c.msg != "" && msg != c.msg {
				t.Errorf("message %q, want %q", msg, c.msg)
			}
			if c.name == "broken" && (msg == "" || strings.Contains(msg, "secret")) {
				t.Errorf("broken message %q: want a sentence without the error's text", msg)
			}
			if dig(t, m, "error.code") != float64(20) {
				t.Errorf("code %v, want 20", dig(t, m, "error.code"))
			}
			if got, want := r.Header.Get("Retry-After"), map[bool]string{true: "2"}[c.name == "building"]; got != want {
				t.Errorf("Retry-After %q, want %q", got, want)
			}
		})
	}
}

// A request that can never succeed is told so, whatever the state.
func TestAPIBadParamWhileNotReady(t *testing.T) {
	for name, h := range map[string]*harness{
		"building": newDataHarness(t, withIndex(func(f *fakeIndex) { f.openGate = make(chan struct{}) }), withServer(), withNeedsBuild()),
		"updated":  dataHarness(t, func(f *fakeIndex) { f.side = true }),
	} {
		r, m := apiGet(t, h, h.login(), "/api/v1/overview?limit=0")
		wantError(t, r, m, 400, "stats overview", 2, "--limit: a whole number from 1 to 500")
		if r.Header.Get("Retry-After") != "" {
			t.Errorf("%s: Retry-After on a request that cannot succeed", name)
		}
	}
}

// An index error of a class of its own is blocked (503) with the sentence
// the command line gives, not a generic failure.
func TestAPIIndexErrorClasses(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		msg  string
	}{
		{"busy", index.ErrInUse, index.ErrInUse.Error()},
		{"unreadable", index.ErrUnreadable, views.UnreadableSentence},
		{"full", index.ErrDiskFull, index.ErrDiskFull.Error()},
		{"updating", index.ErrUpdateInProgress, views.UpdatingSentence},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := dataHarness(t, func(f *fakeIndex) { f.catchUpErr = c.err })
			r, m := apiGet(t, h, h.login(), "/api/v1/overview")
			wantError(t, r, m, 503, "stats overview", 20, c.msg)
		})
	}
}

func TestAPIUnclassifiedFailureIs500(t *testing.T) {
	h := dataHarness(t, func(f *fakeIndex) { f.catchUpErr = errors.New("disk exploded at /home/x/secret.db") })
	r, m := apiGet(t, h, h.login(), "/api/v1/overview")
	wantError(t, r, m, 500, "stats overview", 20, "the view failed: run backsight doctor")
}

func TestHTTPStatus(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{metrics.NotFoundError{Kind: "session", ID: "x"}, 404},
		{fmt.Errorf("wrapped: %w", metrics.NotFoundError{Kind: "memory", ID: "x"}), 404},
		{fmt.Errorf("%w %q", metrics.ErrUnknownView, "x"), 404},
		{context.DeadlineExceeded, 503},
		{fmt.Errorf("catch up: %w", context.DeadlineExceeded), 503},
		{index.ErrUpdateInProgress, 503},
		{views.Usage("bad"), 400},
		{&output.Error{Code: output.Blocked, Message: "x"}, 503},
		{&output.Error{Code: output.NeedsUser, Message: "x"}, 500},
		{fmt.Errorf("catch up: %w", index.ErrInUse), 503},
		{fmt.Errorf("catch up: %w", index.ErrUnreadable), 503},
		{fmt.Errorf("catch up: %w", index.ErrDiskFull), 503},
		{index.ErrSchemaChanged, 503},
		{errors.New("other"), 500},
		{context.Canceled, 500},
	} {
		if got := httpStatus(c.err); got != c.want {
			t.Errorf("httpStatus(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestNoWorkBeforeAuthentication(t *testing.T) {
	h := dataHarness(t)
	// The doctor's warm-up runs after Start: wait for it, then count.
	deadline := context.Background()
	if _, err := h.s.data.src.Doctor(deadline); err != nil {
		t.Fatal(err)
	}
	doctors := h.doctorRuns.Load()
	expired, err := MintCode(h.dirs, h.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	// Past the code's life and the doctor's kept answer: a doctor run
	// before authentication would now run again and be counted.
	h.clock.add(doctorTTL + time.Second)
	never := "0000000000000000000000000000000000000000000"
	wrong := &http.Cookie{Name: cookieName(h.port), Value: "not-a-session"}
	for _, c := range []struct {
		path string
		hdr  []string
		want int
	}{
		{"/", nil, 401},
		{"/api/v1/overview", nil, 401},
		{"/api/v1/sessions/x", nil, 401},
		{"/memories/x/text", nil, 401},
		{"/quality?code=" + never, []string{"Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"}, 401},
		{"/quality?code=" + expired, []string{"Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"}, 401},
		{"/api/v1/overview", cookieHdr(wrong), 401},
		{"/api/v1/overview", []string{"Host", "evil.example:" + fmt.Sprint(h.port)}, 421},
		{"/api/v1/overview", []string{"Sec-Fetch-Site", "cross-site"}, 403},
	} {
		if r := h.do("GET", c.path, c.hdr...); r.StatusCode != c.want {
			t.Errorf("GET %s: %d, want %d", c.path, r.StatusCode, c.want)
		}
	}
	cu, rt, _ := h.ix.counts()
	if cu != 0 || rt != 0 || len(h.srv.calls) != 0 || h.doctorRuns.Load() != doctors {
		t.Errorf("work before authentication: %d catch-ups, %d read transactions, server %v, doctor runs %d (was %d)",
			cu, rt, h.srv.calls, h.doctorRuns.Load(), doctors)
	}

	ck := h.login()
	if r := h.do("GET", "/api/v1/overview", cookieHdr(ck)...); r.StatusCode != 200 {
		t.Fatalf("authenticated: %d", r.StatusCode)
	}
	h.srv.mu.Lock()
	calls := len(h.srv.calls)
	h.srv.mu.Unlock()
	if cu, _, _ := h.ix.counts(); cu != 1 || calls == 0 {
		t.Errorf("authenticated: %d catch-ups, %d kinds of server call; want 1 and some", cu, calls)
	}
}

func TestAPIServerDown(t *testing.T) {
	old := metrics.ServerTimeout
	metrics.ServerTimeout = 20 * time.Millisecond
	t.Cleanup(func() { metrics.ServerTimeout = old })
	h := dataHarness(t)
	h.srv.set(func(f *fakeServer) { f.down = true })
	r, m := apiGet(t, h, h.login(), "/api/v1/overview")
	if r.StatusCode != 200 {
		t.Fatalf("status %d: %v", r.StatusCode, m)
	}
	if dig(t, m, "data.server.ok") != false {
		t.Errorf("data.server.ok %v, want false", dig(t, m, "data.server.ok"))
	}
	hit, ok := dig(t, m, "data.metrics").(map[string]any)["recall.hit_rate"].(map[string]any)
	if !ok || hit["status"] == "no_data" {
		t.Errorf("recall.hit_rate %v: want a status other than no_data", hit)
	}
}

func TestRenderTimeout(t *testing.T) {
	old := renderTimeout
	renderTimeout = 20 * time.Millisecond
	t.Cleanup(func() { renderTimeout = old })
	h := dataHarness(t)
	h.ix.set(func(f *fakeIndex) { f.block = make(chan struct{}) }) // held until the context ends
	r, m := apiGet(t, h, h.login(), "/api/v1/overview")
	wantError(t, r, m, 503, "stats overview", 20, "the page took too long: try again")
}

// The guard bounds a request on its own, whatever the cache does: the
// version check of the index is outside every cache.
func TestGuardBoundsTheRequest(t *testing.T) {
	old := renderTimeout
	renderTimeout = 20 * time.Millisecond
	t.Cleanup(func() { renderTimeout = old })
	h := dataHarness(t)
	ck := h.login()
	h.client.Timeout = 10 * time.Second                   // a fuse: without the guard's bound the request never ends
	h.ix.set(func(f *fakeIndex) { f.versionHold = true }) // held until the request's context ends
	r, m := apiGet(t, h, ck, "/api/v1/overview")
	wantError(t, r, m, 503, "stats overview", 20, "the page took too long: try again")
	if r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("security headers missing on the timeout: %v", r.Header)
	}
}
