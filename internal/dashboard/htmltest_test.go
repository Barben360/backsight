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
	line  int        // line of the start tag
}

type nodeItem struct {
	text string
	kid  *node
}

var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true,
}

var rawEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// selfCloseAt reports whether s[i:] starts with "/>".
func selfCloseAt(s string, i int) bool { return strings.HasPrefix(s[i:], "/>") }

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// normalize rewrites body so the XML decoder reads it as HTML: unquoted
// attribute values are quoted (a value ends at white space or '>'), '/>'
// is dropped on void elements and expanded to an explicit end tag inside
// <svg> (foreign content), and the bodies of <script> are escaped to plain
// text. Every newline is kept, so line numbers stay.
func normalize(body string) string {
	var out strings.Builder
	svg := 0
	for i := 0; i < len(body); {
		c := body[i]
		if c != '<' || i+1 >= len(body) {
			out.WriteByte(c)
			i++
			continue
		}
		rest := body[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				out.WriteString(rest)
				return out.String()
			}
			out.WriteString(rest[:4+end+3])
			i += 4 + end + 3
		case rest[1] == '!' || rest[1] == '?' || rest[1] == '/':
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				out.WriteString(rest)
				return out.String()
			}
			if rest[1] == '/' && strings.EqualFold(strings.TrimSpace(rest[2:end]), "svg") && svg > 0 {
				svg--
			}
			out.WriteString(rest[:end+1])
			i += end + 1
		case isAlpha(rest[1]):
			j := 1
			for j < len(rest) && !isSpace(rest[j]) && rest[j] != '>' && !selfCloseAt(rest, j) {
				j++
			}
			name := strings.ToLower(rest[1:j])
			var tag strings.Builder
			tag.WriteString(rest[:j])
			selfClosed := false
			for j < len(rest) && rest[j] != '>' {
				switch {
				case isSpace(rest[j]):
					tag.WriteByte(rest[j])
					j++
				case selfCloseAt(rest, j):
					selfClosed = true
					j++
				case rest[j] == '/':
					tag.WriteByte(' ')
					j++
				default:
					k := j
					for k < len(rest) && !isSpace(rest[k]) && rest[k] != '=' && rest[k] != '>' && !selfCloseAt(rest, k) {
						k++
					}
					tag.WriteString(rest[j:k])
					j = k
					m := j
					for m < len(rest) && isSpace(rest[m]) {
						m++
					}
					if m >= len(rest) || rest[m] != '=' {
						continue
					}
					tag.WriteString(rest[j : m+1])
					m++
					for m < len(rest) && isSpace(rest[m]) {
						tag.WriteByte(rest[m])
						m++
					}
					switch {
					case m < len(rest) && (rest[m] == '"' || rest[m] == '\''):
						end := strings.IndexByte(rest[m+1:], rest[m])
						if end < 0 {
							end = len(rest) - m - 2
						}
						tag.WriteString(rest[m : m+end+2])
						m += end + 2
					default:
						k = m
						for k < len(rest) && !isSpace(rest[k]) && rest[k] != '>' {
							k++
						}
						tag.WriteString(`"` + strings.ReplaceAll(rest[m:k], `"`, "&quot;") + `"`)
						m = k
					}
					j = m
				}
			}
			if j >= len(rest) {
				out.WriteString(rest)
				return out.String()
			}
			out.WriteString(tag.String() + ">")
			i += j + 1
			if selfClosed && !voidElements[name] && (svg > 0 || name == "svg") {
				out.WriteString("</" + name + ">")
			}
			if !selfClosed && name == "svg" {
				svg++
			}
			if !selfClosed && name == "script" {
				end := strings.Index(strings.ToLower(body[i:]), "</script")
				if end < 0 {
					end = len(body) - i
				}
				out.WriteString(rawEscape.Replace(body[i : i+end]))
				i += end
			}
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String()
}

// parseHTMLErr parses body strictly: every non-void element is closed in
// order, a void element never has an end tag.
func parseHTMLErr(body string) (*node, error) {
	d := xml.NewDecoder(strings.NewReader(normalize(body)))
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
			n := &node{Name: strings.ToLower(t.Name.Local), Attr: map[string]string{}, Parent: top, line: line}
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
		return nil, fmt.Errorf("line %d: <%s> is still open at the end of the page", top.line, top.Name)
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

var blockElements = map[string]bool{
	"div": true, "p": true, "li": true, "td": true, "th": true, "tr": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "section": true, "article": true, "header": true,
	"footer": true, "nav": true, "main": true, "ul": true, "ol": true, "table": true, "details": true,
	"summary": true, "figure": true, "figcaption": true, "dl": true, "dt": true, "dd": true, "br": true,
}

// text is the subtree's text as a reader sees it: <code> contents removed,
// a space at block-level boundaries only (inline elements do not split
// words), entities decoded, whitespace collapsed.
func (n *node) text() string {
	var b strings.Builder
	var walk func(*node)
	walk = func(c *node) {
		for _, it := range c.items {
			switch {
			case it.kid == nil:
				b.WriteString(it.text)
			case it.kid.Name == "code":
			case blockElements[it.kid.Name]:
				b.WriteByte(' ')
				walk(it.kid)
				b.WriteByte(' ')
			default:
				walk(it.kid)
			}
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

var spokenAttr = regexp.MustCompile(`(?is)\s(?:aria-label|title)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)

// spokenText is visibleText plus every aria-label and title value, with
// <code> contents removed.
func spokenText(page string) string {
	out := []string{visibleText(page)}
	for _, m := range spokenAttr.FindAllStringSubmatch(codeSpan.ReplaceAllString(page, ""), -1) {
		v := m[1] + m[2] + m[3]
		// A copy button is named after the <code> command it copies; code
		// is left out above, so its name is too.
		if strings.HasPrefix(v, "Copy: ") {
			continue
		}
		if v = strings.Join(strings.Fields(html.UnescapeString(v)), " "); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, " ")
}
