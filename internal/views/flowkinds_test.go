package views

import (
	"testing"

	"github.com/Barben360/backsight/internal/metrics"
)

// The items sent are named by kind, a kind with none left out; a metric that
// does not say reads nothing, never zeros.
func TestFlowKindsNameWhatIsSent(t *testing.T) {
	for _, c := range []struct {
		extra map[string]any
		want  string
	}{
		{map[string]any{"sessions": int64(35), "notes": int64(160), "subagent_reports": int64(80)}, "35 from sessions, 160 notes, 80 subagent reports"},
		{map[string]any{"sessions": int64(1), "notes": int64(0), "subagent_reports": int64(1)}, "1 from a session, 1 subagent report"},
		{map[string]any{"ok": int64(3)}, ""},
	} {
		if got := FlowKinds(metrics.Metric{Extra: c.extra}); got != c.want {
			t.Errorf("%v: %q, want %q", c.extra, got, c.want)
		}
	}
}
