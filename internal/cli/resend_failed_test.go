package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
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

// Retains still queued on the server: nothing can be sent again yet, but
// --plan still lists the sessions and what it would leave alone, and
// exits saying why it cannot go on; a run that would send refuses with
// nothing changed.
func TestResendFailedPlansWhileRetainsAreQueued(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withBackupDocker(t)
	_ = os.MkdirAll(filepath.Join(home, "billing"), 0o700)
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick")
	writeTranscript(t, home, "settled", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "sent:1") {
		t.Fatalf("first send: exit %d\n%s", code, out)
	}
	old := fakeOperations
	t.Cleanup(func() { fakeOperations = old })
	failedAt := time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC)
	fakeOperations = []string{failedRetainOp("failed", "conversation:settled", failedAt), failedRetainOp("completed", "note:x", failedAt.Add(10*time.Minute))}
	withClock(t, time.Date(2026, 10, 24, 10, 30, 0, 0, time.UTC))
	processing.Store(4)

	code, out, errOut := run(t, "retain", "resend-failed", "--plan")
	if code != output.Blocked || !strings.Contains(out, "send again session settled") || !strings.Contains(errOut, "4 operation(s) still queued") {
		t.Errorf("plan while queued: exit %d\n%s%s", code, out, errOut)
	}
	code, out, _ = run(t, "retain", "resend-failed", "--plan", "--json")
	var env struct {
		Data  resendFailedResult `json:"data"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); code != output.Blocked || err != nil || env.Data.Action != "planned" || len(env.Data.Plan.Resends) != 1 || !strings.Contains(env.Error.Message, "still queued") {
		t.Errorf("plan while queued, json: exit %d %v\n%s", code, err, out)
	}
	if code, out, errOut := run(t, "retain", "resend-failed", "--answer", "resend=yes"); code != output.Blocked || !strings.Contains(errOut, "nothing was changed") || len(deletedDocs()) != 0 {
		t.Errorf("a run while queued: exit %d, deleted %v\n%s%s", code, deletedDocs(), out, errOut)
	}
}

// The text says how many records of a session come from a removed
// directory, and a reason is printed without control characters.
func TestResendFailedTextSaysWhatComesFromARemovedDirectory(t *testing.T) {
	r := resendFailedResult{Action: "planned", Plan: retain.FailedPlan{
		Resends: []retain.Resend{{Session: "s1", Records: 5, Gone: 2, Projects: []string{"billing"}}},
		Held:    []retain.HeldSession{{Session: "s2", Reason: retain.HeldGone + ": /w/fix\x1b[2J (no record of the project its turns were filed under)"}},
	}}
	text := resendFailedText(r)
	if !strings.Contains(text, "2 of them from removed directories") {
		t.Errorf("no word of the removed directory's records:\n%s", text)
	}
	if strings.ContainsRune(text, '\x1b') || !strings.Contains(text, "left alone: session s2 (directory gone: /w/fix") {
		t.Errorf("reason:\n%q", text)
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
