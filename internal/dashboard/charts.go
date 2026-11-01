package dashboard

import (
	"html/template"
	"math"

	"github.com/Barben360/backsight/internal/dashboard/chart"
	"github.com/Barben360/backsight/internal/dashboard/format"
)

// sparkOf is the small trend line of metric v over its daily series, in the
// metric's own unit; label names it for a reader who cannot see it.
func sparkOf(v MetricView, label string, l format.Locale) template.HTML {
	// The window's first day is cut short (Partial) and would draw as a dip:
	// it is left out, so a 7-day window draws its 7 days, today last.
	series := v.M.Series
	if len(series) > 0 && series[0].Partial {
		series = series[1:]
	}
	y := make([]*float64, len(series))
	for i, p := range series {
		y[i] = p.Value
	}
	return chart.Spark{Label: label, Y: y, Fmt: unitFormat(v.Def.Unit, l)}.HTML()
}

// unitFormat writes a value of a catalog unit the way the pages write it.
func unitFormat(unit string, l format.Locale) chart.Num {
	switch unit {
	case "ratio":
		return l.Percent
	case "ms":
		return l.Duration
	case "chars", "tokens":
		return func(x float64) string { return l.Size(x, unit) }
	}
	return func(x float64) string { return l.Int(int64(math.Round(x))) }
}
