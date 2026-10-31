package dashboard

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"text/template/parse"

	"github.com/Barben360/backsight/internal/metrics"
)

// The parity mechanism: a template may read only fields its declared views
// return. checkTemplate types every field path of a page against the Go
// structs, so a typo or a field no view has fails a test, not a reader.

var (
	pageDataT   = reflect.TypeFor[PageData]()
	viewsMapT   = reflect.TypeFor[map[string]*metrics.Result]()
	metricsMapT = reflect.TypeFor[map[string]metrics.Metric]()
	axesMapT    = reflect.TypeFor[map[string]metrics.Axis]()
	resultPtrT  = reflect.TypeFor[*metrics.Result]()
	resultT     = reflect.TypeFor[metrics.Result]()
	anyT        = reflect.TypeFor[any]()
	stringT     = reflect.TypeFor[string]()
	intT        = reflect.TypeFor[int]()
	floatT      = reflect.TypeFor[float64]()
	boolT       = reflect.TypeFor[bool]()
	// badT is the type of an expression already reported: reads on it
	// say nothing more.
	badT = reflect.TypeFor[checkFailed]()
)

type checkFailed struct{}

// textPage is the one template that may read Result.Text.
const textPage = "memory_text.html"

var axisNames = []string{"found", "right", "cost", "flowing"}

// scope holds the static types of the variables of a template block.
type scope struct {
	vars   map[string]reflect.Type
	parent *scope
}

func newScope(parent *scope) *scope { return &scope{vars: map[string]reflect.Type{}, parent: parent} }

func (s *scope) get(name string) reflect.Type {
	for ; s != nil; s = s.parent {
		if t, ok := s.vars[name]; ok {
			return t
		}
	}
	return badT
}

// assign sets an existing variable where it was declared.
func (s *scope) assign(name string, t reflect.Type) {
	for c := s; c != nil; c = c.parent {
		if _, ok := c.vars[name]; ok {
			c.vars[name] = t
			return
		}
	}
	s.vars[name] = t
}

type partialKey struct {
	name string
	t    reflect.Type
}

// checker walks one page of one template set.
type checker struct {
	set   *template.Template
	spec  pageSpec
	tree  *parse.Tree
	probs []string
	seen  map[partialKey]bool
	queue []partialKey
}

// checkTemplate returns the problems of the page spec in set, as
// "file:line: …". set must come from loadPages and never have been
// executed: html/template rewrites its trees on first execution.
func checkTemplate(set *template.Template, spec pageSpec) []string {
	for _, t := range set.Templates() {
		if t.Tree == nil || t.Tree.Root == nil {
			continue
		}
		if usesEscaper(t.Tree.Root) {
			return []string{"check a fresh set from loadPages"}
		}
	}
	c := &checker{set: set, spec: spec, seen: map[partialKey]bool{}}
	c.enqueue(spec.File, pageDataT)
	for len(c.queue) > 0 {
		k := c.queue[0]
		c.queue = c.queue[1:]
		t := c.set.Lookup(k.name)
		if t == nil || t.Tree == nil {
			c.probs = append(c.probs, fmt.Sprintf("%s: template %q is not defined", spec.File, k.name))
			continue
		}
		c.tree = t.Tree
		s := newScope(nil)
		s.vars["$"] = k.t
		c.list(t.Tree.Root, k.t, s)
	}
	return c.probs
}

// usesEscaper reports an identifier html/template added.
func usesEscaper(n parse.Node) bool {
	found := false
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		switch x := n.(type) {
		case *parse.ListNode:
			if x != nil {
				for _, c := range x.Nodes {
					walk(c)
				}
			}
		case *parse.ActionNode:
			walk(x.Pipe)
		case *parse.PipeNode:
			if x != nil {
				for _, c := range x.Cmds {
					walk(c)
				}
			}
		case *parse.CommandNode:
			for _, a := range x.Args {
				walk(a)
			}
		case *parse.IdentifierNode:
			if strings.HasPrefix(x.Ident, "_html_template_") {
				found = true
			}
		case *parse.IfNode:
			walkBranch(&x.BranchNode, walk)
		case *parse.RangeNode:
			walkBranch(&x.BranchNode, walk)
		case *parse.WithNode:
			walkBranch(&x.BranchNode, walk)
		case *parse.TemplateNode:
			walk(x.Pipe)
		}
	}
	walk(n)
	return found
}

