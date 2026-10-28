package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
)

func TestJobsTickSendsSettledSessionsAndNotes(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // first tick: retain_since = 08:00
	writeTranscript(t, home, "settled", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	writeTranscript(t, home, "fresh", time.Date(2026, 10, 24, 9, 50, 0, 0, time.UTC))
	_, _ = notesOutbox(pathsOf(home)).Append(newTestNote())
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	before := retained.Load()
	code, out, errOut := run(t, "jobs", "tick")
	if code != output.OK || !strings.Contains(out, "sent:1") || !strings.Contains(out, "waiting:1") || !strings.Contains(out, "notes sent 1") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if retained.Load() != before+2 {
		t.Errorf("want 1 session + 1 note retained, got %d", retained.Load()-before)
	}
	raw, _ := os.ReadFile(filepath.Join(pathsOf(home).TelemetryDir(), "events-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if !strings.Contains(string(raw), `"event":"job"`) || !strings.Contains(string(raw), `"name":"tick"`) {
		t.Errorf("telemetry: %s", raw)
	}
	// the tick recorded the settled session's turn and rebuilt the families
	fam, err := os.ReadFile(pathsOf(home).FamiliesFile())
	if err != nil || !strings.Contains(string(fam), `"turns":1`) || !strings.Contains(string(raw), `"event":"attribution"`) {
		t.Errorf("families %s %v; telemetry %s", fam, err, raw)
	}
}

// A tick that cannot read the bank's restore epoch sends no session (a
// stale cursor would bake a restore's loss in); the next tick does.
func TestJobsTickHoldsSessionsWhileTheEpochIsUnreadable(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick")
	writeTranscript(t, home, "settled", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	epochStatus.Store(500)
	before := retained.Load()
	if code, out, _ := run(t, "jobs", "tick"); code != output.Partial || !strings.Contains(out, "held: ") || retained.Load() != before {
		t.Fatalf("exit %d, %d retained\n%s", code, retained.Load()-before, out)
	}
	epochStatus.Store(0)
	epochDoc = `{"epoch":"2","restored_at":"2026-10-24T09:50:00Z","backup_created_at":"2026-10-24T03:00:00Z"}`
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "sent:1") || !strings.Contains(out, "restore epoch 0 -> 2") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.RestoreEpoch == nil || st.RestoreEpoch.Epoch != 2 {
		t.Errorf("epoch not recorded: %+v", st.RestoreEpoch)
	}
}

func TestJobsTickSkipsWhenAnotherRuns(t *testing.T) {
	home := devInstall(t, true)
	l, err := state.TryAcquire(filepath.Join(pathsOf(home).LocksDir(), "job-tick.lock"), "jobs tick")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "another tick") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestJobsTickSkipsDuringMaintenance(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	_ = os.MkdirAll(filepath.Dir(d.MaintenanceLock()), 0o700)
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":1}`), 0o600)
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "maintenance") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestJobsTickServerDownIsPartial(t *testing.T) {
	home := devInstall(t, true) // closed port
	_, _ = notesOutbox(pathsOf(home)).Append(newTestNote())
	if code, out, _ := run(t, "jobs", "tick"); code != output.Partial || !strings.Contains(out, "queued 1") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// jobs tick deletes the briefing pages of projects quiet for 14 days, and
// never a mental model backsight did not create.
func TestTickPrunesIdleBriefingPages(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recentHasEventTime.Store(true)
	page := func(id, project string) string {
		old := time.Now().Add(-20 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
		return `{"id":"` + id + `","content":"x","tags":["project:` + project + `"],"last_refreshed_at":"` + old + `"}`
	}
	active, idle := briefingModelID("billing"), briefingModelID("old")
	mentalModels[active], mentalModels[idle], mentalModels["mine"] = page(active, "billing"), page(idle, "old"), page("mine", "old")
	day := func(d int) string {
		return time.Now().Add(-time.Duration(d) * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	recentByTag["project:billing"] = `[{"id":"a","text":"t","mentioned_at":"` + day(2) + `"}]`
	recentByTag["project:old"] = `[{"id":"b","text":"t","mentioned_at":"` + day(20) + `"}]`
	run(t, "jobs", "tick")
	if _, ok := mentalModels[idle]; ok {
		t.Error("idle project's page kept")
	}
	if _, ok := mentalModels[active]; !ok {
		t.Error("active project's page deleted")
	}
	if _, ok := mentalModels["mine"]; !ok {
		t.Error("a mental model backsight did not create was deleted")
	}
	// back to the recent-observations briefing: every backsight page goes
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[recall]\nbriefing = \"recent\"\n")...), 0o600)
	run(t, "jobs", "tick")
	if _, ok := mentalModels[active]; ok || mentalModels["mine"] == "" {
		t.Errorf("briefing = recent: %v", mentalModels)
	}
}

// A page refreshed lately stays even if its project looks idle (a project
// back after a break): deleting it would only recreate it next session.
func TestTickKeepsARecentlyRefreshedPage(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recentHasEventTime.Store(true)
	id := briefingModelID("old")
	mentalModels[id] = `{"id":"` + id + `","content":"x","tags":["project:old"],"last_refreshed_at":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + `"}`
	recentByTag["project:old"] = `[{"id":"b","text":"t","mentioned_at":"` + time.Now().Add(-20*24*time.Hour).UTC().Format(time.RFC3339) + `"}]`
	run(t, "jobs", "tick")
	if _, ok := mentalModels[id]; !ok {
		t.Error("a page refreshed an hour ago was pruned")
	}
}

// Paused sessions outlive the 30-day pruning, not the 90-day one.
func TestPausedSessionsArePrunedAfterNinetyDays(t *testing.T) {
	home := devInstall(t, true)
	d := pathsOf(home)
	for id, age := range map[string]int{"recent-pause": 40, "old-pause": 100} {
		_ = sessions(home).Update(id, func(st *session.State) error { st.SetPaused(true, time.Now()); return nil })
		at := time.Now().Add(-time.Duration(age) * 24 * time.Hour)
		_ = os.Chtimes(filepath.Join(d.SessionsDir(), id+".json"), at, at)
	}
	pruneSessions(d)
	if _, err := os.Stat(filepath.Join(d.SessionsDir(), "recent-pause.json")); err != nil {
		t.Error("a 40-day-old pause was pruned")
	}
	if _, err := os.Stat(filepath.Join(d.SessionsDir(), "old-pause.json")); err == nil {
		t.Error("a 100-day-old pause was kept")
	}
	if session.AnyPaused(d.SessionsDir(), 48*time.Hour) {
		t.Error("old paused sessions count as paused now")
	}
}

// With the read side off nothing reads the pages: they all go.
func TestTickPrunesEveryPageWhenReadIsOff(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	id := briefingModelID("billing")
	mentalModels[id] = `{"id":"` + id + `","content":"x","tags":["project:billing"],"last_refreshed_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	cfg := filepath.Join(home, "backsight", "config", "config.toml")
	raw, _ := os.ReadFile(cfg)
	_ = os.WriteFile(cfg, append(raw, []byte("\n[components]\nread = false\n")...), 0o600)
	run(t, "jobs", "tick")
	if _, ok := mentalModels[id]; ok {
		t.Error("page kept with read off")
	}
}

// A page just created (no refresh yet) is kept even if its project looks
// idle: the server is writing it.
func TestTickKeepsANewPage(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recentHasEventTime.Store(true)
	id := briefingModelID("old")
	mentalModels[id] = `{"id":"` + id + `","content":"","tags":["project:old"]}`
	recentByTag["project:old"] = `[{"id":"b","text":"t","mentioned_at":"` + time.Now().Add(-20*24*time.Hour).UTC().Format(time.RFC3339) + `"}]`
	run(t, "jobs", "tick")
	if _, ok := mentalModels[id]; !ok {
		t.Error("a page with no refresh yet was pruned")
	}
}

// The daily job keeps the transverse noise reading of its doctor snapshot.
func TestMaintainRecordsTheNoiseReading(t *testing.T) {
	home := devInstall(t, true)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	d := pathsOf(home)
	_ = os.MkdirAll(d.State, 0o700)
	recordNoise(d, doctor.Report{Checks: []doctor.Check{{ID: "noise-subagent", Measure: map[string]float64{"total": 9}},
		{ID: "noise-transverse", Measure: map[string]float64{"named": 7, "total": 40}}}})
	recordNoise(d, doctor.Report{Checks: []doctor.Check{{ID: "noise-transverse", Summary: "skipped: no server"}}})
	got, err := retain.LoadNoise(d.State)
	if err != nil || len(got) != 1 || got[0].Named != 7 || got[0].Total != 40 || !got[0].At.Equal(time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("history %+v %v", got, err)
	}
}

// On a client whose server another one replaced at the same address, a
// tick sends neither sessions nor notes and records the other server's
// id; once the joined server answers again, both go out.
func TestJobsTickSendsNothingWhileAnotherServerAnswers(t *testing.T) {
	home := devInstall(t, true)
	port := fakeHindsight(t)
	pointAt(t, home, port)
	cfgPath := pathsOf(home).ConfigFile()
	raw, _ := os.ReadFile(cfgPath)
	_ = os.WriteFile(cfgPath, []byte(strings.Replace(string(raw), "[server]\n", "[server]\nmode = \"remote\"\n", 1)), 0o600)
	cl, _ := hindsight.New("http://127.0.0.1:"+strconv.Itoa(port), "k", 5*time.Second)
	other, err := retain.EnsureServerID(context.Background(), cl, "claude-memory")
	if err != nil {
		t.Fatal(err)
	}
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.RemoteServer = &state.RemoteServer{ID: "joined", URL: "http://127.0.0.1:" + strconv.Itoa(port)}
		return nil
	})
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // first tick: retain_since = 08:00
	writeTranscript(t, home, "settled", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	_, _ = notesOutbox(pathsOf(home)).Append(newTestNote())
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	before := retained.Load()
	if code, out, _ := run(t, "jobs", "tick"); code != output.Partial || !strings.Contains(out, "a different server answers at this address") || retained.Load() != before {
		t.Fatalf("exit %d, %d retained\n%s", code, retained.Load()-before, out)
	}
	if n, _ := notesOutbox(pathsOf(home)).Pending(); n != 1 {
		t.Errorf("want the note kept, got %d queued", n)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.RemoteServer == nil || st.RemoteServer.Mismatch != other {
		t.Errorf("mismatch not recorded: %+v", st.RemoteServer)
	}
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error { s.RemoteServer.ID = other; return nil })
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "sent:1") || !strings.Contains(out, "notes sent 1") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if st, _ := state.Load(pathsOf(home).StateFile()); st.RemoteServer.Mismatch != "" {
		t.Errorf("mismatch not cleared: %+v", st.RemoteServer)
	}
}

// recordMismatch makes home a client (server.mode remote) that joined
// server a, whose last check found server b at its address.
func recordMismatch(t *testing.T, home string) {
	t.Helper()
	cfgPath := pathsOf(home).ConfigFile()
	raw, _ := os.ReadFile(cfgPath)
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(raw), "[server]\n", "[server]\nmode = \"remote\"\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	setMismatch(t, home, "b")
}

func setMismatch(t *testing.T, home, got string) {
	t.Helper()
	if err := state.Update(pathsOf(home), "test", func(s *state.State) error {
		s.RemoteServer = &state.RemoteServer{ID: "a", URL: "http://100.64.0.1:8888", Mismatch: got}
		if got != "" {
			s.RemoteServer.MismatchAt = time.Now()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// With recording off, a tick still prunes the briefing pages, but not on
// another server than the joined one.
func TestTickWithWriteOffPrunesNothingOnAnotherServer(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	id := briefingModelID("billing")
	mentalModels[id] = `{"id":"` + id + `","content":"x","tags":["project:billing"],"last_refreshed_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	recordMismatch(t, home)
	appendConfig(t, home, "[components]\nread = false\nwrite = false")
	run(t, "jobs", "tick")
	if _, ok := mentalModels[id]; !ok {
		t.Fatal("a page deleted on another server")
	}
	setMismatch(t, home, "")
	run(t, "jobs", "tick")
	if _, ok := mentalModels[id]; ok {
		t.Error("page kept once the joined server answers")
	}
}

// A tick asks the server once whether it answers and records it, with
// every session still to send (not only the one a stopped flush tried):
// doctor and the session banner read them without a call of their own.
func TestTickRecordsReachAndWaiting(t *testing.T) {
	home := devInstall(t, true) // closed port: the server is down
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // first tick: retain_since = 08:00
	for _, sid := range []string{"s1", "s2", "s3"} {
		writeTranscript(t, home, sid, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	}
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	if code, out, _ := run(t, "jobs", "tick"); code != output.Partial || !strings.Contains(out, "retry:1") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	r := state.LoadReach(pathsOf(home))
	if !r.FailingSince.Equal(time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)) || !r.LastOK.IsZero() || r.SessionsWaiting != 3 {
		t.Fatalf("server down: %+v", r)
	}
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 10, 15, 0, 0, time.UTC))
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK || !strings.Contains(out, "sent:3") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	r = state.LoadReach(pathsOf(home))
	if !r.FailingSince.IsZero() || !r.LastOK.Equal(time.Date(2026, 10, 24, 10, 15, 0, 0, time.UTC)) || r.SessionsWaiting != 0 {
		t.Errorf("server back: %+v", r)
	}
}

// With recording off nothing is sent, and the count of sessions waiting
// is still refreshed, never left from an older tick.
func TestTickWithWriteOffRefreshesWaiting(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // first tick: retain_since = 08:00
	state.RecordWaiting(pathsOf(home), 7)
	appendConfig(t, home, "[components]\nwrite = false")
	writeTranscript(t, home, "s1", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	writeTranscript(t, home, "s2", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick")
	if r := state.LoadReach(pathsOf(home)); r.SessionsWaiting != 2 || !r.LastOK.Equal(time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("write off: %+v", r)
	}
}

// Another server answering at the joined address is not this PC's server
// answering: the tick leaves the last answer as it was.
func TestTickRecordsNoAnswerFromAnotherServer(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	recordMismatch(t, home)
	last := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)
	state.RecordReach(pathsOf(home), true, last)
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick")
	if r := state.LoadReach(pathsOf(home)); !r.LastOK.Equal(last) || !r.FailingSince.IsZero() {
		t.Errorf("another server: %+v", r)
	}
}

// A tick whose flusher was built before connect started this PC's sends
// over (here while the tick asks /health) sends no queued note to the old
// server: the note stays for the next tick.
func TestTickKeepsNotesAfterConnectStartedOver(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // first tick: retain_since = 08:00
	ob := notesOutbox(pathsOf(home))
	_, _ = ob.Append(newTestNote())
	fakeHook = func(r *http.Request) {
		if r.URL.Path == "/health" {
			_ = state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil })
		}
	}
	before := retained.Load()
	code, out, errOut := run(t, "jobs", "tick", "--json")
	var env tickEnvelope
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.Partial || env.Data.Status != "partial" || retained.Load() != before {
		t.Fatalf("exit %d, %d retained\n%s\n%s", code, retained.Load()-before, out, errOut)
	}
	if !strings.Contains(out, "started over for another server") || env.Data.Notes.Kept != 1 {
		t.Errorf("notes %+v\n%s", env.Data.Notes, out)
	}
	if n, _ := ob.Pending(); n != 1 {
		t.Errorf("want the note kept, got %d queued", n)
	}
}

// A tick whose flusher predates connect deletes nothing on the old server
// either: no purged document purged again, no briefing page pruned,
// whether or not a note was queued.
func TestStaleTickDeletesNothingOnTheServer(t *testing.T) {
	for name, note := range map[string]bool{"a queued note": true, "no note": false} {
		t.Run(name, func(t *testing.T) {
			home := devInstall(t, true)
			rs := withRecreated(t, home)
			recentHasEventTime.Store(true)
			idle := briefingModelID("old")
			old := time.Now().Add(-20 * 24 * time.Hour).UTC()
			mentalModels[idle] = `{"id":"` + idle + `","content":"x","tags":["project:old"],"last_refreshed_at":"` + old.Format(time.RFC3339Nano) + `"}`
			recentByTag["project:old"] = `[{"id":"b","text":"t","mentioned_at":"` + old.Format(time.RFC3339) + `"}]`
			if note {
				_, _ = notesOutbox(pathsOf(home)).Append(newTestNote())
			}
			fakeHook = func(r *http.Request) {
				if r.URL.Path == "/health" {
					_ = state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil })
				}
			}
			code, out, errOut := run(t, "jobs", "tick", "--json")
			if exists, deleted := rs.state(); code != output.Partial || !strings.Contains(out, "started over for another server") || !exists || deleted != 0 {
				t.Fatalf("exit %d, s9 exists %v, %d deletes\n%s\n%s", code, exists, deleted, out, errOut)
			}
			if _, ok := mentalModels[idle]; !ok {
				t.Error("the idle project's page was pruned")
			}
		})
	}
}

