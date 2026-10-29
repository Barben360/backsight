package doctor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// checkTypes are the composite-literal types whose ID field names a check
// that doctor, setup's preflight or verify reports to the user.
var checkTypes = map[string]bool{"Check": true, "doctor.Check": true, "setup.Check": true, "verifyCheck": true}

// checkIDs collects the string literal of each ID: field of a check literal
// in the non-test Go files of the given packages (relative to internal/).
func checkIDs(t *testing.T, pkgs ...string) map[string]string {
	t.Helper()
	ids := map[string]string{}
	fset := token.NewFileSet()
	for _, pkg := range pkgs {
		files, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no Go files in internal/%s: %v", pkg, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, f, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !checkTypes[typeName(lit.Type)] {
					return true
				}
				for _, e := range lit.Elts {
					kv, ok := e.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "ID" {
						continue
					}
					if s, ok := kv.Value.(*ast.BasicLit); ok && s.Kind == token.STRING {
						id, _ := strconv.Unquote(s.Value)
						ids[id] = f
					}
				}
				return true
			})
		}
	}
	return ids
}

func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if p, ok := x.X.(*ast.Ident); ok {
			return p.Name + "." + x.Sel.Name
		}
	}
	return ""
}

// Every check doctor, setup's preflight and verify can report is named in
// the troubleshooting page, so a user who sees one finds what to do.
func TestTroubleshootingNamesEveryDoctorCheck(t *testing.T) {
	ids := checkIDs(t, "doctor", "cli", "setup")
	if len(ids) < 45 {
		t.Fatalf("found %d check ids: want at least 45 (the parse lost some)", len(ids))
	}
	for _, want := range []string{"server-id", "server-reach", "version-skew", "split-session", "replace-unconfirmed"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("the id scan misses %q", want)
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	var missing []string
	for id, f := range ids {
		if !strings.Contains(doc, "`"+id+"`") {
			missing = append(missing, id+" ("+filepath.Base(f)+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("docs/troubleshooting.md does not name these checks in backticks: %s", strings.Join(missing, ", "))
	}
}
