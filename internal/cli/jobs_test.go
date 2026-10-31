package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/feedback"
	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/session"
	"github.com/Barben360/backsight/internal/state"
	"github.com/Barben360/backsight/internal/testtool"
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
	_ = os.WriteFile(d.MaintenanceLock(), []byte(`{"pid":`+strconv.Itoa(testtool.OtherPID())+`}`), 0o600)
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

// configPredatesConnect makes home a PC that sends to fake A, a session
// settled, a ledger entry queued for sharing (feedback on), and a whole
// connect to fake B happen once the next process loaded its config
// (config.toml rewritten, the sends generation raised). It returns the
// hosts each retain went to, B's host, the session's cursor, the hosts
// each backsight:feedback:* control write went to, and home.
func configPredatesConnect(t *testing.T) (hosts func() []string, newHost, cursor string, shared func() []string, home string) {
	t.Helper()
	home = devInstall(t, true)
	a, b := fakeHindsight(t), fakeHindsight(t)
	pointAt(t, home, a)
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // first tick: retain_since = 08:00
	writeTranscript(t, home, "settled", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	appendConfig(t, home, "[feedback]\nenabled = true\nshare_ledger = true")
	writeShareLedger(t, pathsOf(home), map[string]feedback.Entry{shareM1: {Shown: 1, Credits: 1, CreditsAt: time.Now(), LastShown: time.Now()}})
	var mu sync.Mutex
	var seen, feedbackWrites []string
	fakeHook = func(r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/memories") {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			mu.Lock()
			seen = append(seen, r.Host)
			if strings.Contains(r.URL.Path, "-control/") && strings.Contains(string(body), "backsight:feedback:") {
				feedbackWrites = append(feedbackWrites, r.Host)
			}
			mu.Unlock()
		}
	}
	old := sendsConfigLoaded
	sendsConfigLoaded = func() {
		pointAt(t, home, b)
		if err := state.Update(pathsOf(home), "connect", func(s *state.State) error { s.SendsGeneration++; return nil }); err != nil {
			t.Fatal(err)
		}
		sendsConfigLoaded = old // one connect
	}
	t.Cleanup(func() { sendsConfigLoaded = old })
	return func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(seen)
		}, "127.0.0.1:" + strconv.Itoa(b), filepath.Join(pathsOf(home).CursorsDir(), "settled.json"), func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(feedbackWrites)
		}, home
}

// sentOnlyTo: at least one retain, every one to host.
func sentOnlyTo(hosts []string, host string) bool {
	return len(hosts) > 0 && !slices.ContainsFunc(hosts, func(h string) bool { return h != host })
}

// A tick whose config predates a whole connect (read before connect wrote
// the new server's, the flusher built after connect raised the sends
// generation) sends nothing to either server, shares no ledger counts and
// saves no cursor; the next tick sends the session to the new server.
func TestTickWhoseConfigPredatesConnectSendsNothing(t *testing.T) {
	hosts, newHost, cursor, shared, _ := configPredatesConnect(t)
	code, out, errOut := run(t, "jobs", "tick", "--json")
	if got := shared(); len(got) != 0 {
		t.Fatalf("ledger counts shared to %v\nexit %d\n%s%s", got, code, out, errOut)
	}
	if got := hosts(); len(got) != 0 {
		t.Fatalf("retains sent to %v\nexit %d\n%s%s", got, code, out, errOut)
	}
	if _, err := os.Stat(cursor); err == nil {
		t.Errorf("cursor saved\n%s", out)
	}
	// The session was sendable: the next tick sends it, to B only.
	if code, out, errOut := run(t, "jobs", "tick", "--json"); code != output.OK || !sentOnlyTo(hosts(), newHost) {
		t.Fatalf("next tick: exit %d, retains to %v, want %s\n%s%s", code, hosts(), newHost, out, errOut)
	}
	if _, err := os.Stat(cursor); err != nil {
		t.Errorf("next tick saved no cursor: %v", err)
	}
}

