package index

import (
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/index/indexgen"
)

// The budgets' synthetic state is written by indexgen (shared with the
// metrics views' budgets); these names keep the bench tests short.
var (
	budgetSpec = indexgen.Budget
	genNow     = indexgen.Now
)

// TestGeneratorPromptReasons: indexgen cannot import this package, so it
// carries its own copy of the closed prompt reasons; a reason the index
// does not know would land as an ingest error and skew the budgets.
func TestGeneratorPromptReasons(t *testing.T) {
	if !slices.Equal(indexgen.PromptReasons, promptReasons) {
		t.Errorf("indexgen.PromptReasons = %v, the index's closed set is %v", indexgen.PromptReasons, promptReasons)
	}
	if !slices.Equal(indexgen.FailureKinds, failureKinds) {
		t.Errorf("indexgen.FailureKinds = %v, the index's closed set is %v", indexgen.FailureKinds, failureKinds)
	}
}
