package chart

import (
	"os/exec"
	"strings"
	"testing"
)

// The chart package is pure drawing: it must not know the metrics, the
// dashboard or its number formats.
func TestChartImports(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := strings.Fields(string(out))
	if len(deps) == 0 {
		t.Fatal("go list -deps listed nothing")
	}
	const mod = "github.com/Barben360/backsight/"
	for _, dep := range deps {
		switch dep {
		case mod + "internal/metrics", mod + "internal/dashboard", mod + "internal/dashboard/format":
			t.Errorf("internal/dashboard/chart depends on %s", dep)
		}
	}
}
