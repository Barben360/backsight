package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

func writeTranscript(t *testing.T, home, sid string, at time.Time) {
	t.Helper()
	p := filepath.Join(home, ".claude", "projects", "-home-u-billing", sid+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	line := `{"type":"user","uuid":"` + sid + `-u1","sessionId":"` + sid + `","cwd":"` + filepath.Join(home, "billing") +
		`","timestamp":"` + at.UTC().Format(time.RFC3339) + `","message":{"role":"user","content":"We keep PostgreSQL 16 for billing."}}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func withClock(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}

func TestRetainFlushLeavesHistoryToBackfill(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	writeTranscript(t, home, "old", time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC))
	before := retained.Load()
	code, out, errOut := run(t, "retain", "flush")
	if code != output.OK || !strings.Contains(out, "1 backfill") || retained.Load() != before {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.RetainSince != nil {
		t.Errorf("retain flush must not record retain_since (only jobs tick does): %v", st.RetainSince)
	}
	run(t, "jobs", "tick")
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.RetainSince == nil || !st.RetainSince.Equal(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("retain_since %v", st.RetainSince)
	}
	// Named explicitly, an old session is sent.
	code, out, _ = run(t, "retain", "flush", "--session", "old", "--json")
	var env struct {
		Data flushReport `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || env.Data.Counts["sent"] != 1 || retained.Load() != before+1 {
		t.Errorf("exit %d\n%s", code, out)
	}
	if raw, err := os.ReadFile(filepath.Join(pathsOf(home).TelemetryDir(), "events-"+time.Now().UTC().Format("2006-01")+".jsonl")); err != nil ||
		!strings.Contains(string(raw), `"event":"retain"`) || !strings.Contains(string(raw), `"status":"ok"`) {
		t.Errorf("telemetry: %v %s", err, raw)
	}
}

func TestRetainFlushSendsLiveSessions(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)) // retain_since before the session
	run(t, "jobs", "tick")
	writeTranscript(t, home, "live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	before := retained.Load()
	_, out, _ := run(t, "retain", "flush")
	if !strings.Contains(out, "sent        live (billing, 1 records, segment 1)") || retained.Load() != before+1 {
		t.Errorf("%s", out)
	}
}

