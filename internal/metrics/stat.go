package metrics

import (
	"math"
	"slices"
)

// z is the normal quantile of a two-sided 95% interval.
const z = 1.96

// Wilson is the 95% Wilson score interval of num successes in den trials.
// With no trials it is (0, 0); the bounds are exact 0 at num = 0 and exact 1
// at num = den (no rounding residue). num is held within [0, den].
func Wilson(num, den int64) (lo, hi float64) {
	if den <= 0 {
		return 0, 0
	}
	num = max(0, min(num, den))
	n := float64(den)
	p := float64(num) / n
	d := 1 + z*z/n
	centre := (p + z*z/(2*n)) / d
	half := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n)) / d
	lo, hi = max(0, centre-half), min(1, centre+half)
	if num == 0 {
		lo = 0
	}
	if num == den {
		hi = 1
	}
	return lo, hi
}

// MDC is the smallest change between two windows of den trials each that
// the data can show: 1.96·√(0.5/den), the two-window difference at the
// worst-case p (a 0.24 change at n = 33). 0 for den <= 0 (nothing can be
// shown; callers leave the field null below their minimum n).
func MDC(den int64) float64 {
	if den <= 0 {
		return 0
	}
	return z * math.Sqrt(0.5/float64(den))
}

// Percentile is the nearest-rank percentile p (1..100) of xs, the rule of
// internal/stats.Percentile; 0 for none.
func Percentile(xs []int64, p int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := (p*len(s)+99)/100 - 1
	return float64(s[max(0, min(i, len(s)-1))])
}
