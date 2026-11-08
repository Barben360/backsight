package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every retain.Flusher this package builds names ReportMinChars: its zero
// value turns the report gate off, so a constructor that forgot it would
// have a reconcile rebuild every short subagent report the sweep removed.
func TestEveryFlusherWiresTheReportGate(t *testing.T) {
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Flusher" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "retain" {
				return true
			}
			found++
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "ReportMinChars" {
						return true
					}
				}
			}
			t.Errorf("%s: a retain.Flusher without ReportMinChars", fset.Position(lit.Pos()))
			return true
		})
	}
	if found == 0 {
		t.Fatal("no retain.Flusher literal found: the check looks at nothing")
	}
}
