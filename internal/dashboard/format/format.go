package format

import (
	"math"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/metrics"
)

const (
	minus = "\u2212"
	dash  = "—"
)

func bad(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) }

// grouped inserts the thousands separator into digits, from 4 digits.
func (l Locale) grouped(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	sep := l.sepGroup()
	var b strings.Builder
	lead := len(digits) % 3
	b.WriteString(digits[:lead])
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteString(sep)
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// Int writes n with grouped thousands.
func (l Locale) Int(n int64) string {
	if n < 0 {
		// uint64 negation is safe for MinInt64.
		return "-" + l.grouped(strconv.FormatUint(-uint64(n), 10))
	}
	return l.grouped(strconv.FormatInt(n, 10))
}

// Fixed writes v with digits decimals, rounded half away from zero.
func (l Locale) Fixed(v float64, digits int) string {
	if bad(v) {
		return dash
	}
	scale := math.Pow10(digits)
	r := math.Round(math.Abs(v) * scale)
	s := strconv.FormatFloat(r, 'f', 0, 64)
	if len(s) <= digits {
		s = strings.Repeat("0", digits-len(s)+1) + s
	}
	out := l.grouped(s[:len(s)-digits])
	if digits > 0 {
		out += l.sepDecimal() + s[len(s)-digits:]
	}
	if v < 0 && r != 0 {
		out = "-" + out
	}
	return out
}

// pctNumber is a ratio as a whole percent, one decimal under 1 %.
func (l Locale) pctNumber(r float64) string {
	if bad(r) {
		return dash
	}
	if r > 0 && r < 0.0005 {
		return "< 0.1"
	}
	if r > 0 && r < 0.01 {
		return l.Fixed(r*100, 1)
	}
	return l.Fixed(r*100, 0)
}

// Percent writes a ratio as a percent.
func (l Locale) Percent(r float64) string {
	if bad(r) {
		return dash
	}
	return l.pctNumber(r) + " %"
}

// Interval writes a confidence interval of ratios.
func (l Locale) Interval(ci [2]float64) string {
	if bad(ci[0]) || bad(ci[1]) {
		return dash
	}
	return "(" + l.pctNumber(ci[0]) + "–" + l.pctNumber(ci[1]) + " %)"
}

// sizeNumber writes a count of characters or tokens: whole below 10 000,
// then thousands with one decimal (none from 100 k).
func (l Locale) sizeNumber(v float64) string {
	a := math.Abs(v)
	switch {
	case a < 10000 && math.Round(a) < 10000:
		return l.Fixed(v, 0)
	case a < 100000 && math.Round(a/100) < 1000:
		s := l.Fixed(v/1000, 1)
		return strings.TrimSuffix(s, l.sepDecimal()+"0") + " k"
	default:
		return l.Fixed(v/1000, 0) + " k"
	}
}

// Size writes v as chars or tokens ("" unit: the number alone).
func (l Locale) Size(v float64, unit string) string {
	if bad(v) {
		return dash
	}
	s := l.sizeNumber(v)
	if unit == "" {
		return s
	}
	return s + " " + unit
}

// durationParts is ms rounded to the millisecond, as a number and a unit.
func (l Locale) durationParts(ms float64) (string, string) {
	ms = math.Round(ms)
	if math.Abs(ms) < 1000 {
		return l.Fixed(ms, 0), "ms"
	}
	return l.Fixed(ms/1000, 1), "s"
}

// Duration writes ms: whole milliseconds under a second, else seconds with
// one decimal.
func (l Locale) Duration(ms float64) string {
	if bad(ms) {
		return dash
	}
	n, u := l.durationParts(ms)
	return n + " " + u
}

// Money writes v with two decimals and the currency's mark.
func (l Locale) Money(v float64, currency string) string {
	if bad(v) {
		return dash
	}
	num := l.Fixed(math.Abs(v), 2)
	sign := ""
	if v < 0 && num != l.Fixed(0, 2) {
		sign = "-"
	}
	switch currency {
	case "USD":
		return sign + "$" + num
	case "GBP":
		return sign + "£" + num
	case "EUR":
		return sign + num + " €"
	case "":
		return sign + num
	}
	return sign + num + " " + currency
}

// Parts is a value split for the big-number layout.
type Parts struct{ Big, Unit, CI string }

func currencyOf(m metrics.Metric) string {
	c, _ := m.Extra["currency"].(string)
	return c
}

// ValueParts splits a metric's value by its catalog unit.
func (l Locale) ValueParts(d metrics.Def, m metrics.Metric) Parts {
	if m.Value == nil {
		if d.Unit == "money" {
			return Parts{Big: "price not set"}
		}
		return Parts{Big: "—"}
	}
	v := *m.Value
	if bad(v) {
		return Parts{Big: dash}
	}
	switch d.Unit {
	case "ratio":
		p := Parts{Big: l.pctNumber(v), Unit: "%"}
		if m.CI != nil {
			p.CI = l.Interval(*m.CI)
		}
		return p
	case "ms":
		n, u := l.durationParts(v)
		return Parts{Big: n, Unit: u}
	case "chars", "tokens":
		return Parts{Big: l.Size(v, ""), Unit: d.Unit}
	case "money":
		return Parts{Big: l.Money(v, currencyOf(m))}
	}
	return Parts{Big: l.Fixed(v, 0)}
}

// Value writes a metric's value in one line.
func (l Locale) Value(d metrics.Def, m metrics.Metric) string {
	p := l.ValueParts(d, m)
	var out []string
	for _, s := range []string{p.Big, p.Unit, p.CI} {
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

// Delta is a change against the previous window.
type Delta struct{ Arrow, Text, Class string }

func hasTag(m metrics.Metric, tag string) bool {
	for _, t := range m.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Delta writes the change of a metric; false when there is none to show: no
// delta, status no_data or too_few, or a snapshot.
func (l Locale) Delta(d metrics.Def, m metrics.Metric) (Delta, bool) {
	if m.Delta == nil || m.Status == metrics.StatusNoData || m.Status == metrics.StatusTooFew || hasTag(m, "snapshot") {
		return Delta{}, false
	}
	dv := *m.Delta
	if math.IsNaN(dv) || math.IsInf(dv, 0) {
		return Delta{}, false
	}
	var cur float64
	if m.Value != nil {
		cur = *m.Value
	}
	text, zero := l.deltaMagnitude(d, dv, cur, currencyOf(m))
	if zero {
		return Delta{Arrow: "=", Text: "no change"}, true
	}
	out := Delta{Arrow: "▲"}
	sign := "+"
	if dv < 0 {
		out.Arrow, sign = "▼", minus
	}
	out.Text = sign + text
	better := dv > 0
	switch d.Direction {
	case "up":
	case "down":
		better = !better
	default:
		return out, true
	}
	if better {
		out.Class = "better"
	} else {
		out.Class = "worse"
	}
	out.Text += ", " + out.Class
	return out, true
}

// deltaMagnitude writes |dv| in the unit's words; zero when it shows as 0.
func (l Locale) deltaMagnitude(d metrics.Def, dv, cur float64, currency string) (string, bool) {
	a := math.Abs(dv)
	switch d.Unit {
	case "ratio":
		pts := a * 100
		if r := math.Round(pts); r >= 1 {
			if r == 1 {
				return l.Fixed(r, 0) + " pt", false
			}
			return l.Fixed(r, 0) + " pts", false
		}
		if math.Round(pts*10) == 0 {
			return "", true
		}
		return l.Fixed(pts, 1) + " pts", false
	case "ms":
		if math.Round(a) == 0 {
			return "", true
		}
		if math.Abs(cur) >= 1000 && math.Round(a/100) > 0 {
			return l.Fixed(a/1000, 1) + " s", false
		}
		return l.Fixed(a, 0) + " ms", false
	case "chars", "tokens":
		if math.Round(a) == 0 {
			return "", true
		}
		return l.Size(a, d.Unit), false
	case "money":
		if math.Round(a*100) == 0 {
			return "", true
		}
		return l.Money(a, currency), false
	}
	if math.Round(a) == 0 {
		return "", true
	}
	return l.Fixed(a, 0), false
}

// Count writes n with its noun: "1 turn", "61 turns".
func Count(l Locale, n int64, one, many string) string {
	if n == 1 {
		return l.Int(n) + " " + one
	}
	return l.Int(n) + " " + many
}
