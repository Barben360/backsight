package cli

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/telemetry"
)

func TestStatsReportsTelemetryAndExtraction(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	llmInput.Store(2_000_000)
	tw := telemetry.New(pathsOf(home).State, telemetry.Common{Session: "s1"})
	tw.Emit(telemetry.Prompt{N: 1, Decision: "recall", Plen: 80, MS: 1200, Injected: 1, Scopes: map[string]telemetry.ScopeResult{"project": {OK: true}}})
	tw.Emit(telemetry.Outcome{N: 1, Searched: true})
	code, out, _ := run(t, "stats", "3")
	if code != output.OK || !strings.Contains(out, "last 3 day(s)") || !strings.Contains(out, "1 typed") || !strings.Contains(out, "p50 1200 ms") ||
		!strings.Contains(out, "followed by a search 100%") || !strings.Contains(out, "2000000 input") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if code, out, _ := run(t, "stats", "--json"); code != output.OK || !strings.Contains(out, `"days": 7`) || !strings.Contains(out, `"recalls": 1`) {
		t.Errorf("default window: exit %d\n%s", code, out)
	}
	for _, bad := range []string{"0", "366", "x"} {
		if code, _, _ := run(t, "stats", bad); code != output.Usage {
			t.Errorf("stats %s: exit %d", bad, code)
		}
	}
	// the server down leaves the local report
	statsDown.Store(true)
	if code, out, _ := run(t, "stats"); code != output.OK || !strings.Contains(out, "Extraction   unavailable") || !strings.Contains(out, "1 typed") {
		t.Errorf("server down: exit %d\n%s", code, out)
	}
}
