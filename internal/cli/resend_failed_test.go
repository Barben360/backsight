package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
)

// failedRetainOp is a retain the fake lists with status at time at.
func failedRetainOp(status, doc string, at time.Time) string {
	ts := at.UTC().Format(time.RFC3339)
	return `{"id":"op-` + status + `","operation_type":"retain","status":"` + status + `","document_id":"` + doc + `","created_at":"` + ts + `","updated_at":"` + ts + `"}`
}

// A session whose retain failed on the server (a rejected key) is sent
// again whole (issue 149): --plan shows it and changes nothing; without
// resend=yes nothing changes; while extraction still fails it refuses;
// answered, a verified backup comes first, its document is deleted and
// the next tick sends it again; a second run finds nothing to do.
func TestResendFailedSendsAFailedSessionAgainWhole(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dk := withBackupDocker(t)
	_ = os.MkdirAll(filepath.Join(home, "billing"), 0o700)
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // first tick: retain_since = 08:00
	writeTranscript(t, home, "settled", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "sent:1") {
		t.Fatalf("first send: exit %d\n%s", code, out)
	}
	old := fakeOperations
	t.Cleanup(func() { fakeOperations = old })
	failedAt := time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC)
	fakeOperations = []string{failedRetainOp("failed", "conversation:settled", failedAt), failedRetainOp("completed", "note:x", failedAt.Add(-time.Minute))}
	withClock(t, time.Date(2026, 10, 24, 10, 30, 0, 0, time.UTC))

	code, out, errOut := run(t, "retain", "resend-failed", "--plan", "--json")
	var env struct {
		Data resendFailedResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); code != output.OK || err != nil || env.Data.Action != "planned" || env.Data.FailedRetains != 1 ||
		len(env.Data.Plan.Resends) != 1 || env.Data.Plan.Resends[0].Session != "settled" || env.Data.Note == "" {
		t.Fatalf("plan: exit %d %v\n%s%s", code, err, out, errOut)
	}
	// still failing: refused, nothing changed
	if code, out, errOut := run(t, "retain", "resend-failed", "--answer", "resend=yes"); code != output.Blocked || !strings.Contains(errOut, "latest extraction failed") || strings.Contains(out, "nothing to send again") {
		t.Errorf("while failing: exit %d\n%s%s", code, out, errOut)
	}
	fakeOperations[1] = failedRetainOp("completed", "note:x", failedAt.Add(10*time.Minute)) // the key fixed
	if code, out, errOut := run(t, "retain", "resend-failed"); code != output.NeedsUser || !strings.Contains(out+errOut, "resend=yes") {
		t.Errorf("unanswered: exit %d\n%s%s", code, out, errOut)
	}
	if len(deletedDocs()) != 0 || len(dk.calls) != 0 {
		t.Fatalf("changed before the answer: deleted %v, docker %q", deletedDocs(), dk.calls)
	}
	code, out, errOut = run(t, "retain", "resend-failed", "--answer", "resend=yes")
	if code != output.OK || !strings.Contains(out, "sent again session settled") || !strings.Contains(out, "verified backup") || len(deletedDocs()) != 1 {
		t.Fatalf("answered: exit %d, deleted %v\n%s%s", code, deletedDocs(), out, errOut)
	}
	before := retained.Load()
	withClock(t, time.Date(2026, 10, 24, 10, 45, 0, 0, time.UTC))
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "sent:1") || retained.Load() != before+1 {
		t.Fatalf("the tick after: exit %d, %d retained\n%s", code, retained.Load()-before, out)
	}
	if code, out, _ := run(t, "retain", "resend-failed", "--answer", "resend=yes"); code != output.OK || !strings.Contains(out, "nothing to send again") || !strings.Contains(out, "1 already sent again") || len(deletedDocs()) != 1 {
		t.Errorf("a second run: exit %d, deleted %v\n%s", code, deletedDocs(), out)
	}
}

func TestResendFailedRefusesBadInput(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	for _, args := range [][]string{{"--answer", "resend=maybe"}, {"--answer", "confirm=yes"}, {"--since", "yesterday"}} {
		if code, out, _ := run(t, append([]string{"retain", "resend-failed"}, args...)...); code != output.Usage {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
}
