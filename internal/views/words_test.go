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

// Every status a doctor history day can read, and the tag of an old scan,
// has a plain word of its own.
func TestDoctorHistoryAndScanWords(t *testing.T) {
	for _, k := range metrics.DayStatuses {
		if w := DayStatusWords[k]; w == "" || (w == k && k != "ok") {
			t.Errorf("day status %q has no plain word (%q)", k, w)
		}
	}
	if len(DayStatusWords) != len(metrics.DayStatuses) {
		t.Errorf("DayStatusWords has %d words for %d statuses", len(DayStatusWords), len(metrics.DayStatuses))
	}
	if w := TagWords["old_scan"]; w == "" || w == "old_scan" {
		t.Errorf("old_scan has no plain word (%q)", w)
	}
}

// Every state of a memory's origin has a plain word of its own.
func TestOriginWordsCoverEveryState(t *testing.T) {
	for _, k := range metrics.OriginStates {
		if w := OriginWords[k]; w == "" || w == k {
			t.Errorf("origin state %q has no plain word (%q)", k, w)
		}
	}
	if len(OriginWords) != len(metrics.OriginStates) {
		t.Errorf("OriginWords has %d words for %d states", len(OriginWords), len(metrics.OriginStates))
	}
}

// Every reason a recall can get nothing has a plain word of its own: the
// replacer would otherwise leave the code (snake_case, or "error") raw.
func TestEmptyWordsCoverEveryReason(t *testing.T) {
	for _, k := range []string{"no_candidates", "below_floor", "all_seen", "capped", "error", "unknown"} {
		if w := EmptyWords.Replace(k); w == "" || w == k {
			t.Errorf("empty reason %q has no plain word (%q)", k, w)
		}
	}
}
