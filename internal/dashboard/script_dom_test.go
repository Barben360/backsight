//go:build !race

package dashboard

// The race detector's slowdown breaks the one-second window of the g key,
// so this file is not built under -race (same split as race_off_test.go).

import (
	"bytes"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// domHarness fronts a started dashboard with a test-only wrapper on its own
// listener: it logs every request, serves testdata/harness.js, and adds the
// harness script to HTML. The calendar, Copy summary button and summary text
// the scenarios drive are the Overview's own.
type domHarness struct {
	h    *harness
	keys atomic.Bool // whether the preference cookie turns shortcuts on

	mu  sync.Mutex
	log []string
}

func newDOMHarness(t *testing.T) *domHarness {
	t.Helper()
	d := &domHarness{h: dataHarness(t)}
	ck := d.h.login()
	script, err := os.ReadFile("testdata/harness.js")
	if err != nil {
		t.Fatal(err)
	}
	copyTag := `<script src="` + assetPath("copy.js") + `" defer></script>`
	inner := d.h.s.srv.Handler
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_test/harness.js", func(w http.ResponseWriter, r *http.Request) {
		d.record(r)
		securityHeaders(w.Header(), false)
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write(script)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.record(r)
		prefs := Prefs{Theme: "light", Keys: d.keys.Load()}
		r.Header.Set("Cookie", ck.Name+"="+ck.Value+"; "+prefsCookie+"="+prefs.cookie())
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		out := rec.Body.Bytes()
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			out = bytes.Replace(out, []byte(copyTag), []byte(copyTag+`<script src="/_test/harness.js" defer></script>`), 1)
			rec.Header().Set("Content-Length", strconv.Itoa(len(out)))
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(out)
	})
	d.h.s.srv.Handler = mux
	return d
}

func (d *domHarness) record(r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log = append(d.log, r.Method+" "+r.URL.RequestURI())
}

func (d *domHarness) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.log...)
}

// indexOf is the position of the first request equal to want, or -1.
func indexOf(log []string, want string) int {
	for i, l := range log {
		if l == want {
			return i
		}
	}
	return -1
}

var resultRE = regexp.MustCompile(`data-result="([^"]*)"`)

// run opens the page with Chrome for one scenario and returns the request
// log and body[data-result] of the dumped DOM ("" when it has none).
func (d *domHarness) run(t *testing.T, chrome, scenario string) ([]string, string) {
	t.Helper()
	return d.runAt(t, chrome, "/?days=14", scenario)
}

// runAt is run on another page (path and query).
func (d *domHarness) runAt(t *testing.T, chrome, path, scenario string) ([]string, string) {
	t.Helper()
	d.mu.Lock()
	d.log = nil
	d.mu.Unlock()
	args := []string{"--headless=new", "--disable-gpu", "--no-first-run", "--window-size=1280,900", "--user-data-dir=" + t.TempDir()}
	if os.Getuid() == 0 {
		args = append(args, "--no-sandbox")
	}
	args = append(args, "--virtual-time-budget=5000", "--dump-dom", "http://"+d.h.host()+path+"#s="+scenario)
	cmd := exec.Command(chrome, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("chrome %s: %v\n%s", scenario, err, errb.String())
	}
	// A scenario that navigates leaves the new page's DOM: no result.
	res := ""
	if m := resultRE.FindStringSubmatch(out.String()); m != nil {
		res = m[1]
	}
	return d.requests(), res
}

