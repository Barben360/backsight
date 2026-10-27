package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/selfcheck"
	"github.com/Barben360/backsight/internal/signing"
	"github.com/Barben360/backsight/internal/state"
)

// fakeRelease installs a fake release binary of version v in dir, with the
// checksums.txt and signature install.sh keeps next to it, signed by a key
// the binary trusts as its test key. signed=false leaves both out.
func fakeRelease(t *testing.T, dir, v string, signed bool) string {
	t.Helper()
	body := []byte("#!/bin/sh\necho fake backsight " + v + "\n")
	_ = os.MkdirAll(dir, 0o755)
	exe := filepath.Join(dir, "backsight")
	if err := os.WriteFile(exe, body, 0o755); err != nil {
		t.Fatal(err)
	}
	old := selfExe
	selfExe = func() (string, error) { return exe, nil }
	t.Cleanup(func() { selfExe = old })
	if !signed {
		return exe
	}
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := signing.TestKey
	signing.TestKey = pub.String()
	t.Cleanup(func() { signing.TestKey = oldKey })
	h := sha256.Sum256(body)
	sums := hex.EncodeToString(h[:]) + "  " + selfcheck.BinaryName(v, runtime.GOOS, runtime.GOARCH) + "\n"
	r := minisign.NewReader(strings.NewReader(sums))
	_, _ = io.Copy(io.Discard, r)
	_ = os.WriteFile(filepath.Join(dir, selfcheck.ChecksumsFile), []byte(sums), 0o644)
	_ = os.WriteFile(filepath.Join(dir, selfcheck.SignatureFile), r.SignWithComments(priv, "timestamp:1\tfile:checksums.txt\thashed", "x"), 0o644)
	return exe
}