func TestRetainFlushServerDownIsPartial(t *testing.T) {
	home := devInstall(t, true) // closed port
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	writeTranscript(t, home, "live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	code, out, _ := run(t, "retain", "flush")
	if code != output.Partial || !strings.Contains(out, "retry") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestRetainFlushUnknownSession(t *testing.T) {
	devInstall(t, true)
	if code, _, _ := run(t, "retain", "flush", "--session", "nope"); code != output.Usage {
		t.Errorf("exit %d", code)
	}
}

// A failing server stops the run after the first session: the others are
// not hammered with the same error.
func TestFlushStopsOnServerFailure(t *testing.T) {
	home := devInstall(t, true) // closed port: unreachable
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	writeTranscript(t, home, "a-live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	writeTranscript(t, home, "b-live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	_, out, _ := run(t, "retain", "flush", "--json")
	var env struct {
		Data flushReport `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if env.Data.Counts["retry"] != 1 || len(env.Data.Results) != 1 {
		t.Errorf("%s", out)
	}
}

func TestRetryQuarantine(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	_ = os.MkdirAll(d.CursorsDir(), 0o700)
	_ = os.WriteFile(filepath.Join(d.CursorsDir(), "s1.json"), []byte(`{"quarantined":true}`), 0o600)
	_ = os.WriteFile(filepath.Join(d.CursorsDir(), "s2.json"), []byte(`{"quarantined":false}`), 0o600)
	code, out, _ := run(t, "retain", "retry-quarantine", "--json")
	if code != output.OK || !strings.Contains(out, `"s1"`) || strings.Contains(out, `"s2"`) {
		t.Errorf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(d.CursorsDir(), "s1.json")); err == nil {
		t.Error("the quarantined cursor must be dropped so the session resyncs")
	}
	if _, err := os.Stat(filepath.Join(d.CursorsDir(), "s2.json")); err != nil {
		t.Error("a healthy cursor was dropped")
	}
}

// A server refusing backsight's credentials needs the user, not a retry
// loop: 20, not 40 (M1 review).
func TestFlushRefusedCredentialsAreBlocked(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	oldSession(t, home, "s1", time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC), false)
	retainStatus.Store(401)
	if code, out, _ := run(t, "retain", "flush", "--session", "s1"); code != output.Blocked {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// Record types Claude Code is not known to write are counted once, when
// the cursor passes them (a format change shows in stats and doctor);
// known ones are not.
func TestFlushCountsUnknownTranscriptRecords(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick")
	writeTranscript(t, home, "live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	p := filepath.Join(home, ".claude", "projects", "-home-u-billing", "live.jsonl")
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.WriteString(`{"type":"brand-new-kind","uuid":"z"}` + "\n" + `{"type":"ai-title","uuid":"y"}` + "\n")
	_ = f.Close()
	withClock(t, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	run(t, "retain", "flush") // sent: the cursor stops after the last turn
	run(t, "retain", "flush") // the trailing records are passed now
	run(t, "retain", "flush") // and never counted again
	tel := telemetryText(t, home)
	if strings.Count(tel, `"event":"transcript_unknown"`) != 1 || !strings.Contains(tel, `"brand-new-kind":1`) || strings.Contains(tel, `"ai-title"`) {
		t.Errorf("telemetry:\n%s", tel)
	}
}

// A scheduled job has no CLAUDE_CONFIG_DIR: the transcripts come from the
// profile client install recorded, not from ~/.claude.
func TestFlushReadsTheWiredProfile(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick")
	profile := filepath.Join(home, ".claude-dev")
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error { s.Client = &state.Client{Profile: profile}; return nil })
	writeTranscript(t, home, "main", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)) // ~/.claude
	p := filepath.Join(profile, "projects", "-home-u-billing", "wired.jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	raw, _ := os.ReadFile(filepath.Join(home, ".claude", "projects", "-home-u-billing", "main.jsonl"))
	_ = os.WriteFile(p, []byte(strings.ReplaceAll(string(raw), "main", "wired")), 0o600)
	withClock(t, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	_, out, _ := run(t, "retain", "flush")
	if !strings.Contains(out, "wired") || strings.Contains(out, "main (") {
		t.Errorf("%s", out)
	}
}

// One event per document a flush wrote, then one for the failed send: the
// failing document, else the session's home document.
func TestRetainEventsNameTheDocumentsWritten(t *testing.T) {
	type ev struct{ doc, status string }
	for _, c := range []struct {
		name string
		r    retain.Result
		want []ev
	}{
		{"two documents", retain.Result{Session: "s1", Status: retain.Sent, Documents: []string{"conversation:s1", "conversation:s1@beta"}},
			[]ev{{"conversation:s1", "ok"}, {"conversation:s1@beta", "ok"}}},
		{"partly sent", retain.Result{Session: "s1", Status: retain.Sent, Documents: []string{"conversation:s1"}, FailedDocument: "conversation:s1@beta"},
			[]ev{{"conversation:s1", "ok"}, {"conversation:s1@beta", "retry"}}},
		{"refused after a send", retain.Result{Session: "s1", Status: retain.Quarantined, Documents: []string{"conversation:s1"}, FailedDocument: "conversation:s1@beta"},
			[]ev{{"conversation:s1", "ok"}, {"conversation:s1@beta", "quarantined"}}},
		{"server position unreadable", retain.Result{Session: "s1", Status: retain.Retry}, []ev{{"conversation:s1", "retry"}}},
		{"nothing to send", retain.Result{Session: "s1", Status: retain.Nothing}, nil},
	} {
		var got []ev
		for _, e := range retainEvents(c.r, 5) {
			got = append(got, ev{e.Document, e.Status})
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
