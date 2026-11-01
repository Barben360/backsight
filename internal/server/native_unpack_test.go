package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/testtool"
)

// pgTools are the bundled programs the native runtime runs from pg0's
// installation: the db unit, the identity check, VerifyDump and the
// database tools.
var pgTools = []string{"initdb", "postgres", "pg_ctl", "pg_controldata", "psql", "pg_dump", "pg_restore", "dropdb", "createdb"}

// pg0Of is the pg0 a venv carries.
func pg0Of(venv string) string {
	return filepath.Join(venv, "lib", "python3.11", "site-packages", "pg0", "bin", "pg0")
}

// unitEnv is the -E KEY=value a systemd-run call sets, "" when none.
func unitEnv(args []string, key string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, key+"="); ok && i > 0 && args[i-1] == "-E" {
			return v
		}
	}
	return ""
}

// isUnpack: the call is the systemd-run that has pg0 unpack its Postgres.
func isUnpack(c toolCall) bool {
	return c.Path == "systemd-run" && len(c.Args) > 0 && strings.Contains(c.Args[len(c.Args)-1], " start --name backsight-unpack ")
}

// pg0Effects does to the disk what the real pg0 does in the unpack unit:
// the first start extracts the installation under HOME, creates the
// throwaway instance and leaves a .tmp* in TMPDIR; drop removes the
// instance.
func pg0Effects(c toolCall) {
	if !isUnpack(c) {
		return
	}
	home, tmp := unitEnv(c.Args, "HOME"), unitEnv(c.Args, "TMPDIR")
	for _, b := range pgTools {
		p := filepath.Join(home, ".pg0", "installation", nativelock.PGInstallation, "bin", b)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	}
	vc := filepath.Join(home, ".pg0", "installation", nativelock.PGInstallation, "share", "extension", "vector.control")
	_ = os.MkdirAll(filepath.Dir(vc), 0o755)
	_ = os.WriteFile(vc, []byte("default_version = '0.8.5'\n"), 0o644)
	inst := filepath.Join(home, ".pg0", "instances", "backsight-unpack")
	_ = os.MkdirAll(filepath.Join(inst, "data"), 0o700)
	_ = os.WriteFile(filepath.Join(inst, "instance.json"), []byte("{}"), 0o600)
	if tmp != "" {
		_ = os.MkdirAll(filepath.Join(tmp, ".tmpAbCdEf"), 0o700)
		_ = os.WriteFile(filepath.Join(tmp, ".tmpAbCdEf", ".pgpass"), []byte("x"), 0o600)
	}
	if strings.Contains(c.Args[len(c.Args)-1], " drop --force --name backsight-unpack") {
		_ = os.RemoveAll(inst)
	}
}

// missingIs127 fails a call the way the shell does when the program is not
// there: an absolute tool path, or a program a systemd-run script runs,
// that does not exist exits 127.
func missingIs127(c toolCall) error {
	if filepath.IsAbs(c.Path) && !exists(c.Path) {
		return errors.New("exit status 127: " + c.Path + ": not found")
	}
	if c.Path != "systemd-run" || len(c.Args) == 0 {
		return nil
	}
	for _, w := range strings.Fields(c.Args[len(c.Args)-1]) {
		w = strings.Trim(w, "'")
		if filepath.IsAbs(w) && strings.Contains(w, "/bin/") && !strings.HasPrefix(w, "/bin/") && !exists(w) {
			return errors.New("exit status 127: " + w + ": not found")
		}
	}
	return nil
}

