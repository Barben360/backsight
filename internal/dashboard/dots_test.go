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
