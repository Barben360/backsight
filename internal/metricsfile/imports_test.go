package metricsfile

import (
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMetricsfileImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	have := map[string]bool{}
	for _, d := range strings.Fields(string(out)) {
		have[d] = true
	}
	for _, want := range []string{
		"github.com/Barben360/backsight/internal/metricsfile",
		"github.com/Barben360/backsight/internal/session",
		"github.com/Barben360/backsight/internal/fsutil",
	} {
		if !have[want] {
			t.Fatalf("go list -deps does not list %s; the check below would prove nothing", want)
		}
	}
	for _, banned := range []string{"hindsight", "index", "metrics"} {
		if p := "github.com/Barben360/backsight/internal/" + banned; have[p] {
			t.Errorf("internal/metricsfile depends on %s", p)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files: %v", err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, banned := range []string{"internal/hindsight", "internal/index", "internal/metrics"} {
				if strings.HasSuffix(p, banned) || strings.Contains(p, banned+"/") {
					t.Errorf("%s imports %s", f, p)
				}
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no non-test source checked")
	}
}