func walkBranch(b *parse.BranchNode, walk func(parse.Node)) {
	walk(b.Pipe)
	walk(b.List)
	walk(b.ElseList)
}

func (c *checker) enqueue(name string, t reflect.Type) {
	k := partialKey{name, t}
	if !c.seen[k] {
		c.seen[k] = true
		c.queue = append(c.queue, k)
	}
}

// problem records a problem at n.
func (c *checker) problem(n parse.Node, format string, args ...any) {
	loc, _ := c.tree.ErrorContext(n)
	if i := strings.LastIndexByte(loc, ':'); i > 0 {
		loc = loc[:i] // the column
	}
	c.probs = append(c.probs, fmt.Sprintf("%s: %s: %s", loc, fmt.Sprintf(format, args...), n))
}

func (c *checker) list(l *parse.ListNode, dot reflect.Type, s *scope) {
	if l == nil {
		return
	}
	for _, n := range l.Nodes {
		switch x := n.(type) {
		case *parse.ActionNode:
			t := c.pipe(x.Pipe, dot, s)
			if len(x.Pipe.Decl) == 1 {
				name := x.Pipe.Decl[0].Ident[0]
				if x.Pipe.IsAssign {
					s.assign(name, t)
				} else {
					s.vars[name] = t
				}
			}
		case *parse.IfNode:
			c.branch(&x.BranchNode, dot, s, false)
		case *parse.WithNode:
			c.branch(&x.BranchNode, dot, s, true)
		case *parse.RangeNode:
			c.rangeNode(x, dot, s)
		case *parse.TemplateNode:
			t := anyT
			if x.Pipe != nil {
				t = c.pipe(x.Pipe, dot, s)
			}
			c.enqueue(x.Name, t)
		}
	}
}

// branch walks an if or a with: with narrows dot to the pipe's value.
func (c *checker) branch(b *parse.BranchNode, dot reflect.Type, s *scope, narrow bool) {
	t := c.pipe(b.Pipe, dot, s)
	inner := dot
	if narrow {
		inner = t
	}
	for _, l := range []*parse.ListNode{b.List, b.ElseList} {
		child := newScope(s)
		if len(b.Pipe.Decl) == 1 {
			child.vars[b.Pipe.Decl[0].Ident[0]] = t
		}
		d := inner
		if l == b.ElseList {
			d = dot
		}
		c.list(l, d, child)
	}
}

func (c *checker) rangeNode(r *parse.RangeNode, dot reflect.Type, s *scope) {
	t := c.pipe(r.Pipe, dot, s)
	key, elem := anyT, anyT
	switch u := deref(t); {
	case u == badT:
		key, elem = badT, badT
	case u.Kind() == reflect.Slice || u.Kind() == reflect.Array:
		key, elem = intT, u.Elem()
	case u.Kind() == reflect.Map:
		key, elem = u.Key(), u.Elem()
	case u.Kind() >= reflect.Int && u.Kind() <= reflect.Uint64:
		key, elem = u, u
	case u.Kind() == reflect.Interface:
	default:
		c.problem(r.Pipe, "cannot range over %s", u)
		key, elem = badT, badT
	}
	body := newScope(s)
	switch d := r.Pipe.Decl; len(d) {
	case 1:
		body.vars[d[0].Ident[0]] = elem
	case 2:
		body.vars[d[0].Ident[0]], body.vars[d[1].Ident[0]] = key, elem
	}
	c.list(r.List, elem, body)
	c.list(r.ElseList, dot, newScope(s))
}

// pipe is the type of the last command of p.
func (c *checker) pipe(p *parse.PipeNode, dot reflect.Type, s *scope) reflect.Type {
	var prev reflect.Type
	for _, cmd := range p.Cmds {
		prev = c.command(cmd, dot, s, prev)
	}
	if prev == nil {
		return anyT
	}
	return prev
}

