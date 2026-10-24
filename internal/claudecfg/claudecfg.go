// Package claudecfg is the config editor (decision 0042): every change
// backsight makes to Claude Code's own files goes through these operations.
// JSON is patched in place at the byte level, so key order, formatting,
// unknown keys and other tools' entries survive; each patch is then checked
// against the same operation applied to the decoded tree (decision 0052).
package claudecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/redact"
)

// Change is one edited JSON path (or marked block) with its old and new
// values as JSON text; nil means absent.
type Change struct {
	Path string  `json:"path"`
	Old  *string `json:"old"`
	New  *string `json:"new"`
}

// Changed says whether the operation modified anything.
func (c Change) Changed() bool {
	if c.Old == nil || c.New == nil {
		return c.Old != c.New
	}
	return *c.Old != *c.New
}

var bom = []byte("\xef\xbb\xbf")

// Set makes path hold value, creating missing parent objects.
func Set(doc []byte, path []string, value any) ([]byte, Change, error) {
	ch := Change{Path: showPath(path)}
	enc, err := encode(value)
	if err != nil {
		return nil, ch, err
	}
	ch.New = &enc
	out, err := patch(doc, func(src []byte, root *node) ([]byte, error) {
		obj, i, err := walk(root, path)
		if err != nil {
			return nil, err
		}
		if i == len(path) { // exists: replace the value
			old := string(src[obj.start:obj.end])
			if equalJSON(old, enc) {
				ch.Old = ch.New
				return src, nil
			}
			shown, _ := redact.Text(old) // the value replaced may be a token
			ch.Old = &shown
			return splice(src, obj.start, obj.end, reindent(enc, lineIndent(src, obj.start), indentUnit(src))), nil
		}
		return insertMember(src, obj, path[i], nest(path[i+1:], value))
	}, func(tree any) (any, error) { return treeSet(tree, path, value) })
	return out, ch, err
}

// EnsureInArray appends value to the array at path unless an equal element
// is already there; a missing array is created.
func EnsureInArray(doc []byte, path []string, value any) ([]byte, Change, error) {
	ch := Change{Path: showPath(path) + "[+]"} // the appended element, not the whole array
	enc, err := encode(value)
	if err != nil {
		return nil, ch, err
	}
	out, err := patch(doc, func(src []byte, root *node) ([]byte, error) {
		n, i, err := walk(root, path)
		if err != nil {
			return nil, err
		}
		if i < len(path) {
			ch.New = &enc
			return insertMember(src, n, path[i], nest(path[i+1:], []any{value}))
		}
		if n.kind != '[' {
			return nil, fmt.Errorf("%s is not an array", showPath(path))
		}
		for _, e := range n.elems {
			if equalJSON(string(src[e.start:e.end]), enc) {
				return src, nil
			}
		}
		ch.New = &enc
		return insertElem(src, n, enc), nil
	}, func(tree any) (any, error) { return treeEnsure(tree, path, value) })
	return out, ch, err
}

// RemoveWhere removes the members (object) or elements (array) at path for
// which match(key or index, decoded value) is true. Removed values are
// shown redacted: retired entries may hold tokens.
func RemoveWhere(doc []byte, path []string, match func(key string, v any) bool) ([]byte, []Change, error) {
	var changes []Change
	out := doc
	for {
		var hit *Change
		var hitKey string
		next, err := patch(out, func(src []byte, root *node) ([]byte, error) {
			n, i, err := walk(root, path)
			if err != nil || i < len(path) {
				return src, err
			}
			k := -1
			var key string
			switch n.kind {
			case '{':
				for j, m := range n.members {
					if match(m.key, decode(src[m.val.start:m.val.end])) {
						k, key = j, m.key
						break
					}
				}
			case '[':
				for j, e := range n.elems {
					if match(strconv.Itoa(j), decode(src[e.start:e.end])) {
						k, key = j, strconv.Itoa(j)
						break
					}
				}
			default:
				return nil, fmt.Errorf("%s is neither an object nor an array", showPath(path))
			}
			if k < 0 {
				return src, nil
			}
			old, _ := redact.Text(string(src[n.item(k).start:n.item(k).end]))
			hit, hitKey = &Change{Path: showPath(append(append([]string{}, path...), key)), Old: &old}, key
			return removeItem(src, n, k), nil
		}, func(tree any) (any, error) {
			if hit == nil {
				return tree, nil
			}
			return treeRemove(tree, path, hitKey)
		})
		if err != nil {
			return nil, nil, err
		}
		if hit == nil {
			return out, changes, nil
		}
		changes = append(changes, *hit)
		out = next
	}
}

