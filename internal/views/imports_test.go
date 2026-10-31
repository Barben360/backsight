package views

import (
	"os/exec"
	"strings"
	"testing"
)

// The views are shared with the dashboard server: they must not pull the
// command line or the server in.
func TestViewsImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range []string{"github.com/spf13/cobra", "backsight/internal/cli", "backsight/internal/dashboard"} {
			if strings.HasSuffix(dep, bad) {
				t.Errorf("internal/views depends on %s", dep)
			}
		}
	}
}
