package views

import (
	"testing"

	"github.com/Barben360/backsight/internal/metrics"
)

// A too-few metric says what it needs: the count its own n lacks, else, for
// one judged on a p95 its n reached, the 100 samples the p95 needs.
func TestTooFewSaysWhatIsNeeded(t *testing.T) {
	ttr, _ := metrics.DefOf("pipeline.time_to_recall")
	n := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		den  int64
		want string
	}{
		{5, "too few, 15 more needed"},
		{33, "too few, the slowest 1 in 20 needs 100"},
	} {
		if got := StatusText(ttr, metrics.Metric{Status: metrics.StatusTooFew, Den: n(c.den)}); got != c.want {
			t.Errorf("n %d: %q, want %q", c.den, got, c.want)
		}
	}
	hit, _ := metrics.DefOf("recall.hit_rate")
	if got := StatusText(hit, metrics.Metric{Status: metrics.StatusTooFew, Den: n(20)}); got != "too few" {
		t.Errorf("a rate at its n: %q, want too few", got)
	}
}
