package dashboard

import (
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
	"testing"
)

// node is one element of a parsed page. Text is the character data
// directly inside it; the root returned by parseHTML is a nameless node
// holding the page's top-level elements.
type node struct {
	Name   string
	Attr   map[string]string
	Kids   []*node
	Parent *node
	Text   string

	items []nodeItem // text and children in document order
}

type nodeItem struct {
	text string
	kid  *node
}

var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true,
}

var scriptBody = regexp.MustCompile(`(?is)(<script\b[^>]*>)(.*?)(</script\s*>)`)

// rawScripts escapes the body of every <script> so the XML decoder reads
// it as plain text; newlines stay, so line numbers stay.
func rawScripts(body string) string {
	return scriptBody.ReplaceAllStringFunc(body, func(m string) string {
		g := scriptBody.FindStringSubmatch(m)
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(g[2]))
		return g[1] + b.String() + g[3]
	})
}

// parseHTMLErr parses body strictly: every non-void element is closed in
// order, a void element never has an end tag.
func parseHTMLErr(body string) (*node, error) {
	d := xml.NewDecoder(strings.NewReader(rawScripts(body)))
	d.Strict = false
	d.AutoClose = nil
	d.Entity = xml.HTMLEntity
	root := &node{}
	top := root
	for {
		tok, err := d.RawToken()
		line, _ := d.InputPos()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{Name: strings.ToLower(t.Name.Local), Attr: map[string]string{}, Parent: top}
			for _, a := range t.Attr {
				n.Attr[strings.ToLower(a.Name.Local)] = a.Value
			}
			top.Kids = append(top.Kids, n)
			top.items = append(top.items, nodeItem{kid: n})
			if !voidElements[n.Name] {
				top = n
			}
		case xml.EndElement:
			name := strings.ToLower(t.Name.Local)
			if voidElements[name] {
				return nil, fmt.Errorf("line %d: end tag </%s> of a void element", line, name)
			}
			if top == root || top.Name != name {
				open := "nothing"
				if top != root {
					open = "<" + top.Name + ">"
				}
				return nil, fmt.Errorf("line %d: end tag </%s> does not match %s", line, name, open)
			}
			top = top.Parent
		case xml.CharData:
			top.Text += string(t)
			top.items = append(top.items, nodeItem{text: string(t)})
		}
	}
	if top != root {
		line, _ := d.InputPos()
		return nil, fmt.Errorf("line %d: <%s> is still open at the end of the page", line, top.Name)
	}
	return root, nil
}

// parseHTML fails the test when body does not parse strictly.
func parseHTML(t testing.TB, body string) *node {
	t.Helper()
	n, err := parseHTMLErr(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return n
}

// simple is one compound selector: tag, classes, id, attribute tests.
type simple struct {
	tag, id string
	classes []string
	attrs   [][2]string // name, value; value "" with has=false means presence
	hasVal  []bool
}

var simpleToken = regexp.MustCompile(`^(?:([A-Za-z][A-Za-z0-9-]*)|\.([A-Za-z0-9_-]+)|#([A-Za-z0-9_-]+)|\[([A-Za-z0-9_:-]+)(?:=([^\]]*))?\])`)

func parseSimple(s string) simple {
	var out simple
	for s != "" {
		m := simpleToken.FindStringSubmatchIndex(s)
		if m == nil {
			panic("unsupported selector part: " + s)
		}
		g := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return s[m[2*i]:m[2*i+1]]
		}
		switch {
		case g(1) != "":
			out.tag = strings.ToLower(g(1))
		case g(2) != "":
			out.classes = append(out.classes, g(2))
		case g(3) != "":
			out.id = g(3)
		default:
			out.attrs = append(out.attrs, [2]string{g(4), strings.Trim(g(5), `"'`)})
			out.hasVal = append(out.hasVal, m[10] >= 0)
		}
		s = s[m[1]:]
	}
	return out
}

func (s simple) matches(n *node) bool {
	if s.tag != "" && n.Name != s.tag {
		return false
	}
	if s.id != "" && n.Attr["id"] != s.id {
		return false
	}
	have := strings.Fields(n.Attr["class"])
	for _, c := range s.classes {
		found := false
		for _, h := range have {
			found = found || h == c
		}
		if !found {
			return false
		}
	}
	for i, a := range s.attrs {
		v, ok := n.Attr[a[0]]
		if !ok || (s.hasVal[i] && v != a[1]) {
			return false
		}
	}
	return true
}

// find returns the descendants of n matching sel, in document order.
// Selectors: tag, .class, tag.class, #id, tag[attr], tag[attr=value], and
// descendant chains separated by spaces.
func (n *node) find(sel string) []*node {
	var chain []simple
	for _, p := range strings.Fields(sel) {
		chain = append(chain, parseSimple(p))
	}
	if len(chain) == 0 {
		return nil
	}
	var out []*node
	var walk func(*node)
	walk = func(c *node) {
		for _, k := range c.Kids {
			if chain[len(chain)-1].matches(k) && ancestorsMatch(k, n, chain[:len(chain)-1]) {
				out = append(out, k)
			}
			walk(k)
		}
	}
	walk(n)
	return out
}

// ancestorsMatch reports whether the ancestors of k below scope satisfy
// the rest of the chain, last element nearest.
func ancestorsMatch(k, scope *node, rest []simple) bool {
	for i := len(rest) - 1; i >= 0; i-- {
		p := k.Parent
		for p != nil && p != scope && !rest[i].matches(p) {
			p = p.Parent
		}
		if p == nil || p == scope {
			return false
		}
		k = p
	}
	return true
}

// first is the first match of find, or nil.
func (n *node) first(sel string) *node {
	if r := n.find(sel); len(r) > 0 {
		return r[0]
	}
	return nil
}

func (n *node) attr(k string) string { return n.Attr[k] }

func (n *node) has(k string) bool { _, ok := n.Attr[k]; return ok }

// text is the subtree's text as a reader sees it: <code> contents removed,
// pieces joined by single spaces, entities decoded, whitespace collapsed.
func (n *node) text() string {
	var parts []string
	var walk func(*node)
	walk = func(c *node) {
		for _, it := range c.items {
			switch {
			case it.kid == nil:
				parts = append(parts, it.text)
			case it.kid.Name != "code":
				walk(it.kid)
			}
		}
	}
	walk(n)
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

var spokenAttr = regexp.MustCompile(`(?is)\b(?:aria-label|title)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// spokenText is visibleText plus every aria-label and title value, with
// <code> contents removed.
func spokenText(page string) string {
	out := []string{visibleText(page)}
	for _, m := range spokenAttr.FindAllStringSubmatch(codeSpan.ReplaceAllString(page, ""), -1) {
		v := m[1] + m[2]
		if v = strings.Join(strings.Fields(html.UnescapeString(v)), " "); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, " ")
}
