// Package guide is backsight's embedded documentation (decision 0069): the
// docs/*.md topics split into sections, a local keyword search over them,
// and a semantic search through the server's docs bank `<bank>-docs`.
package guide

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Barben360/backsight/docs"
)

// Section is one `##` section of a topic, or the topic's text before its
// first `##` heading (Slug ""). Text is what the docs bank stores: the
// topic's title line, then the section's Markdown, heading included.
type Section struct {
	Topic   string `json:"topic"`
	Slug    string `json:"slug"`
	Heading string `json:"heading"`
	Text    string `json:"-"`
	Hash    string `json:"hash"`
	body    string // Text without the title line the bank's copy starts with
}

// Ref names the section for `guide <ref>`: topic#slug, or the topic alone.
func (s Section) Ref() string {
	if s.Slug == "" {
		return s.Topic
	}
	return s.Topic + "#" + s.Slug
}

// ID is the section's document id in the docs bank.
func (s Section) ID() string { return "docs:" + s.Ref() }

// Topic is one docs file.
type Topic struct {
	Name     string    `json:"name"`
	Title    string    `json:"title"`
	Text     string    `json:"-"`
	Sections []Section `json:"sections"`
}

// Corpus is every topic, by name, and the docs hash over all of them.
type Corpus struct {
	Topics []Topic
	Hash   string
}

// Embedded is the docs this binary ships.
func Embedded() (Corpus, error) { return Load(docs.FS) }

// Load reads every top-level *.md file of fsys.
func Load(fsys fs.FS) (Corpus, error) {
	names, err := fs.Glob(fsys, "*.md")
	if err != nil {
		return Corpus{}, err
	}
	if len(names) == 0 {
		return Corpus{}, errors.New("no docs (*.md) found")
	}
	sort.Strings(names)
	var c Corpus
	sum := sha256.New()
	for _, n := range names {
		raw, err := fs.ReadFile(fsys, n)
		if err != nil {
			return Corpus{}, err
		}
		name := strings.TrimSuffix(path.Base(n), ".md")
		if "docs:"+name == VersionsID {
			return Corpus{}, fmt.Errorf("docs file %s: its name is the docs bank's record of versions (%s)", n, VersionsID)
		}
		sum.Write([]byte(name + "\x00" + string(raw) + "\x00"))
		c.Topics = append(c.Topics, parse(name, string(raw)))
	}
	c.Hash = hex.EncodeToString(sum.Sum(nil))
	return c, nil
}

// Tag marks the docs bank's documents with this docs version.
func (c Corpus) Tag() string { return "docs-version:" + c.Hash[:16] }

// Sections lists every section, topic by topic, in file order.
func (c Corpus) Sections() []Section {
	var out []Section
	for _, t := range c.Topics {
		out = append(out, t.Sections...)
	}
	return out
}

// Topic finds a topic by name.
func (c Corpus) Topic(name string) (Topic, bool) {
	for _, t := range c.Topics {
		if t.Name == name {
			return t, true
		}
	}
	return Topic{}, false
}

// Section finds a section by its ref (topic#slug, or topic for the text
// before the first heading).
func (c Corpus) Section(ref string) (Section, bool) {
	topic, _, _ := strings.Cut(ref, "#")
	t, ok := c.Topic(topic)
	if !ok {
		return Section{}, false
	}
	for _, s := range t.Sections {
		if s.Ref() == ref {
			return s, true
		}
	}
	return Section{}, false
}

// parse splits a topic on its `##` lines outside code fences.
func parse(name, raw string) Topic {
	t := Topic{Name: name, Title: name, Text: raw}
	lines := strings.SplitAfter(raw, "\n")
	for _, l := range lines {
		if h, ok := strings.CutPrefix(strings.TrimRight(l, "\r\n"), "# "); ok {
			t.Title = strings.TrimSpace(h)
			break
		}
	}
	titleLine := "# " + t.Title + "\n\n"
	seen := map[string]int{}
	var cur *Section
	var body strings.Builder
	flush := func() {
		text := body.String()
		body.Reset()
		if cur == nil { // the preamble: kept when it says more than the title
			rest := strings.TrimSpace(strings.Replace(text, "# "+t.Title, "", 1))
			if rest == "" {
				return
			}
			s := Section{Topic: name, Heading: t.Title, Text: strings.TrimRight(text, "\n") + "\n", body: rest}
			s.Hash = hash(s.Text)
			t.Sections = append(t.Sections, s)
			return
		}
		cur.body = strings.TrimRight(text, "\n") + "\n"
		cur.Text = titleLine + cur.body
		cur.Hash = hash(cur.Text)
		t.Sections = append(t.Sections, *cur)
	}
	fence := ""
	for _, l := range lines {
		trimmed := strings.TrimRight(l, "\r\n")
		if f := fenceOf(trimmed); f != "" {
			switch {
			case fence == "":
				fence = f
			case strings.HasPrefix(f, fence):
				fence = ""
			}
		}
		if h, ok := strings.CutPrefix(trimmed, "## "); ok && fence == "" {
			flush()
			heading := strings.TrimSpace(h)
			sl := slug(heading)
			if n := seen[sl]; n > 0 {
				seen[sl] = n + 1
				sl += "-" + strconv.Itoa(n)
			} else {
				seen[sl] = 1
			}
			cur = &Section{Topic: name, Slug: sl, Heading: heading}
		}
		body.WriteString(l)
	}
	flush()
	return t
}

// fenceOf is the fence marker a line opens or closes (``` or ~~~, any
// length), or "".
func fenceOf(line string) string {
	l := strings.TrimLeft(line, " ")
	for _, m := range []string{"```", "~~~"} {
		if strings.HasPrefix(l, m) {
			n := 0
			for n < len(l) && l[n] == m[0] {
				n++
			}
			return l[:n]
		}
	}
	return ""
}

// slug is GitHub's anchor for a heading: lower case, punctuation dropped
// (letters, digits, -, _ and spaces kept), spaces as hyphens. A repeated
// heading takes -1, -2… as on GitHub.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
