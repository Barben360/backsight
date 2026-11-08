package retain

import (
	"context"
	"strings"

	"github.com/Barben360/backsight/internal/hindsight"
)

// BriefingTrigger is the trigger of backsight's briefing pages (decisions
// 0060 and 0083 C2): a low-budget full rewrite after a consolidation of
// the project's memories, no sooner than floorSeconds after the previous
// refresh (0: no floor).
func BriefingTrigger(floorSeconds int) hindsight.MentalModelTrigger {
	return hindsight.MentalModelTrigger{Budget: "low", Mode: "full", RefreshAfterConsolidation: true, MinRefreshIntervalSeconds: &floorSeconds}
}

// DriftedBriefings are the ids of the briefing pages among models whose
// trigger is not want (none sent counts as drifted), and how many briefing
// pages there are. Other mental models are never looked at.
func DriftedBriefings(models []hindsight.MentalModel, want hindsight.MentalModelTrigger) (pages int, drifted []string) {
	for _, m := range models {
		if !strings.HasPrefix(m.ID, BriefingModelPrefix) {
			continue
		}
		pages++
		if m.Trigger == nil || !m.Trigger.Equal(want) {
			drifted = append(drifted, m.ID)
		}
	}
	return pages, drifted
}

// BriefingTriggers is what a reconcile of the briefing pages' triggers did.
type BriefingTriggers struct {
	Checked int      `json:"checked"`
	Patched []string `json:"patched,omitempty"`
	// Failed: "<page id>: <error>" for each page whose update failed.
	Failed []string `json:"failed,omitempty"`
}

// ReconcileBriefingTriggers gives every briefing page of bank whose
// trigger is not want the whole of want (a partial trigger would reset the
// others to the server's defaults). A failed page does not stop the
// others; the error is the listing's.
func ReconcileBriefingTriggers(ctx context.Context, cl *hindsight.Client, bank string, want hindsight.MentalModelTrigger) (BriefingTriggers, error) {
	var r BriefingTriggers
	models, err := cl.MentalModels(ctx, bank)
	if err != nil {
		return r, err
	}
	var drifted []string
	r.Checked, drifted = DriftedBriefings(models, want)
	for _, id := range drifted {
		if err := cl.UpdateMentalModelTrigger(ctx, bank, id, want); err != nil {
			r.Failed = append(r.Failed, id+": "+err.Error())
			continue
		}
		r.Patched = append(r.Patched, id)
	}
	return r, nil
}