// Get returns the decoded value at path (numbers as json.Number) and
// whether it exists.
func Get(doc []byte, path []string) (any, bool, error) {
	var v any
	var found bool
	_, err := patch(doc, func(src []byte, root *node) ([]byte, error) {
		n, i, err := walk(root, path)
		if err == nil && i == len(path) {
			v, found = decode(src[n.start:n.end]), true
		}
		return src, err
	}, nil)
	return v, found, err
}

// MarkedBlock puts text between backsight's markers for id, replacing an
// existing block; an empty text removes the block. Unbalanced or repeated
// markers are refused: editing them would swallow the user's text.
func MarkedBlock(doc []byte, id, text string) ([]byte, Change, error) {
	begin, end := "<!-- backsight:"+id+":begin -->", "<!-- backsight:"+id+":end -->"
	block := begin + "\n" + strings.TrimRight(text, "\n") + "\n" + end
	ch := Change{Path: "block " + id}
	s := string(doc)
	if text != "" {
		ch.New = &block
	}
	nb, ne := strings.Count(s, begin), strings.Count(s, end)
	b, e := strings.Index(s, begin), strings.Index(s, end)
	if nb > 1 || ne > 1 || nb != ne || (nb == 1 && e < b) {
		return nil, ch, fmt.Errorf("the %s markers are unbalanced or repeated: fix them by hand first", id)
	}
	if nb == 1 {
		old, _ := redact.Text(s[b : e+len(end)])
		ch.Old = &old
		if s[b:e+len(end)] == block {
			ch.Old = ch.New
		}
		if text == "" { // drop the block, its line break and the blank line before it
			before, after := strings.TrimRight(s[:b], "\n"), strings.TrimPrefix(s[e+len(end):], "\n")
			if before != "" {
				before += "\n"
				if after != "" {
					before += "\n"
				}
			}
			return []byte(before + after), ch, nil
		}
		return []byte(s[:b] + block + s[e+len(end):]), ch, nil
	}
	if text == "" {
		return doc, ch, nil
	}
	if s == "" {
		return []byte(block + "\n"), ch, nil
	}
	return []byte(strings.TrimRight(s, "\n") + "\n\n" + block + "\n"), ch, nil
}

// patch parses doc (an empty doc is {}), runs edit on the bytes and, when
// check is set, verifies the result decodes to check(decoded doc).
func patch(doc []byte, edit func([]byte, *node) ([]byte, error), check func(any) (any, error)) ([]byte, error) {
	prefix := []byte{}
	if bytes.HasPrefix(doc, bom) {
		prefix, doc = bom, doc[len(bom):]
	}
	suffix := ""
	if len(bytes.TrimSpace(doc)) == 0 {
		doc, suffix = []byte("{}"), "\n"
	}
	if !json.Valid(doc) {
		return nil, errors.New("not valid JSON: fix the file by hand first, backsight never overwrites it")
	}
	p := parser{src: doc}
	p.ws()
	root := p.value()
	if root.kind != '{' {
		return nil, errors.New("the top level is not a JSON object")
	}
	out, err := edit(doc, root)
	if err != nil {
		return nil, err
	}
	if !json.Valid(out) {
		return nil, errors.New("internal: the patched file is not valid JSON; nothing written")
	}
	if check != nil {
		want, err := check(decode(doc))
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(decode(out), want) {
			return nil, errors.New("internal: the byte patch does not match the intended change; nothing written")
		}
	}
	return append(append(append([]byte{}, prefix...), out...), suffix...), nil
}

// node is a parsed JSON value with its byte span.
type node struct {
	kind       byte // { [ " 0 (number) t f n
	start, end int
	members    []member
	elems      []*node
}

type member struct {
	key            string
	keyStart, kEnd int // span of the quoted key
	val            *node
}

// item is the k-th member value's or element's span start/end holder.
func (n *node) item(k int) *node {
	if n.kind == '{' {
		m := n.members[k]
		return &node{start: m.keyStart, end: m.val.end}
	}
	return n.elems[k]
}

