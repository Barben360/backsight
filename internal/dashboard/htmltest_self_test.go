package dashboard

import (
	"os"
	"strings"
	"testing"
)

func TestParserRefusesBadNesting(t *testing.T) {
	for _, bad := range []string{
		"<ul><li>a<li>b</ul>",
		"<div><p>x</div>",
		"<p>x",
		"<br></br>",
		"</p>",
	} {
		if _, err := parseHTMLErr(bad); err == nil {
			t.Errorf("%q parsed without error", bad)
		}
	}
	if _, err := parseHTMLErr("<br></br>"); err == nil || !strings.Contains(err.Error(), "void element") {
		t.Errorf("explicit end tag of a void element: %v", err)
	}
	if _, err := parseHTMLErr("<div>\n<p>x\n\n"); err == nil || !strings.Contains(err.Error(), "line 2: <p> is still open") {
		t.Errorf("EOF error should carry the open element's start line: %v", err)
	}
	_, err := parseHTMLErr("<div>\n<p>x\n</div>")
	if err == nil || !strings.Contains(err.Error(), "line 3") || !strings.Contains(err.Error(), "</div>") {
		t.Errorf("error should name the element and its line: %v", err)
	}
	if _, err := parseHTMLErr(`<p>a<br>b<img src=x alt="y"></p>`); err != nil {
		t.Errorf("void elements refused: %v", err)
	}
	root := parseHTML(t, "<details open><summary>s</summary></details>")
	if d := root.first("details"); d == nil || !d.has("open") || d.has("closed") {
		t.Errorf("open attribute not read: %+v", d)
	}
	script := parseHTML(t, `<!doctype html><!-- c --><script>if (a < b && c) { x("</i>") }</script>`)
	if s := script.first("script"); s == nil || !strings.Contains(s.Text, `a < b && c`) {
		t.Errorf("script body not raw text: %+v", s)
	}
}

func TestParserAcceptsSelfClosingAndUnquoted(t *testing.T) {
	root := parseHTML(t, `<p>a<br/>b<br />c<img src="x" alt="y"/><input disabled/></p>
<svg viewBox="0 0 4 4"><g><rect x="1" width="2"/><path d="M0 0"/></g><line x1="0"/></svg><svg/>`)
	if got := len(root.find("svg g rect")) + len(root.find("svg path")) + len(root.find("svg line")); got != 3 {
		t.Errorf("self-closed svg children: %d", got)
	}
	if got := root.first("p").text(); got != "a b c" {
		t.Errorf("p text: %q", got)
	}
	if len(root.find("svg")) != 2 || root.first("input") == nil || !root.first("input").has("disabled") {
		t.Errorf("tree wrong: %d svgs", len(root.find("svg")))
	}
	if _, err := parseHTMLErr("<div/>x"); err == nil {
		t.Error("a self-closed div outside svg is an open div")
	}
	a := parseHTML(t, "<a href=/x/y?a=1&b=2:3 id=k class=on>t</a><a href=x/y>u</a>").find("a")
	if len(a) != 2 || a[0].attr("href") != "/x/y?a=1&b=2:3" || a[0].attr("id") != "k" || !a[0].has("class") || a[1].attr("href") != "x/y" {
		t.Errorf("unquoted values: %+v", a)
	}
}

func TestParserReadsTheReferences(t *testing.T) {
	var overview *node
	for _, name := range []string{"overview.html", "memory.html"} {
		b, err := os.ReadFile("design/reference/" + name)
		if err != nil {
			t.Fatal(err)
		}
		n, err := parseHTMLErr(string(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "overview.html" {
			overview = n
		}
	}
	if got := len(overview.find("article.tile")); got < 6 {
		t.Errorf("overview tiles: %d", got)
	}
	v := overview.first("section.verdict")
	if v == nil || v.first("h2") == nil {
		t.Fatal("no section.verdict h2")
	}
	if got := v.first("h2").text(); !strings.HasPrefix(got, "Memory is working.") {
		t.Errorf("verdict heading: %q", got)
	}
}

const selectorPage = `<body>
<nav aria-label="Pages" class="a b"><a href="/">One</a><a href="/two" class="on">Two</a></nav>
<nav aria-label="Other"><a href="/x">Other</a></nav>
<main id="main"><section class="verdict"><p>Metric <code class="term">x</code> is &amp; fine</p>
<button title="Copy it" class="copy" hidden>c</button><code>secret --flag</code><code title="leak" aria-label="leak2">x</code><i data-title="zz" title=bare>i</i></section></main>
</body>`

func TestTextSplitsAtBlocksOnly(t *testing.T) {
	for html, want := range map[string]string{
		"<p>foo<b>bar</b></p>":                         "foobar",
		"<p><b>1</b>%</p>":                             "1%",
		"<table><tr><td>a</td><td>b</td></tr></table>": "a b",
		"<ul><li>x</li><li>y</li></ul>":                "x y",
		"<p>a<code>c</code>b</p>":                      "ab",
	} {
		if got := parseHTML(t, html).text(); got != want {
			t.Errorf("%s: %q, want %q", html, got, want)
		}
	}
}

func TestSelectors(t *testing.T) {
	root := parseHTML(t, selectorPage)
	var hrefs []string
	for _, a := range root.find("nav[aria-label=Pages] a") {
		hrefs = append(hrefs, a.attr("href"))
	}
	if strings.Join(hrefs, ",") != "/,/two" {
		t.Errorf("nav links: %v", hrefs)
	}
	if m := root.first("#main"); m == nil || m.Name != "main" {
		t.Errorf("#main: %+v", m)
	}
	if got := len(root.find("a.on")); got != 1 {
		t.Errorf("a.on: %d", got)
	}
	if got := len(root.find("nav.a.b")); got != 1 {
		t.Errorf("nav.a.b: %d", got)
	}
	if got := len(root.find("main a")); got != 0 {
		t.Errorf("descendant chain matched outside main: %d", got)
	}
	if got := len(root.find("main section p")); got != 1 {
		t.Errorf("main section p: %d", got)
	}
	if root.first(".missing") != nil {
		t.Error("first of no match must be nil")
	}
	if b := root.first("button[hidden]"); b == nil || !b.has("hidden") || b.attr("title") != "Copy it" {
		t.Errorf("button[hidden]: %+v", b)
	}
	if got := root.first("p").text(); got != "Metric is & fine" {
		t.Errorf("text: %q", got)
	}
	if got := root.first("section").text(); strings.Contains(got, "secret") || strings.Contains(got, "term") {
		t.Errorf("text kept code: %q", got)
	}
	spoken := spokenText(selectorPage)
	for _, want := range []string{"Pages", "Copy it", "Metric"} {
		if !strings.Contains(spoken, want) {
			t.Errorf("spokenText lacks %q: %q", want, spoken)
		}
	}
	if strings.Contains(spoken, "leak") {
		t.Errorf("spokenText read attributes inside <code>: %q", spoken)
	}
	if !strings.Contains(spoken, "bare") || strings.Contains(spoken, "zz") {
		t.Errorf("spokenText must read unquoted title and not data-title: %q", spoken)
	}
	if strings.Contains(spoken, "secret") || strings.Contains(spoken, "--flag") {
		t.Errorf("spokenText kept a code body: %q", spoken)
	}
}
