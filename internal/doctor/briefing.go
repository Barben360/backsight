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
// rewrites a page after every consolidation, an LLM call each. A failed
// listing says nothing: the check is about the bank.
func briefingTriggers(ctx context.Context, cl *hindsight.Client, bank string, cfg config.Config, c *Check) {
	if cfg.Recall.Briefing != "model" || !cfg.Components.Read {
		return // jobs tick deletes the pages
	}
	models, err := cl.MentalModels(ctx, bank)
	if err != nil {
		return
	}
	pages, drifted := retain.DriftedBriefings(models, retain.BriefingTrigger(cfg.Recall.BriefingRefreshInterval.Seconds()))
	if len(drifted) == 0 {
		return
	}
	if c.Status == OK || c.Status == Info {
		c.Status = Warn
	}
	c.Detail = strings.TrimPrefix(c.Detail+fmt.Sprintf("; %d of %d briefing page(s) refresh without the %s floor (%s)", len(drifted), pages, cfg.Recall.BriefingRefreshInterval, strings.Join(drifted, ", ")), "; ")
	const fix = "backsight jobs maintain"
	if c.Fix == "" {
		c.Fix = fix
	} else {
		c.Fix += "; then " + fix + " gives the briefing pages their refresh floor"
	}
}
