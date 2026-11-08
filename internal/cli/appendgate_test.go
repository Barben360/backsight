package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/testtool"
)

// addTurn appends one user turn at at to the session writeTranscript wrote.
func addTurn(t *testing.T, home, sid, uuid string, at time.Time) {
	t.Helper()
	p := filepath.Join(home, ".claude", "projects", "-home-u-billing", sid+".jsonl")
	line := `{"type":"user","uuid":"` + uuid + `","sessionId":"` + sid + `","cwd":"` + testtool.JSONPath(filepath.Join(home, "billing")) +
		`","timestamp":"` + at.UTC().Format(time.RFC3339) + `","message":{"role":"user","content":"And the replica stays on 15."}}` + "\n"
	fh, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fh.WriteString(line)
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// jobs tick holds a small append back at the defaults (decision 0083 C1),
// retain waiting says why, and retain.append_min_chars = 0 lets it go.
func TestTickHoldsASmallAppendUntilTheGateOpens(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // retain_since = 08:00
	writeTranscript(t, home, "s1", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || retained.Load() != 1 {
		t.Fatalf("first send: exit %d, %d retained\n%s", code, retained.Load(), out)
	}
	addTurn(t, home, "s1", "s1-u2", time.Date(2026, 10, 24, 10, 5, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)) // settled, last send 1 h ago
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || retained.Load() != 1 {
		t.Fatalf("held append: exit %d, %d retained\n%s", code, retained.Load(), out)
	}
	_, text, _ := run(t, "retain", "waiting")
	if !strings.Contains(text, "s1") || !strings.Contains(text, "waiting for 30,000 characters or 6 h") {
		t.Errorf("retain waiting does not name the gate:\n%s", text)
	}
	appendConfig(t, home, "[retain]\nappend_min_chars = 0")
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || retained.Load() != 2 {
		t.Fatalf("gate off: exit %d, %d retained\n%s", code, retained.Load(), out)
	}
}

func TestWaitingTextNamesTheAppendGate(t *testing.T) {
	got := waitingText(waitingReport{Sessions: []retain.Waiter{{Session: "s1", Reason: retain.WaitAppend, Idle: 50 * time.Minute,
		Detail: "waiting for 30,000 characters or 6 h (1,234 pending, last sent 1h0m0s ago)"}}})
	for _, want := range []string{"s1", "waiting for 30,000 characters or 6 h (1,234 pending", "when the session ends"} {
		if !strings.Contains(got, want) {
			t.Errorf("text lacks %q:\n%s", want, got)
		}
	}
}
