package index

import (
	"slices"
	"testing"

	"github.com/Barben360/backsight/internal/index/indexgen"
	"github.com/Barben360/backsight/internal/metricsfile"
)

// TestGeneratorPromptReasons: indexgen cannot import this package, so it
// carries its own copy of the closed prompt reasons; a reason the index
// does not know would land as an ingest error.
func TestGeneratorPromptReasons(t *testing.T) {
	if !slices.Equal(indexgen.PromptReasons, promptReasons) {
		t.Errorf("indexgen.PromptReasons = %v, the index's closed set is %v", indexgen.PromptReasons, promptReasons)
	}
	if !slices.Equal(indexgen.FailureKinds, failureKinds) {
		t.Errorf("indexgen.FailureKinds = %v, the index's closed set is %v", indexgen.FailureKinds, failureKinds)
	}
}

// TestCollectionFamiliesAgree: neither the index nor indexgen may import
// internal/metricsfile (an import cycle through internal/instructions'
// tests), so each names the families and their schema itself.
func TestCollectionFamiliesAgree(t *testing.T) {
	var kinds []string
	for _, k := range sourceKinds[2:] {
		kinds = append(kinds, k.prefix)
	}
	var want []string
	for _, f := range metricsfile.Families {
		want = append(want, f+"-")
	}
	if !slices.Equal(kinds, want) {
		t.Errorf("index collection prefixes %v, metricsfile's families %v", kinds, want)
	}
	if !slices.Equal(indexgen.CollectionFamilies, metricsfile.Families) {
		t.Errorf("indexgen.CollectionFamilies = %v, metricsfile.Families = %v", indexgen.CollectionFamilies, metricsfile.Families)
	}
	if collectionSchema != metricsfile.Schema {
		t.Errorf("collectionSchema %d, metricsfile.Schema %d", collectionSchema, metricsfile.Schema)
	}
}
