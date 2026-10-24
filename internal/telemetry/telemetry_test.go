package telemetry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var common = Common{V: "fr-1", Session: "s1", Project: "backsight", BinaryVersion: "0.1.0"}

func lines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("invalid line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func fixedClock(ts string) func() time.Time {
	return func() time.Time { t, _ := time.Parse(time.RFC3339, ts); return t }
}

func TestEmitWritesCommonAndEventFields(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, common)
	w.now = fixedClock("2026-10-24T09:30:00+02:00")
	w.Emit(Gate{N: 4, Decision: "deny", Reason: "no_search"})
	got := lines(t, filepath.Join(dir, "telemetry", "events-2026-10.jsonl"))
	if len(got) != 1 {
		t.Fatalf("%d lines", len(got))
	}
	want := map[string]any{"ts": "2026-10-24T07:30:00.000Z", "v": "fr-1", "event": "gate", "session": "s1",
		"project": "backsight", "binary_version": "0.1.0", "n": float64(4), "decision": "deny", "reason": "no_search"}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("%s = %v, want %v", k, got[0][k], v)
		}
	}
}

func TestMonthlyFilesUseUTC(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, common)
	w.now = fixedClock("2026-11-01T00:30:00+02:00") // still October in UTC
	w.Emit(Job{Name: "tick", Status: "ok"})
	w.now = fixedClock("2026-11-01T03:00:00+02:00")
	w.Emit(Job{Name: "tick", Status: "ok"})
	if n := len(lines(t, filepath.Join(dir, "telemetry", "events-2026-10.jsonl"))); n != 1 {
		t.Errorf("october: %d lines", n)
	}
	if n := len(lines(t, filepath.Join(dir, "telemetry", "events-2026-11.jsonl"))); n != 1 {
		t.Errorf("november: %d lines", n)
	}
}

// Every A.4 event names itself as the spec does.
func TestEventNames(t *testing.T) {
	cases := map[string]Event{"prompt": Prompt{}, "outcome": Outcome{}, "briefing": Briefing{}, "briefing_outcome": BriefingOutcome{},
		"tool": Tool{}, "gate": Gate{}, "retain": Retain{}, "hook_failure": HookFailure{}, "job": Job{}}
	for want, e := range cases {
		if e.EventName() != want {
			t.Errorf("%T: %q, want %q", e, e.EventName(), want)
		}
	}
}

// An event field must not overwrite a common field.
func TestCommonFieldsWin(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, common)
	w.Emit(Raw{Name: "custom", Fields: map[string]any{"session": "forged", "extra": 1}})
	got := lines(t, w.currentFile())[0]
	if got["session"] != "s1" || got["event"] != "custom" || got["extra"] != float64(1) {
		t.Errorf("%v", got)
	}
}

func TestEmitNeverFails(t *testing.T) {
	dir := t.TempDir()
	// A file where the telemetry directory should be makes every write fail.
	if err := os.WriteFile(filepath.Join(dir, "telemetry"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w := New(dir, common)
	w.Emit(Job{Name: "tick"})
	w.Emit(Job{Name: "tick"})
	if w.Failures() != 2 {
		t.Errorf("failures = %d", w.Failures())
	}
}

// Hooks are separate processes writing the same file at once.
func TestConcurrentProcessesKeepLinesWhole(t *testing.T) {
	if os.Getenv("BACKSIGHT_TELEMETRY_CHILD") != "" {
		return
	}
	dir := t.TempDir()
	const procs, per = 8, 200
	var cmds []*exec.Cmd
	for i := range procs {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperWriter$")
		cmd.Env = append(os.Environ(), "BACKSIGHT_TELEMETRY_CHILD="+dir, "CHILD_ID="+strconv.Itoa(i), "CHILD_N="+strconv.Itoa(per))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	w := New(dir, common)
	got := lines(t, w.currentFile())
	if len(got) != procs*per {
		t.Errorf("%d lines, want %d", len(got), procs*per)
	}
}

func TestHelperWriter(t *testing.T) {
	dir := os.Getenv("BACKSIGHT_TELEMETRY_CHILD")
	if dir == "" {
		t.Skip("helper process only")
	}
	n, _ := strconv.Atoi(os.Getenv("CHILD_N"))
	w := New(dir, common)
	pad := fmt.Sprintf("%03000d", 0) // long lines make interleaving visible
	for i := range n {
		w.Emit(Raw{Name: "load", Fields: map[string]any{"child": os.Getenv("CHILD_ID"), "i": i, "pad": pad}})
	}
	if w.Failures() != 0 {
		t.Fatal("write failures in child")
	}
}
