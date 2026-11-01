package dashboard

import (
	"bytes"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/views"
)

// shot is one page of the screenshots: its name, the page it draws and
// how its data is built.
type shot struct {
	name, page, id string
	build          func(t *testing.T, d *PageData)
}

// shots are the five pages screenshotted, each in both themes.
var shots = []shot{
	{name: "overview-healthy", page: "overview", build: func(t *testing.T, d *PageData) { overviewShot(t, d, "healthy") }},
	{name: "overview-serverdown", page: "overview", build: func(t *testing.T, d *PageData) { overviewShot(t, d, "serverdown") }},
	{name: "overview-new", page: "overview", build: func(t *testing.T, d *PageData) { overviewShot(t, d, "new") }},
	{name: "session", page: "session", id: "sess-long-1"},
	{name: "memory", page: "memory", id: "mem-long-1"},
}

func overviewShot(t *testing.T, d *PageData, scenario string) {
	r := scenarioNamed(t, scenario)
	d.V["overview"], d.Loc = r, r.Window.Loc
	d.Frame.Scope, d.Frame.Generated = &r.Scope, r.GeneratedAt
}

// probeJS drives the copy script in the probe page: with no clipboard API,
// then with a clipboard that refuses, it clicks the first copy button and
// records the visible outcome on the body.
const probeJS = `'use strict';
document.addEventListener('DOMContentLoaded', function () {
  var b = document.body;
  var button = document.querySelector('button.copy');
  var status = document.getElementById('copy-status');
  var code = button.closest('.cmd').querySelector('code');
  b.setAttribute('data-hidden-after-run', String(button.hasAttribute('hidden')));
  document.querySelector('details.cli').open = true;
  function record(prefix, next) {
    setTimeout(function () {
      b.setAttribute('data-' + prefix + '-status', status.textContent);
      b.setAttribute('data-' + prefix + '-label', button.textContent.trim());
      b.setAttribute('data-' + prefix + '-selected', String(window.getSelection()) === code.textContent ? 'command' : 'other:' + String(window.getSelection()));
      next();
    }, 0);
  }
  Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
  button.click();
  record('missing', function () {
    window.getSelection().removeAllRanges();
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: function () { return Promise.reject(new Error('refused')); } } });
    button.click();
    record('refused', function () {
      window.getSelection().removeAllRanges();
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: function () { return Promise.resolve(); } } });
      button.click();
      record('ok', function () { b.setAttribute('data-done', '1'); });
    });
  });
});
`

