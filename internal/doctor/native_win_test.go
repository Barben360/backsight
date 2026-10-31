package doctor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/buildinfo"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/supervise"
)

// windowsFacts is a healthy native install on Windows (decision 0080).
func windowsFacts() NativeFacts {
	return NativeFacts{
		Windows:     true,
		TaskName:    `\backsight\server`,
		Task:        server.TaskEnabled,
		Supervisor:  &supervise.Status{Proto: supervise.Proto, Version: "0.9.0", PID: 4242, DB: "running", API: "running"},
		LogDir:      `C:\Users\jo\AppData\Local\backsight\state\logs`,
		Fsync:       "on",
		Home:        `C:\Users\jo\AppData\Local\backsight\data\server\native\home`,
		HomeACL:     true,
		Venv:        `C:\Users\jo\AppData\Local\backsight\data\server\native\venv\1.1.0-14d79a0a7914`,
		Pin:         `C:\Users\jo\AppData\Local\backsight\data\server\native\venv\1.1.0-14d79a0a7914`,
		DLLManifest: `C:\Users\jo\AppData\Local\backsight\data\server\native\dlls.json`,
		DLLCopies:   27,
		Private: []PrivatePath{{Path: `C:\Users\jo\AppData\Local\backsight\data`}, {Path: `C:\Users\jo\AppData\Local\backsight\config`},
			{Path: `C:\Users\jo\AppData\Local\backsight\config\secrets.toml`}, {Path: `C:\Users\jo\AppData\Local\backsight\data\server\native\server.env`}},
	}
}

// wantWinIDs are the checks of a native install on Windows, in order.
var wantWinIDs = []string{"native-task", "supervisor", "native-fsync", "native-identity", "native-stray", "native-home", "native-venv",
	"native-dlls", "native-private", "path-length"}

func withVersion(t *testing.T, v string) {
	t.Helper()
	old := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = old })
}

