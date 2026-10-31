package metrics

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMetricsImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	have := map[string]bool{}
	for _, d := range deps {
		have[d] = true
	}
	// The list is real (not empty by accident): it holds this package and
	// the index it reads.
	for _, want := range []string{
		"github.com/Barben360/backsight/internal/metrics",
		"github.com/Barben360/backsight/internal/index",
	} {
		if !have[want] {
			t.Fatalf("go list -deps does not list %s; the check below would prove nothing", want)
		}
	}
	for _, banned := range []string{
		"github.com/spf13/cobra",
		"github.com/Barben360/backsight/internal/cli",
		"github.com/Barben360/backsight/internal/dashboard",
		"github.com/Barben360/backsight/internal/server",
	} {
		if have[banned] {
			t.Errorf("internal/metrics depends on %s", banned)
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
		if _, err := os.Stat(f); err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		checked++
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if p == "net/http" {
				t.Errorf("%s imports net/http", f)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test file was parsed")
	}
}
