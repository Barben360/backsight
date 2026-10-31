package server

import (
	"io"
	"slices"
	"strings"
	"testing"

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
