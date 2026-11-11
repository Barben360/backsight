// Package codexcfg is the narrow TOML editor of Codex's config.toml
// (decision 0085, D8). It changes whole tables or single keys, at the byte
// level, so everything else in the file (order, comments, spacing, other
// tools' tables) stays byte for byte; each edit is then checked against
// the same operation applied to the decoded document, and refused when
// they differ. It writes nothing itself: its operations are the Apply of a
// claudecfg.Edit, so the config editor's rules hold (a pre-image first, the
// journal, a write only over the bytes read, undo).
//
// Narrow means: standard tables ([a.b."c"]) and key = value lines, keys
// bare, "basic" or 'literal', dotted or not. Values are only skipped over
// (strings of every kind, multi-line ones included, arrays and inline
// tables that span lines), never rewritten but as whole lines.
package codexcfg

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/Barben360/backsight/internal/claudecfg"
)

// item is one logical line of the document: a header, a key = value
// (spanning several lines when its value does), a comment or a blank line.
type item struct {
	start, end int      // bytes [start, end), its newline included
	kind       itemKind // header, keyval, comment, blank
	path       []string // header: the table's key; keyval: the key, full (table + key)
	array      bool     // header of an array of tables ([[…]])
	valStart   int      // keyval: where the value starts
	valEnd     int      // keyval: where the value ends (before a comment and spaces)
}

type itemKind int

const (
	blankItem itemKind = iota
	commentItem
	headerItem
	keyvalItem
)

// doc is a parsed document: its items in order, and its line ending.
type doc struct {
	src   []byte
	items []item
	nl    string
}

// ErrUnsupported: the document holds what the narrow editor does not read
// (it is then left alone, never guessed at).
var ErrUnsupported = errors.New("config.toml holds TOML this editor does not handle")

func parse(src []byte) (*doc, error) {
	d := &doc{src: src, nl: "\n"}
	if bytes.Contains(src, []byte("\r\n")) {
		d.nl = "\r\n"
	}
	var table []string
	for at := 0; at < len(src); {
		it, err := d.next(at, table)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
		}
		if it.kind == headerItem {
			table = it.path
		}
		d.items = append(d.items, it)
		at = it.end
	}
	return d, nil
}

// next reads the logical line starting at at.
func (d *doc) next(at int, table []string) (item, error) {
	src := d.src
	i := skipSpace(src, at)
	it := item{start: at}
	switch {
	case i >= len(src) || src[i] == '\n' || (src[i] == '\r' && i+1 < len(src) && src[i+1] == '\n'):
		it.kind, it.end = blankItem, lineEnd(src, i)
		return it, nil
	case src[i] == '#':
		it.kind, it.end = commentItem, lineEnd(src, i)
		return it, nil
	case src[i] == '[':
		it.kind = headerItem
		j := i + 1
		if j < len(src) && src[j] == '[' {
			it.array, j = true, j+1
		}
		path, k, err := readKey(src, j)
		if err != nil {
			return it, err
		}
		k = skipSpace(src, k)
		closing := "]"
		if it.array {
			closing = "]]"
		}
		if !bytes.HasPrefix(src[k:], []byte(closing)) {
			return it, fmt.Errorf("unclosed table header at byte %d", i)
		}
		k = skipSpace(src, k+len(closing))
		if k < len(src) && src[k] != '#' && src[k] != '\n' && src[k] != '\r' {
			return it, fmt.Errorf("text after a table header at byte %d", k)
		}
		it.path, it.end = path, lineEnd(src, k)
		return it, nil
	}
	it.kind = keyvalItem
	key, k, err := readKey(src, i)
	if err != nil {
		return it, err
	}
	k = skipSpace(src, k)
	if k >= len(src) || src[k] != '=' {
		return it, fmt.Errorf("a key without = at byte %d", i)
	}
	it.path = append(append([]string{}, table...), key...)
	it.valStart = skipSpace(src, k+1)
	end, valEnd, err := skipValue(src, it.valStart)
	if err != nil {
		return it, err
	}
	it.valEnd, it.end = valEnd, end
	return it, nil
}

func skipSpace(src []byte, i int) int {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return i
}