// With recording off, a tick whose config predates a whole connect still
// writes nothing to the old server: no ledger counts shared, no idle
// briefing page pruned.
func TestWriteOffTickWhoseConfigPredatesConnectWritesNothing(t *testing.T) {
	_, _, _, shared, home := configPredatesConnect(t)
	appendConfig(t, home, "[components]\nwrite = false")
	recentHasEventTime.Store(true)
	idle := briefingModelID("old")
	old := time.Now().Add(-20 * 24 * time.Hour).UTC()
	mentalModels[idle] = `{"id":"` + idle + `","content":"x","tags":["project:old"],"last_refreshed_at":"` + old.Format(time.RFC3339Nano) + `"}`
	recentByTag["project:old"] = `[{"id":"b","text":"t","mentioned_at":"` + old.Format(time.RFC3339) + `"}]`
	code, out, errOut := run(t, "jobs", "tick", "--json")
	if got := shared(); len(got) != 0 {
		t.Errorf("ledger counts shared to %v\nexit %d\n%s%s", got, code, out, errOut)
	}
	if _, ok := mentalModels[idle]; !ok {
		t.Errorf("the idle project's page was pruned\nexit %d\n%s%s", code, out, errOut)
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

// A ledger.json that does not parse: the tick keeps it aside, rebuilds
// it from the journal, and its line says so.
func TestTickRebuildsACorruptLedger(t *testing.T) {
	home := devInstall(t, true) // closed port: the rebuild is local
	dir := feedbackJournal(t, home)
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, errOut := run(t, "jobs", "tick")
	if !strings.Contains(out, ", feedback ledger unreadable: kept as ledger.json.corrupt-") || !strings.Contains(out, "rebuilt from the journal") {
		t.Errorf("tick line:\n%s%s", out, errOut)
	}
	if l, err := feedback.LoadLedger(dir); err != nil || l.Counted() != 2 {
		t.Errorf("ledger %+v %v", l, err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "ledger.json.corrupt-*")); len(files) != 1 {
		t.Errorf("corrupt files %v", files)
	}
}

// hideAll journals a hide of each id at at, as the memory_credit tool does.
func hideAll(t *testing.T, dir string, at time.Time, ids ...string) {
	t.Helper()
	j := feedback.NewJournal(dir, feedback.Common{Session: "s1"})
	for _, id := range ids {
		if err := j.Append(feedback.Event{Kind: feedback.KindHide, TS: at, IDs: []string{id}, Verdict: "wrong", Note: "n-" + id}); err != nil {
			t.Fatal(err)
		}
	}
}

// sorted is a sorted copy of s.
func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// historyCalls counts the history reads the fake server answered.
func historyCalls() int {
	n := 0
	for _, c := range bankCalls {
		if strings.HasSuffix(c, "/history") {
			n++
		}
	}
	return n
}

// The tick lifts a hiding the server made moot: an observation rewritten
// after it was hidden, or gone from the server (no history, and no memory
// either). One changed only before, whose history the server fails to
// give, or without history but still readable or not readable, stays
// hidden; once lifted, a hiding is not journaled again. With feedback
// off, nothing is read.
func TestHiddenObservationIsLiftedWhenRewritten(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	dir := pathsOf(home).FeedbackDir()
	hidAt := time.Now().Add(-2 * time.Hour).UTC()
	change := func(at time.Time) string {
		return `[{"previous_text":"old","changed_at":"` + at.Format("2006-01-02T15:04:05.999999-07:00") + `"}]`
	}
	memoryHistory["o-after"] = `[{"previous_text":null,"changed_at":"` + hidAt.Add(-time.Hour).Format(time.RFC3339) + `"},` + change(hidAt.Add(time.Hour))[1:]
	memoryHistory["o-before"] = change(hidAt.Add(-time.Minute))
	memoryHistory["o-error"] = change(hidAt.Add(time.Hour))
	memoryStatus["GET o-error/history"] = http.StatusInternalServerError
	// No history, yet the memory is there, or its read fails: not gone.
	memories["o-present"] = `{"id":"o-present","text":"obs","type":"observation","tags":["project:billing"]}`
	memoryStatus["GET o-flaky"] = http.StatusInternalServerError
	hideAll(t, dir, hidAt, "o-after", "o-before", "o-gone", "o-error", "o-present", "o-flaky")

	tick := func() tickResult {
		t.Helper()
		_, out, errOut := run(t, "jobs", "tick", "--json")
		var env struct {
			Data tickResult `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("not an envelope: %s %s", out, errOut)
		}
		return env.Data
	}
	if res := tick(); res.Unhidden != 2 || historyCalls() != 6 {
		t.Errorf("unhidden %d, %d history reads, want 2 of 6", res.Unhidden, historyCalls())
	}
	if got, want := unhiddenIDs(t, home), "gone:o-gone rewritten:o-after"; strings.Join(sorted(got), " ") != want { // in the order the tick's time reads them
		t.Errorf("unhide events %q, want %q", got, want)
	}
	bankCalls = nil
	if res := tick(); res.Unhidden != 0 || historyCalls() != 4 || len(unhiddenIDs(t, home)) != 2 {
		t.Errorf("second tick: unhidden %d, %d history reads, unhides %q", res.Unhidden, historyCalls(), unhiddenIDs(t, home))
	}
	l, _ := feedback.LoadLedger(dir)
	for id, hidden := range map[string]bool{"o-after": false, "o-gone": false, "o-before": true, "o-error": true, "o-present": true, "o-flaky": true} {
		if (l.Memories[id].Hidden != nil) != hidden {
			t.Errorf("%s: hidden %+v, want %v", id, l.Memories[id].Hidden, hidden)
		}
	}

	// [feedback] enabled = false: no history is read.
	cfg, _ := os.ReadFile(pathsOf(home).ConfigFile())
	_ = os.WriteFile(pathsOf(home).ConfigFile(), append(cfg, "[feedback]\nenabled = false\n"...), 0o600)
	bankCalls = nil
	if res := tick(); res.Unhidden != 0 || historyCalls() != 0 {
		t.Errorf("disabled feedback: unhidden %d, %d history reads", res.Unhidden, historyCalls())
	}
}

// One tick reads at most 20 histories, from a place that moves on by 20
// each tick period: 25 hidings are all read over two ticks. A tick stops
// reading once its 10 s have passed on the clock.
func TestHiddenChecksAreCapped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "feedback")
	start := time.Now().Add(-48 * time.Hour)
	for i := range 25 {
		hideAll(t, dir, start.Add(time.Duration(i)*time.Minute), "o"+strconv.Itoa(i))
	}
	tick := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock, perRead := tick, time.Duration(0)
	old := now
	now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	t.Cleanup(func() { now = old })
	var read []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		read = append(read, strings.TrimSuffix(r.URL.Path[strings.Index(r.URL.Path, "/memories/")+len("/memories/"):], "/history"))
		clock = clock.Add(perRead)
		mu.Unlock()
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	cl, _ := hindsight.New(srv.URL, "k", 5*time.Second)
	if n := checkHidden(context.Background(), cl, "b", dir, tick); n != 0 || len(read) != 20 {
		t.Fatalf("first tick: lifted %d, %d reads, want 0 and 20", n, len(read))
	}
	seen := map[string]bool{}
	for _, id := range read {
		seen[id] = true
	}
	read = nil
	if n := checkHidden(context.Background(), cl, "b", dir, tick.Add(15*time.Minute)); n != 0 || len(read) != 20 {
		t.Fatalf("next tick: lifted %d, %d reads, want 0 and 20", n, len(read))
	}
	for _, id := range read {
		seen[id] = true
	}
	if len(seen) != 25 {
		t.Errorf("two ticks read %d of the 25 hidings", len(seen))
	}

	// Each read takes 4 s on the clock: the third one ends past 10 s.
	read, perRead = nil, 4*time.Second
	if n := checkHidden(context.Background(), cl, "b", dir, tick); n != 0 || len(read) != 3 {
		t.Errorf("out of time: lifted %d, %d reads, want 0 and 3", n, len(read))
	}
}

// noteRetains are the retain events telemetry holds for doc, in order.
func noteRetains(t *testing.T, home, doc string) []telemetryRetain {
	t.Helper()
	var out []telemetryRetain
	for l := range strings.SplitSeq(telemetryText(t, home), "\n") {
		var e telemetryRetain
		if json.Unmarshal([]byte(l), &e) == nil && e.Event == "retain" && e.Document == doc {
			out = append(out, e)
		}
	}
	return out
}

type telemetryRetain struct {
	Event, Document, Status string
	MS                      *int `json:"ms"`
}

// The tick's notes flush logs each note it tried, as a session's flush
// does: a subagent note the hook logged queued ends ok once sent (with its
// send's duration); a note the server refuses ends quarantined; with the
// server down the note that failed is logged retry and stays queued.
func TestTickLogsEachNoteItTried(t *testing.T) {
	sub := retain.NewSubagentNote("claude-memory", "s1", "agent-7", "Explore", "p", "Found the leak.", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	t.Run("sent", func(t *testing.T) {
		home := devInstall(t, true)
		pointAt(t, home, fakeHindsight(t))
		n := newTestNote()
		for _, x := range []retain.Note{sub, n} {
			if _, err := notesOutbox(pathsOf(home)).Append(x); err != nil {
				t.Fatal(err)
			}
		}
		run(t, "jobs", "tick")
		for _, doc := range []string{sub.DocumentID, n.DocumentID} {
			if got := noteRetains(t, home, doc); len(got) != 1 || got[0].Status != "ok" || got[0].MS == nil {
				t.Errorf("%s: retain events %+v, want one ok with its ms", doc, got)
			}
		}
	})
	t.Run("refused", func(t *testing.T) {
		home := devInstall(t, true)
		pointAt(t, home, fakeHindsight(t))
		retainStatus.Store(400)
		t.Cleanup(func() { retainStatus.Store(0) })
		_, _ = notesOutbox(pathsOf(home)).Append(sub)
		run(t, "jobs", "tick")
		if got := noteRetains(t, home, sub.DocumentID); len(got) != 1 || got[0].Status != "quarantined" {
			t.Errorf("retain events %+v, want one quarantined", got)
		}
	})
	t.Run("server down", func(t *testing.T) {
		home := devInstall(t, true) // closed port
		_, _ = notesOutbox(pathsOf(home)).Append(sub)
		run(t, "jobs", "tick")
		if got := noteRetains(t, home, sub.DocumentID); len(got) != 1 || got[0].Status != "retry" {
			t.Errorf("retain events %+v, want one retry", got)
		}
		if n, _ := notesOutbox(pathsOf(home)).Pending(); n != 1 {
			t.Errorf("%d notes queued, want the note kept", n)
		}
	})
}
