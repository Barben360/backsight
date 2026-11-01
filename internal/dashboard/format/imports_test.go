package format

import (
	"os/exec"
	"strings"
	"testing"
)

// The formats serve pages only: they never reach the dashboard server, the
// command line's views or the network on their own.
func TestFormatImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatal("go list -deps listed nothing")
	}
	const mod = "github.com/Barben360/backsight/"
	for _, dep := range deps {
		if dep == mod+"internal/dashboard" || dep == mod+"internal/views" {
			t.Errorf("internal/dashboard/format depends on %s", dep)
		}
	}
	// net/http is below internal/metrics (the server client), so only the
	// package's own imports can be held to it.
	own, err := exec.Command("go", "list", "-f", `{{join .Imports "\n"}}`, ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, imp := range strings.Fields(string(own)) {
		if imp == "net/http" {
			t.Errorf("internal/dashboard/format imports net/http")
		}
	}
}