type parser struct {
	src []byte
	i   int
}

func (p *parser) ws() {
	for p.i < len(p.src) && strings.IndexByte(" \t\r\n", p.src[p.i]) >= 0 {
		p.i++
	}
}

// value parses one value; the input is already known to be valid JSON.
func (p *parser) value() *node {
	n := &node{start: p.i, kind: p.src[p.i]}
	switch n.kind {
	case '{':
		p.i++
		for p.ws(); p.src[p.i] != '}'; p.ws() {
			ks := p.i
			p.str()
			m := member{keyStart: ks, kEnd: p.i}
			_ = json.Unmarshal(p.src[ks:p.i], &m.key)
			p.ws()
			p.i++ // :
			p.ws()
			m.val = p.value()
			n.members = append(n.members, m)
			p.ws()
			if p.src[p.i] == ',' {
				p.i++
			}
		}
		p.i++
	case '[':
		p.i++
		for p.ws(); p.src[p.i] != ']'; p.ws() {
			n.elems = append(n.elems, p.value())
			p.ws()
			if p.src[p.i] == ',' {
				p.i++
			}
		}
		p.i++
	case '"':
		p.str()
	default:
		if strings.IndexByte("tfn", n.kind) < 0 {
			n.kind = '0'
		}
		for p.i < len(p.src) && strings.IndexByte(",]} \t\r\n", p.src[p.i]) < 0 {
			p.i++
		}
	}
	n.end = p.i
	return n
}

func (p *parser) str() {
	p.i++
	for p.src[p.i] != '"' {
		if p.src[p.i] == '\\' {
			p.i++
		}
		p.i++
	}
	p.i++
}

// walk follows path from root through objects and returns the deepest node
// reached and how many keys it consumed. Duplicate keys: the last wins.
func walk(root *node, path []string) (*node, int, error) {
	n := root
	for i, k := range path {
		if n.kind != '{' {
			return nil, 0, fmt.Errorf("%s is not an object", showPath(path[:i]))
		}
		var next *node
		for _, m := range n.members {
			if m.key == k {
				next = m.val
			}
		}
		if next == nil {
			return n, i, nil
		}
		n = next
	}
	return n, len(path), nil
}

// insertMember adds "key": value as the last member of obj.
func insertMember(src []byte, obj *node, key string, value any) ([]byte, error) {
	if obj.kind != '{' {
		return nil, errors.New("cannot add a key to a value that is not an object")
	}
	kb, _ := json.Marshal(key)
	enc, err := encode(value)
	if err != nil {
		return nil, err
	}
	if len(obj.members) == 0 {
		ind := lineIndent(src, obj.start)
		inner := ind + indentUnit(src)
		text := "\n" + inner + string(kb) + ": " + reindent(enc, inner, indentUnit(src)) + "\n" + ind
		return splice(src, obj.start+1, obj.end-1, text), nil
	}
	first, last := obj.members[0], obj.members[len(obj.members)-1]
	sep := string(src[obj.start+1 : first.keyStart])
	if !strings.Contains(sep, "\n") {
		sep = " " // inline object
	}
	colon := string(src[first.kEnd:first.val.start])
	text := "," + sep + string(kb) + colon + reindent(enc, lineIndent(src, first.keyStart), indentUnit(src))
	return splice(src, last.val.end, last.val.end, text), nil
}

// insertElem appends enc as the last element of arr.
func insertElem(src []byte, arr *node, enc string) []byte {
	if len(arr.elems) == 0 {
		ind := lineIndent(src, arr.start)
		inner := ind + indentUnit(src)
		return splice(src, arr.start+1, arr.end-1, "\n"+inner+reindent(enc, inner, indentUnit(src))+"\n"+ind)
	}
	first, last := arr.elems[0], arr.elems[len(arr.elems)-1]
	sep := string(src[arr.start+1 : first.start])
	if !strings.Contains(sep, "\n") {
		sep = " "
	}
	return splice(src, last.end, last.end, ","+sep+reindent(enc, lineIndent(src, first.start), indentUnit(src)))
}

// removeItem deletes the k-th member or element with its separator.
func removeItem(src []byte, n *node, k int) []byte {
	count := len(n.elems)
	if n.kind == '{' {
		count = len(n.members)
	}
	it := n.item(k)
	switch {
	case count == 1:
		return splice(src, n.start+1, n.end-1, "")
	case k < count-1:
		return splice(src, it.start, n.item(k+1).start, "")
	default:
		return splice(src, n.item(k-1).end, it.end, "")
	}
}

