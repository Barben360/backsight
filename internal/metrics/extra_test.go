package metrics

import (
	"slices"
	"sort"
	"testing"

	"github.com/Barben360/backsight/internal/hindsight"
)

// TestEveryExtraKeyIsDeclared: over the default 7 days, --days 100, --dev,
// --path search and --project alpha, every page view of the full fixture
// carries only the extra keys its metric declares in the catalog, and every
// declared key shows up in one of those runs. Under --project alpha the
// server also fails the reflect operation's LLM stats, the one case where
// the extraction metrics count failed_operations.
func TestEveryExtraKeyIsDeclared(t *testing.T) {
	x := fullFx(t)
	seen := map[string]map[string]bool{}
	for _, c := range []struct {
		name string
		q    Query
	}{
		{"7 days", x.query(7)},
		{"100 days", x.query(100)},
		{"dev", func() Query { q := x.query(7); q.Dev = true; return q }()},
		{"path search", func() Query { q := x.query(7); q.Path = "search"; return q }()},
		{"project alpha", func() Query { q := x.query(7); q.Project = "alpha"; return q }()},
	} {
		if c.name == "project alpha" {
			x.server.fail("LLMStats:reflect", &hindsight.APIError{Kind: hindsight.NotFound})
		}
		for _, v := range pageViews {
			for id, m := range x.run(v, c.q).Metrics {
				d, _ := DefOf(id)
				if seen[id] == nil {
					seen[id] = map[string]bool{}
				}
				for k := range m.Extra {
					if !slices.Contains(d.Extra, k) {
						t.Errorf("%s, %s: %s carries extra %q, not in its catalog keys %v", c.name, v, id, k, d.Extra)
					}
					seen[id][k] = true
				}
			}
		}
	}
	for _, d := range Catalog {
		var missing []string
		for _, k := range d.Extra {
			if !seen[d.ID][k] {
				missing = append(missing, k)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: declared extra %v never appears in any run", d.ID, missing)
		}
	}
}
