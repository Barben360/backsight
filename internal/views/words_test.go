package views

import (
	"testing"

	"github.com/Barben360/backsight/internal/metrics"
)

// Every key the time-to-recall funnel carries, and every state of a
// never-extracted session, has a plain word of its own (Word falls back to
// the key itself, which a person must not read as a snake_case name).
func TestFunnelAndNeverExtractedWordsCoverEveryKey(t *testing.T) {
	for _, k := range metrics.FunnelKeys {
		if w := FunnelWords[k]; w == "" || (w == k && k != "sent") { // "sent" is its own plain word
			t.Errorf("funnel key %q has no plain word (%q)", k, w)
		}
	}
	for _, k := range metrics.NeverStates {
		if w := NeverExtractedWords[k]; w == "" || w == k {
			t.Errorf("never-extracted state %q has no plain word (%q)", k, w)
		}
	}
	if len(FunnelWords) != len(metrics.FunnelKeys) {
		t.Errorf("FunnelWords has %d words for %d funnel keys", len(FunnelWords), len(metrics.FunnelKeys))
	}
}
