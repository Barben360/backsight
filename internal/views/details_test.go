package views

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metrics"
)

// keyReaders are the helpers that read an Extra key from a metric.
var keyReaders = []string{"extraInt", "extraFloat", "extraCounts", "extraTime", "group"}

func lit(e ast.Expr) (string, bool) {
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	return s, err == nil
}

// readKeys parses file and returns, per switch case of d.ID, the Extra
// keys its clause reads: literals given to a key reader, and the elements
// of a []string a loop walks to read them.
func readKeys(t *testing.T, file string) map[string][]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		var ids []string
		for _, e := range cc.List {
			if s, ok := lit(e); ok && metricIDLike.MatchString(s) {
				ids = append(ids, s)
			}
		}
		if len(ids) == 0 {
			return true
		}
		var keys []string
		for _, st := range cc.Body {
			ast.Inspect(st, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && slices.Contains(keyReaders, id.Name) {
						for _, a := range x.Args {
							if s, ok := lit(a); ok {
								keys = append(keys, s)
							}
						}
					}
				case *ast.RangeStmt:
					cl, ok := x.X.(*ast.CompositeLit)
					v, vok := x.Value.(*ast.Ident)
					if !ok || !vok || !callsWith(x.Body, v.Name) {
						return true
					}
					for _, e := range cl.Elts {
						if s, ok := lit(e); ok {
							keys = append(keys, s)
						}
					}
				}
				return true
			})
		}
		for _, id := range ids {
			out[id] = append(out[id], keys...)
		}
		return true
	})
	return out
}

var metricIDLike = regexp.MustCompile(`^[a-z_]+\.[a-z0-9_]+$`)

// callsWith reports a key reader called with the variable name.
func callsWith(body ast.Node, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && slices.Contains(keyReaders, id.Name) {
				for _, a := range c.Args {
					if v, ok := a.(*ast.Ident); ok && v.Name == name {
						found = true
					}
				}
			}
		}
		return true
	})
	return found
}

// Every Extra key the words read is one the catalog declares for the
// metric: a key renamed on one side only breaks this.
func TestExtraKeysReadAreDeclaredInTheCatalog(t *testing.T) {
	total := 0
	for _, file := range []string{"details.go", "extras.go"} {
		for id, keys := range readKeys(t, file) {
			d, ok := metrics.DefOf(id)
			if !ok {
				t.Errorf("%s: %s is not in the catalog", file, id)
				continue
			}
			for _, k := range keys {
				total++
				if !slices.Contains(d.Extra, k) {
					t.Errorf("%s reads Extra %q of %s, which the catalog declares as %v", file, k, id, d.Extra)
				}
			}
		}
	}
	if total < 30 {
		t.Errorf("only %d keys found: the reader of the source is broken", total)
	}
}

func TestRetainOutcomesSlowestIsShown(t *testing.T) {
	d, _ := metrics.DefOf("retain.outcomes")
	if !slices.Contains(d.Extra, "p95_ms") {
		t.Fatal("the catalog no longer declares p95_ms")
	}
	m := metrics.Metric{Status: metrics.StatusOK, Extra: map[string]any{"by_status": map[string]int64{"ok": 2}, "p95_ms": 2500.0}}
	got := strings.Join(ExtraLines(d, m), "|")
	if !strings.Contains(got, "slowest 1 in 20: 2.5 s") || !strings.Contains(got, "by result: sent 2") {
		t.Errorf("ExtraLines: %q", got)
	}
}

func TestSkipAndFailureWordsCoverEveryName(t *testing.T) {
	for _, r := range append(slices.Clone(indexgen.PromptReasons), "unknown") {
		if SkipWords[r] == "" || SkipWords[r] == r {
			t.Errorf("prompt reason %q has no words", r)
		}
	}
	for _, k := range append(slices.Clone(indexgen.FailureKinds), "unknown") {
		if FailureKindWords[k] == "" || FailureKindWords[k] == k {
			t.Errorf("hook failure kind %q has no words", k)
		}
	}
}

func TestPageDetailsNameSkipsAndHooks(t *testing.T) {
	empty, _ := metrics.DefOf("recall.empty_reasons")
	hooks, _ := metrics.DefOf("hooks.failures")
	skips := map[string]int64{}
	for _, r := range indexgen.PromptReasons {
		skips[r] = 1
	}
	em := metrics.Metric{Status: metrics.StatusOK, Extra: map[string]any{"skips": skips}}
	hm := metrics.Metric{Status: metrics.StatusOK, Extra: map[string]any{"last_24h": map[string]int64{"panic": 2, "input_too_large": 1, "spawn": 3}}}
	page := strings.Join(append(PageDetails(empty, em, time.UTC), PageDetails(hooks, hm, time.UTC)...), "\n")
	cli := strings.Join(append(MetricDetails(empty, em, time.UTC), MetricDetails(hooks, hm, time.UTC)...), "\n")
	for _, raw := range append(slices.Clone(indexgen.PromptReasons), "panic", "input_too_large", "spawn") {
		if regexp.MustCompile(`\b` + raw + `\b`).MatchString(page) {
			t.Errorf("the page shows the stored name %q: %q", raw, page)
		}
		if !strings.Contains(cli, raw) {
			t.Errorf("the command line lost the stored name %q", raw)
		}
	}
	for _, w := range []string{"same topic as before 1", "too short 1", "could not start 3", "input too large 1"} {
		if !strings.Contains(page, w) {
			t.Errorf("page lacks %q in %q", w, page)
		}
	}
}
