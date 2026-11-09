package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/state"
)

// sessionRetains are the statuses logged on a session's documents
// (conversation:<sid> and its per-project ones), in order.
func sessionRetains(t *testing.T, home, sid string) []string {
	t.Helper()
	var out []string
	for l := range strings.SplitSeq(telemetryText(t, home), "\n") {
		var e telemetryRetain
		if json.Unmarshal([]byte(l), &e) == nil && e.Event == "retain" &&
			(e.Document == "conversation:"+sid || strings.HasPrefix(e.Document, "conversation:"+sid+"@")) {
			out = append(out, e.Status)
		}
	}
	return out
}

// A flush held before sending (here an extraction hold) logs its retry on a
// session that has something to send, never on one whose cursor reached the
// end of its transcript: that retry would never be superseded, and the
// dashboard would read the sent session as waiting for ever. The run goes
// past the sent session to the one that waits, which gets the retry.
func TestHeldFlushLogsNoRetryForASessionAlreadySent(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // live retain begins
	writeTranscript(t, home, "a-sent", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	if code, out, errOut := run(t, "retain", "flush"); code != 0 {
		t.Fatalf("first flush: exit %d\n%s%s", code, out, errOut)
	}
	if got := sessionRetains(t, home, "a-sent"); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("a-sent logged %v, want [ok]", got)
	}
	hold := state.Extraction{Bank: "claude-memory", HeldSince: time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC), KeyRejected: true}
	if err := state.SaveExtraction(pathsOf(home), hold); err != nil {
		t.Fatal(err)
	}

	t.Run("nothing waits", func(t *testing.T) {
		run(t, "retain", "flush")
		if got := sessionRetains(t, home, "a-sent"); len(got) != 1 || got[0] != "ok" {
			t.Errorf("a-sent logged %v, want [ok]: no retry for a session with nothing to send", got)
		}
	})
	t.Run("a session that waits gets the retry", func(t *testing.T) {
		writeTranscript(t, home, "b-new", time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
		_, out, _ := run(t, "retain", "flush", "--json")
		if got := sessionRetains(t, home, "b-new"); len(got) != 1 || got[0] != "retry" {
			t.Errorf("b-new logged %v, want [retry]", got)
		}
		if got := sessionRetains(t, home, "a-sent"); len(got) != 1 || got[0] != "ok" {
			t.Errorf("a-sent logged %v, want [ok]", got)
		}
		var env struct {
			Data flushReport `json:"data"`
		}
		_ = json.Unmarshal([]byte(out), &env)
		if env.Data.Counts["retry"] != 1 || len(env.Data.Results) != 1 || env.Data.Results[0].Session != "b-new" {
			t.Errorf("report %+v, want one retry, b-new's\n%s", env.Data, out)
		}
	})
}
