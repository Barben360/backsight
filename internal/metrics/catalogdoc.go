package metrics

import (
	"fmt"
	"strings"
)

// CatalogMarkdown is the metrics table of docs/dashboard.md, generated from
// Catalog: one row per metric, in catalog order.
func CatalogMarkdown() string { return catalogMarkdown(Catalog) }

func catalogMarkdown(defs []Def) string {
	var b strings.Builder
	b.WriteString("| Id | Label | What it counts | Unit | Status rule | Minimum n | Scope | Developer |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, d := range defs {
		dev := ""
		if d.Dev {
			dev = "yes (`--dev`)"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %d | %s | %s |\n",
			d.ID, cell(d.Label), cell(d.Definition), cell(unitWords(d.Unit)), cell(statusRule(d)), d.MinN, scopeWords(d.Scope), dev)
	}
	return b.String()
}

// cell keeps a value inside one table cell.
func cell(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ").Replace(s)
}

func unitWords(u string) string {
	switch u {
	case "ratio":
		return "share"
	case "ms":
		return "milliseconds"
	}
	return u
}

func scopeWords(s string) string {
	if s == "server" {
		return "server (all PCs)"
	}
	return "this PC"
}

// amount writes a threshold in its unit.
func amount(v float64, unit string) string {
	switch unit {
	case "ratio":
		return fmt.Sprintf("%g%%", v*100)
	case "ms":
		if v >= 60000 {
			return fmt.Sprintf("%g min", v/60000)
		}
		return fmt.Sprintf("%g ms", v)
	}
	return fmt.Sprintf("%g %s", v, unit)
}

// statusRule says in words how a metric is judged: its absolute rule, its
// comparison with the weeks before, its error rule.
func statusRule(d Def) string {
	var parts []string
	t := d.Absolute
	switch t.On {
	case "hi":
		if t.Watch != nil {
			parts = append(parts, "watch when even the upper end of the interval is under "+amount(*t.Watch, d.Unit))
		}
	case "lo", "p95", "value":
		what := "the lower end of the interval"
		switch t.On {
		case "p95":
			what = "the 95th percentile"
		case "value":
			what = "the value"
		}
		switch {
		case t.Watch != nil && t.Bad != nil:
			parts = append(parts, fmt.Sprintf("watch when %s is over %s, bad over %s", what, amount(*t.Watch, d.Unit), amount(*t.Bad, d.Unit)))
		case t.Watch != nil:
			parts = append(parts, fmt.Sprintf("watch when %s is over %s", what, amount(*t.Watch, d.Unit)))
		case t.On == "p95":
			parts = append(parts, "watch when the 95th percentile is over 1.5 times the configured session budget")
		}
	}
	if d.Relative {
		if d.Direction == "down" {
			parts = append(parts, "watch when clearly above its usual level (the median of the four previous weeks, once two of them have enough data)")
		} else {
			parts = append(parts, "watch when clearly below 70% of its usual level (the median of the four previous weeks, once two of them have enough data)")
		}
	}
	if d.ErrorClass {
		parts = append(parts, "error rule, given in what it counts")
	}
	if len(parts) == 0 {
		return "shown, not judged"
	}
	return strings.Join(parts, "; ")
}
