package speed

import (
	"testing"
	"time"
)

func TestFactorIsCappedBothWays(t *testing.T) {
	for _, c := range []struct {
		ref, baseline time.Duration
		want          float64
	}{
		{30 * time.Millisecond, 60 * time.Millisecond, 1},    // a faster machine never tightens
		{60 * time.Millisecond, 60 * time.Millisecond, 1},    // the baseline itself
		{72 * time.Millisecond, 60 * time.Millisecond, 1.2},  // in between: the ratio
		{180 * time.Millisecond, 60 * time.Millisecond, 1.5}, // a very slow one: the cap
		{60 * time.Millisecond, 0, 1},                        // no baseline: no scaling
	} {
		if got := FactorOf(c.ref, c.baseline); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("FactorOf(%v, %v) = %v, want %v", c.ref, c.baseline, got, c.want)
		}
	}
	if got := Scale(900*time.Millisecond, 1.5); got != 1350*time.Millisecond {
		t.Errorf("Scale(900 ms, 1.5) = %v", got)
	}
}

// The reference runs, is measured once per binary, and is in the range
// the factor reads: not zero, and a second call gives the same Measure.
func TestRunnerIsMeasuredOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("measures the machine")
	}
	m, err := Runner()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("runner speed: %v", m)
	if m.Reference <= 0 || m.Factor < 1 || m.Factor > MaxFactor {
		t.Errorf("measure %+v", m)
	}
	if again, _ := Runner(); again != m {
		t.Errorf("measured again: %+v then %+v", m, again)
	}
}
