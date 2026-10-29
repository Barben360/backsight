package doctor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/plugin"
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
// the troubleshooting page, so a user who sees one finds what to do. Only
// the packages doctor, cli and setup are scanned, and only the types in
// checkTypes: the floor of 45 ids guards against the scan silently
// shrinking (a renamed type, a moved check).
func TestTroubleshootingNamesEveryDoctorCheck(t *testing.T) {
	ids := checkIDs(t, "doctor", "cli", "setup")
	// Ids built from a variable, which the literal scan cannot see.
	// setup's preflight makes the port ids from a table: the guard fails
	// when that table changes, so this list is revisited.
	pre, err := os.ReadFile(filepath.Join("..", "setup", "preflight.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pre), `{"api_port", p.APIPort}, {"ui_port", p.UIPort}`) {
		t.Error("setup/preflight.go no longer builds the api_port and ui_port ids as this test assumes: update the list")
	}
	ids["api_port"], ids["ui_port"] = "preflight.go", "preflight.go"
	// verify's hook checks are "hook <arg>", one per plugin hook.
	if len(plugin.Hooks()) == 0 {
		t.Fatal("the plugin wires no hook")
	}
	ids["hook <arg>"] = "verify.go"
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
	named := checksNamed(string(raw))
	var missing []string
	for id, f := range ids {
		if !named[id] {
			missing = append(missing, id+" ("+filepath.Base(f)+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("docs/troubleshooting.md names these checks in no heading, table row or sentence calling it a check: %s", strings.Join(missing, ", "))
	}
}

// checksNamed is what doc names as a check: a backticked word in a
// heading or in the first cell of a table row, or one a sentence calls a
// check or a warning ("doctor's `jobs` check", "a `client` warning"). A
// mention as anything else (a command word, a config key, a group) is no
// entry a user can find.
func checksNamed(doc string) map[string]bool {
	named := map[string]bool{}
	tick := regexp.MustCompile("`([^`]+)`")
	asCheck := regexp.MustCompile("`([^`]+)` (?:check|warning)")
	for line := range strings.SplitSeq(doc, "\n") {
		re := tick
		switch {
		case strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "|"):
			line = strings.SplitN(line, "|", 3)[1]
		default:
			re = asCheck
		}
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			named[m[1]] = true
		}
	}
	return named
}

// A check id counts as named in a heading, a table row's first cell, or
// a sentence calling it a check or a warning; never as a command word, a
// config key or a row's second cell.
func TestChecksNamedOnlyAsChecks(t *testing.T) {
	doc := "# Troubleshooting\n\nRun `update` to fetch it; set `mcp` in config; add you to the `docker` group.\n" +
		"doctor's `jobs` check warns; a `client` warning means drift.\n\n## doctor: `server-id` differs\n\n" +
		"| doctor check | What to do |\n|---|---|\n| `split-session` (info): x | run `recall` again, see the `gpu` check |\n"
	got := checksNamed(doc)
	for _, id := range []string{"server-id", "split-session", "jobs", "client"} {
		if !got[id] {
			t.Errorf("%s not found", id)
		}
	}
	for _, id := range []string{"update", "mcp", "docker", "recall", "gpu"} {
		if got[id] {
			t.Errorf("%s found, though only mentioned", id)
		}
	}
}