func TestScriptBehaviourInChrome(t *testing.T) {
	if os.Getenv("BACKSIGHT_CHROME_TESTS") != "1" {
		t.Skip("BACKSIGHT_CHROME_TESTS is not 1")
	}
	chrome := chromePath()
	if chrome == "" {
		t.Fatal("BACKSIGHT_CHROME_TESTS=1 but no Chrome or Chromium on PATH")
	}
	d := newDOMHarness(t)
	d.keys.Store(true)
	const page = "GET /?days=14"

	t.Run("wrapper", func(t *testing.T) {
		r := d.h.do("GET", "/?days=14")
		b := body(t, r)
		if got := r.Header.Get("Content-Security-Policy"); got != csp || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("CSP %q", got)
		}
		tag := `<script src="` + assetPath("copy.js") + `" defer></script><script src="/_test/harness.js" defer></script>`
		if !strings.Contains(b, tag) {
			t.Errorf("the harness script does not follow copy.js")
		}
		if strings.Contains(b, "<script>") {
			t.Error("an inline script (the CSP would refuse it)")
		}
	})

	stays := func(t *testing.T, scenario, want string) {
		t.Helper()
		log, res := d.run(t, chrome, scenario)
		if res != want {
			t.Errorf("%s: data-result %q, want %q", scenario, res, want)
		}
		if i := indexOf(log, page); i != 0 {
			t.Errorf("%s: the page's own request is at %d in %q", scenario, i, log)
		}
		for _, l := range log {
			if strings.HasPrefix(l, "GET /quality") {
				t.Errorf("%s: navigated: %q", scenario, log)
			}
		}
	}

	t.Run("g then q goes to Quality with the query", func(t *testing.T) {
		log, _ := d.run(t, chrome, "gq")
		if indexOf(log, "GET /_test/harness.js") < 0 {
			t.Errorf("the harness did not load: %q", log)
		}
		i, j := indexOf(log, page), indexOf(log, "GET /quality?days=14")
		if i != 0 || j <= i {
			t.Errorf("request log %q: want %q first, then GET /quality?days=14", log, page)
		}
	})
	t.Run("r follows Refresh", func(t *testing.T) {
		log, _ := d.run(t, chrome, "refresh")
		n := 0
		for _, l := range log {
			if l == page {
				n++
			}
		}
		if n != 2 {
			t.Errorf("%d requests of %s in %q, want 2", n, page, log)
		}
	})
	t.Run("Ctrl+g then q", func(t *testing.T) { stays(t, "ctrl", "stayed") })
	t.Run("Alt+g then q", func(t *testing.T) { stays(t, "alt", "stayed") })
	t.Run("Meta+g then q", func(t *testing.T) { stays(t, "meta", "stayed") })
	t.Run("g then Ctrl+q", func(t *testing.T) { stays(t, "ctrlq", "stayed") })
	t.Run("g q typed in the project field", func(t *testing.T) { stays(t, "typing", "stayed") })
	t.Run("g, 1.5 s, q", func(t *testing.T) { stays(t, "slow", "stayed") })

	t.Run("slash opens the project filter", func(t *testing.T) {
		_, res := d.run(t, chrome, "slash")
		if want := "open=true;focus=input.[project]"; res != want {
			t.Errorf("data-result %q, want %q", res, want)
		}
	})
	t.Run("? opens the list, Esc closes it and focus returns", func(t *testing.T) {
		_, res := d.run(t, chrome, "help")
		want := "opened=true:button.btn+btn-quiet;closed=true;focus=a.btn+btn-quiet+refresh-t;before=a.btn+btn-quiet+refresh-t"
		if res != want {
			t.Errorf("data-result %q, want %q", res, want)
		}
	})
	t.Run("the footer button opens the list", func(t *testing.T) {
		_, res := d.run(t, chrome, "footer")
		want := "hidden=false;opened=true:button.btn+btn-quiet;closed=true;focus=button.btn+btn-quiet+keyhint-btn;btn=button.btn+btn-quiet+keyhint-btn"
		if res != want {
			t.Errorf("data-result %q, want %q", res, want)
		}
	})
	t.Run("calendar arrow keys", func(t *testing.T) {
		cal := apiPath(t, d.h, "/api/v1/days").Tables.Calendar
		day := func(i int) string { return "/days/" + cal[i].Day }
		_, res := d.run(t, chrome, "cal")
		// From the first day: a week forward (right), then a day forward
		// (down), the last day (End), the first (Home). The tab stop moves
		// with the focus, and the tip says the focused day's label.
		want := strings.Join([]string{
			"right:" + day(7) + ":0:-1:tip=ok", "down:" + day(8) + ":0:-1:tip=ok",
			"end:" + day(len(cal)-1) + ":0:-1:tip=ok", "home:" + day(0) + ":0:-1:tip=ok", "zeros=1"}, ";")
		if res != want {
			t.Errorf("data-result %q, want %q", res, want)
		}
	})
	t.Run("g then q with a calendar day focused", func(t *testing.T) {
		log, _ := d.run(t, chrome, "calg")
		if i, j := indexOf(log, page), indexOf(log, "GET /quality?days=14"); i != 0 || j <= i {
			t.Errorf("request log %q: want GET /quality?days=14 after the page", log)
		}
	})
	t.Run("? and Esc with a calendar day focused", func(t *testing.T) {
		_, res := d.run(t, chrome, "calhelp")
		if want := "opened=true;closed=true;focus=a.hm-c+today"; !strings.HasPrefix(res, "opened=true;closed=true;focus=a.hm-c") {
			t.Errorf("data-result %q, want prefix of %q", res, want)
		}
	})
	t.Run("Copy summary refused shows the text", func(t *testing.T) {
		_, res := d.run(t, chrome, "copyfail")
		if want := "open=true;label=Couldn't copy: the text is shown below"; res != want {
			t.Errorf("data-result %q, want %q", res, want)
		}
	})
	t.Run("Copy summary copies data-copy", func(t *testing.T) {
		_, res := d.run(t, chrome, "copyok")
		// The text the page carries: its button's data-copy, which the
		// disclosure repeats.
		doc := parseHTML(t, body(t, d.h.do("GET", "/?days=14")))
		btn := doc.first("button.copy[data-copy]")
		if btn == nil {
			t.Fatal("no Copy summary button on /")
		}
		text := btn.attr("data-copy")
		if !strings.HasPrefix(text, "backsight · Is my memory working? · last 14 days (") || !strings.Contains(text, "\nNeeds attention:") {
			t.Fatalf("the page's summary text:\n%s", text)
		}
		if want := "text=" + html.UnescapeString(text) + ";open=false;label=Copied"; res != want {
			t.Errorf("data-result %q, want %q", res, want)
		}
	})
	t.Run("the closed list is not displayed until ?", func(t *testing.T) {
		_, res := d.run(t, chrome, "closed")
		if want := "closed=none;open=flex"; res != want {
			t.Errorf("data-result %q, want %q (computed display before and after ?)", res, want)
		}
	})
	t.Run("g carries the window and scope, not the page's own keys", func(t *testing.T) {
		const from = "/?days=14&project=proj-3&hm=used"
		log, _ := d.runAt(t, chrome, from, "gq")
		if i, j := indexOf(log, "GET "+from), indexOf(log, "GET /quality?days=14&project=proj-3"); i != 0 || j <= i {
			t.Errorf("request log %q: want GET %s, then GET /quality?days=14&project=proj-3", log, from)
		}
		for _, l := range log {
			if strings.Contains(l, "hm=") && strings.HasPrefix(l, "GET /quality") {
				t.Errorf("the page's colour choice followed the chord: %q", l)
			}
		}
		// The two targets: the carried one is served, the old one was refused.
		if r := d.h.do("GET", "/quality?days=14&project=proj-3"); r.StatusCode != 200 {
			t.Errorf("GET /quality?days=14&project=proj-3: status %d", r.StatusCode)
		}
		if r := d.h.do("GET", "/quality?days=14&project=proj-3&hm=used"); r.StatusCode != 400 {
			t.Errorf("GET /quality with hm: status %d, want 400 (the fixture of this guard)", r.StatusCode)
		}
	})
	t.Run("the open list is modal to the shortcuts", func(t *testing.T) {
		log, res := d.run(t, chrome, "modal")
		want := "proj=false;closed=true;focus=a.btn+btn-quiet+refresh-t"
		if res != want {
			t.Errorf("data-result %q, want %q", res, want)
		}
		n := 0
		for _, l := range log {
			if l == page {
				n++
			}
			if strings.HasPrefix(l, "GET /quality") {
				t.Errorf("g q under the dialog navigated: %q", log)
			}
		}
		if n != 1 {
			t.Errorf("%d requests of %s in %q, want 1 (r reloaded under the dialog)", n, page, log)
		}
	})
	t.Run("Copy id without a command box says it could not copy", func(t *testing.T) {
		rows := apiPath(t, d.h, "/api/v1/memories").Tables.Memories
		if len(rows) == 0 {
			t.Fatal("no memory in the fixture")
		}
		path := "/memories/" + url.PathEscape(rows[0].ID)
		if r := d.h.do("GET", path); r.StatusCode != 200 {
			t.Fatalf("GET %s: status %d", path, r.StatusCode)
		}
		_, res := d.runAt(t, chrome, path, "copyid")
		if want := "label=Couldn't copy;sel=0"; res != want {
			t.Errorf("data-result %q, want %q", res, want)
		}
	})
	t.Run("shortcuts off", func(t *testing.T) {
		d.keys.Store(false)
		defer d.keys.Store(true)
		log, _ := d.run(t, chrome, "gq")
		if indexOf(log, "GET /_test/harness.js") < 0 || indexOf(log, "GET /quality?days=14") >= 0 {
			t.Errorf("g q with shortcuts off: %q", log)
		}
		_, res := d.run(t, chrome, "nodialog")
		if want := "dialog=false;button=false"; res != want {
			t.Errorf("data-result %q, want %q", res, want)
		}
	})
}
