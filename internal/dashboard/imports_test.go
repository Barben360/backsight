package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The dashboard is served by the command line, never the reverse: it must
// not pull cobra or the command line in.
func TestDashboardImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatal("go list -deps listed nothing")
	}
	for _, dep := range deps {
		for _, bad := range []string{"github.com/spf13/cobra", "backsight/internal/cli"} {
			if strings.HasSuffix(dep, bad) || strings.Contains(dep, bad+"/") {
				t.Errorf("internal/dashboard depends on %s", dep)
			}
		}
	}
}
