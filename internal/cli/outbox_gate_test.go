package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/retain"
)

// jobs tick's notes flush drops a queued short subagent report (one an
// older binary queued, or retry-quarantine gave another try), with the
// hook's skipped event, and sends a long one.
func TestTickDropsAQueuedShortSubagentReport(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	short := retain.NewSubagentNote("claude-memory", "s1", "a1", "general-purpose", "billing", "I'm waiting for CI", at)
	long := retain.NewSubagentNote("claude-memory", "s1", "a2", "general-purpose", "billing", padTo("Found the leak in pool.go", 600), at)
	ob := retain.NotesOutbox(pathsOf(home))
	for _, n := range []retain.Note{short, long} {
		if _, err := ob.Append(n); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := run(t, "jobs", "tick")
	if slices.Contains(retainedDocIDs, short.DocumentID) || !slices.Contains(retainedDocIDs, long.DocumentID) {
		t.Fatalf("exit %d, sent %v\n%s%s", code, retainedDocIDs, out, errOut)
	}
	if n, _ := ob.Pending(); n != 0 {
		t.Errorf("%d note(s) left in the outbox", n)
	}
	ev := eventsNamed(t, home, "subagent_report_skipped")
	if len(ev) != 1 || ev[0]["length"] != "<50" || ev[0]["agent_type"] != "general-purpose" || ev[0]["at"] != "flush" {
		t.Errorf("skipped events %v", ev)
	}
	if strings.Contains(telemetryText(t, home), "waiting for CI") {
		t.Error("the dropped report's text reached the telemetry")
	}
}