// fetchedButPG is a native root as a first switch to native finds it
// after the venv was built: uv, Python, the venv (with its pg0) and the
// models, and no .pg0 under home/.
func fetchedButPG(t *testing.T, n Native) {
	t.Helper()
	installedUV(t, n)
	touch(t, n.PythonBin(), 0o755)
	completeVenv(t, n, n.Version())
	touch(t, pg0Of(n.VenvDir(n.Version())), 0o755)
	touch(t, filepath.Join(n.HF(), modelsDone), 0o644)
	if err := os.WriteFile(filepath.Join(n.HF(), modelsDone), []byte(nativelock.Hash12()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installation(n Native) string {
	return filepath.Join(n.Home(), ".pg0", "installation", nativelock.PGInstallation)
}

// The bug a real round trip found: pg0 extracts its Postgres at its first
// start only, and a switch to native verifies the archive with those
// binaries before anything starts. Fetch now has pg0 unpack it, in a
// throwaway instance it drops, never the hindsight one.
func TestNativeFetchUnpacksPostgres(t *testing.T) {
	tools := &fakeTools{Fail: missingIs127, Effect: pg0Effects}
	n := newNative(t, tools)
	fetchedButPG(t, n)
	withFreePort(t, 28799)
	if exists(filepath.Join(n.Home(), ".pg0")) {
		t.Fatal("the fixture already has .pg0")
	}
	// The server's own instance, were there one, is never touched.
	keep := filepath.Join(n.InstanceDir(), "data", "PG_VERSION")
	touch(t, keep, 0o600)

	if err := n.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, b := range pgTools {
		if p := filepath.Join(installation(n), "bin", b); !isRegular(p) {
			t.Errorf("%s missing after Fetch", p)
		}
	}
	if !isRegular(filepath.Join(installation(n), ".backsight-unpacked")) {
		t.Error("no .backsight-unpacked marker after a successful unpack")
	}
	i := slices.IndexFunc(tools.Calls, isUnpack)
	if i < 0 || len(tools.Calls) != 1 {
		t.Fatalf("calls %+v, want the unpack alone (everything else present)", tools.Calls)
	}
	c := tools.Calls[i]
	script := c.Args[len(c.Args)-1]
	pg0 := shellWord(pg0Of(n.VenvDir(n.Version())))
	for _, w := range []string{pg0 + " start --name backsight-unpack --port 28799 ", "-c listen_addresses=127.0.0.1",
		pg0 + " drop --force --name backsight-unpack"} {
		if !strings.Contains(script, w) {
			t.Errorf("the unpack script lacks %q:\n%s", w, script)
		}
	}
	if strings.Contains(script, "hindsight") {
		t.Errorf("the unpack names the hindsight instance:\n%s", script)
	}
	for _, f := range []string{"--user", "--wait", "--collect", "--pipe"} {
		if !slices.Contains(c.Args, f) {
			t.Errorf("systemd-run lacks %s: %q", f, c.Args)
		}
	}
	for _, p := range []string{"KillMode=control-group", "RuntimeMaxSec=600", "UnsetEnvironment=" + unsetKeys} {
		if i := slices.Index(c.Args, p); i < 1 || c.Args[i-1] != "-p" {
			t.Errorf("the unpack unit lacks -p %s: %q", p, c.Args)
		}
	}
	if unitEnv(c.Args, "HOME") != n.Home() {
		t.Errorf("HOME %q, want %s", unitEnv(c.Args, "HOME"), n.Home())
	}
	tmp := unitEnv(c.Args, "TMPDIR")
	if !under(n.Home(), tmp) {
		t.Errorf("TMPDIR %q is not under %s (ruling R21)", tmp, n.Home())
	}
	if exists(tmp) {
		t.Errorf("TMPDIR %s left after the unpack", tmp)
	}
	if exists(filepath.Join(n.Home(), ".pg0", "instances", "backsight-unpack")) {
		t.Error("the throwaway instance is left")
	}
	if !exists(keep) {
		t.Error("the hindsight instance was touched")
	}
	testtool.RequirePrivate(t, n.Home())

	t.Run("present, nothing runs", func(t *testing.T) {
		before := len(tools.Calls)
		if err := n.Fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(tools.Calls) != before {
			t.Errorf("ran %+v", tools.Calls[before:])
		}
	})

	t.Run("one binary missing, unpacked again", func(t *testing.T) {
		if err := os.Remove(filepath.Join(installation(n), "bin", "pg_controldata")); err != nil {
			t.Fatal(err)
		}
		before := len(tools.Calls)
		withFreePort(t, 28798)
		if err := n.Fetch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(tools.Calls) != before+1 || !isUnpack(tools.Calls[before]) {
			t.Errorf("ran %+v, want the unpack", tools.Calls[before:])
		}
	})
}

// A failed unpack is an error that says what is missing, and leaves no
// throwaway instance and no TMPDIR.
func TestNativeFetchUnpackFailure(t *testing.T) {
	for _, name := range []string{"pg0 fails", "pg0 extracts nothing"} {
		t.Run(name, func(t *testing.T) {
			tools := &fakeTools{}
			n := newNative(t, tools)
			fetchedButPG(t, n)
			withFreePort(t, 28799)
			var tmp string
			// pg0 got as far as creating its instance, then failed (or
			// extracted nothing).
			tools.Fail = func(c toolCall) error {
				if !isUnpack(c) {
					return nil
				}
				tmp = unitEnv(c.Args, "TMPDIR")
				_ = os.MkdirAll(filepath.Join(n.Home(), ".pg0", "instances", "backsight-unpack", "data"), 0o700)
				if name == "pg0 fails" {
					return errors.New("exit status 1: could not bind")
				}
				return nil
			}
			err := n.Fetch(context.Background())
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), installation(n)) || !strings.Contains(err.Error(), "PostgreSQL") {
				t.Errorf("the error does not name what is missing: %v", err)
			}
			if name == "pg0 fails" && !strings.Contains(err.Error(), "could not bind") {
				t.Errorf("pg0's error is lost: %v", err)
			}
			if name == "pg0 extracts nothing" && !strings.Contains(err.Error(), "initdb") {
				t.Errorf("the missing binary is not named: %v", err)
			}
			if tmp == "" || exists(tmp) {
				t.Errorf("TMPDIR %q left (or never set)", tmp)
			}
			if exists(filepath.Join(n.Home(), ".pg0", "instances", "backsight-unpack")) {
				t.Error("the throwaway instance is left")
			}
		})
	}
}

// The switch's verify step on a root that never ran native: before the
// fix VerifyDump ran <installation>/bin/initdb, which was not there (exit
// 127). After Fetch, the switch's download step, it runs.
func TestNativeVerifyDumpAfterFetchOnAFreshRoot(t *testing.T) {
	n, _, tools := nativeFixture(t, secrets)
	n.Tools = tools
	fetchedButPG(t, n)
	withFreePort(t, 28799, 28800)
	sys := &ctxSys{}
	n.Sys = sys
	tools.Fail = missingIs127
	tools.Effect = pg0Effects
	tools.Out = func(c toolCall) []byte {
		if filepath.Base(c.Path) == "psql" && slices.Contains(c.Args, "select count(*) from documents") {
			return []byte("3\n")
		}
		return nil
	}
	if err := n.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	counts, err := n.VerifyDump(context.Background(), filepath.Join(t.TempDir(), "db.dump"), []string{"documents"})
	if err != nil || counts["documents"] != 3 {
		t.Fatalf("VerifyDump after Fetch = %v, %v", counts, err)
	}
	for _, c := range tools.Calls {
		if c.Path == "systemd-run" && strings.Contains(strings.Join(c.Args, " "), "--name hindsight") {
			t.Errorf("a unit named the hindsight instance: %q", c.Args)
		}
	}
	if exists(n.InstanceDir()) {
		t.Error("the hindsight instance was created outside ensureInstance")
	}
}

// orderSys is the user manager: it records each call, whether its context
// was live, and, at each stop of the unpack unit, whether the unit's
// TMPDIR and throwaway instance were still there (a stop must come before
// their removal).
type orderSys struct {
	n       Native
	calls   []string
	live    []bool
	stopped []bool // at each stop: TMPDIR and instance still there
}

func (s *orderSys) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, line)
	s.live = append(s.live, ctx.Err() == nil)
	if line == "systemctl --user stop backsight-pg-unpack.service" {
		s.stopped = append(s.stopped, exists(filepath.Join(s.n.Home(), "tmp-unpack")) &&
			exists(filepath.Join(s.n.Home(), ".pg0", "instances", "backsight-unpack")))
	}
	return nil, nil
}