// jobs tick is where a new version checks itself, once.
func TestJobsTickRecordsTheSelfCheck(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	setBuild(t, "0.5.0", "abc")
	fakeRelease(t, filepath.Join(pathsOf(home).Data, "bin", "0.5.0"), "0.5.0", true)
	code, out, _ := run(t, "jobs", "tick", "--json")
	var env struct {
		Data struct {
			Binary string `json:"binary"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != output.OK || env.Data.Binary != "verified" {
		t.Fatalf("exit %d %v\n%s", code, err, out)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	if st.SelfCheck == nil || st.SelfCheck.Status != "verified" || st.SelfCheck.Version != "0.5.0" {
		t.Errorf("state: %+v", st.SelfCheck)
	}
}

// A tick that finds maintenance in progress (update apply's first tick of
// the new binary, say) writes nothing: the self-check waits for the next
// tick, so state.json's lock stays free for the update.
func TestJobsTickUnderMaintenanceDoesNotRunTheSelfCheck(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	setBuild(t, "0.5.0", "abc")
	fakeRelease(t, filepath.Join(pathsOf(home).Data, "bin", "0.5.0"), "0.5.0", true)
	d := pathsOf(home)
	raw, _ := json.Marshal(state.Owner{PID: liveProcess(t), Command: "update apply", Since: time.Now().UTC().Truncate(time.Second)})
	_ = os.MkdirAll(d.State, 0o700)
	if err := os.WriteFile(d.MaintenanceLock(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(d.StateFile())
	code, out, _ := run(t, "jobs", "tick", "--json")
	if code != output.OK || !strings.Contains(out, "maintenance in progress") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if after, _ := os.ReadFile(d.StateFile()); string(after) != string(before) {
		t.Errorf("the tick wrote state.json under maintenance:\n%s", after)
	}
	// Once the maintenance is over, the next tick checks the binary.
	_ = os.Remove(d.MaintenanceLock())
	if code, out, _ := run(t, "jobs", "tick", "--json"); code != output.OK || !strings.Contains(out, `"binary": "verified"`) {
		t.Errorf("after the maintenance: exit %d\n%s", code, out)
	}
}

// A binary that does not match its release is reported by tick and doctor,
// and the tick still does its work.
func TestATamperedBinaryIsADoctorErrorAndTheTickRuns(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	setBuild(t, "0.5.0", "abc")
	withDate(t, "2026-10-25T10:00:00Z")
	exe := fakeRelease(t, filepath.Join(pathsOf(home).Data, "bin", "0.5.0"), "0.5.0", true)
	_ = os.WriteFile(exe, []byte("#!/bin/sh\necho tampered\n"), 0o755)
	withClock(t, time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC))
	run(t, "jobs", "tick") // the first tick: live retain starts from here
	writeTranscript(t, home, "live", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	withClock(t, time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC))
	before := retained.Load()
	code, out, _ := run(t, "jobs", "tick", "--json")
	var env struct {
		Data struct {
			Status   string         `json:"status"`
			Binary   string         `json:"binary"`
			Sessions map[string]int `json:"sessions"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &env)
	if code != output.OK || env.Data.Binary != "failed" || env.Data.Status != "done" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// The tick did its work: the settled session was sent.
	if retained.Load() == before || env.Data.Sessions["sent"] != 1 {
		t.Errorf("the tick sent nothing (%d retained): %s", retained.Load()-before, out)
	}
	e := doctor.Env{Dirs: pathsOf(home)}
	realProbes(&e)
	if e.SelfCheck == nil {
		t.Fatal("doctor has no binary probe")
	}
	if got := e.SelfCheck(); got.Status != "failed" || !strings.Contains(got.Detail, "checksum mismatch") {
		t.Errorf("doctor sees %+v", got)
	}
}

// doctor's probe reads the binary without writing state (doctor is
// read-only).
func TestDoctorProbeChecksTheBinaryReadOnly(t *testing.T) {
	home := devInstall(t, true)
	fakeRelease(t, filepath.Join(pathsOf(home).Data, "bin", "dev"), "dev", false)
	e := doctor.Env{Dirs: pathsOf(home)}
	realProbes(&e)
	before, _ := os.ReadFile(pathsOf(home).StateFile())
	if got := e.SelfCheck(); got.Status != "unsigned" {
		t.Errorf("%+v", got)
	}
	if after, _ := os.ReadFile(pathsOf(home).StateFile()); string(after) != string(before) {
		t.Error("the doctor probe wrote state.json")
	}
}

// Hooks never hash the binary: no self-check is recorded by any hook.
func TestHooksNeverRunTheSelfCheck(t *testing.T) {
	home := devInstall(t, true)
	pointAt(t, home, fakeHindsight(t))
	childFromTestBinary(t, "")
	d := pathsOf(home)
	in := `{"session_id":"s1","source":"startup","cwd":"` + filepath.Join(home, "billing") + `","prompt":"how do we deploy billing?"}`
	for _, ev := range []string{"session-start", "user-prompt-submit", "pre-tool-use", "post-tool-use", "post-tool-use-failure", "subagent-start", "subagent-stop", "stop", "session-end"} {
		code, out := runHookCmd(t, in, ev)
		if code != 0 || (ev == "session-start" && !strings.Contains(out, "additionalContext")) {
			t.Fatalf("%s: exit %d %q", ev, code, out)
		}
	}
	// The hooks did their work (a session state, their telemetry)...
	if _, err := os.Stat(filepath.Join(d.SessionsDir(), "s1.json")); err != nil {
		t.Errorf("the hooks recorded no session: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(d.TelemetryDir(), "*.jsonl"))
	var events string
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		events += string(raw)
	}
	if !strings.Contains(events, `"event":"briefing"`) || !strings.Contains(events, `"event":"prompt"`) {
		t.Errorf("hook telemetry: %s", events)
	}
	// ...without checking the binary.
	if st, _ := state.Load(d.StateFile()); st.SelfCheck != nil {
		t.Errorf("a hook ran the self-check: %+v", st.SelfCheck)
	}
	// The control: a tick on the same install does record it.
	if code, out, _ := run(t, "jobs", "tick"); code != output.OK {
		t.Fatalf("tick: exit %d\n%s", code, out)
	}
	if st, _ := state.Load(d.StateFile()); st.SelfCheck == nil {
		t.Error("the tick recorded no self-check: this test reads the wrong state")
	}
}

// client install copies the binary to <data>/bin/<v>-<hash>/: the signed
// checksums go with it, so the copy can check itself.
func TestClientInstallKeepsTheChecksumsWithTheBinary(t *testing.T) {
	home := devInstall(t, true)
	withClaude(t)
	setBuild(t, "0.5.0", "abc")
	src := filepath.Join(t.TempDir(), "0.5.0")
	fakeRelease(t, src, "0.5.0", true)
	if code, out, _ := run(t, "client", "install"); code != output.OK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	st, _ := state.Load(pathsOf(home).StateFile())
	for _, f := range []string{selfcheck.ChecksumsFile, selfcheck.SignatureFile} {
		want, _ := os.ReadFile(filepath.Join(src, f))
		got, err := os.ReadFile(filepath.Join(filepath.Dir(st.ShimTarget), f))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s next to %s: %v", f, st.ShimTarget, err)
		}
	}
	if got := selfcheck.Check(st.ShimTarget, "0.5.0", runtime.GOOS, runtime.GOARCH, time.Now()); got.Status != selfcheck.Verified {
		t.Errorf("the installed copy does not verify: %+v", got)
	}
}

func withDate(t *testing.T, date string) {
	t.Helper()
	old := buildinfo.Date
	buildinfo.Date = date
	t.Cleanup(func() { buildinfo.Date = old })
}
