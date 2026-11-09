package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// feedbackImport is the import path of the package that reads ledger.json.
const feedbackImport = "github.com/Barben360/backsight/internal/feedback"

// ledgerJSONReaders are the functions allowed to read ledger.json, each
// "<package dir>.<function>" or "<package dir>.<receiver type>.<method>"
// ("*" any method): the fallback reader and what it is built of, the
// export that writes the file, the cli chain that falls back to it, the
// index's default reader and the bench fixtures' generator.
var ledgerJSONReaders = []string{
	"internal/feedback.LoadLedger",
	"internal/feedback.Current",
	"internal/feedback.journalSince",
	"internal/feedback.CurrentReader.*",
	"internal/feedback.stampedName", // the names CurrentReader.Stamp stamps
	"internal/feedback.Journal.AppendFolded",
	"internal/feedback.ledgerFile",
	"internal/feedback/ledgerdb.Writer.Export",
	"internal/cli.ledgerReader",
	"internal/cli.indexLedger",
	"internal/cli.ledgerChain", // the type of their chain: its fallback field
	"internal/index.Options.withDefaults",
	"internal/index/indexgen.Generate",
}

// ledgerJSONAllowed: fn of the package in pkgDir may read ledger.json.
func ledgerJSONAllowed(pkgDir, fn string) bool {
	for _, a := range ledgerJSONReaders {
		pkg, name, _ := strings.Cut(a, ".")
		if pkg != pkgDir {
			continue
		}
		if recv, ok := strings.CutSuffix(name, ".*"); ok {
			if r, _, isMethod := strings.Cut(fn, "."); isMethod && r == recv {
				return true
			}
		}
		if name == fn {
			return true
		}
	}
	return false
}

// ledgerJSONUse is one place that reads ledger.json or names it.
type ledgerJSONUse struct {
	line int
	fn   string // the enclosing function, "" outside one
	what string
}

func (u ledgerJSONUse) String() string { return fmt.Sprintf("%d %s %s", u.line, u.fn, u.what) }

// ledgerJSONNames are the feedback package's names that read ledger.json.
var ledgerJSONNames = map[string]bool{"LoadLedger": true, "Current": true, "Rebuild": true, "CurrentReader": true}

// ledgerJSONUses is every place of f, a file of the package in pkgDir
// (slash-separated, from the module root), that reads ledger.json or
// names it: any use of feedback.LoadLedger, Current, Rebuild or
// CurrentReader (a call, a function value, a type, new, a literal), bare
// inside internal/feedback or under a dot import of it, and the literal
// "ledger.json". Declaring one of those names is no use of it. A use in a
// type declaration is enclosed by that type, any other outside a
// function by "".
func ledgerJSONUses(fset *token.FileSet, f *ast.File, pkgDir string) []ledgerJSONUse {
	names := map[string]bool{} // the local names of the feedback package
	bare := pkgDir == "internal/feedback"
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == feedbackImport {
			switch {
			case imp.Name == nil:
				names["feedback"] = true
			case imp.Name.Name == ".":
				bare = true // its names, unqualified
			default:
				names[imp.Name.Name] = true
			}
		}
	}
	var out []ledgerJSONUse
	scan := func(n ast.Node, fn string) {
		var visit func(n ast.Node) bool
		use := func(n ast.Node, what string) {
			out = append(out, ledgerJSONUse{fset.Position(n.Pos()).Line, fn, what})
		}
		visit = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && names[x.Name] && ledgerJSONNames[n.Sel.Name] {
					use(n, "uses "+n.Sel.Name)
				}
				ast.Inspect(n.X, visit) // a field or method named Current is no use
				return false
			case *ast.Ident:
				if bare && ledgerJSONNames[n.Name] {
					use(n, "uses "+n.Name)
				}
			case *ast.BasicLit:
				if s, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && s == "ledger.json" {
					use(n, `names "ledger.json"`)
				}
			}
			return true
		}
		ast.Inspect(n, visit)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			gd := d.(*ast.GenDecl)
			for _, sp := range gd.Specs {
				if ts, ok := sp.(*ast.TypeSpec); ok { // its uses count as the type's; its name aside
					scan(ts.Type, ts.Name.Name)
					if ts.TypeParams != nil {
						scan(ts.TypeParams, ts.Name.Name)
					}
					continue
				}
				scan(sp, "")
			}
			continue
		}
		fn := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			fn = receiverType(fd.Recv.List[0].Type) + "." + fn
			scan(fd.Recv, fn)
		}
		scan(fd.Type, fn)
		if fd.Body != nil {
			scan(fd.Body, fn)
		}
	}
	return out
}