func TestScreenshots(t *testing.T) {
	dir := os.Getenv("BACKSIGHT_SCREENSHOTS")
	if dir == "" {
		t.Skip("BACKSIGHT_SCREENSHOTS names no directory")
	}
	chrome := ""
	for _, c := range []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(c); err == nil {
			chrome = p
			break
		}
	}
	if chrome == "" {
		t.Skip("no Chrome or Chromium on PATH")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	set, err := newPageSet()
	if err != nil {
		t.Fatal(err)
	}
	page := func(s shot, theme string) []byte {
		spec := set.specs[s.page]
		d := fixtureData(t, spec, false)
		d.L, d.P = en, views.Params{Days: 7}
		path := "/"
		switch s.page {
		case "session":
			path = "/sessions/" + s.id
		case "memory":
			path = "/memories/" + s.id
		}
		d.ID = s.id
		d.Frame = Frame{
			Title: pageTitle(spec, s.id), Path: path, Version: "dev", HelpURL: helpURL,
			Nav: navFor(path, false), Windows: windowsFor(path, url.Values{}), PrefMenu: prefMenu(path, Prefs{Theme: theme}),
			Prefs: Prefs{Theme: theme}, AllProjects: path, Keep: [][2]string{{"days", "7"}},
		}
		head := d.V[spec.Views[0]]
		d.Frame.Scope, d.Frame.Generated, d.Loc = &head.Scope, head.GeneratedAt, head.Window.Loc
		d.Frame.Window = windowPhrase(d.P, head.Window)
		// The footer reads "Rendered at 10:21 in 12 ms", not a placeholder.
		d.Frame.RenderedAt, d.Frame.RenderMS = head.GeneratedAt.Add(2*time.Minute), 12
		d.CLI = cliFor(spec.Views[0], s.id, d.P)
		if s.page == "session" || s.page == "memory" {
			d.Frame.Windows, d.Frame.Window = nil, "all time"
			d.Frame.SessionHead = s.page == "session"
			if d.Frame.SessionHead {
				d.Frame.Window = ""
			}
			d.CLI = cliFor(spec.Views[0], s.id, allTimeParams(d.P))
		}
		if s.build != nil {
			s.build(t, &d)
		}
		body, err := renderPage(set, s.page, d)
		if err != nil {
			t.Fatalf("%s %s: %v", s.name, theme, err)
		}
		if strings.Contains(string(body), "Rendered at —") || strings.Contains(string(body), " in 0 ms") {
			t.Errorf("%s %s: the footer shows a placeholder render time", s.name, theme)
		}
		return body
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/assets/", serveAsset)
	mux.HandleFunc("/probe.js", func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w.Header(), false)
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(probeJS))
	})
	serve := func(p string, body []byte) {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			securityHeaders(w.Header(), false)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(body)
		})
	}
	var names []string
	for _, s := range shots {
		for _, theme := range []string{"light", "dark"} {
			serve("/"+s.name+"-"+theme, page(s, theme))
			names = append(names, s.name+"-"+theme)
		}
	}
	// The probe is the healthy Overview with one more script after the copy script.
	probe := bytes.Replace(page(shots[0], "light"), []byte("</body>"), []byte(`<script src="/probe.js" defer></script></body>`), 1)
	if !bytes.Contains(probe, []byte("/probe.js")) {
		t.Fatal("the probe script was not added")
	}
	serve("/probe", probe)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Fatalf("server at %s, want 127.0.0.1", srv.URL)
	}
	if os.Getenv("BACKSIGHT_SCREENSHOTS_WAIT") == "1" {
		for _, n := range names {
			fmt.Println(srv.URL + "/" + n)
		}
		fmt.Println(srv.URL + "/probe")
		select {} // by-hand fallback: stop it with ^C
	}

	run := func(args ...string) []byte {
		prof := t.TempDir()
		a := []string{"--headless=new", "--disable-gpu", "--hide-scrollbars", "--user-data-dir=" + prof}
		if os.Getuid() == 0 {
			a = append(a, "--no-sandbox")
		}
		cmd := exec.Command(chrome, append(a, args...)...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("chrome %v: %v\n%s", args, err, errb.String())
		}
		return out.Bytes()
	}
	for _, n := range names {
		file := filepath.Join(dir, n+".png")
		run("--window-size=1440,900", "--screenshot="+file, srv.URL+"/"+n)
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
			t.Fatalf("%s: not a PNG", file)
		}
		img, err := png.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if b := img.Bounds(); b.Dx() != 1440 || b.Dy() != 900 {
			t.Errorf("%s: %dx%d, want 1440x900", file, b.Dx(), b.Dy())
		}
	}

	// The copy script ran: its buttons are shown.
	dom := string(run("--dump-dom", srv.URL+"/overview-healthy-light"))
	if !hasVisibleCopyButton(dom) {
		t.Errorf("no button.copy without hidden in the dumped DOM")
	}

	// The fallback path: no clipboard API, then a refusing one, then a working one.
	pd := string(run("--dump-dom", "--virtual-time-budget=5000", srv.URL+"/probe"))
	for _, want := range []string{
		`data-hidden-after-run="false"`,
		`data-missing-status="Selected: press Ctrl+C"`, `data-missing-selected="command"`,
		`data-refused-status="Selected: press Ctrl+C"`, `data-refused-selected="command"`,
		`data-ok-status="Copied"`, `data-ok-selected="other:"`,
	} {
		if !strings.Contains(pd, want) {
			t.Errorf("probe DOM lacks %s", want)
		}
	}
}

// hasVisibleCopyButton reports whether the DOM has a <button class="copy">
// whose tag carries no hidden attribute.
func hasVisibleCopyButton(dom string) bool {
	for rest := dom; ; {
		i := strings.Index(rest, "<button")
		if i < 0 {
			return false
		}
		rest = rest[i:]
		j := strings.Index(rest, ">")
		if j < 0 {
			return false
		}
		tag := rest[:j]
		if strings.Contains(tag, `class="copy"`) && !strings.Contains(tag, "hidden") {
			return true
		}
		rest = rest[j:]
	}
}
