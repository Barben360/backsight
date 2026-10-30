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
	if st, err := os.Stat(n.Home()); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("home: %v, %v; want 0700", st, err)
	}

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
			if !strings.Contains(err.Error(), filepath.Join(installation(n), "bin")) || !strings.Contains(err.Error(), "PostgreSQL") {
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
