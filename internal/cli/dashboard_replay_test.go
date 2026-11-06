package cli

import (
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/dashboard"
)

// The learned-ranking card offers a command to try another weight. It must
// be one `feedback replay` accepts: its key is one of replayKeys, and the
// command's own setter takes the whole setting.
func TestDashboardReplayCommandIsOneReplayAccepts(t *testing.T) {
	setting, ok := strings.CutPrefix(dashboard.BoostReplayCommand, "backsight feedback replay --set ")
	if !ok {
		t.Fatalf("command %q is not a replay with --set", dashboard.BoostReplayCommand)
	}
	key, _, _ := strings.Cut(setting, "=")
	if _, known := replayKeys[key]; !known {
		t.Fatalf("key %q of %q is not one of replayKeys", key, dashboard.BoostReplayCommand)
	}
	var f config.Feedback
	got, _, err := setFeedbackKey(&f, setting)
	if err != nil || got != key {
		t.Errorf("replay refuses %q: key %q, error %v", setting, got, err)
	}
	if f.BoostWeight != 0.15 {
		t.Errorf("boost weight %v after %q, want 0.15", f.BoostWeight, setting)
	}
}
