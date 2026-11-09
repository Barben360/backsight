package metrics

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/Barben360/backsight/docs"
)

// Agents read a no_data reason from the JSON: every reason a metric can
// carry is listed, in backticks, in the "No data" entry of the dashboard
// guide's "Reading the numbers".
func TestEveryNoDataReasonIsDocumented(t *testing.T) {
	raw, err := fs.ReadFile(docs.FS, "dashboard.md")
	if err != nil {
		t.Fatal(err)
	}
	var entry string
	for l := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(l, "- **No data.**") {
			entry = l
		}
	}
	if entry == "" {
		t.Fatal(`no "No data" entry in dashboard.md`)
	}
	for _, r := range []string{ReasonNoEvents, ReasonFeedbackOff, ReasonServerOnlyHost, ReasonServerDown, ReasonNotConfigured,
		ReasonServerUnsupported, ReasonRecordCatchingUp, ReasonNoRecord, ReasonNoCompleteWeek} {
		if !strings.Contains(entry, "`"+r+"`") {
			t.Errorf("reason %s not in the guide's No data entry", r)
		}
	}
}