// A cancel (Ctrl-C, a timeout) only kills systemd-run's client: the
// transient unit, pg0 and its postgres run on. The unit is stopped, on a
// live context, before its TMPDIR and instance are removed; a leftover
// unit of that name is stopped before a new unpack, which then succeeds.
func TestNativeFetchUnpackCancelled(t *testing.T) {
	tools := &fakeTools{}
	n := newNative(t, tools)
	sys := &orderSys{n: n}
	n.Sys = sys
	fetchedButPG(t, n)
	withFreePort(t, 28799, 28798)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tools.Fail = func(c toolCall) error {
		if !isUnpack(c) {
			return nil
		}
		if unit := argOf(c.Args, "--unit"); unit != "backsight-pg-unpack.service" {
			t.Errorf("the unpack unit is %q", unit)
		}
		// pg0 is running in the unit: its instance and temporary files.
		_ = os.MkdirAll(filepath.Join(n.Home(), ".pg0", "instances", "backsight-unpack", "data"), 0o700)
		_ = os.MkdirAll(filepath.Join(unitEnv(c.Args, "TMPDIR"), ".tmpXyZ"), 0o700)
		cancel()
		return context.Canceled
	}
	if err := n.Fetch(ctx); err == nil {
		t.Fatal("a cancelled unpack reported no error")
	}
	stop := slices.Index(sys.calls, "systemctl --user stop backsight-pg-unpack.service")
	last := slices.Index(sys.calls[stop+1:], "systemctl --user stop backsight-pg-unpack.service")
	if stop < 0 || last < 0 {
		t.Fatalf("the unit was not stopped after the cancel (want one stop before the unpack, one after): %q", sys.calls)
	}
	last += stop + 1
	if !sys.live[last] {
		t.Error("the stop ran on the cancelled context: it would not run at all")
	}
	if !sys.stopped[len(sys.stopped)-1] {
		t.Error("TMPDIR or the instance was removed before the unit was stopped")
	}
	if !slices.Contains(sys.calls[last:], "systemctl --user reset-failed backsight-pg-unpack.service") {
		t.Errorf("no reset-failed after the stop: %q", sys.calls[last:])
	}
	if exists(filepath.Join(n.Home(), "tmp-unpack")) || exists(filepath.Join(n.Home(), ".pg0", "instances", "backsight-unpack")) {
		t.Error("TMPDIR or the throwaway instance is left after the cancel")
	}

	// A rerun: the leftover unit is stopped before the new one starts.
	sys.calls, sys.live = nil, nil
	var before []string
	tools.Fail = func(c toolCall) error {
		if isUnpack(c) {
			before = slices.Clone(sys.calls)
		}
		return nil
	}
	tools.Effect = pg0Effects
	if err := n.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch after a cancelled unpack: %v", err)
	}
	if !slices.Contains(before, "systemctl --user stop backsight-pg-unpack.service") {
		t.Errorf("a leftover unit was not stopped before the new unpack: %q", before)
	}
	if missing := n.pgMissing(); missing != "" {
		t.Errorf("%s missing after the rerun", missing)
	}
}