// lineEnd is the offset after the newline that ends the line holding i.
func lineEnd(src []byte, i int) int {
	if j := bytes.IndexByte(src[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(src)
}

func bare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// readKey reads a (dotted) key from i: its parts, and where it stops.
func readKey(src []byte, i int) ([]string, int, error) {
	var parts []string
	for {
		i = skipSpace(src, i)
		if i >= len(src) {
			return nil, i, errors.New("a key ends the file")
		}
		switch c := src[i]; {
		case c == '"':
			s, j, err := basicString(src, i)
			if err != nil {
				return nil, i, err
			}
			parts, i = append(parts, s), j
		case c == '\'':
			j := bytes.IndexAny(src[i+1:], "'\n")
			if j < 0 || src[i+1+j] != '\'' {
				return nil, i, fmt.Errorf("an unclosed literal key at byte %d", i)
			}
			parts, i = append(parts, string(src[i+1:i+1+j])), i+j+2
		case bare(c):
			j := i
			for j < len(src) && bare(src[j]) {
				j++
			}
			parts, i = append(parts, string(src[i:j])), j
		default:
			return nil, i, fmt.Errorf("an unexpected %q in a key at byte %d", c, i)
		}
		j := skipSpace(src, i)
		if j < len(src) && src[j] == '.' {
			i = j + 1
			continue
		}
		return parts, i, nil
	}
}

// basicString reads the "basic" string at i (one line): its value, and the
// offset after its closing quote.
func basicString(src []byte, i int) (string, int, error) {
	j := i + 1
	for j < len(src) && src[j] != '"' {
		switch src[j] {
		case '\\':
			j++
		case '\n':
			return "", i, fmt.Errorf("an unclosed string at byte %d", i)
		}
		j++
	}
	if j >= len(src) {
		return "", i, fmt.Errorf("an unclosed string at byte %d", i)
	}
	raw := string(src[i : j+1])
	var v string
	if err := toml.Unmarshal([]byte("k = "+raw), &struct {
		K *string `toml:"k"`
	}{&v}); err != nil {
		return "", i, fmt.Errorf("the string at byte %d: %w", i, err)
	}
	return v, j + 1, nil
}

// skipValue skips the value starting at i, over every line it spans:
// strings of each kind, arrays and inline tables nested to any depth.
// It returns the end of its logical line (after the newline) and where the
// value itself ends (before trailing spaces and a comment).
func skipValue(src []byte, i int) (end, valEnd int, err error) {
	depth := 0
	valEnd = i
	for i < len(src) {
		c := src[i]
		switch {
		case bytes.HasPrefix(src[i:], []byte(`"""`)), bytes.HasPrefix(src[i:], []byte(`'''`)):
			q := src[i : i+3]
			j := i + 3
			for {
				k := bytes.Index(src[j:], q)
				if k < 0 {
					return 0, 0, fmt.Errorf("an unclosed multi-line string at byte %d", i)
				}
				j += k
				if q[0] == '"' && escaped(src, j) {
					j++
					continue
				}
				break
			}
			// Up to two more quotes belong to the string ("""a"""").
			j += 3
			for n := 0; n < 2 && j < len(src) && src[j] == q[0]; n++ {
				j++
			}
			i, valEnd = j, j
		case c == '"':
			_, j, err := basicString(src, i)
			if err != nil {
				return 0, 0, err
			}
			i, valEnd = j, j
		case c == '\'':
			j := bytes.IndexAny(src[i+1:], "'\n")
			if j < 0 || src[i+1+j] != '\'' {
				return 0, 0, fmt.Errorf("an unclosed literal string at byte %d", i)
			}
			i, valEnd = i+j+2, i+j+2
		case c == '[' || c == '{':
			depth++
			i++
			valEnd = i
		case c == ']' || c == '}':
			depth--
			if depth < 0 {
				return 0, 0, fmt.Errorf("an unbalanced %q at byte %d", c, i)
			}
			i++
			valEnd = i
		case c == '#':
			i = lineEnd(src, i)
			if depth == 0 {
				return i, valEnd, nil
			}
		case c == '\n':
			if depth == 0 {
				return i + 1, valEnd, nil
			}
			i++
		case c == ' ' || c == '\t' || c == '\r' || (depth > 0 && c == ','):
			i++
		default:
			i++
			valEnd = i
		}
	}
	if depth != 0 {
		return 0, 0, errors.New("an array or inline table is not closed")
	}
	return len(src), valEnd, nil
}

// escaped reports whether the quote at j is escaped by an odd run of
// backslashes.
func escaped(src []byte, j int) bool {
	n := 0
	for k := j - 1; k >= 0 && src[k] == '\\'; k-- {
		n++
	}
	return n%2 == 1
}

// ShowPath is path as TOML writes it: dotted, a part quoted when needed.
func ShowPath(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = keyPart(p)
	}
	return strings.Join(parts, ".")
}

func keyPart(p string) string {
	ok := p != ""
	for i := 0; i < len(p); i++ {
		ok = ok && bare(p[i])
	}
	if ok {
		return p
	}
	return strconv.Quote(p)
}

func equal(a, b []string) bool { return slices.Equal(a, b) }

// decode is the document as a tree, for the check of each edit.
func decode(src []byte) (map[string]any, error) {
	out := map[string]any{}
	if err := toml.Unmarshal(src, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// check verifies that after is before with want applied to its tree: an
// edit the decoder reads otherwise than intended is refused.
func check(before, after []byte, want func(tree map[string]any)) error {
	tb, err := decode(before)
	if err != nil {
		return fmt.Errorf("config.toml is not valid TOML: %w", err)
	}
	want(tb)
	ta, err := decode(after)
	if err != nil {
		return fmt.Errorf("%w: the edit would leave invalid TOML (%w)", ErrUnsupported, err)
	}
	if !reflect.DeepEqual(pruned(tb), pruned(ta)) {
		return fmt.Errorf("%w: the edit would change more than intended", ErrUnsupported)
	}
	return nil
}

// pruned is tree without its empty tables, at any depth: removing the
// last table under a key leaves an empty one in the tree, none in the file
// unless a header names it.
func pruned(tree map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range tree {
		if m, ok := v.(map[string]any); ok {
			if m = pruned(m); len(m) == 0 {
				continue
			}
			v = m
		}
		out[k] = v
	}
	return out
}

// treeDelete removes path from tree; empty is kept, as the bytes keep it.
func treeDelete(tree map[string]any, path []string) {
	m := tree
	for _, p := range path[:len(path)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			return
		}
		m = next
	}
	delete(m, path[len(path)-1])
}

// treeSet puts v at path in tree, creating the tables on the way.
func treeSet(tree map[string]any, path []string, v any) {
	m := tree
	for _, p := range path[:len(path)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[path[len(path)-1]] = v
}

// RemoveTables removes every standard table whose header key matches,
// whole: its header, its lines up to its last key, and the blank lines
// just before its header (where TOML writers put the space between
// tables). A comment after its last key stays: it heads what follows.
func RemoveTables(src []byte, match func(path []string) bool) ([]byte, []claudecfg.Change, error) {
	d, err := parse(src)
	if err != nil {
		return nil, nil, err
	}
	var cut [][2]int
	var changes []claudecfg.Change
	var removed [][]string
	for i, it := range d.items {
		if it.kind != headerItem || it.array || !match(it.path) {
			continue
		}
		start, last := i, i
		for start > 0 && d.items[start-1].kind == blankItem {
			start--
		}
		for j := i + 1; j < len(d.items) && d.items[j].kind != headerItem; j++ {
			if d.items[j].kind == keyvalItem {
				last = j
			}
		}
		cut = append(cut, [2]int{d.items[start].start, d.items[last].end})
		text := string(src[it.start:d.items[last].end])
		changes = append(changes, claudecfg.Change{Path: "[" + ShowPath(it.path) + "]", Old: &text})
		removed = append(removed, it.path)
	}
	if len(cut) == 0 {
		return src, nil, nil
	}
	out := splice(src, cut)
	if err := check(src, out, func(tree map[string]any) {
		for _, p := range removed {
			treeDelete(tree, p)
		}
	}); err != nil {
		return nil, nil, err
	}
	return out, changes, nil
}

// splice is src without the ranges cut (in order, disjoint).
func splice(src []byte, cut [][2]int) []byte {
	var out []byte
	at := 0
	for _, c := range cut {
		if c[0] < at {
			c[0] = at
		}
		out = append(out, src[at:c[0]]...)
		at = c[1]
	}
	return append(out, src[at:]...)
}

// Get is the value at path, as its TOML text, when a key = value line sets
// it (in its table or dotted from an enclosing one).
func Get(src []byte, path []string) (string, bool, error) {
	d, err := parse(src)
	if err != nil {
		return "", false, err
	}
	if it, ok := d.find(path); ok {
		return string(src[it.valStart:it.valEnd]), true, nil
	}
	return "", false, nil
}

func (d *doc) find(path []string) (item, bool) {
	for _, it := range d.items {
		if it.kind == keyvalItem && equal(it.path, path) {
			return it, true
		}
	}
	return item{}, false
}

// table is the index of the standard table whose header is path, -1 when
// none.
func (d *doc) table(path []string) int {
	for i, it := range d.items {
		if it.kind == headerItem && !it.array && equal(it.path, path) {
			return i
		}
	}
	return -1
}

// Remove removes the key = value line that sets path, when there is one.
func Remove(src []byte, path []string) ([]byte, claudecfg.Change, error) {
	ch := claudecfg.Change{Path: ShowPath(path)}
	d, err := parse(src)
	if err != nil {
		return nil, ch, err
	}
	it, ok := d.find(path)
	if !ok {
		return src, ch, nil
	}
	old := string(src[it.valStart:it.valEnd])
	ch.Old = &old
	out := splice(src, [][2]int{{it.start, it.end}})
	if err := check(src, out, func(tree map[string]any) { treeDelete(tree, path) }); err != nil {
		return nil, ch, err
	}
	return out, ch, nil
}

// Set makes path hold value (TOML text, e.g. `true` or `"approve"`): the
// value of its line replaced, else a line added at the end of its table,
// else the table added at the end of the file.
func Set(src []byte, path []string, value string) ([]byte, claudecfg.Change, error) {
	return insert(src, path, value, nil, "")
}

// insert is Set; after, when its line is new, names the line it goes after
// (a key path of the same table, or the table itself for its first line),
// and raw, when set, is that line as written (Restore's).
func insert(src []byte, path []string, value string, after []string, raw string) ([]byte, claudecfg.Change, error) {
	ch := claudecfg.Change{Path: ShowPath(path), New: &value}
	var v any
	if err := toml.Unmarshal([]byte("v = "+value), &struct {
		V *any `toml:"v"`
	}{&v}); err != nil {
		return nil, ch, fmt.Errorf("%s = %s: %w", ShowPath(path), value, err)
	}
	d, err := parse(src)
	if err != nil {
		return nil, ch, err
	}
	var out []byte
	if it, ok := d.find(path); ok {
		old := string(src[it.valStart:it.valEnd])
		ch.Old = &old
		if old == value {
			return src, ch, nil
		}
		out = append(append(append([]byte{}, src[:it.valStart]...), value...), src[it.valEnd:]...)
	} else {
		parent, key := path[:len(path)-1], path[len(path)-1]
		line := keyPart(key) + " = " + value + d.nl
		if raw != "" {
			line = strings.TrimRight(raw, "\r\n") + d.nl
		}
		if t := d.table(parent); t >= 0 {
			at := d.items[t].end // first in its table
			if after == nil || !equal(after, parent) {
				placed := false
				for j := t + 1; j < len(d.items) && d.items[j].kind != headerItem; j++ {
					it := d.items[j]
					if it.kind != keyvalItem || placed {
						continue
					}
					at = it.end // after the last key, unless the one named is found
					placed = after != nil && equal(it.path, after)
				}
			}
			out = insertAt(src, at, line, d.nl)
		} else {
			add := "[" + ShowPath(parent) + "]" + d.nl + line
			body := src
			if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
				body = append(append([]byte{}, body...), d.nl...)
			}
			if len(bytes.TrimSpace(body)) > 0 {
				add = d.nl + add
			}
			out = append(append([]byte{}, body...), add...)
		}
	}
	if err := check(src, out, func(tree map[string]any) { treeSet(tree, path, v) }); err != nil {
		return nil, ch, err
	}
	return out, ch, nil
}

// insertAt puts line at offset at (a line start), ending the line before
// it first when the file did not.
func insertAt(src []byte, at int, line, nl string) []byte {
	head := append([]byte{}, src[:at]...)
	if len(head) > 0 && head[len(head)-1] != '\n' {
		head = append(head, nl...)
	}
	return append(append(head, line...), src[at:]...)
}

// Restore puts the key = value line of path back as pre (the file before
// a Remove) had it, at its place there: after the same key it followed,
// else first in its table. Nothing changes when path is set again, or pre
// did not set it.
func Restore(src, pre []byte, path []string) ([]byte, claudecfg.Change, error) {
	ch := claudecfg.Change{Path: ShowPath(path)}
	if _, there, err := Get(src, path); err != nil || there {
		return src, ch, err
	}
	p, err := parse(pre)
	if err != nil {
		return nil, ch, err
	}
	it, ok := p.find(path)
	if !ok {
		return src, ch, nil
	}
	value := string(pre[it.valStart:it.valEnd])
	parent := path[:len(path)-1]
	after := parent // first in its table, unless a key came before it
	var table []string
	for i := range p.items {
		if p.items[i].kind == headerItem {
			table = p.items[i].path
		}
		if p.items[i].start != it.start {
			continue
		}
		for j := i - 1; j >= 0 && p.items[j].kind != headerItem; j-- {
			if prev := p.items[j]; prev.kind == keyvalItem && equal(prev.path[:len(prev.path)-1], parent) {
				after = prev.path
				break
			}
		}
		break
	}
	raw := "" // the line as written, when its key is written within its own table
	if equal(table, parent) {
		raw = string(pre[it.start:it.end])
	}
	return insert(src, path, value, after, raw)
}