// Settling is local: a tick settles the journal into the ledger while the
// server does not answer.
func TestTickSettlesWhileTheServerIsDown(t *testing.T) {
	home := devInstall(t, true) // closed port
	d := pathsOf(home)
	j := feedback.NewJournal(d.FeedbackDir(), feedback.Common{Session: "s1"})
	start := time.Now().Add(-3 * time.Hour)
	for _, e := range []feedback.Event{
		{Kind: "turn", Turn: "p1", TS: start},
		{Kind: "shows", Turn: "p1", Path: "prompt", TS: start.Add(time.Minute), Items: []feedback.Item{{ID: "m1", Rank: 1}, {ID: "m2", Rank: 2}}},
		{Kind: "credit", Turn: "p1", Verdict: "used", IDs: []string{"m1"}, TS: start.Add(2 * time.Minute)},
		{Kind: "stop", Turn: "p1", Outcome: "declared", TS: start.Add(3 * time.Minute)},
	} {
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	_, out, errOut := run(t, "jobs", "tick", "--json")
	var env struct {
		Data tickResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not an envelope: %s %s", out, errOut)
	}
	if f := env.Data.Feedback; f.Shows != 2 || f.Credits != 1 || f.Misses != 1 || f.Unknown != 0 {
		t.Errorf("feedback %+v\n%s", f, out)
	}
	raw, _ := os.ReadFile(filepath.Join(d.TelemetryDir(), "events-"+time.Now().UTC().Format("2006-01")+".jsonl"))
	if !strings.Contains(string(raw), `"event":"feedback_settle"`) || !strings.Contains(string(raw), `"shows":2`) {
		t.Errorf("telemetry: %s", raw)
	}
	l, err := feedback.LoadLedger(d.FeedbackDir())
	if err != nil || l.Memories["m1"].Credits != 1 || l.Memories["m2"].Misses != 1 {
		t.Errorf("ledger %+v %v", l, err)
	}
	// [feedback] enabled = false: the tick leaves the journal alone.
	home = devInstall(t, true)
	d = pathsOf(home)
	cfg, _ := os.ReadFile(d.ConfigFile())
	_ = os.WriteFile(d.ConfigFile(), append(cfg, "[feedback]\nenabled = false\n"...), 0o600)
	j = feedback.NewJournal(d.FeedbackDir(), feedback.Common{Session: "s1"})
	_ = j.Append(feedback.Event{Kind: "shows", Turn: "p1", Path: "prompt", TS: start, Items: []feedback.Item{{ID: "m1", Rank: 1}}})
	_, out, _ = run(t, "jobs", "tick", "--json")
	if strings.Contains(out, `"shows": 1`) {
		t.Errorf("disabled feedback settled: %s", out)
	}
	if _, err := os.Stat(filepath.Join(d.FeedbackDir(), "ledger.json")); err == nil {
		t.Error("disabled feedback wrote a ledger")
	}
}
