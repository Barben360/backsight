package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Barben360/backsight/internal/scheduler"
	"github.com/Barben360/backsight/internal/supervise"
)

// StartIfDown starts the supervisor exactly as StartDB's detached
// fallback does (conhost --headless, the task's own action, through the
// same start), once, and never waits for it to answer; a supervisor that
// answers, or a pipe failing otherwise, starts nothing.
func TestStartIfDownStartsThroughConhostWithoutWaiting(t *testing.T) {
	h, f := taskFixture(t)
	taskRunWait = 5 * time.Second // a wait for the answer would show
	start := time.Now()
	started, err := h.n.StartIfDown(context.Background())
	if err != nil || !started {
		t.Fatalf("started %v, %v", started, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("StartIfDown took %s: it waited for the supervisor", took)
	}
	want := scheduler.HeadlessArgs(h.n.shim(), "server", "supervise", "--backsight-home", h.n.Env["BACKSIGHT_HOME"])
	if len(f.detached) != 1 || !slices.Equal(f.detached[0], want) || !slices.Equal(f.detached[0], h.superviseArgs(false)) {
		t.Errorf("detached starts %q, want one of %q", f.detached, want)
	}
	if got := f.ev.all(); !slices.Equal(got, []string{"pipe status", "detached " + strings.Join(want, " ")}) {
		t.Errorf("events %q: one status question, then the start", got)
	}
	// Running now: nothing more starts.
	f.ev = &events{}
	if started, err := h.n.StartIfDown(context.Background()); err != nil || started || len(f.detached) != 1 {
		t.Errorf("a running supervisor: started %v, %v, detached %q", started, err, f.detached)
	}
	// A pipe that fails otherwise (busy, hung past the hook's bound) starts nothing.
	n := h.n
	n.Ask = func(context.Context, string, string) (supervise.Status, error) {
		return supervise.Status{}, context.DeadlineExceeded
	}
	f.up = false
	if started, err := n.StartIfDown(context.Background()); started || !errors.Is(err, context.DeadlineExceeded) || len(f.detached) != 1 {
		t.Errorf("a failing pipe: started %v, %v, detached %q", started, err, f.detached)
	}
	// Native.Detach, when set, is the start.
	var got []string
	n.Ask, n.Detach = nil, func(args ...string) error { got = args; return nil }
	if started, err := n.StartIfDown(context.Background()); !started || err != nil || !slices.Equal(got, want) || len(f.detached) != 1 {
		t.Errorf("with Detach: started %v, %v, args %q", started, err, got)
	}
	// A failed start says so.
	n.Detach = func(...string) error { return errors.New("access denied") }
	if started, err := n.StartIfDown(context.Background()); started || err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("a failed start: %v %v", started, err)
	}
}