// fullInstallation is pg0's Postgres as createInstance's first start
// leaves it: every file the runtime runs, no backsight marker.
func fullInstallation(t *testing.T, n Native) {
	t.Helper()
	for _, b := range pgTools {
		touch(t, filepath.Join(installation(n), "bin", b), 0o755)
	}
	touch(t, filepath.Join(installation(n), "share", "extension", "vector.control"), 0o644)
}

// An installation is complete when every file the runtime runs is there:
// one extracted by createInstance (no marker) is accepted and marked; a
// partial one (a cut extraction, marker or not) is removed and unpacked
// again.
func TestNativeFetchUnpackCompleteness(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: Linux's uv and unpack; Windows' Fetch (uv.exe, copyPostgres) has its own tests")
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, n Native)
		unpack bool
	}{
		{"complete without marker", fullInstallation, false},
		{"partial, no pgvector", func(t *testing.T, n Native) {
			fullInstallation(t, n)
			_ = os.Remove(filepath.Join(installation(n), "share", "extension", "vector.control"))
		}, true},
		{"partial, no pg_dump", func(t *testing.T, n Native) {
			fullInstallation(t, n)
			_ = os.Remove(filepath.Join(installation(n), "bin", "pg_dump"))
		}, true},
		{"marked but partial", func(t *testing.T, n Native) {
			fullInstallation(t, n)
			touch(t, filepath.Join(installation(n), ".backsight-unpacked"), 0o644)
			_ = os.Remove(filepath.Join(installation(n), "bin", "psql"))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := &fakeTools{Effect: pg0Effects}
			n := newNative(t, tools)
			fetchedButPG(t, n)
			withFreePort(t, 28799)
			tc.setup(t, n)
			stray := filepath.Join(installation(n), "lib", "half-written.so")
			touch(t, stray, 0o644)
			// The unpack starts from no installation: pg0 extracts only
			// where none is.
			tools.Fail = func(c toolCall) error {
				if isUnpack(c) && exists(installation(n)) {
					return errors.New("pg0 found a partial installation and skipped the extraction")
				}
				return nil
			}
			if err := n.Fetch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := slices.ContainsFunc(tools.Calls, isUnpack); got != tc.unpack {
				t.Errorf("unpacked %v, want %v", got, tc.unpack)
			}
			if exists(stray) == tc.unpack {
				t.Errorf("the partial installation's stray file present %v, want %v", exists(stray), !tc.unpack)
			}
			if !isRegular(filepath.Join(installation(n), ".backsight-unpacked")) {
				t.Error("the complete installation is not marked")
			}
			if items, _, _ := n.Downloads(context.Background()); len(items) != 0 {
				t.Errorf("Downloads after Fetch: %q", items)
			}
		})
	}
}
