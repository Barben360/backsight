package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/guide"
)

var guideRefRE = regexp.MustCompile(`guide ([a-z-]+)(#[a-z0-9-]+)?`)

// Words that follow "guide" in prose, not as a topic: `backsight guide index`
// and `guide search` are subcommands, the rest is English.
var guideWords = map[string]bool{
	"index": true, "search": true, "lists": true, "shows": true, "is": true, "and": true,
	"or": true, "always": true, "forbids": true, "serach": true,
}

// Every `guide <topic>#<section>` reference in the code's strings names a
// section the embedded docs hold, so the pointer a message gives is not dead.
// Only whole string literals of internal/ are read: a reference built with
// Sprintf or by concatenation, and one inside docs/ or the skills, is not.
func TestGuideRefsInCodeResolve(t *testing.T) {
	c, err := guide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	topics := map[string]bool{}
	for _, tp := range c.Topics {
		topics[tp.Name] = true
	}
	for _, s := range c.Sections() {
		have[s.Ref()] = true
	}

	found := map[string]string{}
	fset := token.NewFileSet()
	root := filepath.Join("..", "..", "internal")
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range guideRefRE.FindAllStringSubmatch(s, -1) {
				ref := m[1] + m[2]
				if m[2] == "" && guideWords[m[1]] {
					continue
				}
				found[ref] = fset.Position(lit.Pos()).String()
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"troubleshooting#slow-recall-and-gpu-acceleration",
		"troubleshooting#doctor-a-session-part-was-never-confirmed",
	} {
		if _, ok := found[want]; !ok {
			t.Errorf("the scan misses %q", want)
		}
	}
	for ref, where := range found {
		if !have[ref] && (strings.Contains(ref, "#") || !topics[ref]) {
			t.Errorf("%s: `guide %s` names no embedded topic or section", where, ref)
		}
	}
}