// Doctor on Windows: the task and the supervisor replace the units and
// linger, and the DLLs, the DACLs and the path bound are checked; each
// check's ok, info, warn and error cases.
func TestDoctorWindowsChecks(t *testing.T) {
	withVersion(t, "0.9.0")
	f := newFixture(t)
	nativeInstall(f, windowsFacts)
	r := f.run()
	var got []string
	for _, id := range ids(r) {
		if slices.Contains(wantWinIDs, id) || strings.HasPrefix(id, "native-") {
			got = append(got, id)
		}
	}
	if !slices.Equal(got, wantWinIDs) {
		t.Errorf("checks %v\nwant %v", got, wantWinIDs)
	}
	for _, id := range wantWinIDs {
		if c := check(t, r, id); c.Status != OK {
			t.Errorf("healthy: %s %+v", id, c)
		}
	}
	expect(t, check(t, r, "native-task"), OK, `\backsight\server is registered and enabled`)
	expect(t, check(t, r, "supervisor"), OK, "pid 4242")
	expect(t, check(t, r, "native-dlls"), OK, "27 copies")
	expect(t, check(t, r, "path-length"), OK, "78 characters")

	long := `C:\` + strings.Repeat("v", 108) // 111 + 140 = 251
	for _, tc := range []struct {
		name         string
		bad          func(*NativeFacts)
		id           string
		status       Status
		summary, fix string
		alsoNotOK    []string // checks that follow from it
	}{
		{"task not registered", func(n *NativeFacts) { n.Task = server.NotInstalled; n.Supervisor = nil }, "native-task", Error,
			`\backsight\server is not registered`, "backsight server start", []string{"supervisor"}},
		{"task refused at setup", func(n *NativeFacts) { n.Task, n.StartOnSession = server.NotInstalled, true }, "native-task", Info,
			"Task Scheduler refused it at setup", "", nil},
		{"task disabled by server stop", func(n *NativeFacts) { n.Task, n.Supervisor = server.TaskDisabled, nil }, "native-task", Info,
			`\backsight\server is disabled`, "backsight server start", []string{"supervisor"}},
		{"task unread", func(n *NativeFacts) {
			n.Task, n.Unread = "", map[string]error{"native-task": errors.New("schtasks: not found")}
		}, "native-task", Error,
			"could not read", "", nil},
		{"supervisor down", func(n *NativeFacts) { n.Supervisor = nil }, "supervisor", Error,
			"no supervisor answers", "backsight server start", nil},
		{"supervisor stopped with the task", func(n *NativeFacts) { n.Task, n.Supervisor = server.TaskDisabled, nil }, "supervisor", Info,
			"backsight server stop", "", []string{"native-task"}},
		{"fsync unread after server stop", func(n *NativeFacts) { n.Task, n.Supervisor, n.Fsync = server.TaskDisabled, nil, "" }, "native-fsync", Info,
			"the native server is stopped", "", []string{"native-task", "supervisor"}},
		{"fsync unread while it should run", func(n *NativeFacts) { n.Supervisor, n.Fsync = nil, "" }, "native-fsync", Error,
			"could not read fsync", "backsight server start", []string{"supervisor"}},
		{"supervisor of another protocol", func(n *NativeFacts) { n.Supervisor.Proto = supervise.Proto + 1 }, "supervisor", Warn,
			"restart it", "backsight server stop, then backsight server start", nil},
		{"supervisor of an older backsight", func(n *NativeFacts) { n.Supervisor.Version = "0.8.1" }, "supervisor", Info,
			"backsight 0.8.1, older than this one (0.9.0)", "", nil},
		{"supervisor refused the database", func(n *NativeFacts) { n.Supervisor.DB, n.Supervisor.Refusal = "refused", "the instance is empty" }, "supervisor", Error,
			"the instance is empty", "backsight server status", nil},
		{"API restarting", func(n *NativeFacts) { n.Supervisor.API = "restarting" }, "supervisor", Warn,
			"the API is restarting", `api.log`, nil},
		{"supervisor unread", func(n *NativeFacts) {
			n.Supervisor, n.Unread = nil, map[string]error{"supervisor": errors.New("pipe busy")}
		}, "supervisor", Error, "did not answer", "", nil},
		{"a DLL missing", func(n *NativeFacts) { n.DLLProblems = []string{`C:\n\pg\18.1.0\bin\msvcp140.dll: missing`} }, "native-dlls", Error,
			`msvcp140.dll: missing`, `1.1.0-14d79a0a7914\.backsight-complete, then backsight server install`, nil},
		{"dlls.json unread", func(n *NativeFacts) { n.Unread = map[string]error{"native-dlls": errors.New("no such file")} }, "native-dlls", Error,
			"could not read", "backsight server install", nil},
		{"secrets readable by others", func(n *NativeFacts) { n.Private[2].Readers = []string{`BUILTIN\Users`} }, "native-private", Error,
			`secrets.toml can be read by BUILTIN\Users`, `icacls`, nil},
		{"a DACL unread", func(n *NativeFacts) { n.Private[3].Err = errors.New("access denied") }, "native-private", Warn,
			"could not read who can read", "", nil},
		{"venv path too long", func(n *NativeFacts) { n.Venv = long }, "path-length", Error,
			"111 characters", "BACKSIGHT_HOME", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			nativeInstall(f, func() NativeFacts { n := windowsFacts(); tc.bad(&n); return n })
			r := f.run()
			c := check(t, r, tc.id)
			expect(t, c, tc.status, tc.summary)
			if !strings.Contains(c.Fix, tc.fix) || (tc.fix == "" && c.Fix != "" && tc.status == Info) {
				t.Errorf("%s fix %q, want %q", c.ID, c.Fix, tc.fix)
			}
			for _, other := range wantWinIDs {
				if other != tc.id && !slices.Contains(tc.alsoNotOK, other) && check(t, r, other).Status != OK {
					t.Errorf("%s: %+v, want ok", other, check(t, r, other))
				}
			}
		})
	}
	// 250 exactly passes.
	f = newFixture(t)
	nativeInstall(f, func() NativeFacts { n := windowsFacts(); n.Venv = long[:len(long)-1]; return n })
	expect(t, check(t, f.run(), "path-length"), OK, "110 characters")
}

// Windows' fix strings never name systemd, journalctl or a unit; the
// Linux checks' still do (TestDoctorNativeChecks).
func TestDoctorWindowsTextsAreHostNeutral(t *testing.T) {
	withVersion(t, "0.9.0")
	for _, bad := range []func(*NativeFacts){
		func(n *NativeFacts) { n.Fsync = "off" },
		func(n *NativeFacts) { n.Strays = []int{4242} },
		func(n *NativeFacts) { n.Supervisor = nil },
		func(n *NativeFacts) { n.Supervisor.API = "restarting" },
	} {
		f := newFixture(t)
		nativeInstall(f, func() NativeFacts { n := windowsFacts(); bad(&n); return n })
		for _, c := range f.run().Checks {
			text := c.Summary + " " + c.Detail + " " + c.Fix
			for _, word := range []string{"systemctl", "journalctl", ".service", "linger", " unit"} {
				if strings.Contains(text, word) {
					t.Errorf("%s names %q on Windows: %s", c.ID, word, text)
				}
			}
		}
	}
}

// A native probe that fails on Windows lists the Windows checks, each
// skipped, never the Linux ones.
func TestDoctorWindowsProbeFails(t *testing.T) {
	f := newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nruntime = \"native\"\n", 0o600)
	f.env.Native = func(context.Context) (NativeFacts, error) { return NativeFacts{Windows: true}, errors.New("no config") }
	r := f.run()
	expect(t, check(t, r, "native-task"), Error, "could not read the native runtime")
	for _, id := range wantWinIDs[1:] {
		expect(t, check(t, r, id), Info, "skipped")
	}
	for _, id := range []string{"native-unit", "native-linger"} {
		if slices.Contains(ids(r), id) {
			t.Errorf("%s listed on Windows", id)
		}
	}
}

// The Linux native list is the one recorded at 884c942 (the cli golden
// doctor-checks-native.golden pins it with the rest of doctor's): the
// Windows checks add nothing to it.
func TestLinuxDoctorListUnchanged(t *testing.T) {
	want := []string{"native-unit", "native-fsync", "native-identity", "native-stray", "native-linger", "native-home", "native-venv", "web-ui"}
	f := newFixture(t)
	nativeInstall(f, goodFacts)
	var got []string
	for _, id := range ids(f.run()) {
		if strings.HasPrefix(id, "native-") || id == "web-ui" || id == "supervisor" || id == "path-length" {
			got = append(got, id)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("linux native checks %v\nwant %v", got, want)
	}
	// And when its probe fails.
	f = newFixture(t)
	put(t, f.env.Dirs.ConfigFile(), "[server]\nruntime = \"native\"\n", 0o600)
	f.env.Native = func(context.Context) (NativeFacts, error) { return NativeFacts{}, errors.New("no user manager") }
	got = nil
	for _, id := range ids(f.run()) {
		if strings.HasPrefix(id, "native-") || id == "web-ui" || id == "supervisor" || id == "path-length" {
			got = append(got, id)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("linux native checks on a failed probe %v\nwant %v", got, want)
	}
}
