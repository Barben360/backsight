package dashboard

import (
	"bytes"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestTokensMatchTheDesign(t *testing.T) {
	emb, err := assetFS.ReadFile("assets/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	design, err := os.ReadFile("design/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(emb, design) {
		t.Error("assets/tokens.css differs from design/tokens.css: copy it again")
	}
}

// canvasOnly names the rules the component sheet needs and pages do not.
var canvasOnly = regexp.MustCompile(`fc-preview|dc-in|dc-solo|\.pad\b`)

func TestStylesheetIsTheDesigns(t *testing.T) {
	got, err := assetFS.ReadFile("assets/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(got)
	for _, bad := range []string{"fc-preview", "dc-in", "dc-solo"} {
		if strings.Contains(css, bad) {
			t.Errorf("dashboard.css names %q", bad)
		}
	}
	for _, want := range []string{"@media (forced-colors: active)", "@container page (max-width: 899px)", ":focus-visible"} {
		if !strings.Contains(css, want) {
			t.Errorf("dashboard.css lacks %q", want)
		}
	}
	ref, err := os.ReadFile("design/reference/dashboard.css")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(ref), "\n")
	if len(lines) < 478 || !strings.HasPrefix(lines[477], "/* A simulation of a forced-colors theme") {
		t.Fatalf("reference/dashboard.css: the canvas-only simulation no longer starts at L478")
	}
	// Before L478, only a comment names fc-preview: drop that line (it
	// points at the block the stylesheet leaves out).
	var kept []string
	for _, l := range lines[:477] {
		if !canvasOnly.MatchString(l) {
			kept = append(kept, l)
		}
	}
	pos, n := 0, 0
	for _, rule := range strings.Split(strings.Join(kept, "\n"), "}") {
		if strings.TrimSpace(rule) == "" {
			continue
		}
		i := strings.Index(css[pos:], rule+"}")
		if i < 0 {
			t.Fatalf("rule %d of the design is missing or out of order:\n%s}", n, rule)
		}
		pos += i + len(rule) + 1
		n++
	}
	if n < 100 {
		t.Errorf("only %d rules compared", n)
	}
}

func TestCopyScriptBudget(t *testing.T) {
	b, err := assetFS.ReadFile("assets/copy.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 10240 {
		t.Errorf("copy.js is %d bytes, over 10240", len(b))
	}
	js := string(b)
	for _, want := range []string{"'use strict'", "button.copy", "navigator.clipboard", "copy-status"} {
		if !strings.Contains(js, want) {
			t.Errorf("copy.js lacks %q", want)
		}
	}
	for _, bad := range []string{"innerHTML", "eval(", "Function(", "fetch(", "XMLHttpRequest", "document.write"} {
		if strings.Contains(js, bad) {
			t.Errorf("copy.js contains %q", bad)
		}
	}
	imp := regexp.MustCompile(`^\s*import\s`)
	for i, l := range strings.Split(js, "\n") {
		if imp.MatchString(l) {
			t.Errorf("copy.js line %d is an import: %s", i+1, l)
		}
	}
	if n := strings.Count(js, "setTimeout("); n != 1 {
		t.Errorf("copy.js has %d timers, want 1", n)
	}
}

func TestHeadLinksTheAssets(t *testing.T) {
	h := dataHarness(t)
	ck := h.login()
	check := func(name, page, scheme string) {
		t.Helper()
		root := parseHTML(t, page)
		var sheets []string
		for _, l := range root.find("link") {
			if l.attr("rel") == "stylesheet" {
				sheets = append(sheets, l.attr("href"))
			}
		}
		want := []string{assetPath("tokens.css"), assetPath("dashboard.css")}
		if strings.Join(sheets, " ") != strings.Join(want, " ") {
			t.Errorf("%s: stylesheets %q, want %q", name, sheets, want)
		}
		scripts := root.find("script")
		if len(scripts) != 1 || scripts[0].attr("src") != assetPath("copy.js") || !scripts[0].has("defer") {
			t.Errorf("%s: scripts %v, want one deferred copy.js", name, scripts)
		}
		m := root.first("meta[name=color-scheme]")
		if m == nil || m.attr("content") != scheme {
			t.Errorf("%s: color-scheme meta %v, want %q", name, m, scheme)
		}
	}
	page := func(prefs string) string {
		cookie := ck.Name + "=" + ck.Value
		if prefs != "" {
			cookie += "; " + prefsCookie + "=" + prefs
		}
		r := h.do("GET", "/", "Cookie", cookie)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET /: %d", r.StatusCode)
		}
		return body(t, r)
	}
	check("no cookie", page(""), "light dark")
	check("dark", page("v1.dark.0.0"), "dark")
	check("light", page("v1.light.0.0"), "light")
	check("system", page("v1.system.0.0"), "light dark")
	r := h.do("GET", "/")
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET / without a session: %d", r.StatusCode)
	}
	check("401", body(t, r), "light dark")
}