type argument struct {
	node parse.Node // nil for the value a pipe passes on
	t    reflect.Type
}

func (c *checker) command(cmd *parse.CommandNode, dot reflect.Type, s *scope, prev reflect.Type) reflect.Type {
	first, rest := cmd.Args[0], cmd.Args[1:]
	id, isFunc := first.(*parse.IdentifierNode)
	if !isFunc {
		t := c.operand(first, dot, s)
		if len(rest) > 0 || prev != nil {
			c.problem(cmd, "a field or value cannot be called")
			return badT
		}
		return t
	}
	if id.Ident == "call" {
		c.problem(cmd, "call is not allowed")
		return badT
	}
	var args []argument
	for _, a := range rest {
		args = append(args, argument{a, c.operand(a, dot, s)})
	}
	if prev != nil {
		args = append(args, argument{nil, prev})
	}
	return c.function(cmd, id.Ident, args)
}

// operand is the type of one argument or command head.
func (c *checker) operand(n parse.Node, dot reflect.Type, s *scope) reflect.Type {
	switch x := n.(type) {
	case *parse.FieldNode:
		return c.resolve(dot, x.Ident, x)
	case *parse.VariableNode:
		return c.resolve(s.get(x.Ident[0]), x.Ident[1:], x)
	case *parse.ChainNode:
		return c.resolve(c.operand(x.Node, dot, s), x.Field, x)
	case *parse.PipeNode:
		return c.pipe(x, dot, s)
	case *parse.DotNode:
		return dot
	case *parse.StringNode:
		return stringT
	case *parse.BoolNode:
		return boolT
	case *parse.NumberNode:
		if x.IsInt {
			return intT
		}
		return floatT
	case *parse.IdentifierNode: // a function as an argument: a call without arguments
		return c.function(x, x.Ident, nil)
	}
	return anyT
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// resolve follows a field chain from t.
func (c *checker) resolve(t reflect.Type, names []string, n parse.Node) reflect.Type {
	for _, name := range names {
		t = deref(t)
		switch {
		case t == badT:
			return badT
		case t.Kind() == reflect.Struct:
			f, ok := t.FieldByName(name)
			if !ok || !f.IsExported() {
				_, method := reflect.PointerTo(t).MethodByName(name)
				c.problem(n, "%s has no field %s (method: %v)", t, name, method)
				return badT
			}
			if t == resultT && name == "Text" && c.spec.File != textPage {
				c.problem(n, "Result.Text is read only on %s", textPage)
			}
			t = f.Type
		case t.Kind() == reflect.Map && t.Key().Kind() == reflect.String:
			switch {
			case t == metricsMapT:
				c.problem(n, "metric ids hold dots: use metric, not .Metrics.%s", name)
				return badT
			case t == viewsMapT:
				if !slices.Contains(c.spec.Views, name) {
					c.problem(n, "view %q is not declared (views: %s)", name, strings.Join(c.spec.Views, ", "))
					return badT
				}
			case t == axesMapT:
				if !slices.Contains(axisNames, name) {
					c.problem(n, "no axis %q", name)
					return badT
				}
			case t.Elem().Kind() == reflect.Interface:
				c.problem(n, "untyped read of %s", name)
				return badT
			}
			t = t.Elem()
		case t.Kind() == reflect.Interface:
			c.problem(n, "untyped read of %s", name)
			return badT
		default:
			c.problem(n, "cannot read %s on %s", name, t)
			return badT
		}
	}
	return t
}

var (
	stringFuncs = []string{"print", "printf", "println", "html", "urlquery", "js"}
	boolFuncs   = []string{"eq", "ne", "lt", "le", "gt", "ge", "not"}
)

// function is the type of a call of name with args.
func (c *checker) function(n parse.Node, name string, args []argument) reflect.Type {
	for _, a := range args {
		if a.t == badT {
			return badT // already reported
		}
	}
	switch {
	case name == "metric":
		if len(args) != 2 {
			c.problem(n, "metric takes a result and an id")
			return badT
		}
		if args[0].t != resultPtrT {
			c.problem(n, "metric reads a *Result, not %s", args[0].t)
		}
		c.checkKey(n, resultKeys, args[1])
	case name == "index":
		return c.index(n, args)
	case name == "slice":
		if len(args) == 0 {
			return badT
		}
		return args[0].t // a slice of a slice has its type
	case name == "len":
		return intT
	case slices.Contains(boolFuncs, name):
		return boolT
	case name == "and" || name == "or":
		return anyT
	case slices.Contains(stringFuncs, name):
		return stringT
	}
	fn, ok := funcs[name]
	if !ok {
		c.problem(n, "unknown function %s", name)
		return badT
	}
	ft := reflect.TypeOf(fn)
	if ft.Kind() != reflect.Func || ft.NumOut() == 0 {
		c.problem(n, "function %s returns nothing", name)
		return badT
	}
	return ft.Out(0)
}

// resultKeys checks a literal metric id against the catalog.
func resultKeys(key string) bool { _, ok := metrics.DefOf(key); return ok }

// checkKey checks a literal argument with ok; a computed one passes.
func (c *checker) checkKey(n parse.Node, ok func(string) bool, a argument) {
	if s, lit := a.node.(*parse.StringNode); lit && !ok(s.Text) {
		c.problem(n, "no such key %q", s.Text)
	}
}

func (c *checker) index(n parse.Node, args []argument) reflect.Type {
	if len(args) == 0 {
		return badT
	}
	t := args[0].t
	for _, k := range args[1:] {
		t = deref(t)
		switch {
		case t == badT:
			return badT
		case t.Kind() == reflect.Map:
			switch t {
			case metricsMapT:
				c.checkKey(n, resultKeys, k)
			case axesMapT:
				c.checkKey(n, func(s string) bool { return slices.Contains(axisNames, s) }, k)
			case viewsMapT:
				c.checkKey(n, func(s string) bool { return slices.Contains(c.spec.Views, s) }, k)
			}
			t = t.Elem()
		case t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.String:
			if t.Kind() == reflect.String {
				return reflect.TypeFor[byte]()
			}
			t = t.Elem()
		case t.Kind() == reflect.Interface:
			t = anyT
		default:
			c.problem(n, "cannot index %s", t)
			return badT
		}
	}
	return t
}

// synthSet parses src as the page x.html of a set whose frame defines
// only a metric partial, the way loadPages does.
func synthSet(t *testing.T, src string) *template.Template {
	t.Helper()
	fsys := fstest.MapFS{
		"frame.html": {Data: []byte("{{/* views: none */}}\n{{define \"metric\"}}{{.Def.Question}}{{end}}")},
		"x.html":     {Data: []byte("{{/* views: overview */}}\n" + src)},
	}
	set, err := parseWithFrame(fsys, "x.html")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func synthSpec() pageSpec { return pageSpec{Name: "x", File: "x.html", Views: []string{"overview"}} }

func TestFieldCheckCatches(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{`{{.V.overview.Nope}}`, `.V.overview.Nope`},
		{`{{.V.quality.View}}`, `.V.quality`},
		{`{{(metric .V.overview "no.such").M.Value}}`, `no.such`},
		{`{{range .V.overview.Tables.Lately}}{{.Nope}}{{end}}`, `.Nope`},
		{`{{index .V.overview.Verdict.Axes "north"}}`, `north`},
		{`{{with .V.overview.Text}}{{.Text}}{{end}}`, `.V.overview.Text`},
		{`{{call .X}}`, `call`},
		{`{{$m := metric .V.overview "recall.hit_rate"}}{{$m.M.Nope}}`, `$m.M.Nope`},
		{`{{(metric .V.overview "recall.hit_rate").M.Extra.pending}}`, `Extra.pending`},
		{`{{.V.overview.Metrics.recall}}`, `.Metrics`},
		{`{{with .V.overview.Verdict}}{{.Axes.north}}{{end}}`, `north`},
		{`{{index .V "quality"}}`, `quality`},
		{`{{.Frame.Title.Foo}}`, `.Frame.Title.Foo`},
		{`{{template "metric" .Frame}}`, `.Def`},
		{`{{.Frame.Prefs.String}}`, `.Frame.Prefs.String`},
	} {
		got := checkTemplate(synthSet(t, tc.src), synthSpec())
		if len(got) != 1 || !strings.Contains(got[0], tc.want) || !strings.Contains(got[0], ".html:") {
			t.Errorf("%s: problems %q, want one naming %q", tc.src, got, tc.want)
		}
	}
}

func TestFieldCheckAccepts(t *testing.T) {
	for _, src := range []string{
		`{{with .V.overview.Verdict}}{{range $k, $a := .Axes}}{{$k}} {{axis $a.State}}{{end}}{{end}}`,
		`{{$m := metric .V.overview "recall.hit_rate"}}{{value $m}} {{$m.M.Num}}`,
		`{{range .V.overview.Tables.Lately}}{{clip .Project}} {{when $.Loc .Last}}{{end}}`,
		`{{template "metric" metric .V.overview "recall.hit_rate"}}`,
	} {
		if got := checkTemplate(synthSet(t, src), synthSpec()); len(got) != 0 {
			t.Errorf("%s: problems %q", src, got)
		}
	}
}

func TestFieldCheckTextPage(t *testing.T) {
	fsys := fstest.MapFS{
		"frame.html": {Data: []byte("{{/* views: none */}}\n")},
		textPage:     {Data: []byte("{{/* views: memory */}}\n{{.V.memory.Text.Text}}")},
	}
	set, err := parseWithFrame(fsys, textPage)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkTemplate(set, pageSpec{Name: "memory_text", File: textPage, Views: []string{"memory"}}); len(got) != 0 {
		t.Errorf("the text page reads Text: %q", got)
	}
}

func TestCheckerRunsOnAFreshSet(t *testing.T) {
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	spec := specs["overview"]
	if got := checkTemplate(tpls["overview"], spec); len(got) != 0 {
		t.Fatalf("a fresh set: %q", got)
	}
	if err := tpls["overview"].ExecuteTemplate(&bytes.Buffer{}, spec.File, fixtureData(t, spec, true)); err != nil {
		t.Fatal(err)
	}
	if got := checkTemplate(tpls["overview"], spec); len(got) != 1 || got[0] != "check a fresh set from loadPages" {
		t.Errorf("an executed set: %q", got)
	}
}

func TestPageTemplatesReadOnlyViewFields(t *testing.T) {
	tpls, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range specs {
		for _, p := range checkTemplate(tpls[name], spec) {
			t.Errorf("%s: %s", name, p)
		}
	}
}

func TestPageTemplatesRunOnFixtures(t *testing.T) {
	_, specs, err := loadPages(Templates())
	if err != nil {
		t.Fatal(err)
	}
	for name := range specs {
		if _, ok := pageGoldens[name]; !ok {
			t.Errorf("page %s has no entry in pageGoldens", name)
		}
	}
	for name, goldens := range pageGoldens {
		if _, ok := specs[name]; !ok {
			t.Errorf("pageGoldens names no page %s", name)
		}
		for _, g := range goldens {
			if _, err := os.Stat(goldenPath(g)); err != nil {
				t.Errorf("page %s: %v", name, err)
			}
		}
	}
	for _, empty := range []bool{false, true} {
		tpls, specs, err := loadPages(Templates()) // a set of its own per run
		if err != nil {
			t.Fatal(err)
		}
		for name, spec := range specs {
			var buf bytes.Buffer
			if err := tpls[name].ExecuteTemplate(&buf, spec.File, fixtureData(t, spec, empty)); err != nil {
				t.Errorf("%s (empty %v): %v", name, empty, err)
			} else if buf.Len() == 0 {
				t.Errorf("%s (empty %v): no output", name, empty)
			}
		}
	}
}
