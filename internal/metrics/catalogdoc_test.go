package metrics

import (
	"flag"
	"os"
	"slices"
	"strings"
	"testing"
)

var updateDoc = flag.Bool("update", false, "rewrite the catalog table of docs/dashboard.md")

const (
	catalogStart = "<!-- catalog:start -->"
	catalogEnd   = "<!-- catalog:end -->"
	dashboardDoc = "../../docs/dashboard.md"
)

func TestDashboardDocCatalogIsCurrent(t *testing.T) {
	raw, err := os.ReadFile(dashboardDoc)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	i, j := strings.Index(doc, catalogStart), strings.Index(doc, catalogEnd)
	if i < 0 || j < i {
		t.Fatalf("docs/dashboard.md lacks the %s ... %s markers", catalogStart, catalogEnd)
	}
	body := doc[i+len(catalogStart) : j]
	want := "\n" + CatalogMarkdown()
	if *updateDoc {
		if body != want {
			doc = doc[:i+len(catalogStart)] + want + doc[j:]
			if err := os.WriteFile(dashboardDoc, []byte(doc), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	if body != want {
		t.Fatalf("the metrics table in docs/dashboard.md is stale; regenerate it with: go test ./internal/metrics/ -run TestDashboardDocCatalogIsCurrent -update")
	}
	for _, d := range Catalog {
		if n := strings.Count(body, "| `"+d.ID+"` |"); n != 1 {
			t.Errorf("%s appears %d times in the table, want once", d.ID, n)
		}
	}
}

func TestCatalogMarkdownBites(t *testing.T) {
	defs := slices.Clone(Catalog)
	defs[0].Label += " (changed)"
	if !strings.Contains(catalogMarkdown(defs), defs[0].Label) {
		t.Error("the table does not follow the catalog's labels")
	}
	defs = slices.Clone(Catalog)
	defs[0].Definition = "changed definition"
	if !strings.Contains(catalogMarkdown(defs), "changed definition") {
		t.Error("the table does not follow the catalog's definitions")
	}
}

func TestCatalogMarkdownStatusRules(t *testing.T) {
	md := CatalogMarkdown()
	row := func(id string) string {
		for _, l := range strings.Split(md, "\n") {
			if strings.HasPrefix(l, "| `"+id+"` |") {
				return l
			}
		}
		t.Fatalf("no row for %s", id)
		return ""
	}
	for id, want := range map[string]string{
		"recall.hit_rate":     "under 10%",
		"verdict.wrong_rate":  "over 5%, bad over 15%",
		"recall.latency":      "over 2000 ms, bad over 2500 ms",
		"context.per_session": "1.5 times the configured session budget",
		"hooks.failures":      "see What it counts",
	} {
		if r := row(id); !strings.Contains(r, want) {
			t.Errorf("%s: row lacks %q:\n%s", id, want, r)
		}
	}
	if strings.Count(md, "\n") != len(Catalog)+2 {
		t.Errorf("table has %d lines for %d metrics", strings.Count(md, "\n"), len(Catalog))
	}
}

// An error-class metric has no number in the Status rule cell: its rule is
// said in its definition, so every one must say it.
func TestErrorClassDefinitionsStateTheirRule(t *testing.T) {
	n := 0
	for _, d := range Catalog {
		if !d.ErrorClass {
			continue
		}
		n++
		if !strings.Contains(d.Definition, "Watched") && !strings.Contains(d.Definition, "Bad") && !strings.Contains(d.Definition, "bad") {
			t.Errorf("%s: its definition states no rule: %q", d.ID, d.Definition)
		}
	}
	if n == 0 {
		t.Error("no error-class metric")
	}
}

func TestCatalogRowsUseNoJargon(t *testing.T) {
	for _, bad := range []string{"Wilson", "p50", "p95", "percentile", "failed_capped", "breaker"} {
		for _, d := range Catalog {
			for col, cell := range map[string]string{"label": d.Label, "question": d.Question, "definition": d.Definition, "status": statusRule(d)} {
				if strings.Contains(strings.ToLower(cell), strings.ToLower(bad)) {
					t.Errorf("%s: %q in its %s", d.ID, bad, col)
				}
			}
		}
	}
}
