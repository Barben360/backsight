package dashboard

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Barben360/backsight/internal/metrics"
	"github.com/Barben360/backsight/internal/views"
)

// Pager is the paging links of a list: Prev and Next are paths with their
// offset, "" at either end.
type Pager struct {
	Prev, Next string
	From, To   int // 1-based first and last row shown; 0 for an empty page
	Total      int
}

// pagerOf is the paging of a list shown at path (the request path with its
// query): the links keep every other parameter. A nil page has no links.
func pagerOf(path string, p *metrics.Page) Pager {
	if p == nil {
		return Pager{}
	}
	out := Pager{Total: p.Total}
	if p.Total > p.Offset {
		out.From, out.To = p.Offset+1, min(p.Offset+p.Limit, p.Total)
	}
	base := without(path, "offset")
	at := func(offset int) string {
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		return base + sep + "offset=" + strconv.Itoa(offset)
	}
	if p.Offset > 0 {
		out.Prev = at(max(0, p.Offset-p.Limit))
	}
	if p.Limit > 0 && p.Offset+p.Limit < p.Total {
		out.Next = at(p.Offset + p.Limit)
	}
	return out
}

// filterLinks are the links of a list's filter or order named key at path:
// "All" clears it, one link per value with its words. The paging starts
// over, the other parameters stay.
func filterLinks(path, key string) []Choice {
	var values []string
	var words map[string]string
	switch key {
	case "has":
		values, words = views.HasValues(), views.HasWords
	case "sort":
		values, words = views.SortValues(), views.SortWords
	case "state":
		values, words = views.StateValues(), views.MemoryStateWords
	}
	_, raw, _ := strings.Cut(path, "?")
	q, _ := url.ParseQuery(raw) // a garbled query has no current choice
	cur := q.Get(key)
	base := without(path, key, "offset")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	out := []Choice{{Label: "All", Href: base, Current: cur == ""}}
	if key == "sort" {
		out[0].Label = "Default order"
	}
	for _, v := range values {
		out = append(out, Choice{Label: views.Word(words, v), Href: base + sep + key + "=" + url.QueryEscape(v), Current: cur == v})
	}
	return out
}

// rowMetric is the flagship metric id of a release row as the metric
// partial shows it.
func rowMetric(id string, row map[string]metrics.Metric) (MetricView, error) {
	d, ok := metrics.DefOf(id)
	if !ok {
		return MetricView{}, fmt.Errorf("no metric %q in the catalog", id)
	}
	m, found := row[id]
	return MetricView{ID: id, Def: d, M: m, Found: found}, nil
}

// flagships are the metrics a release row judges, in the table's order.
func flagships() []metrics.Def {
	out := make([]metrics.Def, 0, len(metrics.ReleaseFlagship))
	for _, id := range metrics.ReleaseFlagship {
		if d, ok := metrics.DefOf(id); ok {
			out = append(out, d)
		}
	}
	return out
}

// shares are a release's project shares as lines, the largest first.
func shares(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		name := views.Clip(k)
		if k == metrics.NoProject {
			name = "no project"
		}
		out = append(out, name+" "+views.Percent(m[k])+" %")
	}
	return out
}

// settingLines are the settings in force as "name value" lines, by name.
func settingLines(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+" "+strconv.FormatFloat(m[k], 'f', -1, 64))
	}
	return out
}

// optInt is a count that may be absent.
func optInt(v *int64) string {
	if v == nil {
		return dash
	}
	return views.Grouped(*v)
}

// optPercent is a ratio that may be absent, as a percent.
func optPercent(v *float64) string {
	if v == nil {
		return dash
	}
	return views.Percent(*v) + " %"
}

// weighted is a recent-weighted count, with one decimal.
func weighted(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// signed is a ranking boost with its sign.
func signed(v float64) string { return fmt.Sprintf("%+.2f", v) }

// roundInt is a float that holds a count, grouped.
func roundInt(v float64) string { return views.Grouped(int64(math.Round(v))) }

// words joins the words of the raw names, in order.
func words(m map[string]string, raw []string) string {
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, views.Word(m, r))
	}
	return strings.Join(out, ", ")
}

// rankLines are the rank histogram as lines, the best rank first.
func rankLines(h map[string]int64) []string {
	var out []string
	for _, k := range []string{"1", "2", "3", "4", "5", "6+", "unranked"} {
		if n, ok := h[k]; ok {
			name := "rank " + k
			if k == "unranked" {
				name = "not ranked"
			}
			out = append(out, name+": "+views.Grouped(n))
		}
	}
	return out
}
