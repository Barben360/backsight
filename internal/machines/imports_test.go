package machines

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func modulePath(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			return strings.TrimSpace(m)
		}
	}
	t.Fatal("no module line in go.mod")
	return ""
}

func TestMachinesImports(t *testing.T) {
	mod := modulePath(t)
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no files: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			first, _, _ := strings.Cut(p, "/")
			switch {
			case !strings.Contains(first, "."):
				// standard library
			case p == mod+"/internal/fsutil":
			default:
				t.Errorf("%s imports %s: only the standard library and internal/fsutil are allowed", f, p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test file checked")
	}
}
