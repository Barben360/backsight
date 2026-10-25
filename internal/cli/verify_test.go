package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/hook"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/plugin"
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
	verifyEnv = append(verifyEnv, hook.ChaosEnv+"=panic")
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
