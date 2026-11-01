package server

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
)

// A Windows step as taskHost.Transient runs it, on any host (no file is
// touched): its leading KEY=value words replace env's keys whatever their
// case, the call's stdin reaches "-f -" only, the markers leave the argv
// and send the output to the step's log under <Root>\tmp.
func TestWindowsStepMarkers(t *testing.T) {
	n := Native{GOOS: "windows", Cfg: config.Config{Server: config.Server{ComposeDir: `C:\bs`}}}
	env := []string{`Path=C:\pg0\bin;C:\Windows\System32`, "PGPASSWORD=postgres", `TEMP=C:\bs\native\tmp`}
	stdin := strings.NewReader("ALTER ROLE …")
	for _, tc := range []struct {
		name    string
		step    []string
		argv    []string
		env     []string
		stdin   io.Reader
		log     string
		wantErr bool
	}{
		{"psql: its own PATH, stdin from -f -",
			[]string{`PATH=C:\pg\bin;C:\Windows\System32`, `C:\pg\bin\psql.exe`, "-q", "-f", "-"},
			[]string{`C:\pg\bin\psql.exe`, "-q", "-f", "-"},
			[]string{"PGPASSWORD=postgres", `TEMP=C:\bs\native\tmp`, `PATH=C:\pg\bin;C:\Windows\System32`}, stdin, "", false},
		{"pg0 start: no stdin, its output to a log",
			[]string{`C:\v\pg0.exe`, "start", "--name", "bs", stepToLog},
			[]string{`C:\v\pg0.exe`, "start", "--name", "bs"}, env, nil, `C:\bs\native\tmp\pg0-bs-db-init-2.log`, false},
		{"no stdin, no stdout: a log too",
			[]string{`C:\pg\bin\initdb.exe`, "-D", `C:\d`, stepNoStdin, stepNoStdout},
			[]string{`C:\pg\bin\initdb.exe`, "-D", `C:\d`}, env, nil, `C:\bs\native\tmp\initdb-bs-db-init-2.log`, false},
		{"-f with another file gets no stdin",
			[]string{`C:\pg\bin\psql.exe`, "-f", `C:\x.sql`, "-"},
			[]string{`C:\pg\bin\psql.exe`, "-f", `C:\x.sql`, "-"}, env, nil, "", false},
		{"nothing to run", []string{"KEY=v", stepToLog}, nil, nil, nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := n.windowsStep("bs-db-init", 1, env, stdin, tc.step)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "step 2 of bs-db-init runs nothing") {
					t.Errorf("windowsStep = %+v, %v; want a refusal", s, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(s.argv, tc.argv) || !slices.Equal(s.env, tc.env) || s.stdin != tc.stdin || s.log != tc.log {
				t.Errorf("windowsStep:\n argv %q env %q stdin %v log %q\nwant argv %q env %q stdin %v log %q",
					s.argv, s.env, s.stdin != nil, s.log, tc.argv, tc.env, tc.stdin != nil, tc.log)
			}
		})
	}
	if !slices.Equal(env, []string{`Path=C:\pg0\bin;C:\Windows\System32`, "PGPASSWORD=postgres", `TEMP=C:\bs\native\tmp`}) {
		t.Errorf("windowsStep changed the job's env: %q", env)
	}
}

// Fix round 1: a backend (a process whose parent is one of them and
// older) is no stray, its postmaster is; a parent pid reused by a newer
// process makes no backend; an unknown creation time makes none either
// (reported rather than hidden); skip keeps the supervisor's and a
// verify's.
func TestStrayProcs(t *testing.T) {
	never := func(uint32) bool { return false }
	for _, tc := range []struct {
		name  string
		procs []winProc
		skip  func(uint32) bool
		want  []uint32
	}{
		{"a postmaster and its backends", []winProc{{10, 1, 100}, {11, 10, 200}, {12, 10, 300}}, never, []uint32{10}},
		{"a backend's parent pid reused by a newer postgres", []winProc{{11, 10, 200}, {10, 1, 300}}, never, []uint32{11, 10}},
		{"a creation time unknown", []winProc{{10, 1, 0}, {11, 10, 200}}, never, []uint32{10, 11}},
		{"its own parent", []winProc{{10, 10, 100}}, never, []uint32{10}},
		{"skipped, with its backend", []winProc{{10, 1, 100}, {11, 10, 200}, {20, 1, 100}}, func(p uint32) bool { return p == 10 }, []uint32{20}},
		{"an orphan backend", []winProc{{11, 10, 200}}, never, []uint32{11}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strayProcs(tc.procs, tc.skip); !slices.Equal(got, tc.want) {
				t.Errorf("strayProcs = %v, want %v", got, tc.want)
			}
		})
	}
}

// Fix round 1: a directory's move is retried while it is denied (a file
// just closed, an indexer), a few seconds at most; any other failure is
// not retried.
func TestRenameRetriesWhileDenied(t *testing.T) {
	old, oldWait := osRename, renameRetryWait
	t.Cleanup(func() { osRename, renameRetryWait = old, oldWait })
	renameRetryWait = 2 * time.Second
	denied := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: fs.ErrPermission}
	for _, tc := range []struct {
		name  string
		fails int
		err   error
		calls int
		ok    bool
	}{
		{"denied twice", 2, denied, 3, true},
		{"another failure", 1, &os.LinkError{Op: "rename", Old: "a", New: "b", Err: fs.ErrNotExist}, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			osRename = func(string, string) error {
				if calls++; calls <= tc.fails {
					return tc.err
				}
				return nil
			}
			if err := renameRetry("a", "b"); (err == nil) != tc.ok || calls != tc.calls {
				t.Errorf("renameRetry = %v after %d call(s), want ok %v after %d", err, calls, tc.ok, tc.calls)
			}
		})
	}
	t.Run("denied for good", func(t *testing.T) {
		renameRetryWait = 300 * time.Millisecond
		calls := 0
		osRename = func(string, string) error { calls++; return denied }
		start := time.Now()
		if err := renameRetry("a", "b"); !errors.Is(err, fs.ErrPermission) || calls < 2 || time.Since(start) > 2*time.Second {
			t.Errorf("renameRetry = %v after %d call(s) in %s, want denied after retries within the wait", err, calls, time.Since(start))
		}
	})
}
