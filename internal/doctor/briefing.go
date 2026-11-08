package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/retain"
)

// briefingTriggers adds to the bank check the briefing pages whose trigger
// is not the one cfg wants (decision 0083 C2): without the floor the server
// rewrites a page after every consolidation, an LLM call each. Each page
// says the floor found, so that two PCs of a shared server set to other
// intervals show up. A failed listing says nothing: the check is about the
// bank.
func briefingTriggers(ctx context.Context, cl *hindsight.Client, bank string, cfg config.Config, c *Check) {
	if cfg.Recall.Briefing != "model" || !cfg.Components.Read {
		return // jobs tick deletes the pages
	}
	models, err := cl.MentalModels(ctx, bank)
	if err != nil {
		return
	}
	want := retain.BriefingTrigger(cfg.Recall.BriefingRefreshInterval.Seconds())
	var drifted []string
	for _, m := range models {
		if strings.HasPrefix(m.ID, retain.BriefingModelPrefix) && !retain.BriefingTriggerIs(m.Trigger, want) {
			drifted = append(drifted, m.ID+" (found "+foundTrigger(m.Trigger, want)+")")
		}
	}
	if len(drifted) == 0 {
		return
	}
	if c.Status == OK || c.Status == Info {
		c.Status = Warn
	}
	c.Detail = strings.TrimPrefix(c.Detail+fmt.Sprintf("; %d briefing page(s) have another refresh trigger than this PC's (%s): %s",
		len(drifted), floorText(want.MinRefreshIntervalSeconds), strings.Join(drifted, ", ")), "; ")
	const fix = "backsight jobs maintain"
	if c.Fix == "" {
		c.Fix = fix
	} else {
		c.Fix += "; then " + fix + " gives the briefing pages their refresh floor"
	}
}

// floorText says a floor: "floor 6h", "no floor" for 0 or null.
func floorText(s *int) string {
	if s == nil || *s == 0 {
		return "no floor"
	}
	return "floor " + config.SecondsOf(*s).String()
}

// foundTrigger says how a page's trigger differs from want.
func foundTrigger(got *hindsight.MentalModelTrigger, want hindsight.MentalModelTrigger) string {
	if got == nil {
		return "no trigger"
	}
	if floorText(got.MinRefreshIntervalSeconds) != floorText(want.MinRefreshIntervalSeconds) {
		return floorText(got.MinRefreshIntervalSeconds)
	}
	return fmt.Sprintf("budget %q, mode %q, refresh after consolidation %v", got.Budget, got.Mode, got.RefreshAfterConsolidation)
}
