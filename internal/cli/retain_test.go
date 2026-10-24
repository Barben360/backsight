package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/output"
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
