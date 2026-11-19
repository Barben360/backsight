package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/dashboard/chart"
)

// Past chart.MaxTableRows days outside the server's reach, the table under a record chart shows the latest
// ones and names where every day is, like every other long table: the page's command with --json.
func TestRecordTableNamesEveryDaysSource(t *testing.T) {
	start := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)
	pts := recordDays(start, 300, 280, 10, 12, func(int) map[string]int64 { return map[string]int64{"world": 1} })
	const cmd = "backsight stats bank --days 300 --json"
	doc := parseHTML(t, `<div>`+string(recordBlock(pts, "2025-10-01", true, cmd))+`</div>`)
	if n := len(rowsOf(tableCaptioned(t, doc, "Days that do not come from the server"))); n != chart.MaxTableRows {
		t.Fatalf("%d rows shown, want %d", n, chart.MaxTableRows)
	}
	more := doc.first("p.tbl-more")
	if more == nil {
		t.Fatal("no line under the cut table")
	}
	if !strings.HasPrefix(more.text(), "Showing the latest 120 of 280 days. Every day:") {
		t.Errorf("line under the table: %q", more.text())
	}
	if c := more.first("code"); c == nil || c.text() != cmd {
		t.Errorf("the line names no command: %q", more.text())
	}
	// A table that holds every day says nothing more.
	short := parseHTML(t, `<div>`+string(recordBlock(pts[250:], "2025-10-01", true, cmd))+`</div>`)
	if p := short.first("p.tbl-more"); p != nil {
		t.Errorf("a whole table says %q", p.text())
	}
}
