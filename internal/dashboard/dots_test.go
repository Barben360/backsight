package dashboard

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/metrics"
)

// Every link built from data refuses "", "." and ".." (a link to "/memories/.."
// reads as "/"). Red on the raw url.PathEscape links of the Overview, Quality
// and Bank pages and of the Bank's project bars.
func TestDotSegmentsNeverLink(t *testing.T) {
	bad := func(t *testing.T, n *node, page string) {
		t.Helper()
		for _, a := range n.find("a") {
			h := a.attr("href")
			for _, p := range []string{"/memories/", "/projects/", "/sessions/"} {
				if rest, ok := strings.CutPrefix(h, p); ok {
					rest, _, _ = strings.Cut(rest, "?")
					if rest == "" || rest == "." || rest == ".." {
						t.Errorf("%s: link %q on a dot segment or empty id", page, h)
					}
				}
			}
		}
	}
	for _, v := range []string{"..", "."} {
		n := renderOverviewWith(t, v)
		bad(t, n, "overview")
		if !strings.Contains(n.text(), v) {
			t.Errorf("overview: %q is not shown as text", v)
		}

		n = pageNode(t, "quality", false, func(d *PageData) {
			d.V["quality"].Tables.Reported = []metrics.ReportedRow{{MemoryID: v, Verdict: "wrong", Session: v, Project: v, At: d.V["quality"].GeneratedAt}}
		})
		bad(t, n, "quality")
		if !strings.Contains(cardOf(t, n, "w-reported").text(), v) {
			t.Errorf("quality: %q is not shown as text", v)
		}

		n = pageNode(t, "bank", false, func(d *PageData) {
			r := d.V["bank"]
			richBank(r)
			r.Tables.ByProject = append(r.Tables.ByProject, metrics.ProjectCount{Project: v, Memories: 3})
			r.Tables.Review.NeverUsed = append(r.Tables.Review.NeverUsed, metrics.ReviewRow{ID: v, Project: v, Shown: 1})
		})
		bad(t, n, "bank")
	}
}

func renderOverviewWith(t *testing.T, v string) *node {
	t.Helper()
	r := scenarioNamed(t, "healthy")
	r.Tables.Lately = []metrics.SessionRow{{ID: v, Project: v}}
	return renderOverview(t, r)
}

var titleWithControl = regexp.MustCompile(`title="[^"]*\x01`)

// A title from data is plain text: control characters are gone (red on the
// raw {{.Project}} titles, which carried them into the page).
func TestTitlesFromDataArePlain(t *testing.T) {
	const raw = "p\x01q"
	pages := map[string]string{
		"quality": render(t, "quality", func(d *PageData) {
			d.V["quality"].Tables.Reported = []metrics.ReportedRow{{MemoryID: raw, Verdict: "wrong", Session: raw, Project: raw, At: d.V["quality"].GeneratedAt}}
		}),
		"overview": render(t, "overview", func(d *PageData) {
			d.V["overview"].Tables.Lately = []metrics.SessionRow{{ID: raw, Project: raw}}
			d.Frame.ProjectInput = raw
		}),
	}
	for name, html := range pages {
		if titleWithControl.MatchString(html) {
			t.Errorf("%s: a control character reached a title", name)
		}
		if !strings.Contains(html, "pq") {
			t.Errorf("%s: the value is not shown at all", name)
		}
	}
}

// A metric that says a state now, not events over a window, does not claim
// a window when it has no reading (red on "Nothing recorded in the last
// 30 days." for each).
func TestSnapshotMetricsDoNotNameAWindow(t *testing.T) {
	for _, c := range []struct{ page, card string }{
		{"bank", "w-bank-growth"}, {"pipeline", "w-local-backlog"}, {"system", "w-server-reach"},
	} {
		id := strings.TrimPrefix(c.card, "w-")
		id = strings.Replace(id, "-", ".", 1)
		n := pageNode(t, c.page, false, func(d *PageData) {
			for _, r := range d.V {
				if m, ok := r.Metrics[id]; ok {
					m.Status, m.Reason, m.Value, m.Num, m.Den, m.Extra = metrics.StatusNoData, metrics.ReasonNoEvents, nil, nil, nil, nil
					r.Metrics[id] = m
				}
			}
		})
		e := cardOf(t, n, c.card).first("div.empty")
		if e == nil || !strings.Contains(e.text(), "No reading yet.") || strings.Contains(e.text(), "last 30 days") || strings.Contains(e.text(), "Pick a longer window") {
			t.Errorf("%s: %v", c.card, e)
		}
	}
	// A windowed metric keeps its sentence.
	n := pageNode(t, "bank", false, nil)
	if e := cardOf(t, n, "w-notes-stored").first("div.empty"); e == nil || !strings.Contains(e.text(), "Nothing recorded in the last 30 days.") {
		t.Errorf("windowed no-data: %v", e)
	}
	// The set only names metrics the catalog has.
	for id := range snapshotMetric {
		if _, ok := metrics.DefOf(id); !ok {
			t.Errorf("snapshotMetric names %q, not in the catalog", id)
		}
	}
}
