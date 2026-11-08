package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// padTo pads s with words up to n characters.
func padTo(s string, n int) string {
	for len(s) < n {
		s += " detail"
	}
	return s[:n]
}

// queuedDocs are the document ids of the notes outbox.
func queuedDocs(t *testing.T, home string) string {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(pathsOf(home).OutboxDir(), "notes.jsonl"))
	return string(raw)
}

func TestSubagentStopQueuesOnlyReportsThatPassTheGate(t *testing.T) {
	cases := []struct {
		name, report, config string
		queued               bool
		bucket               string // of the skipped event, when not queued
	}{
		{name: "a 47-character status report", report: "I'm waiting for CI to finish on the open branch", bucket: "<50"},
		{name: "120 characters with a pull request URL", report: padTo("Opened https://github.com/o/r/pull/12 with the fix", 120), queued: true},
		{name: "120 characters with an issue number", report: padTo("Filed #214 for the pool leak", 120), queued: true},
		{name: "600 characters", report: padTo("Found the leak in pool.go", 600), queued: true},
		{name: "a 300-character report", report: padTo("Read the pool code", 300), bucket: "150-399"},
		{name: "a lower threshold from the config", report: padTo("Read the pool code", 120), config: "[hooks]\nsubagent_report_min_chars = 100", queued: true},
		{name: "a higher threshold from the config", report: padTo("Found the leak in pool.go", 600), config: "[hooks]\nsubagent_report_min_chars = 1000", bucket: "400-999"},
		{name: "a threshold of 0 sends every report", report: "Done.", config: "[hooks]\nsubagent_report_min_chars = 0", queued: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := hookHome(t)
			if c.config != "" {
				appendConfig(t, home, c.config)
			}
			fire(t, "subagent-stop", map[string]any{"session_id": "s1", "agent_id": "a1", "agent_type": "general-purpose", "cwd": filepath.Join(home, "billing"),
				"transcript_path": subagentTranscript(t, "s1", "a1"), "last_assistant_message": c.report})
			got := strings.Contains(queuedDocs(t, home), `"document_id":"subagent:s1:a1"`)
			if got != c.queued {
				t.Fatalf("queued %v, want %v; outbox:\n%.300s", got, c.queued, queuedDocs(t, home))
			}
			skipped := eventsNamed(t, home, "subagent_report_skipped")
			switch {
			case c.queued && len(skipped) != 0:
				t.Errorf("a queued report counted as skipped: %v", skipped)
			case !c.queued && (len(skipped) != 1 || skipped[0]["length"] != c.bucket || skipped[0]["agent_type"] != "general-purpose"):
				t.Errorf("skipped events %v, want one with length %q", skipped, c.bucket)
			}
			if !c.queued && strings.Contains(telemetryText(t, home), c.report) {
				t.Error("the skipped report's text reached the telemetry")
			}
		})
	}
}
