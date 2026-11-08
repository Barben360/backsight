package retain

import (
	"context"
	"errors"
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
		if !BriefingTriggerIs(m.Trigger, want) {
			drifted = append(drifted, m.ID)
		}
	}
	return pages, drifted
}

// BriefingTriggerIs reports whether got (nil: none sent) is want. With no
// floor wanted, a null floor is no floor too: the server may send a
// stored 0 back as null.
func BriefingTriggerIs(got *hindsight.MentalModelTrigger, want hindsight.MentalModelTrigger) bool {
	if got == nil {
		return false
	}
	g := *got
	if g.MinRefreshIntervalSeconds == nil && want.MinRefreshIntervalSeconds != nil && *want.MinRefreshIntervalSeconds == 0 {
		zero := 0
		g.MinRefreshIntervalSeconds = &zero
	}
	return g.Equal(want)
}

// BriefingTriggers is what a reconcile of the briefing pages' triggers did.
type BriefingTriggers struct {
	Checked int      `json:"checked"`
	Patched []string `json:"patched,omitempty"`
	// Gone: pages deleted between the listing and their update.
	Gone []string `json:"gone,omitempty"`
	// Failed: "<page id>: <error>" for each page whose update failed.
	Failed []string `json:"failed,omitempty"`
}

// ReconcileBriefingTriggers gives every briefing page of bank whose
// trigger is not want, or whose source query is not query (0083 C7), both
// in one update: the whole of want (a partial trigger would reset the
// others to the server's defaults) and query, which the page's next
// refresh is written from. A failed page does not stop the others, and a
// page deleted meanwhile (404) is done; the error is the listing's.
func ReconcileBriefingTriggers(ctx context.Context, cl *hindsight.Client, bank string, want hindsight.MentalModelTrigger, query string) (BriefingTriggers, error) {
	var r BriefingTriggers
	models, err := cl.MentalModels(ctx, bank)
	if err != nil {
		return r, err
	}
	var drifted []string
	for _, m := range models {
		if !strings.HasPrefix(m.ID, BriefingModelPrefix) {
			continue
		}
		r.Checked++
		if !BriefingTriggerIs(m.Trigger, want) || m.SourceQuery != query {
			drifted = append(drifted, m.ID)
		}
	}
	for _, id := range drifted {
		if err := cl.UpdateMentalModel(ctx, bank, id, query, want); err != nil {
			var apiErr *hindsight.APIError
			if errors.As(err, &apiErr) && apiErr.Kind == hindsight.NotFound {
				r.Gone = append(r.Gone, id) // nothing left to set
				continue
			}
			r.Failed = append(r.Failed, id+": "+err.Error())
			continue
		}
		r.Patched = append(r.Patched, id)
	}
	return r, nil
}
