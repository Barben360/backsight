package metrics

import (
	"math"
	"math/rand"
	"testing"

	"github.com/Barben360/backsight/internal/stats"
)

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.4f ± %g", what, got, want, tol)
	}
}

func TestWilson(t *testing.T) {
	for _, c := range []struct {
		num, den int64
		lo, hi   float64
	}{
		{12, 33, 0.2219, 0.5338}, // the hit-rate example: 12 of 33 recalls
		{0, 40, 0, 0.0876},       // hi = z²/(n+z²)
		{16, 25, 0.4452, 0.7975},
		{30, 30, 0.8865, 1}, // lo = n/(n+z²)
		{0, 0, 0, 0},
	} {
		lo, hi := Wilson(c.num, c.den)
		near(t, "lo", lo, c.lo, 1e-4)
		near(t, "hi", hi, c.hi, 1e-4)
	}
}

func TestWilsonEnds(t *testing.T) {
	// k = 0 and k = n are exactly 0 and 1, with no rounding residue.
	for _, n := range []int64{1, 2, 7, 33, 1000} {
		if lo, _ := Wilson(0, n); lo != 0 {
			t.Errorf("Wilson(0,%d) lo = %v, want exactly 0", n, lo)
		}
		if _, hi := Wilson(n, n); hi != 1 {
			t.Errorf("Wilson(%[1]d,%[1]d) hi = %v, want exactly 1", n, hi)
		}
	}
	// Symmetry: the interval of k mirrors that of n-k.
	lo, hi := Wilson(12, 33)
	lo2, hi2 := Wilson(21, 33)
	near(t, "mirror lo", lo, 1-hi2, 1e-12)
	near(t, "mirror hi", hi, 1-lo2, 1e-12)
	// A count past the trials is held at the trials.
	lo5, _ := Wilson(5, 5)
	if lo, hi := Wilson(9, 5); lo != lo5 || hi != 1 {
		t.Errorf("Wilson(9,5) = (%v, %v), want Wilson(5,5)", lo, hi)
	}
	// One trial, one success: [0.2065, 1].
	lo, hi = Wilson(1, 1)
	near(t, "1/1 lo", lo, 0.2065, 1e-4)
	if hi != 1 {
		t.Errorf("1/1 hi = %v", hi)
	}
}

func TestMDC(t *testing.T) {
	near(t, "mdc(33)", MDC(33), 0.2413, 1e-4)
	near(t, "mdc(20)", MDC(20), 0.3099, 1e-4)
	near(t, "mdc(100)", MDC(100), 0.1386, 1e-4)
	// The plan's ruling: about 0.24 at n = 33, to two places.
	if got := math.Round(MDC(33)*100) / 100; got != 0.24 {
		t.Errorf("mdc(33) rounds to %v, want 0.24", got)
	}
	// Nothing can be shown without trials, and the value stays JSON-safe.
	for _, n := range []int64{0, -3} {
		if v := MDC(n); v != 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			t.Errorf("MDC(%d) = %v, want 0", n, v)
		}
	}
}

func TestPercentileNearestRank(t *testing.T) {
	hundred := make([]int64, 100)
	for i := range hundred {
		hundred[i] = int64(i + 1)
	}
	if got := Percentile(hundred, 50); got != 50 {
		t.Errorf("p50 of 1..100 = %v, want 50", got)
	}
	if got := Percentile(hundred, 95); got != 95 {
		t.Errorf("p95 of 1..100 = %v, want 95", got)
	}
	if got := Percentile([]int64{42}, 95); got != 42 {
		t.Errorf("one value: %v, want 42", got)
	}
	if got := Percentile(nil, 50); got != 0 {
		t.Errorf("none: %v, want 0", got)
	}
	// Small n by hand: rank = ceil(p·n/100), 1-based, on the sorted values.
	three := []int64{30, 10, 20}
	for p, want := range map[int]float64{1: 10, 33: 10, 34: 20, 50: 20, 67: 30, 95: 30, 100: 30} {
		if got := Percentile(three, p); got != want {
			t.Errorf("p%d of {10,20,30} = %v, want %v", p, got, want)
		}
	}
	two := []int64{20, 10}
	if got := Percentile(two, 50); got != 10 {
		t.Errorf("p50 of {10,20} = %v, want 10", got)
	}
	if got := Percentile(two, 51); got != 20 {
		t.Errorf("p51 of {10,20} = %v, want 20", got)
	}
	// The input is left in order.
	if three[0] != 30 || three[1] != 10 || three[2] != 20 {
		t.Errorf("Percentile reordered its input: %v", three)
	}
}

func TestPercentileMatchesStats(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		n := 1 + r.Intn(200)
		xs := make([]int, n)
		ys := make([]int64, n)
		for j := range xs {
			xs[j] = r.Intn(10000)
			ys[j] = int64(xs[j])
		}
		for _, p := range []int{1, 25, 50, 75, 95, 99, 100} {
			if got, want := Percentile(ys, p), float64(stats.Percentile(xs, p)); got != want {
				t.Fatalf("slice %d (n=%d) p%d = %v, stats.Percentile = %v", i, n, p, got, want)
			}
		}
	}
}