func splice(src []byte, from, to int, text string) []byte {
	out := make([]byte, 0, len(src)+len(text))
	out = append(out, src[:from]...)
	out = append(out, text...)
	return append(out, src[to:]...)
}

// lineIndent is the leading whitespace of the line holding offset at.
func lineIndent(src []byte, at int) string {
	ls := bytes.LastIndexByte(src[:at], '\n') + 1
	e := ls
	for e < at && (src[e] == ' ' || src[e] == '\t') {
		e++
	}
	return string(src[ls:e])
}

// indentUnit is the document's indentation step: the first indented line's
// leading whitespace, else two spaces.
func indentUnit(src []byte) string {
	for _, line := range bytes.Split(src, []byte("\n"))[1:] {
		t := bytes.TrimLeft(line, " \t")
		if len(t) < len(line) && len(t) > 0 {
			return string(line[:len(line)-len(t)])
		}
	}
	return "  "
}

// reindent lays out a multi-line encoding under indent.
func reindent(enc, indent, unit string) string {
	if !strings.ContainsAny(enc, "\n") {
		return enc
	}
	var v any
	_ = json.Unmarshal([]byte(enc), &v)
	b, _ := marshal(v, indent, unit)
	return string(b)
}

// encode renders value as JSON: one line for scalars and empty containers,
// indented otherwise (reindent sets the real prefix).
func encode(value any) (string, error) {
	b, err := marshal(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func marshal(v any, prefix, unit string) ([]byte, error) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	e.SetIndent(prefix, unit)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// nest wraps value in objects for the remaining keys.
func nest(keys []string, value any) any {
	for i := len(keys) - 1; i >= 0; i-- {
		value = map[string]any{keys[i]: value}
	}
	return value
}

func decode(b []byte) any {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	_ = d.Decode(&v)
	return v
}

func equalJSON(a, b string) bool {
	return reflect.DeepEqual(decode([]byte(a)), decode([]byte(b)))
}

// normal is value as the decoded tree holds it.
func normal(value any) any {
	enc, _ := encode(value)
	return decode([]byte(enc))
}

func treeSet(tree any, path []string, value any) (any, error) {
	if len(path) == 0 {
		return normal(value), nil
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	child, err := treeSet(obj[path[0]], path[1:], value)
	if obj[path[0]] == nil && len(path) > 1 {
		child, err = treeSet(map[string]any{}, path[1:], value)
	}
	if err != nil {
		return nil, err
	}
	obj[path[0]] = child
	return obj, nil
}

func treeEnsure(tree any, path []string, value any) (any, error) {
	obj, ok := tree.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	if len(path) == 1 {
		arr, _ := obj[path[0]].([]any)
		v := normal(value)
		for _, e := range arr {
			if reflect.DeepEqual(e, v) {
				return obj, nil
			}
		}
		obj[path[0]] = append(arr, v)
		return obj, nil
	}
	next := obj[path[0]]
	if next == nil {
		next = map[string]any{}
	}
	child, err := treeEnsure(next, path[1:], value)
	if err != nil {
		return nil, err
	}
	obj[path[0]] = child
	return obj, nil
}

// treeRemove drops key (a member name or an index) from the container at path.
func treeRemove(tree any, path []string, key string) (any, error) {
	if len(path) > 0 {
		obj, ok := tree.(map[string]any)
		if !ok {
			return nil, errors.New("not an object")
		}
		child, err := treeRemove(obj[path[0]], path[1:], key)
		obj[path[0]] = child
		return obj, err
	}
	switch c := tree.(type) {
	case map[string]any:
		delete(c, key)
		return c, nil
	case []any:
		i, _ := strconv.Atoi(key)
		return append(c[:i], c[i+1:]...), nil
	}
	return nil, errors.New("neither an object nor an array")
}

// showPath renders a JSON path as dotted keys, quoting keys with dots.
func showPath(path []string) string {
	parts := make([]string, len(path))
	for i, k := range path {
		if strings.ContainsAny(k, ". ") || k == "" {
			k = strconv.Quote(k)
		}
		parts[i] = k
	}
	return strings.Join(parts, ".")
}
