package chart_test

import (
	"testing"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/metrics"
)

// The chart package may not import metrics, so it keeps its own interval;
// this pins the copy to the original.
func TestWilsonMatchesMetrics(t *testing.T) {
	for _, c := range [][2]int64{
		{0, 0}, {0, 1}, {1, 1}, {0, 10}, {10, 10}, {1, 2}, {3, 4}, {5, 10}, {2, 7},
		{33, 100}, {1, 1000}, {999, 1000}, {5, 3}, {-2, 5}, {50000, 123456},
	} {
		wl, wh := metrics.Wilson(c[0], c[1])
		gl, gh := chart.Wilson(c[0], c[1])
		if wl != gl || wh != gh {
			t.Errorf("wilson(%d, %d) = (%v, %v), metrics gives (%v, %v)", c[0], c[1], gl, gh, wl, wh)
		}
	}
}
