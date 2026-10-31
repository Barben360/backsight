package metrics

import (
	"slices"
	"strings"
)

// Observation is what a family measured for one metric over a window, the
// input of Judge.
type Observation struct {
	Absent   bool            // the source holds no row at all, or is unavailable
	Reason   string          // its no_data reason
	Num, Den int64           // rates; Den = n for percentiles and counts
	Value    *float64        // the value an On "value" threshold compares
	P50, P95 *float64        // percentiles
	Err      Status          // error-class rule result (rules.go), "" when not error class
	Baseline []BaselineValue // the 4 previous 7-day windows, newest first (relative metrics)
	Watch    *float64        // a run-time watch threshold (context.per_session); nil: Def's
}

// BaselineValue is one previous 7-day window of a relative metric: its
// point value and its denominator.
type BaselineValue struct {
	Value float64
	Den   int64
}

// Judgement is how a metric reads: its status, the no_data reason, whether a
// rate's point estimate is past an absolute threshold its interval is not,
// and the rate's 95% interval.
type Judgement struct {
	Status     Status
	Reason     string
	Borderline bool
	CI         *[2]float64
}

// Judge reads one metric, in one order for every metric:
//  1. the source is absent: no_data, with the observation's reason;
//  2. the denominator is under min_n: too_few;
//  3. the absolute thresholds: a rate's good-side bound, a percentile's or
//     count's value; a rate whose point is past a threshold its bound is
//     not past is borderline (and keeps the bound's status);
//  4. the error-class rule's result;
//  5. the relative threshold against the median of the qualifying
//     baselines: watch at most, never borderline;
//  6. the worst of 3, 4 and 5; ok when none applies, info for the stale
//     lists.
func Judge(d Def, o Observation) Judgement {
	if o.Absent {
		return Judgement{Status: StatusNoData, Reason: o.Reason}
	}
	var j Judgement
	lo, hi := Wilson(o.Num, o.Den)
	if (d.CI == "wilson" || d.CI == "wilson_approx") && o.Den > 0 {
		j.CI = &[2]float64{lo, hi}
	}
	if o.Den < int64(d.MinN) {
		j.Status = StatusTooFew
		return j
	}
	if strings.HasPrefix(d.ID, "stale.") {
		j.Status = StatusInfo
		return j
	}
	abs, borderline := absolute(d.Absolute, o, lo, hi)
	j.Borderline = borderline
	j.Status = worst(worst(worst(StatusOK, abs), o.Err), relative(d, o, lo, hi))
	return j
}

// absolute is step 3. Rates (On hi or lo) need trials; a percentile or a
// value needs its figure. Comparisons are strict: a bound or a value exactly
// on a threshold is not past it.
func absolute(t Threshold, o Observation, lo, hi float64) (Status, bool) {
	watch := t.Watch
	if o.Watch != nil {
		watch = o.Watch
	}
	levels := []struct {
		at *float64
		s  Status
	}{{watch, StatusWatch}, {t.Bad, StatusBad}}
	status, borderline := Status(""), false
	switch t.On {
	case "hi", "lo":
		if o.Den <= 0 {
			return "", false
		}
		p := float64(max(0, min(o.Num, o.Den))) / float64(o.Den)
		for _, l := range levels {
			if l.at == nil {
				continue
			}
			var boundPast, pointPast bool
			if t.On == "hi" { // higher is better: past means below
				boundPast, pointPast = hi < *l.at, p < *l.at
			} else {
				boundPast, pointPast = lo > *l.at, p > *l.at
			}
			if boundPast {
				status = worst(status, l.s)
			} else if pointPast {
				borderline = true
			}
		}
	case "p95", "value":
		v := o.P95
		if t.On == "value" {
			v = o.Value
		}
		if v == nil {
			return "", false
		}
		for _, l := range levels {
			if l.at != nil && *v > *l.at {
				status = worst(status, l.s)
			}
		}
	}
	return status, borderline
}

// relative is step 5: only for relative metrics with at least two baseline
// windows of n >= min_n, whose point values give the median m. Higher is
// better: watch when the upper bound is under 0.7 × m; lower is better:
// watch when the lower bound is over m / 0.7.
func relative(d Def, o Observation, lo, hi float64) Status {
	if !d.Relative {
		return ""
	}
	var vals []float64
	for _, b := range o.Baseline {
		if b.Den >= int64(d.MinN) {
			vals = append(vals, b.Value)
		}
	}
	if len(vals) < 2 {
		return ""
	}
	slices.Sort(vals)
	m := vals[len(vals)/2]
	if len(vals)%2 == 0 {
		m = (vals[len(vals)/2-1] + m) / 2
	}
	switch d.Direction {
	case "up":
		if hi < 0.7*m {
			return StatusWatch
		}
	case "down":
		if lo > m/0.7 {
			return StatusWatch
		}
	}
	return StatusOK
}

// rank orders statuses for worst; anything but ok, watch and bad (no rule
// applied) ranks under ok.
func rank(s Status) int {
	switch s {
	case StatusOK:
		return 1
	case StatusWatch:
		return 2
	case StatusBad:
		return 3
	}
	return 0
}

// worst is the louder of two statuses: bad > watch > ok.
func worst(a, b Status) Status {
	if rank(b) > rank(a) {
		return b
	}
	return a
}
