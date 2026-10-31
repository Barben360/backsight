package views

import (
	"testing"

	"github.com/Barben360/backsight/internal/metrics"
)

func TestIntervalText(t *testing.T) {
	ratio, _ := metrics.DefOf("recall.hit_rate")
	ms, _ := metrics.DefOf("recall.latency")
	for _, c := range []struct {
		d    metrics.Def
		m    metrics.Metric
		want string
	}{
		{ratio, metrics.Metric{}, ""},
		{ratio, metrics.Metric{CI: &[2]float64{0.123, 0.5}}, "12–50 %"},
		{ms, metrics.Metric{CI: &[2]float64{120, 2500}}, "120 ms – 2.5 s"},
	} {
		if got := IntervalText(c.d, c.m); got != c.want {
			t.Errorf("%s: %q, want %q", c.d.ID, got, c.want)
		}
	}
}

func TestCountOf(t *testing.T) {
	if CountOf(1, "failure", "failures") != "1 failure" || CountOf(1500, "failure", "failures") != "1,500 failures" || CountOf(0, "failure", "failures") != "0 failures" {
		t.Error("CountOf")
	}
}
