package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hindsight"
	"github.com/Barben360/backsight/internal/livecheck"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
	"github.com/Barben360/backsight/internal/retain"
	"github.com/Barben360/backsight/internal/state"
)

// asShim makes verify run this test binary as the shim: TestMain turns it
// into backsight (hooks through the real supervisor, the real MCP server).
func asShim(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	withShim(t, exe)
	verifyEnv = []string{"BACKSIGHT_TEST_HOOK_CHILD=1"}
	t.Cleanup(func() { verifyEnv = nil })
}

func withShim(t *testing.T, path string) {
	t.Helper()
	old := verifyShim
	verifyShim = func(paths.Dirs) string { return path }
	t.Cleanup(func() { verifyShim = old })
}

func fastVerify(t *testing.T) {
	t.Helper()
	oldPoll, oldWait := verifyPoll, verifyWait
	verifyPoll, verifyWait = time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { verifyPoll, verifyWait = oldPoll, oldWait })
}

func parseVerify(t *testing.T, out string) map[string]verifyCheck {
	t.Helper()
	var env struct {
		Data verifyResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m := map[string]verifyCheck{}
	for _, c := range env.Data.Checks {
		m[c.ID] = c
	}
	return m
}

// scratchBankDeleted is the scratch bank verify deleted, "" when none.
func scratchBankDeleted() (deleted string) {
	for _, c := range curation {
		if strings.HasPrefix(c, "DELETE /v1/default/banks/backsight-verify-") && !strings.Contains(c, "/documents/") {
			deleted = strings.Fields(c)[1]
		}
	}
	return deleted
}

func TestVerifyRunsEveryHookTheMCPServerAndARoundTrip(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	asShim(t)
	fastVerify(t)
	documentUnits[verifyDoc] = 2
	recallAnswer = `[{"id":"m1","text":"The user chose a verification codeword.","type":"world","scores":{"reranker":0.9}}]`

	code, out, errOut := run(t, "verify", "--json")
	checks := parseVerify(t, out)
	if code != output.OK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, h := range plugin.Hooks() {
		if c := checks["hook "+h.Arg]; c.Status != "ok" {
			t.Errorf("hook %s: %+v", h.Arg, c)
		}
	}
	if checks["mcp"].Status != "ok" || !strings.Contains(checks["mcp"].Summary, "and 3 tools") {
		t.Errorf("mcp: %+v", checks["mcp"])
	}
	if checks["round_trip"].Status != "ok" {
		t.Errorf("round trip: %+v", checks["round_trip"])
	}
	// Every bank call of the round trip goes to one scratch bank, created
	// then deleted; the configured bank is never written.
	var scratch string
	for _, c := range bankCalls {
		f := strings.Fields(c)
		if f[0] == "PUT" && f[2] == "/" {
			scratch = f[1]
		}
	}
	if !strings.HasPrefix(scratch, "backsight-verify-") || retained.Load() != 1 {
		t.Fatalf("no scratch bank created, or %d retains: %v", retained.Load(), bankCalls)
	}
	var seen []string
	for _, c := range bankCalls {
		f := strings.Fields(c)
		switch {
		case f[1] == scratch:
			seen = append(seen, f[0]+" "+f[2])
		case f[0] != "GET": // the hooks may read the real bank, never write it
			t.Errorf("verify wrote into another bank: %s", c)
		}
	}
	if want := "PUT /,POST /memories"; !strings.HasPrefix(strings.Join(seen, ","), want) || seen[len(seen)-1] != "DELETE /" {
		t.Errorf("round trip calls %v", seen)
	}
	if retainedTags[0] != "source:verify" {
		t.Errorf("verify retained with tags %q", retainedTags[0])
	}
	// Hooks and the MCP server ran against a scratch state.
	if st, _ := stateLoad(home); st.FirstSessionNoticeShown {
		t.Error("verify used up the first-session notice")
	}
	if ev, _ := filepath.Glob(filepath.Join(pathsOf(home).TelemetryDir(), "*.jsonl")); len(ev) > 0 {
		t.Errorf("verify wrote telemetry into the real state: %v", ev)
	}
	if _, err := os.Stat(filepath.Join(pathsOf(home).SessionsDir(), "by-pid", strconv.Itoa(os.Getpid())+".json")); err == nil {
		t.Error("the by-pid record of the synthetic session was left behind")
	}
	if left, _ := filepath.Glob(filepath.Join(pathsOf(home).SessionsDir(), "backsight-verify*")); len(left) > 0 {
		t.Errorf("synthetic session state left behind: %v", left)
	}
}

func TestVerifyFailsWhenNothingIsExtractedAndStillDeletesTheBank(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	asShim(t)
	fastVerify(t)
	code, out, _ := run(t, "verify", "--json")
	checks := parseVerify(t, out)
	if code != output.Blocked || checks["round_trip"].Status != "fail" || !strings.Contains(checks["round_trip"].Summary, "no fact extracted") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if deleted := scratchBankDeleted(); deleted == "" {
		t.Error("a failed round trip left its scratch bank")
	}
}

func TestVerifyFailsWhenRecallFindsNothing(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	asShim(t)
	fastVerify(t)
	documentUnits[verifyDoc] = 1 // recallAnswer stays empty
	code, out, _ := run(t, "verify", "--json")
	if c := parseVerify(t, out)["round_trip"]; code != output.Blocked || c.Status != "fail" || !strings.Contains(c.Summary, "recall found nothing") {
		t.Errorf("exit %d %+v", code, c)
	}
	if deleted := scratchBankDeleted(); !strings.HasPrefix(deleted, "/v1/default/banks/backsight-verify-") {
		t.Errorf("a recall that found nothing left its scratch bank (deleted %q)", deleted)
	}
}

func TestVerifyWithoutRoundTripCallsNoLLM(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	asShim(t)
	code, out, _ := run(t, "verify", "--no-round-trip", "--json")
	if c := parseVerify(t, out)["round_trip"]; code != output.OK || c.Status != "skipped" || retained.Load() != 0 {
		t.Errorf("exit %d %+v, %d retained\n%s", code, c, retained.Load(), out)
	}
}

func TestVerifyCatchesBrokenHooks(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	for shim, want := range map[string]string{"/bin/false": "exit:", "/bin/echo": "not JSON"} {
		if _, err := os.Stat(shim); err != nil {
			t.Skip(shim + " missing")
		}
		withShim(t, shim)
		code, out, _ := run(t, "verify", "--no-round-trip", "--json")
		checks := parseVerify(t, out)
		if c := checks["hook stop"]; code != output.Blocked || c.Status != "fail" || !strings.Contains(c.Summary, want) {
			t.Errorf("%s: exit %d %+v", shim, code, c)
		}
		if checks["mcp"].Status != "fail" {
			t.Errorf("%s: mcp %+v", shim, checks["mcp"])
		}
	}
}

func TestVerifyNeedsTheShim(t *testing.T) {
	devInstall(t, true)
	withShim(t, "/nonexistent/backsight")
	if code, out, _ := run(t, "verify", "--json"); code != output.NeedsUser || !strings.Contains(out, "backsight client install") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestHookFailedReadsTheSupervisorsEvent(t *testing.T) {
	home := isolate(t)
	d := pathsOf(home)
	_ = os.MkdirAll(d.TelemetryDir(), 0o700)
	line := `{"event":"hook_failure","session":"backsight-verify-abc","hook":"stop","kind":"timeout"}` + "\n"
	_ = os.WriteFile(filepath.Join(d.TelemetryDir(), "events-2026-10.jsonl"), []byte(line), 0o600)
	if !hookFailed(d, "backsight-verify-abc", "stop") {
		t.Error("the failure of this run was missed")
	}
	if hookFailed(d, "backsight-verify-other", "stop") || hookFailed(d, "backsight-verify-abc", "session-end") {
		t.Error("another run's or another hook's failure was counted")
	}
}

func TestVerifyCatchesAHookFailingBehindTheSupervisor(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	asShim(t)
	verifyEnv = append(verifyEnv, testHookFault+"=panic")
	code, out, _ := run(t, "verify", "--no-round-trip", "--json")
	c := parseVerify(t, out)["hook stop"]
	if code != output.Blocked || c.Status != "fail" || !strings.Contains(c.Summary, "behind the supervisor") {
		t.Errorf("exit %d %+v", code, c)
	}
}

func TestVerifyRedactsProviderErrors(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	asShim(t)
	fastVerify(t)
	retainStatus.Store(500)
	retainBody = `{"detail":"provider said: bad key fakellmkey"}`
	code, out, _ := run(t, "verify", "--json")
	if c := parseVerify(t, out)["round_trip"]; code != output.Blocked || c.Status != "fail" || strings.Contains(out, "fakellmkey") {
		t.Errorf("exit %d %+v\n%s", code, c, out)
	}
}

// liveFake is a read-only server for verify --live.
type liveFake struct {
	docs     map[string]hindsight.Document
	memories []hindsight.Memory
	queued   int
}

func (f *liveFake) GetDocument(_ context.Context, _, id string) (hindsight.Document, error) {
	if d, ok := f.docs[id]; ok {
		return d, nil
	}
	return hindsight.Document{}, &hindsight.APIError{Status: 404, Kind: hindsight.NotFound}
}

func (f *liveFake) ListMemories(_ context.Context, _ string, q hindsight.MemoryQuery, fn func(hindsight.Memory) error) error {
	for _, m := range f.memories {
		if (q.Type == "" || m.FactType == q.Type) && (q.DocumentID == "" || m.DocumentID == q.DocumentID) {
			if err := fn(m); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *liveFake) QueuedOperations(context.Context, string) (int, error) { return f.queued, nil }

func withLiveServer(t *testing.T, f *liveFake) (gotURL *string) {
	t.Helper()
	old := liveServer
	var u string
	liveServer = func(url, _ string) (livecheck.Server, error) { u = url; return f, nil }
	t.Cleanup(func() { liveServer = old })
	return &u
}

func liveSession(t *testing.T, home, sid string, written time.Time) string {
	t.Helper()
	p := filepath.Join(home, ".claude", "projects", "-work-api", sid+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"why does the export job stall?"}}`+"\n"), 0o600)
	_ = os.Chtimes(p, written, written)
	return p
}

func parseLive(t *testing.T, out string) (livecheck.Result, *output.Error) {
	t.Helper()
	var env struct {
		Data  livecheck.Result `json:"data"`
		Error *output.Error    `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return env.Data, env.Error
}

func TestVerifyLivePassPendingAndFail(t *testing.T) {
	home := devInstall(t, true)
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	withClock(t, at)
	f := &liveFake{docs: map[string]hindsight.Document{}}
	gotURL := withLiveServer(t, f)

	// Settled 40 min ago, not sent yet: pending, exit 40, rerun as resume.
	p := liveSession(t, home, "s1", at.Add(-40*time.Minute))
	code, out, _ := run(t, "verify", "--live", "--json")
	res, e := parseLive(t, out)
	if code != output.Partial || res.Status != livecheck.Pending || res.WaitS <= 0 || e == nil || e.Resume != "backsight verify --live" {
		t.Fatalf("pending: exit %d\n%s", code, out)
	}
	if !strings.HasPrefix(*gotURL, "http://127.0.0.1:") {
		t.Errorf("read %q, not the configured server", *gotURL)
	}

	// Sent, stored, consolidated into an observation: pass, exit 0.
	fi, _ := os.Stat(p)
	_ = retain.SaveCursor(pathsOf(home).CursorsDir(), "s1", retain.Cursor{Transcript: p, Offset: fi.Size(), LastUUID: "u9", Segment: 1,
		Document: "conversation:s1", Bank: "claude-memory", SentAt: at.Add(-35 * time.Minute)})
	f.docs["conversation:s1"] = hindsight.Document{Metadata: map[string]string{"segment": "1", "last_uuid": "u9"}}
	f.memories = []hindsight.Memory{
		{ID: "f1", FactType: "world", DocumentID: "conversation:s1", ConsolidatedAt: "2026-10-25T11:40:00+00:00"},
		{ID: "o1", FactType: "observation", SourceMemoryIDs: []string{"f1"}},
	}
	code, out, errOut := run(t, "verify", "--live", "--json")
	if res, _ = parseLive(t, out); code != output.OK || res.Status != livecheck.Pass || res.Observations != 1 {
		t.Fatalf("pass: exit %d\n%s%s", code, out, errOut)
	}
	code, out, _ = run(t, "verify", "--live")
	if code != output.OK || !strings.HasPrefix(out, "pass: session s1 reached the server") {
		t.Errorf("text: exit %d\n%s", code, out)
	}

	// Two hours later the server lost the document: fail, exit 20, a fix.
	withClock(t, at.Add(2*time.Hour))
	delete(f.docs, "conversation:s1")
	code, out, _ = run(t, "verify", "--live", "--json")
	if res, _ = parseLive(t, out); code != output.Blocked || res.Status != livecheck.Fail || res.Fix == "" {
		t.Errorf("fail: exit %d\n%s", code, out)
	}
	code, out, errOut = run(t, "verify", "--live")
	if code != output.Blocked || !strings.Contains(out, "fix: ") || errOut != "" {
		t.Errorf("text fail: exit %d\n%s\nstderr %q", code, out, errOut)
	}
}

func TestVerifyLiveRefusesNoRoundTrip(t *testing.T) {
	devInstall(t, true)
	withLiveServer(t, &liveFake{})
	if code, _, _ := run(t, "verify", "--live", "--no-round-trip"); code != output.Usage {
		t.Errorf("exit %d", code)
	}
}

// --config-dir reads that profile's transcripts, and the rerun keeps it.
func TestVerifyLiveConfigDirIsReadAndKeptInTheResume(t *testing.T) {
	home := devInstall(t, true)
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	withClock(t, at)
	withLiveServer(t, &liveFake{docs: map[string]hindsight.Document{}})
	profile := filepath.Join(home, "profiles", "work dir")
	p := filepath.Join(profile, "projects", "-work-api", "w1.jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = os.WriteFile(p, []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":"why does the export job stall?"}}`+"\n"), 0o600)
	_ = os.Chtimes(p, at.Add(-40*time.Minute), at.Add(-40*time.Minute))
	liveSession(t, home, "default", at.Add(-41*time.Minute)) // ~/.claude's: not this profile's

	code, out, _ := run(t, "verify", "--live", "--json", "--config-dir", profile)
	res, e := parseLive(t, out)
	wantResume := "backsight verify --live --config-dir '" + profile + "'"
	if code != output.Partial || res.Session != "w1" || e == nil || e.Resume != wantResume {
		t.Fatalf("exit %d, session %q, error %+v, want resume %q\n%s", code, res.Session, e, wantResume, out)
	}
	code, out, _ = run(t, "verify", "--live", "--config-dir", profile)
	if code != output.Partial || !strings.Contains(out, "rerun in about 5 min: "+wantResume+"\n") {
		t.Errorf("text: exit %d\n%s", code, out)
	}
	// Without the flag: ~/.claude's session, plain rerun.
	_, out, _ = run(t, "verify", "--live", "--json")
	if res, e = parseLive(t, out); res.Session != "default" || e == nil || e.Resume != "backsight verify --live" {
		t.Errorf("default profile: %+v %+v", res, e)
	}
}

// Sessions last written before retain_since are backfill's.
func TestVerifyLiveLeavesSessionsBeforeRetainSinceToBackfill(t *testing.T) {
	home := devInstall(t, true)
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	withClock(t, at)
	withLiveServer(t, &liveFake{docs: map[string]hindsight.Document{}})
	liveSession(t, home, "old", at.Add(-5*time.Hour))
	since := at.Add(-2 * time.Hour)
	_ = state.Update(pathsOf(home), "test", func(s *state.State) error { s.RetainSince = &since; return nil })
	code, out, _ := run(t, "verify", "--live", "--json")
	if res, _ := parseLive(t, out); code != output.Partial || res.Status != livecheck.Pending || !strings.Contains(res.Summary, "no session") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

func TestVerifyLiveServerClientErrorFailsAndNoConfigNeedsTheUser(t *testing.T) {
	devInstall(t, true)
	old := liveServer
	liveServer = func(string, string) (livecheck.Server, error) { return nil, errors.New("bad server url") }
	t.Cleanup(func() { liveServer = old })
	if code, _, errOut := run(t, "verify", "--live"); code != output.Blocked || !strings.Contains(errOut, "bad server url") {
		t.Errorf("client error: exit %d, stderr %q", code, errOut)
	}

	isolate(t) // no config at all
	code, out, errOut := run(t, "verify", "--live")
	if code != output.NeedsUser || !strings.Contains(errOut, "no config") {
		t.Errorf("no config: exit %d, stderr %q", code, errOut)
	}
	if out != "" {
		t.Errorf("no config: stdout must stay empty, got %q", out)
	}
	code, out, _ = run(t, "verify", "--live", "--json")
	if _, e := parseLive(t, out); code != output.NeedsUser || e == nil || e.Code != output.NeedsUser {
		t.Errorf("no config json: exit %d\n%s", code, out)
	}
}

// Against a real client, verify --live only reads: every request it
// sends to the server is a GET.
func TestVerifyLiveSendsOnlyGetRequests(t *testing.T) {
	home := devInstall(t, true)
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	withClock(t, at)
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, r.Method+" "+r.URL.Path)
		switch {
		case strings.Contains(r.URL.Path, "/documents/conversation%3As1") || strings.HasSuffix(r.URL.Path, "/documents/conversation:s1"):
			_, _ = w.Write([]byte(`{"id":"conversation:s1","document_metadata":{"segment":"1","last_uuid":"u9"}}`))
		case strings.Contains(r.URL.Path, "/documents/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"not found"}`))
		case strings.HasSuffix(r.URL.Path, "/memories/list") && r.URL.Query().Get("type") == "observation":
			_, _ = w.Write([]byte(`{"items":[{"id":"o1","fact_type":"observation","source_memory_ids":["f1"],"updated_at":"2026-10-25T10:30:00Z"}],"total":1}`))
		case strings.HasSuffix(r.URL.Path, "/memories/list"):
			_, _ = w.Write([]byte(`{"items":[{"id":"f1","fact_type":"world","document_id":"conversation:s1","consolidated_at":"2026-10-25T10:20:00+00:00"}],"total":1}`))
		case strings.HasSuffix(r.URL.Path, "/operations"):
			_, _ = w.Write([]byte(`{"total":0}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	pointAt(t, home, port)
	p := liveSession(t, home, "s1", at.Add(-2*time.Hour))
	fi, _ := os.Stat(p)
	_ = retain.SaveCursor(pathsOf(home).CursorsDir(), "s1", retain.Cursor{Transcript: p, Offset: fi.Size(), LastUUID: "u9", Segment: 1,
		Document: "conversation:s1", Bank: "claude-memory", SentAt: at.Add(-100 * time.Minute)})
	code, out, errOut := run(t, "verify", "--live", "--json")
	if res, _ := parseLive(t, out); code != output.OK || res.Status != livecheck.Pass {
		t.Fatalf("pass: exit %d\n%s%s\nrequests %q", code, out, errOut, reqs)
	}
	// The document gone, two hours on: the queue is read too.
	_ = retain.SaveCursor(pathsOf(home).CursorsDir(), "s1", retain.Cursor{Transcript: p, Offset: fi.Size(), LastUUID: "u9", Segment: 1,
		Document: "conversation:s2", Bank: "claude-memory", SentAt: at.Add(-100 * time.Minute)})
	if code, out, _ := run(t, "verify", "--live", "--json"); code != output.Blocked {
		t.Errorf("missing document: exit %d\n%s", code, out)
	}
	var docs, lists, ops int
	for _, r := range reqs {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("a write: %s", r)
		}
		switch {
		case strings.Contains(r, "/documents/"):
			docs++
		case strings.HasSuffix(r, "/memories/list"):
			lists++
		case strings.HasSuffix(r, "/operations"):
			ops++
		}
	}
	if docs < 2 || lists < 2 || ops < 1 {
		t.Errorf("not every read was exercised: %q", reqs)
	}
}
