package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/testtool"
)

// dbToolsOut answers as the bundled Postgres tools would: pg_controldata
// prints the identifier held in the data directory's fake-sysid file (and
// no identifier without it), psql answers SHOW fsync with fsync.
func dbToolsOut(fsync string) func(c toolCall) []byte {
	return func(c toolCall) []byte {
		switch filepath.Base(c.Path) {
		case "pg_controldata":
			i := slices.Index(c.Args, "-D")
			if i < 0 || i+1 >= len(c.Args) {
				return nil
			}
			id, err := os.ReadFile(filepath.Join(c.Args[i+1], "fake-sysid"))
			if err != nil {
				return []byte("pg_controldata: error: could not open file\n")
			}
			return []byte("pg_control version number:            1800\nCatalog version number:               202506291\n" +
				"Database system identifier:           " + strings.TrimSpace(string(id)) + "\nDatabase cluster state:               shut down\n")
		case "psql":
			if slices.Contains(c.Args, "SHOW fsync") {
				return []byte(fsync + "\n")
			}
		}
		return nil
	}
}

// recordInstance makes n's database (PG_VERSION, and id for the fake
// pg_controldata) and records id as its identity.
func recordInstance(t *testing.T, n Native, id string) {
	t.Helper()
	touch(t, filepath.Join(n.DataDir(), "PG_VERSION"), 0o600)
	if err := os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.IdentityFile(), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Requirement 2: a missing, empty, foreign or unrecorded instance is
// refused; a first install (nothing at all) and the recorded one pass.
func TestNativeCheckInstance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, n Native)
		reason string
		msg    func(n Native) string
	}{
		{name: "first install", setup: func(*testing.T, Native) {}},
		{name: "recorded", setup: func(t *testing.T, n Native) { recordInstance(t, n, "111") }},
		{name: "missing", setup: func(t *testing.T, n Native) {
			recordInstance(t, n, "111")
			if err := os.RemoveAll(n.InstanceDir()); err != nil {
				t.Fatal(err)
			}
		}, reason: "missing", msg: func(n Native) string {
			return "the native database " + n.InstanceDir() + " is missing: refusing to start an empty memory in its place. Put it back, or restore a backup: backsight restore <archive>"
		}},
		{name: "empty", setup: func(t *testing.T, n Native) {
			recordInstance(t, n, "111")
			if err := os.RemoveAll(n.DataDir()); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(n.DataDir(), 0o700); err != nil {
				t.Fatal(err)
			}
		}, reason: "empty", msg: func(n Native) string {
			return "the native database " + n.InstanceDir() + " is empty: refusing to start an empty memory in its place. Put it back, or restore a backup: backsight restore <archive>"
		}},
		{name: "another identity", setup: func(t *testing.T, n Native) {
			recordInstance(t, n, "111")
			if err := os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte("222\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, reason: "identity", msg: func(n Native) string {
			return "the native database " + n.InstanceDir() + " is not the one backsight recorded (identifier 222, recorded 111): refusing to start it. Put the right one back, or restore a backup: backsight restore <archive>"
		}},
		{name: "an empty identity file, no instance", setup: func(t *testing.T, n Native) {
			if err := os.WriteFile(n.IdentityFile(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, reason: "missing", msg: func(n Native) string {
			return "the native database " + n.InstanceDir() + " is missing: refusing to start an empty memory in its place. Put it back, or restore a backup: backsight restore <archive>"
		}},
		{name: "empty wins over unrecorded", setup: func(t *testing.T, n Native) {
			if err := os.MkdirAll(n.DataDir(), 0o700); err != nil {
				t.Fatal(err)
			}
		}, reason: "empty", msg: func(n Native) string {
			return "the native database " + n.InstanceDir() + " is empty: refusing to start an empty memory in its place. Put it back, or restore a backup: backsight restore <archive>"
		}},
		{name: "unrecorded", setup: func(t *testing.T, n Native) {
			recordInstance(t, n, "111")
			if err := os.Remove(n.IdentityFile()); err != nil {
				t.Fatal(err)
			}
		}, reason: "unrecorded", msg: func(n Native) string {
			return "the native database " + n.InstanceDir() + " has no recorded identity (" + n.IdentityFile() + " is missing): refusing to start it. Restore a backup to adopt one: backsight restore <archive>"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, _, _ := nativeFixture(t, secrets)
			rendered(t, n, secrets)
			tc.setup(t, n)
			err := n.CheckInstance(context.Background())
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("CheckInstance = %v, want nil", err)
				}
				return
			}
			var ge *GuardError
			if !errors.As(err, &ge) || ge.Reason != tc.reason {
				t.Fatalf("CheckInstance = %v, want a %s refusal", err, tc.reason)
			}
			if got, want := err.Error(), tc.msg(n); got != want {
				t.Errorf("message\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestNativeReadsAndRecordsTheIdentity(t *testing.T) {
	ctx := context.Background()
	n, _, tools := nativeFixture(t, secrets)
	rendered(t, n, secrets)
	touch(t, filepath.Join(n.DataDir(), "PG_VERSION"), 0o600)
	if err := os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte("7702339718603615110"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := n.RecordIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(n.IdentityFile())
	if err != nil || string(raw) != "7702339718603615110\n" {
		t.Errorf("identity %q %v", raw, err)
	}
	testtool.RequirePrivate(t, n.IdentityFile())
	c := tools.Calls[len(tools.Calls)-1]
	inst := n.installation()
	if c.Path != filepath.Join(inst, "bin", "pg_controldata") || !reflect.DeepEqual(c.Args, []string{"-D", n.DataDir()}) {
		t.Errorf("ran %s %v", c.Path, c.Args)
	}
	if !slices.Contains(c.Env, "LD_LIBRARY_PATH="+filepath.Join(inst, "lib")) || !slices.Contains(c.Env, "LANG=C.UTF-8") {
		t.Errorf("env %v", c.Env)
	}
	// No identifier in the output: an error, nothing recorded.
	if err := os.Remove(filepath.Join(n.DataDir(), "fake-sysid")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(n.IdentityFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := n.ReadIdentity(ctx); err == nil {
		t.Error("ReadIdentity without an identifier succeeded")
	}
	if err := n.RecordIdentity(ctx); err == nil || exists(n.IdentityFile()) {
		t.Errorf("RecordIdentity without an identifier: %v, file %v", err, exists(n.IdentityFile()))
	}
}

// fakeProc is a /proc tree: each process's cmdline (NUL-separated),
// cgroup and, when cwd is set, its cwd link.
type fakeProcess struct {
	pid    string
	argv   []string
	cgroup string
	cwd    string
}

func fakeProc(t *testing.T, procs ...fakeProcess) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range procs {
		dir := filepath.Join(root, p.pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(p.argv, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(p.cgroup), 0o644); err != nil {
			t.Fatal(err)
		}
		if p.cwd != "" {
			if err := os.Symlink(p.cwd, filepath.Join(dir, "cwd")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

const scope = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/"

// Requirement 3: a postmaster on the instance's data directory outside
// the db unit's cgroup is a stray; one inside it, one on another data
// directory and the postmaster's own children are not.
func TestNativeStraysAreFoundByDataDirectoryAndCgroup(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: /proc and cgroups are Linux's")
	n, _, _ := nativeFixture(t, secrets)
	data := n.DataDir()
	pg := filepath.Join(n.installation(), "bin", "postgres")
	n.Proc = fakeProc(t,
		fakeProcess{pid: "4242", argv: []string{pg, "-D", data, "-p", "5432"}, cgroup: scope + "foo.scope\n", cwd: data},
		fakeProcess{pid: "4243", argv: []string{pg, "-D", data, "-p", "5432"}, cgroup: scope + "backsight-db.service\n", cwd: data},
		fakeProcess{pid: "4244", argv: []string{pg, "-D", "/other/data"}, cgroup: scope + "foo.scope\n", cwd: "/other/data"},
		fakeProcess{pid: "4245", argv: []string{"postgres: checkpointer "}, cgroup: scope + "foo.scope\n", cwd: data},
		fakeProcess{pid: "4246", argv: []string{"postgres", "-p", "5433"}, cgroup: scope + "bar.scope\n", cwd: data},       // PGDATA, no -D
		fakeProcess{pid: "4247", argv: []string{pg, "-D" + data}, cgroup: scope + "xbacksight-db.service\n"},               // another unit's name ending like ours
		fakeProcess{pid: "4248", argv: []string{"/usr/bin/python3", "-D", data}, cgroup: scope + "foo.scope\n", cwd: data}, // not a postgres
	)
	got, err := n.Strays()
	if err != nil {
		t.Fatal(err)
	}
	want := []Stray{
		{PID: 4242, Cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/foo.scope"},
		{PID: 4246, Cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/bar.scope"},
		{PID: 4247, Cgroup: "/user.slice/user-1000.slice/user@1000.service/app.slice/xbacksight-db.service"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Strays = %+v\nwant %+v", got, want)
	}
}

// A stray is stopped with the bundled pg_ctl (ruling R22), never pg0,
// and must be gone; one still alive after the wait is an error naming it.
func TestNativeStopStraysStopsThemWithPgCtl(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: /proc and cgroups are Linux's")
	ctx := context.Background()
	n, _, tools := nativeFixture(t, secrets)
	data := n.DataDir()
	n.Proc = fakeProc(t, fakeProcess{pid: "4242", argv: []string{"postgres", "-D", data}, cgroup: scope + "foo.scope\n", cwd: data})
	strays, err := n.Strays()
	if err != nil || len(strays) != 1 {
		t.Fatalf("Strays = %v %v", strays, err)
	}

	if err := n.StopStrays(ctx, nil); err != nil || len(tools.Calls) != 0 {
		t.Errorf("StopStrays(nil) = %v, ran %v", err, tools.Calls)
	}

	// Ruling R36: pg_ctl stops postmaster.pid's pid, so it runs only when
	// that is the stray's; else nothing is stopped.
	pidFile := filepath.Join(data, "postmaster.pid")
	err = n.StopStrays(ctx, strays)
	if err == nil || !strings.Contains(err.Error(), "4242") || !strings.Contains(err.Error(), "names no pid") {
		t.Errorf("no postmaster.pid: %v", err)
	}
	touch(t, filepath.Join(data, "PG_VERSION"), 0o600)
	if err := os.WriteFile(pidFile, []byte("4243\n"+data+"\n1730000000\n5432\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = n.StopStrays(ctx, strays)
	if err == nil || !strings.Contains(err.Error(), "pid 4242") || !strings.Contains(err.Error(), "names pid 4243") {
		t.Errorf("postmaster.pid naming another pid: %v, want both pids named", err)
	}
	if len(tools.Calls) != 0 {
		t.Errorf("pg_ctl ran on another pid's postmaster.pid: %+v", tools.Calls)
	}
	if err := os.WriteFile(pidFile, []byte("4242\n"+data+"\n1730000000\n5432\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldPoll := strayPoll
	strayPoll = time.Millisecond
	t.Cleanup(func() { strayPoll = oldPoll })
	n.StrayWait = 30 * time.Millisecond
	err = n.StopStrays(ctx, strays)
	if err == nil || !strings.Contains(err.Error(), "4242") || !strings.Contains(err.Error(), data) {
		t.Errorf("a stray still alive: %v, want an error naming pid 4242 and %s", err, data)
	}

	tools.Calls = nil
	tools.Effect = func(c toolCall) {
		if filepath.Base(c.Path) == "pg_ctl" {
			_ = os.RemoveAll(filepath.Join(n.Proc, "4242"))
		}
	}
	if err := n.StopStrays(ctx, strays); err != nil {
		t.Fatal(err)
	}
	if len(tools.Calls) != 1 {
		t.Fatalf("ran %d tools: %+v", len(tools.Calls), tools.Calls)
	}
	c := tools.Calls[0]
	inst := n.installation()
	if c.Path != filepath.Join(inst, "bin", "pg_ctl") || !reflect.DeepEqual(c.Args[:5], []string{"stop", "-D", data, "-m", "fast"}) {
		t.Errorf("ran %s %v, want pg_ctl stop -D %s -m fast", c.Path, c.Args, data)
	}
	if !slices.Contains(c.Env, "LD_LIBRARY_PATH="+filepath.Join(inst, "lib")) || !slices.Contains(c.Env, "HOME="+n.Home()) {
		t.Errorf("env %v", c.Env)
	}
	for _, kv := range c.Env {
		if strings.HasPrefix(kv, "PGPASSWORD=") {
			t.Errorf("pg_ctl got %s", kv)
		}
	}
}

// Requirement 1: fsync off after a start stops and disables both units.
func TestNativeStartRefusesFsyncOff(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: the Linux units are the systemd host's")
	ctx := context.Background()
	for _, start := range []string{"Start", "StartDatabase"} {
		t.Run(start, func(t *testing.T) {
			n, sys, tools := nativeFixture(t, secrets)
			installed(t, n, secrets)
			tools.Out = dbToolsOut("off")
			var err error
			if start == "Start" {
				err = n.Start(ctx)
			} else {
				err = n.StartDatabase(ctx)
			}
			want := "fsync is off on the native database: refusing to run it (an OS crash could corrupt it); check backsight-db.service's settings"
			if err == nil || err.Error() != want {
				t.Fatalf("%s = %v, want %q", start, err, want)
			}
			lines := sys.lines()
			if !slices.Contains(lines, "systemctl --user disable --now backsight-server.service backsight-db.service") {
				t.Errorf("both units not disabled:\n%s", strings.Join(lines, "\n"))
			}
			for _, l := range lines {
				if strings.Contains(l, "enable") && strings.Contains(l, "backsight-server.service") && !strings.Contains(l, "disable") {
					t.Errorf("the server was started on fsync off: %s", l)
				}
			}
		})
	}
}

// R29: the first start creates the instance and records its identity
// before the db unit starts, so native-check passes on it.
func TestNativeFirstStartRecordsTheIdentity(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: the Linux units are the systemd host's")
	ctx := context.Background()
	n, sys, tools := nativeFixture(t, secrets)
	n.MayCreate = true
	rendered(t, n, secrets)
	created := 0
	tools.Effect = func(c toolCall) {
		if c.Path == "systemd-run" {
			created++
			touch(t, filepath.Join(n.DataDir(), "PG_VERSION"), 0o600)
			if err := os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte("7702339718603615110\n"), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	var recordedAtDBStart string
	sys.Out = func(c []string) ([]byte, error) {
		if strings.Join(c, " ") == "systemctl --user enable --now backsight-db.service" {
			raw, _ := os.ReadFile(n.IdentityFile())
			recordedAtDBStart = string(raw)
		}
		return nil, nil
	}
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Errorf("the instance was created %d times", created)
	}
	if recordedAtDBStart != "7702339718603615110\n" {
		t.Errorf("identity when the db unit started: %q", recordedAtDBStart)
	}
	if st, err := os.Stat(n.IdentityFile()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("identity file %v %v", st, err)
	}
	if err := n.CheckInstance(ctx); err != nil {
		t.Errorf("native-check after the first start: %v", err)
	}
	if err := n.Start(ctx); err != nil || created != 1 {
		t.Errorf("second Start: %v, created %d times", err, created)
	}
}

// A lost instance whose identity was recorded is never replaced by a
// fresh one: Start refuses before enabling anything.
func TestNativeStartNeverRecreatesALostInstance(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: the Linux units are the systemd host's")
	ctx := context.Background()
	for _, tc := range []struct {
		name, reason string
		lose         func(n Native) error
	}{
		{"missing", "missing", func(n Native) error { return os.RemoveAll(n.InstanceDir()) }},
		{"unrecorded", "unrecorded", func(n Native) error { return os.Remove(n.IdentityFile()) }},
		{"foreign", "identity", func(n Native) error {
			return os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte("999\n"), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, sys, tools := nativeFixture(t, secrets)
			n.MayCreate = true // allowed to create, still never over a lost one
			installed(t, n, secrets)
			if err := tc.lose(n); err != nil {
				t.Fatal(err)
			}
			for _, start := range []func(context.Context) error{n.Start, n.StartDatabase} {
				err := start(ctx)
				var ge *GuardError
				if !errors.As(err, &ge) || ge.Reason != tc.reason {
					t.Errorf("start = %v, want the %s refusal", err, tc.reason)
				}
			}
			for _, c := range tools.Calls {
				if c.Path == "systemd-run" {
					t.Error("a new instance was created")
				}
			}
			for _, l := range sys.lines() {
				if strings.Contains(l, "enable") || strings.Contains(l, "start") {
					t.Errorf("a unit was started: %s", l)
				}
			}
		})
	}
}

// systemdRuns counts the instance creations among the tool calls.
func systemdRuns(tools *fakeTools) int {
	k := 0
	for _, c := range tools.Calls {
		if c.Path == "systemd-run" {
			k++
		}
	}
	return k
}

// Ruling R35: without MayCreate, nothing there (a wiped native directory,
// another compose_dir) is the missing refusal, never a new empty memory;
// with it, the first start creates the database and records it.
func TestNativeStartCreatesOnlyWhereAllowed(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: the Linux units are the systemd host's")
	ctx := context.Background()
	n, sys, tools := nativeFixture(t, secrets)
	rendered(t, n, secrets)
	for _, start := range []func(context.Context) error{n.Start, n.StartDatabase} {
		err := start(ctx)
		var ge *GuardError
		if !errors.As(err, &ge) || ge.Reason != "missing" {
			t.Errorf("start without MayCreate = %v, want the missing refusal", err)
		}
	}
	if k := systemdRuns(tools); k != 0 {
		t.Errorf("created %d times without MayCreate", k)
	}
	for _, l := range sys.lines() {
		if strings.Contains(l, "enable") || strings.Contains(l, "start") {
			t.Errorf("a unit was started: %s", l)
		}
	}
	if exists(n.InstanceDir()) || exists(n.IdentityFile()) {
		t.Error("something was created without MayCreate")
	}

	n.MayCreate = true
	tools.Effect = func(c toolCall) {
		if c.Path == "systemd-run" {
			touch(t, filepath.Join(n.DataDir(), "PG_VERSION"), 0o600)
			_ = os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte("42\n"), 0o600)
		}
	}
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(n.IdentityFile()); string(raw) != "42\n" || systemdRuns(tools) != 1 {
		t.Errorf("with MayCreate: identity %q, created %d times", raw, systemdRuns(tools))
	}
}

// The creation needs both the instance and the identity to be absent
// (ENOENT): an empty identity file, or an instance path that cannot be
// read, never creates.
func TestNativeCreationNeedsBothAbsent(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: the Linux units are the systemd host's")
	ctx := context.Background()
	t.Run("an empty identity file", func(t *testing.T) {
		n, _, tools := nativeFixture(t, secrets)
		n.MayCreate = true
		rendered(t, n, secrets)
		if err := os.WriteFile(n.IdentityFile(), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := n.Start(ctx)
		var ge *GuardError
		if !errors.As(err, &ge) || ge.Reason != "missing" || systemdRuns(tools) != 0 {
			t.Errorf("Start = %v, created %d times; want the missing refusal and no creation", err, systemdRuns(tools))
		}
	})
	t.Run("an instance path that is not a directory's", func(t *testing.T) {
		n, sys, tools := nativeFixture(t, secrets)
		n.MayCreate = true
		rendered(t, n, secrets)
		touch(t, filepath.Dir(n.InstanceDir()), 0o600) // .pg0/instances is a file: ENOTDIR, not ENOENT
		err := n.Start(ctx)
		var ge *GuardError
		if err == nil || errors.As(err, &ge) || systemdRuns(tools) != 0 {
			t.Errorf("Start = %v, created %d times; want the stat error and no creation", err, systemdRuns(tools))
		}
		if len(sys.Calls) != 0 {
			t.Errorf("ran %v", sys.lines())
		}
	})
}

// A new instance whose identity cannot be recorded is removed, so the
// next start creates it again instead of refusing it as unrecorded.
func TestNativeFailedIdentityRemovesTheNewInstance(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: the Linux units are the systemd host's")
	ctx := context.Background()
	n, sys, tools := nativeFixture(t, secrets)
	n.MayCreate = true
	rendered(t, n, secrets)
	tools.Effect = func(c toolCall) {
		if c.Path == "systemd-run" {
			touch(t, filepath.Join(n.DataDir(), "PG_VERSION"), 0o600) // no fake-sysid: pg_controldata prints no identifier
		}
	}
	err := n.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "recording the new native database's identity") {
		t.Fatalf("Start = %v", err)
	}
	if exists(n.InstanceDir()) || exists(n.IdentityFile()) {
		t.Errorf("left instance %v, identity %v", exists(n.InstanceDir()), exists(n.IdentityFile()))
	}
	if len(sys.Calls) != 0 {
		t.Errorf("units touched after the failed creation: %v", sys.lines())
	}
	tools.Effect = func(c toolCall) {
		if c.Path == "systemd-run" {
			touch(t, filepath.Join(n.DataDir(), "PG_VERSION"), 0o600)
			_ = os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte("43\n"), 0o600)
		}
	}
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(n.IdentityFile()); string(raw) != "43\n" || systemdRuns(tools) != 2 {
		t.Errorf("second Start: identity %q, created %d times", raw, systemdRuns(tools))
	}
}

// Final review 7: on an install whose config.toml names docker (Cfg's
// runtime, or NotInUse for a switch's target built before config.toml
// names native), a refused native database is what a switch left: the
// way out is server runtime clean, never restore, which acts on Docker's.
func TestNativeRefusalOnADockerInstallPointsAtClean(t *testing.T) {
	setups := map[string]func(t *testing.T, n Native){
		"missing": func(t *testing.T, n Native) {
			if err := os.WriteFile(n.IdentityFile(), []byte("111\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"empty": func(t *testing.T, n Native) {
			if err := os.MkdirAll(n.DataDir(), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"identity": func(t *testing.T, n Native) {
			recordInstance(t, n, "111")
			if err := os.WriteFile(filepath.Join(n.DataDir(), "fake-sysid"), []byte("222\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"unrecorded": func(t *testing.T, n Native) {
			recordInstance(t, n, "111")
			if err := os.Remove(n.IdentityFile()); err != nil {
				t.Fatal(err)
			}
		},
	}
	for _, how := range []string{"config names docker", "a switch's target"} {
		for reason, setup := range setups {
			t.Run(how+"/"+reason, func(t *testing.T) {
				n, _, _ := nativeFixture(t, secrets)
				rendered(t, n, secrets)
				setup(t, n)
				if how == "config names docker" {
					n.Cfg.Server.Runtime = ""
				} else {
					n.NotInUse = true
				}
				for _, err := range []error{n.CheckInstance(context.Background()), n.StartDatabase(context.Background())} {
					var ge *GuardError
					if !errors.As(err, &ge) || ge.Reason != reason || !ge.Leftover {
						t.Fatalf("%v: want a %s refusal of leftover data", err, reason)
					}
					msg := err.Error()
					if !strings.Contains(msg, "native data a runtime switch left behind") || !strings.Contains(msg, "backsight server runtime clean") ||
						strings.Contains(msg, "backsight restore") {
						t.Errorf("message %q", msg)
					}
				}
			})
		}
	}
	// On native, restore stays the way out.
	n, _, _ := nativeFixture(t, secrets)
	rendered(t, n, secrets)
	setups["identity"](t, n)
	if err := n.CheckInstance(context.Background()); err == nil || !strings.Contains(err.Error(), "backsight restore <archive>") || strings.Contains(err.Error(), "runtime clean") {
		t.Errorf("on native: %v", err)
	}
}