// receiverType is the name of a receiver's type, without its pointer or
// type parameters.
func receiverType(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return "?"
		}
	}
}

// forbiddenLedgerJSONUses is ledgerJSONUses of the file at path, parsed
// as one of the package in pkgDir, outside ledgerJSONReaders.
func forbiddenLedgerJSONUses(t *testing.T, path, pkgDir string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, u := range ledgerJSONUses(fset, f, pkgDir) {
		if !ledgerJSONAllowed(pkgDir, u.fn) {
			out = append(out, u.String())
		}
	}
	return out
}

// TestOnlyTheFallbackReadsLedgerJSON: no code but the fallback reads
// ledger.json: every non-test Go file under internal/ and cmd/, a new one
// included, is parsed, and a call of feedback.LoadLedger, Current or
// Rebuild, a feedback.CurrentReader literal or the literal "ledger.json"
// outside the functions allowed fails. Its own control: a file holding
// each of them is flagged, in the package it is read as.
func TestOnlyTheFallbackReadsLedgerJSON(t *testing.T) {
	root := filepath.Join("..", "..")
	scanned := 0
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() && e.Name() == "testdata" {
				return filepath.SkipDir
			}
			if e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			scanned++
			for _, u := range forbiddenLedgerJSONUses(t, path, filepath.ToSlash(rel)) {
				t.Errorf("%s:%s: only the fallback reads ledger.json (ledgerReader, indexLedger)", filepath.ToSlash(path), u)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned %d files: not the module's", scanned)
	}

	fixture := filepath.Join("testdata", "ledgerjson_scan", "forbidden.go.txt")
	every := []string{
		"8 loadsTheLedger uses LoadLedger",
		"10 current uses Current",
		"12 rebuilds uses Rebuild",
		"14 buildsAReader uses CurrentReader",
		`16 namesTheFile names "ledger.json"`,
		"18  uses LoadLedger",
		"22 other.indexLedger uses CurrentReader",
		"24 Writer.Export uses LoadLedger",
		"26 ledgerReader uses CurrentReader",
		"30  uses LoadLedger",
		"32 passesIt uses Current",
		"34 allocates uses CurrentReader",
		"36  uses CurrentReader",
		"38 typed uses CurrentReader",
	}
	for _, c := range []struct {
		pkgDir string
		want   []string
	}{
		// In the cli, its own ledgerReader may build the fallback.
		{"internal/cli", slices.DeleteFunc(slices.Clone(every), func(u string) bool { return strings.Contains(u, " ledgerReader ") })},
		{"internal/recall", every},
		// Inside the feedback package, its bare names count too, but not
		// a field or method of that name.
		{"internal/feedback", slices.Insert(slices.Clone(every), 9, "28 bare uses LoadLedger")},
	} {
		if got := forbiddenLedgerJSONUses(t, fixture, c.pkgDir); !slices.Equal(got, c.want) {
			t.Errorf("the control read as %s: flagged\n%s\nwant\n%s", c.pkgDir, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
	}
	// A dot import is the package itself: its names, bare.
	dot := filepath.Join("testdata", "ledgerjson_scan", "dotimport.go.txt")
	want := []string{"7 dotted uses LoadLedger", "9 dottedReader uses CurrentReader", "9 dottedReader uses CurrentReader"}
	if got := forbiddenLedgerJSONUses(t, dot, "internal/recall"); !slices.Equal(got, want) {
		t.Errorf("the dot import: flagged\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
