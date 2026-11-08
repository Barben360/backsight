package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/fsutil"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/telemetry"
)

// The briefing pages' trigger (decision 0083 C2): every page of the bank
// is given the wanted one, so that pages made by an earlier version get the
// refresh floor too. jobs maintain does it daily; jobs tick once per binary
// version and floor (its marker), so an update applies it within a tick.

// briefingTriggerMarker records the binary version and floor of the last
// reconcile that left every page right.
func briefingTriggerMarker(d paths.Dirs) string { return filepath.Join(d.State, "briefing-trigger") }

func briefingTriggerKey(cfg config.Config) string {
	return buildinfo.Version + " " + strconv.Itoa(cfg.Recall.BriefingRefreshInterval.Seconds())
}

// reconcileBriefingTriggers sets the wanted trigger on every briefing page
// whose trigger differs, emits what it did, and records the marker when
// no page failed. Its line is "" when every page was already right.
func reconcileBriefingTriggers(ctx context.Context, d paths.Dirs, cfg config.Config, cl *hindsight.Client, tw *telemetry.Writer, job string) string {
	r, err := retain.ReconcileBriefingTriggers(ctx, cl, cfg.Server.Bank, retain.BriefingTrigger(cfg.Recall.BriefingRefreshInterval.Seconds()))
	if err != nil {
		tw.Emit(telemetry.Raw{Name: "briefing_trigger", Fields: map[string]any{"job": job, "error": err.Error()}})
		return "briefing pages not checked: " + err.Error()
	}
	if r.Checked > 0 { // a bank without pages has nothing to count
		tw.Emit(telemetry.Raw{Name: "briefing_trigger", Fields: map[string]any{"job": job, "checked": r.Checked, "patched": len(r.Patched), "failed": len(r.Failed)}})
	}
	if len(r.Failed) == 0 {
		_ = fsutil.WriteAtomic(briefingTriggerMarker(d), []byte(briefingTriggerKey(cfg)+"\n"), 0o600)
	}
	var line string
	if len(r.Patched) > 0 {
		line = fmt.Sprintf("%d briefing page(s) given the %s refresh floor", len(r.Patched), cfg.Recall.BriefingRefreshInterval)
	}
	if len(r.Failed) > 0 {
		line = strings.TrimPrefix(line+fmt.Sprintf("; %d briefing page(s) not updated (%s)", len(r.Failed), strings.Join(r.Failed, "; ")), "; ")
	}
	return line
}

// tickBriefingTriggers reconciles once per binary version and floor; ""
// when it does not run. The caller has checked the server is the joined one.
func tickBriefingTriggers(ctx context.Context, d paths.Dirs, cfg config.Config, cl *hindsight.Client, tw *telemetry.Writer) string {
	if briefingFor(cfg) != "model" {
		return ""
	}
	if raw, err := os.ReadFile(briefingTriggerMarker(d)); err == nil && strings.TrimSpace(string(raw)) == briefingTriggerKey(cfg) {
		return ""
	}
	return reconcileBriefingTriggers(ctx, d, cfg, cl, tw, "tick")
}

// maintainBriefingTriggers is jobs maintain's daily reconcile; "" when
// there is nothing to do or say.
func maintainBriefingTriggers(ctx context.Context, d paths.Dirs, tw *telemetry.Writer) string {
	cfg, sec, err := loadServerConfig()
	if err != nil || briefingFor(cfg) != "model" || otherServer(d, cfg) != nil {
		return ""
	}
	cl, err := hindsight.New(cfg.Server.URL, sec.APIKey, 10*time.Second)
	if err != nil {
		return ""
	}
	return reconcileBriefingTriggers(ctx, d, cfg, cl, tw, "maintain")
}