// TaskState reads the task's own Enabled from its XML, never schtasks'
// text: disabled, enabled (a trigger's Enabled does not count), in UTF-16
// too; a task schtasks cannot query is not installed.
func TestTaskStateReadsTheXML(t *testing.T) {
	h, _ := taskFixture(t)
	n := h.n
	for _, c := range []struct {
		name string
		out  []byte
		err  error
		want string
	}{
		{"disabled", []byte("<Task><Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers><Settings><Enabled>false</Enabled></Settings></Task>"), nil, TaskDisabled},
		{"enabled", []byte("<Task><Triggers><TimeTrigger><Enabled>false</Enabled></TimeTrigger></Triggers><Settings><Enabled>true</Enabled></Settings></Task>"), nil, TaskEnabled},
		{"no Enabled: the default", []byte("<Task><Settings><Hidden>false</Hidden></Settings></Task>"), nil, TaskEnabled},
		{"disabled, UTF-16", utf16Bytes("<Task><Settings>\r\n<Enabled>false</Enabled>\r\n</Settings></Task>"), nil, TaskDisabled},
		{"not registered", []byte("ERROR: The system cannot find the file specified."), errors.New("exit status 1"), NotInstalled},
	} {
		n.Sys = &fakeSys{Out: func(args []string) ([]byte, error) {
			if want := []string{"schtasks", "/Query", "/TN", `\bs\server`, "/XML"}; !slices.Equal(args, want) {
				t.Errorf("%s: ran %q", c.name, args)
			}
			return c.out, c.err
		}}
		if got, err := n.TaskState(context.Background()); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
}

func utf16Bytes(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// SupervisorStatus is the pipe's answer, nil when none runs.
func TestSupervisorStatus(t *testing.T) {
	h, f := taskFixture(t)
	if st, err := h.n.SupervisorStatus(context.Background()); st != nil || err != nil {
		t.Errorf("none running: %+v %v", st, err)
	}
	f.start()
	if st, err := h.n.SupervisorStatus(context.Background()); err != nil || st == nil || st.PID != 4242 || st.Proto != supervise.Proto {
		t.Errorf("running: %+v %v", st, err)
	}
}

// DLLProblems checks every copy of every DLL dlls.json names, in each
// place the venv's DLLs go: a missing one and a changed one are named,
// the matching ones counted.
func TestDLLProblems(t *testing.T) {
	h, _ := taskFixture(t)
	n := h.n // this host's layout: the files are real
	v := n.Version()
	if _, _, err := n.DLLProblems(v); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no dlls.json: %v", err)
	}
	dlls := map[string][]byte{"msvcp140.dll": []byte("msvcp"), "vcruntime140.dll": []byte("vcrt")}
	sums := map[string]string{}
	for name, raw := range dlls {
		sum := sha256.Sum256(raw)
		sums[name] = hex.EncodeToString(sum[:])
	}
	raw, _ := json.Marshal(sums)
	if err := os.WriteFile(n.DLLManifest(), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	dirs := n.venvDLLDirs(v)
	if len(dirs) != 2 { // pg0's bin\ and onnxruntime\capi\ (no copied installation yet)
		t.Fatalf("dirs %q", dirs)
	}
	for _, dir := range dirs {
		for name, raw := range dlls {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if probs, copies, err := n.DLLProblems(v); err != nil || len(probs) != 0 || copies != 4 {
		t.Errorf("all matching: %q %d %v", probs, copies, err)
	}
	gone, changed := filepath.Join(dirs[0], "msvcp140.dll"), filepath.Join(dirs[1], "vcruntime140.dll")
	_ = os.Remove(gone)
	_ = os.WriteFile(changed, []byte("other"), 0o644)
	probs, copies, err := n.DLLProblems(v)
	if err != nil || copies != 4 || !slices.Equal(probs, []string{gone + ": missing", changed + ": differs"}) {
		t.Errorf("one missing, one changed: %q %d %v", probs, copies, err)
	}
}

// The texts commands print name the scheduled task and the supervisor's
// logs on Windows, and stay the units and journalctl on Linux (the words
// printed since 0078).
func TestNativeTextsPerOS(t *testing.T) {
	h, _ := taskFixture(t)
	n := h.n
	n.UnitDir = "/u"
	n.GOOS = "linux"
	for got, want := range map[string]string{
		n.ServerName():                       "the server unit bs-server.service",
		n.LogsHint():                         "journalctl --user -u bs-server.service",
		n.Started():                          "units bs-db.service and bs-server.service",
		n.Services():                         "bs-db.service and bs-server.service",
		n.StopText(true) + n.StopText(false): "",
		strings.Join(n.ServiceFiles(), " "):  filepath.Join("/u", "bs-db.service") + " " + filepath.Join("/u", "bs-server.service"),
	} {
		if got != want {
			t.Errorf("linux: %q, want %q", got, want)
		}
	}
	n.GOOS = "windows"
	for _, s := range []string{n.ServerName(), n.LogsHint(), n.Started(), n.Services(), n.StopText(true), n.StopText(false), strings.Join(n.ServiceFiles(), " ")} {
		for _, word := range []string{"systemd", "journalctl", ".service", "unit"} {
			if strings.Contains(s, word) {
				t.Errorf("windows text %q names %q", s, word)
			}
		}
	}
	for _, c := range []struct{ got, want string }{
		{n.ServerName(), `scheduled task \bs\server`},
		{n.LogsHint(), "bs-supervisor.log"},
		{n.LogsHint(), "bs-api.log"},
		{n.Started(), `the scheduled task \bs\server`},
		{n.StopText(true), `would disable the scheduled task \bs\server`},
		{n.StopText(false), "until backsight server start"},
		{strings.Join(n.ServiceFiles(), " "), "supervise.json"},
	} {
		if !strings.Contains(c.got, c.want) {
			t.Errorf("windows: %q lacks %q", c.got, c.want)
		}
	}
}
