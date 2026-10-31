//go:build windows

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/server/nativelock"
	"github.com/Barben360/backsight/internal/supervise"
	"github.com/Barben360/backsight/internal/testtool"
)

// Windows has no execute bit: uv is there when uv.exe is a non-empty file.
func TestUVInstalledOnWindows(t *testing.T) {
	n := newNative(t, &fakeTools{})
	n.GOOS = "windows"
	if filepath.Base(n.UV()) != "uv.exe" {
		t.Fatalf("UV() = %s, want uv.exe", n.UV())
	}
	if n.uvInstalled() {
		t.Fatal("uv installed in an empty root")
	}
	touch(t, n.UV(), 0o644) // no x bit to read
	if !n.uvInstalled() {
		t.Error("uv.exe present but not installed")
	}
	if err := os.WriteFile(n.UV(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if n.uvInstalled() {
		t.Error("an empty uv.exe counts as installed")
	}
}

// winPassword is the fixture's db_password: no argv, no environment, no
// log may hold it.
const winPassword = "s3cret-Windows-db-pw"

// withPg0Home makes pg0's own directory (%USERPROFILE%\.pg0) a temporary
// one.
func withPg0Home(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".pg0")
	old := pg0Home
	pg0Home = func() (string, error) { return dir, nil }
	t.Cleanup(func() { pg0Home = old })
	return dir
}

// pgInstallation writes a complete Postgres installation at dir, as pg0
// extracts it: every program the runtime runs, pgvector, and a library.
func pgInstallation(t *testing.T, dir string) {
	t.Helper()
	for _, f := range append(slices.Clone(pgRequired), "lib/plpgsql.dll") {
		if strings.HasPrefix(f, "bin/") {
			f += ".exe"
		}
		touch(t, filepath.Join(dir, filepath.FromSlash(f)), 0o755)
	}
}

// winPg0Effects does what pg0 does on Windows: start extracts the
// installation into pg0's directory (when none is there), records the
// instance there and creates the cluster in --data-dir (its identifier in
// fake-sysid, for dbToolsOut's pg_controldata); drop forgets the instance.
func winPg0Effects(t *testing.T, pg0dir string) func(toolCall) {
	return func(c toolCall) {
		if filepath.Base(c.Path) != "pg0.exe" || len(c.Args) == 0 {
			return
		}
		arg := func(flag string) string {
			if i := slices.Index(c.Args, flag); i >= 0 && i+1 < len(c.Args) {
				return c.Args[i+1]
			}
			return ""
		}
		inst := filepath.Join(pg0dir, "instances", arg("--name"))
		switch c.Args[0] {
		case "start":
			if inst := filepath.Join(pg0dir, "installation", nativelock.PGInstallation); !exists(inst) {
				pgInstallation(t, inst)
			}
			touch(t, filepath.Join(inst, "instance.json"), 0o644)
			if d := arg("--data-dir"); d != "" {
				touch(t, filepath.Join(d, "PG_VERSION"), 0o600)
				if err := os.WriteFile(filepath.Join(d, "fake-sysid"), []byte("7702339718603615110\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		case "drop":
			_ = os.RemoveAll(inst)
		}
	}
}

// winInstall is a Windows install past Fetch, before its first start:
// secrets.toml with winPassword, server.env naming the database's port,
// the venv complete with pg0 and the runtime DLLs, pg0's directory a
// temporary one; the tools are fakes whose pg0 acts.
func winInstall(t *testing.T) (Native, *fakeTools, string) {
	t.Helper()
	base := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(base, "data"), Config: filepath.Join(base, "config"), State: filepath.Join(base, "state")}
	cfg := config.Default(d, base)
	cfg.Server.Runtime, cfg.Server.Project = "native", "bs"
	if err := config.UpdateSecrets(d.SecretsFile(), func(s *config.Secrets) error {
		*s = secrets
		s.DBPassword = winPassword
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tools := &fakeTools{Out: dbToolsOut("on")}
	n := Native{Cfg: cfg, Dirs: d, Tools: tools, Sys: &fakeSys{}, GOOS: "windows", Env: map[string]string{}}
	pg0dir := withPg0Home(t)
	tools.Effect = winPg0Effects(t, pg0dir)
	v := n.Version()
	completeVenv(t, n, v)
	touch(t, n.pg0Exe(v), 0o755)
	touch(t, n.venvBin(v, "python"), 0o755)
	for _, dll := range runtimeDLLs {
		if err := os.WriteFile(filepath.Join(n.VenvDir(v), "Scripts", dll), []byte("dll "+dll), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(n.EnvFile(), []byte(pgPortKey+"=5433\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return n, tools, pg0dir
}

// firstStart is what Start does before the supervisor on a first install:
// the database created and its identity recorded.
func firstStart(t *testing.T, n Native) error {
	t.Helper()
	n.MayCreate = true
	return n.guardStart(context.Background())
}

// requireNoPassword fails when a recorded call holds winPassword on its
// argv or in its environment.
func requireNoPassword(t *testing.T, calls []toolCall) {
	t.Helper()
	for _, c := range calls {
		for _, w := range append(append([]string{c.Path}, c.Args...), c.Env...) {
			if strings.Contains(w, winPassword) {
				t.Errorf("%s %q carries the password (%q)", c.Path, c.Args, w)
			}
		}
	}
}

// Review focus 5: the first install puts the real password nowhere but
// psql's stdin (pg0 and psql run on pg0's default one, PGPASSWORD).
func TestWindowsInstanceCreationNeverPutsThePasswordOnArgv(t *testing.T) {
	n, tools, _ := winInstall(t)
	pgInstallation(t, n.installation()) // Fetch's copy
	if err := firstStart(t, n); err != nil {
		t.Fatal(err)
	}
	requireNoPassword(t, tools.Calls)
	var withStdin []toolCall
	for _, c := range tools.Calls {
		if len(c.Stdin) > 0 {
			withStdin = append(withStdin, c)
		}
	}
	if len(withStdin) != 1 || withStdin[0].Path != n.pgBin("psql") {
		t.Fatalf("calls with stdin %+v, want psql's alone", withStdin)
	}
	psql := withStdin[0]
	if !strings.Contains(string(psql.Stdin), "ALTER ROLE hindsight PASSWORD '"+winPassword+"'") || !slices.Equal(psql.Args[len(psql.Args)-2:], []string{"-f", "-"}) {
		t.Errorf("psql %q stdin %q: want the ALTER ROLE from -f -", psql.Args, psql.Stdin)
	}
	if !slices.Contains(psql.Env, "PGPASSWORD=postgres") || !slices.Contains(psql.Env, "PATH="+filepath.Join(n.installation(), "bin")+";"+filepath.Join(os.Getenv("SYSTEMROOT"), "System32")) {
		t.Errorf("psql env %q: want pg0's default password and the copy's bin\\ first on PATH", psql.Env)
	}
	for _, c := range tools.Calls[:3] {
		if !c.Step || !c.InJob {
			t.Errorf("%s %q ran outside a job's step", c.Path, c.Args)
		}
	}
	if got, err := os.ReadFile(n.IdentityFile()); err != nil || strings.TrimSpace(string(got)) != "7702339718603615110" {
		t.Errorf("identity %q %v", got, err)
	}
}

// E34 ruling 2: pg0's output goes to a file under <Root>\tmp, never a
// pipe (its postmaster inherits it), in the first install and the unpack;
// a runner that cannot run a step in a job runs nothing; a failed pg0's
// log reaches the error.
func TestPg0NeverRunsOnAPipe(t *testing.T) {
	n, tools, _ := winInstall(t)
	withFreePort(t, 18999)
	if err := n.unpackPostgres(context.Background(), io.Discard, n.Version()); err != nil {
		t.Fatal(err)
	}
	if err := firstStart(t, n); err != nil {
		t.Fatal(err)
	}
	var pg0 int
	for _, c := range tools.Calls {
		if filepath.Base(c.Path) != "pg0.exe" {
			continue
		}
		pg0++
		if !c.Step || !c.InJob || filepath.Dir(c.OutFile) != filepath.Join(n.Root(), "tmp") || !strings.HasPrefix(filepath.Base(c.OutFile), "pg0-") || len(c.Stdin) > 0 {
			t.Errorf("pg0 %q: step %v, in a job %v, output to %q, stdin %q; want a step in a job, its output to <Root>\\tmp\\pg0-*.log, no stdin",
				c.Args, c.Step, c.InJob, c.OutFile, c.Stdin)
		}
	}
	if pg0 != 4 {
		t.Errorf("pg0 ran %d times, want 4 (the unpack's start and drop, the first install's start and stop)", pg0)
	}

	t.Run("a runner without steps runs nothing", func(t *testing.T) {
		n, tools, _ := winInstall(t)
		n.Tools = runOnly{tools}
		err := firstStart(t, n)
		if err == nil || !strings.Contains(err.Error(), "cannot run") {
			t.Errorf("first start = %v, want the runner refused", err)
		}
		if len(tools.Calls) != 0 {
			t.Errorf("ran %+v", tools.Calls)
		}
	})

	t.Run("a failed pg0 start's log is in the error", func(t *testing.T) {
		n, tools, _ := winInstall(t)
		out := tools.Out
		tools.Out = func(c toolCall) []byte {
			if filepath.Base(c.Path) == "pg0.exe" {
				return []byte("FATAL: could not bind IPv4 address \"127.0.0.1\"\n")
			}
			return out(c)
		}
		tools.Fail = func(c toolCall) error {
			if filepath.Base(c.Path) == "pg0.exe" {
				return errors.New("exit status 1")
			}
			return nil
		}
		err := firstStart(t, n)
		if err == nil || !strings.Contains(err.Error(), "could not bind IPv4") || !strings.Contains(err.Error(), "pg0-bs-db-init-1.log") {
			t.Errorf("first start = %v, want pg0's log", err)
		}
	})
}

// runOnly is a tool runner without RunStep.
type runOnly struct{ t *fakeTools }

func (r runOnly) Run(ctx context.Context, env []string, stdin io.Reader, path string, args ...string) ([]byte, error) {
	return r.t.Run(ctx, env, stdin, path, args...)
}

// E34 W4 and ruling 8: the first install moves pg0's extracted Postgres
// under <Root>\pg (a rename, or a copy across volumes), forgets pg0's
// instance (deleting its entry, never pg0 drop), and leaves the data in
// <Root>\db as pg0 created it; a complete copy is kept; a failure leaves
// no database on pg0's default password.
func TestFirstInstallCopiesPostgresAndForgetsPg0sInstance(t *testing.T) {
	check := func(t *testing.T, n Native, tools *fakeTools, pg0dir string) {
		t.Helper()
		for _, f := range []string{`bin\postgres.exe`, `lib\plpgsql.dll`, `bin\msvcp140.dll`, pgUnpacked} {
			if !exists(filepath.Join(n.installation(), f)) {
				t.Errorf("%s missing from the copy", f)
			}
		}
		if exists(filepath.Join(pg0dir, "instances", "bs")) {
			t.Error("pg0's instance entry stays")
		}
		if exists(filepath.Join(pg0dir, "installation", nativelock.PGInstallation)) {
			t.Error("pg0's extracted installation stays: moved, not copied")
		}
		if raw, err := os.ReadFile(filepath.Join(n.DataDir(), "fake-sysid")); err != nil || !exists(filepath.Join(n.DataDir(), "PG_VERSION")) || string(raw) != "7702339718603615110\n" {
			t.Errorf("<Root>\\db changed: %q %v", raw, err)
		}
		if n.DataDir() != filepath.Join(n.Root(), "db") {
			t.Errorf("DataDir() = %s, want <Root>\\db", n.DataDir())
		}
		for _, c := range tools.Calls {
			if filepath.Base(c.Path) == "pg0.exe" && c.Args[0] == "drop" {
				t.Errorf("pg0 drop ran: %q", c.Args)
			}
		}
	}

	t.Run("moved", func(t *testing.T) {
		n, tools, pg0dir := winInstall(t)
		if err := firstStart(t, n); err != nil {
			t.Fatal(err)
		}
		check(t, n, tools, pg0dir)
		// No copy before: psql was the one pg0 extracted.
		if !slices.ContainsFunc(tools.Calls, func(c toolCall) bool {
			return c.Path == filepath.Join(pg0dir, "installation", nativelock.PGInstallation, "bin", "psql.exe")
		}) {
			t.Errorf("psql did not run from pg0's extraction: %+v", tools.Calls)
		}
	})

	t.Run("copied across volumes", func(t *testing.T) {
		n, tools, pg0dir := winInstall(t)
		old := renameDir
		var refused []string
		renameDir = func(from, to string) error {
			refused = append(refused, from)
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: windows.ERROR_NOT_SAME_DEVICE}
		}
		t.Cleanup(func() { renameDir = old })
		if err := firstStart(t, n); err != nil {
			t.Fatal(err)
		}
		if len(refused) == 0 {
			t.Fatal("no rename was tried")
		}
		check(t, n, tools, pg0dir)
	})

	t.Run("a complete copy is kept", func(t *testing.T) {
		n, tools, pg0dir := winInstall(t)
		pgInstallation(t, n.installation())
		touch(t, filepath.Join(n.installation(), pgUnpacked), 0o644)
		sentinel := filepath.Join(n.installation(), "kept")
		touch(t, sentinel, 0o644)
		if err := firstStart(t, n); err != nil {
			t.Fatal(err)
		}
		if !exists(sentinel) {
			t.Error("the complete copy was replaced")
		}
		check(t, n, tools, pg0dir)
	})

	t.Run("a failed first start removes the new database", func(t *testing.T) {
		n, tools, pg0dir := winInstall(t)
		tools.Fail = func(c toolCall) error {
			if filepath.Base(c.Path) == "psql.exe" {
				return errors.New("exit status 2")
			}
			return nil
		}
		err := firstStart(t, n)
		if err == nil || strings.Contains(err.Error(), winPassword) {
			t.Fatalf("first start = %v, want a failure without the password", err)
		}
		for _, p := range []string{n.DataDir(), filepath.Join(pg0dir, "instances", "bs"), n.IdentityFile()} {
			if exists(p) {
				t.Errorf("%s stays after a failed first install", p)
			}
		}
	})
}

// E34 ruling 3: the nine msvc-runtime DLLs are copied from the venv's Scripts\
// into each place, the manifest maps each to its sha256; one missing from
// the venv is an error naming it.
func TestRuntimeDLLsCopiedWithAManifest(t *testing.T) {
	n, _, _ := winInstall(t)
	v := n.Version()
	dirs := []string{filepath.Join(n.installation(), "bin"), n.pg0BinDir(v), n.onnxCapi(v)}
	if err := n.copyRuntimeDLLs(v, dirs...); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(n.Root(), "dlls.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 9 || len(runtimeDLLs) != 9 {
		t.Errorf("manifest %v, want the nine DLLs", manifest)
	}
	for _, dll := range runtimeDLLs {
		sum := sha256.Sum256([]byte("dll " + dll))
		if manifest[dll] != hex.EncodeToString(sum[:]) {
			t.Errorf("manifest %s = %q, want its sha256", dll, manifest[dll])
		}
		for _, dir := range dirs {
			if b, err := os.ReadFile(filepath.Join(dir, dll)); err != nil || string(b) != "dll "+dll {
				t.Errorf("%s: %q %v", filepath.Join(dir, dll), b, err)
			}
		}
	}
	if err := os.Remove(filepath.Join(n.VenvDir(v), "Scripts", "vcruntime140_1.dll")); err != nil {
		t.Fatal(err)
	}
	if err := n.copyRuntimeDLLs(v, dirs...); err == nil || !strings.Contains(err.Error(), "vcruntime140_1.dll") {
		t.Errorf("copyRuntimeDLLs without vcruntime140_1.dll = %v", err)
	}
}

// jobName is the supervisor's Job Object of project for this user.
func jobName(t *testing.T, project string) string {
	t.Helper()
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	return `Global\backsight-` + project + "-" + sid
}

// startFake starts a fake and kills it at the test's end.
func startFake(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

// Requirement 3 on Windows (0080): a postgres.exe of <Root>\pg\ outside
// the supervisor's Job Object is a stray, postmaster.pid's first; one in
// the job is not, nor one of another directory. StopStrays stops the
// pidfile's with pg_ctl and waits for it; one the pidfile does not name is
// refused, stopping nothing.
func TestStraysOnWindows(t *testing.T) {
	n, _, _ := winInstall(t)
	n.Cfg.Server.Project = "bs-test-" + strconv.Itoa(os.Getpid()) + "-strays"
	tools := &execTools{}
	n.Tools = tools
	n.StrayWait = 10 * time.Second
	bin := filepath.Join(n.installation(), "bin")
	stop := filepath.Join(t.TempDir(), "stop")
	t.Cleanup(func() { _ = os.WriteFile(stop, nil, 0o644) })
	pg := testtool.Write(t, bin, "postgres", testtool.Fake{Script: "waitfile " + stop, NoLog: true})
	testtool.Write(t, bin, "pg_ctl", testtool.Fake{Script: "write " + stop + " x"})
	other := testtool.Write(t, filepath.Join(t.TempDir(), "elsewhere"), "postgres", testtool.Fake{Script: "waitfile " + stop, NoLog: true})

	// The supervisor's job, by name, holding one of them.
	name, err := windows.UTF16PtrFromString(jobName(t, n.Cfg.Server.Project))
	if err != nil {
		t.Fatal(err)
	}
	job, err := windows.CreateJobObject(nil, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(job) })
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		t.Fatal(err)
	}
	// The job's own: also a postgres.exe of <Root>\pg\, which pg_ctl's fake
	// does not stop.
	jobStop := filepath.Join(t.TempDir(), "job-stop")
	t.Cleanup(func() { _ = os.WriteFile(jobStop, nil, 0o644) })
	jobPG := testtool.Write(t, filepath.Join(n.Root(), "pg", "other", "bin"), "postgres", testtool.Fake{Script: "waitfile " + jobStop, NoLog: true})
	inJob := startFake(t, jobPG)
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(inJob.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		t.Fatal(err)
	}
	_ = windows.CloseHandle(h)
	strayA, strayB := startFake(t, pg), startFake(t, pg)
	startFake(t, other)
	time.Sleep(300 * time.Millisecond) // the fakes are running

	writePidFile := func(pid int) {
		t.Helper()
		touch(t, filepath.Join(n.DataDir(), "postmaster.pid"), 0o600)
		if err := os.WriteFile(filepath.Join(n.DataDir(), "postmaster.pid"), []byte(strconv.Itoa(pid)+"\r\n"+n.DataDir()+"\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePidFile(max(strayA.Process.Pid, strayB.Process.Pid))
	strays, err := n.Strays()
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, s := range strays {
		got = append(got, s.PID)
	}
	lo, hi := min(strayA.Process.Pid, strayB.Process.Pid), max(strayA.Process.Pid, strayB.Process.Pid)
	if want := []int{hi, lo}; !slices.Equal(got, want) {
		t.Fatalf("Strays = %v, want %v (postmaster.pid's first; the job's pid %d and the other directory's are none)", got, want, inJob.Process.Pid)
	}

	// Two strays, the pidfile names one: refused, nothing stopped.
	err = n.StopStrays(context.Background(), strays)
	if err == nil || !strings.Contains(err.Error(), "taskkill /F /PID "+strconv.Itoa(lo)) || strings.Contains(err.Error(), ".service") {
		t.Errorf("StopStrays of a stray the pidfile does not name = %v, want the refusal with taskkill", err)
	}
	if len(tools.Calls) != 0 || exists(stop) {
		t.Errorf("ran %+v on a refusal", tools.Calls)
	}

	// The pidfile's own: pg_ctl stop -m fast -D <db>, then gone.
	_ = strayA.Process.Kill()
	_, _ = strayA.Process.Wait()
	_ = strayB.Process.Kill()
	_, _ = strayB.Process.Wait()
	_ = os.Remove(stop)
	one := startFake(t, pg)
	time.Sleep(300 * time.Millisecond)
	writePidFile(one.Process.Pid)
	if strays, err = n.Strays(); err != nil || len(strays) != 1 || strays[0].PID != one.Process.Pid {
		t.Fatalf("Strays = %v %v, want pid %d", strays, err, one.Process.Pid)
	}
	if err := n.StopStrays(context.Background(), strays); err != nil {
		t.Fatal(err)
	}
	if len(tools.Calls) != 1 || tools.Calls[0].Path != filepath.Join(bin, "pg_ctl.exe") || !slices.Equal(tools.Calls[0].Args[:5], []string{"stop", "-D", n.DataDir(), "-m", "fast"}) {
		t.Errorf("ran %+v, want pg_ctl stop -D <db> -m fast", tools.Calls)
	}
	if n.strayRunning(one.Process.Pid) {
		t.Error("the stray still runs after StopStrays")
	}
	if n.strayRunning(inJob.Process.Pid) != true {
		t.Error("the job's postgres was stopped too, or is not seen")
	}
}

// pidsAlive are the pids in path that still run.
func pidsAlive(path string) []int {
	raw, _ := os.ReadFile(path)
	var out []int
	for _, f := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			continue
		}
		if ev, _ := windows.WaitForSingleObject(h, 0); ev == uint32(windows.WAIT_TIMEOUT) {
			out = append(out, pid)
		}
		_ = windows.CloseHandle(h)
	}
	return out
}

// 0080 requirement 5 on Windows: backup verify's throwaway database (the
// copy's initdb then postgres, in <Root>\tmp\verify-<id> on a free port)
// is restored into and counted, then nothing of it runs (postgres and the
// backend it started) and its directory, lock and logs are gone, also
// when it never answers.
func TestVerifyDumpLeavesNothingRunning(t *testing.T) {
	for _, answers := range []bool{true, false} {
		t.Run(map[bool]string{true: "verified", false: "never answers"}[answers], func(t *testing.T) {
			n, _, _ := winInstall(t)
			n.Tools = &execTools{}
			withFreePort(t, 18998)
			old := verifyWait
			verifyWait = 3 * time.Second
			t.Cleanup(func() { verifyWait = old })
			bin := filepath.Join(n.installation(), "bin")
			pids := filepath.Join(t.TempDir(), "pids")
			never := filepath.Join(t.TempDir(), "never")
			t.Cleanup(func() { _ = os.WriteFile(never, nil, 0o644) })
			backend := testtool.Write(t, t.TempDir(), "backend", testtool.Fake{Script: "pid " + pids + "\nwaitfile " + never})
			testtool.Write(t, bin, "initdb", testtool.Fake{})
			testtool.Write(t, bin, "postgres", testtool.Fake{Script: "pid " + pids + "\nstart " + backend + "\nwaitfile " + never})
			psql := testtool.Fake{Stdout: "3"}
			if !answers {
				psql = testtool.Fake{Code: 2, Stderr: "could not connect"}
			}
			testtool.Write(t, bin, "psql", psql)
			for _, tool := range []string{"dropdb", "createdb", "pg_restore"} {
				testtool.Write(t, bin, tool, testtool.Fake{})
			}
			dump := filepath.Join(t.TempDir(), "db.dump")
			touch(t, dump, 0o600)

			counts, err := n.VerifyDump(context.Background(), dump, []string{"memory_units", "documents"})
			if answers && (err != nil || counts["memory_units"] != 3 || counts["documents"] != 3) {
				t.Errorf("VerifyDump = %v, %v; want 3 rows each", counts, err)
			}
			if !answers && (err == nil || !strings.Contains(err.Error(), "did not answer")) {
				t.Errorf("VerifyDump = %v, %v; want the wait's failure", counts, err)
			}
			if raw, _ := os.ReadFile(pids); len(strings.Fields(string(raw))) != 2 {
				t.Fatalf("pids %q: want postgres' and its backend's", raw)
			}
			var alive []int
			for end := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
				if alive = pidsAlive(pids); len(alive) == 0 || time.Now().After(end) {
					break
				}
			}
			if len(alive) != 0 {
				t.Errorf("pids %v still run after VerifyDump", alive)
			}
			left, _ := filepath.Glob(filepath.Join(n.Root(), "tmp", "*verify*"))
			if len(left) != 0 {
				t.Errorf("left in <Root>\\tmp: %v", left)
			}
			calls := testtool.Calls(t, filepath.Join(bin, "postgres.exe"))
			if len(calls) != 1 || !slices.Contains(calls[0], "listen_addresses=127.0.0.1") || !slices.Contains(calls[0], "18998") {
				t.Errorf("postgres ran %q, want once on loopback and the free port", calls)
			}
		})
	}
}

// Final review 3's minor (Task 10): the root and anything above it the
// native runtime creates is private on Windows, under a parent anyone may
// read (MkdirAll alone would inherit the parent's entries).
func TestNativeRootIsCreatedPrivate(t *testing.T) {
	loose := t.TempDir()
	if out, err := exec.Command("icacls", loose, "/grant", "*S-1-5-32-545:(OI)(CI)(RX)").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v\n%s", err, out)
	}
	d := paths.Dirs{Data: filepath.Join(loose, "fresh", "data"), Config: filepath.Join(loose, "config"), State: filepath.Join(loose, "fresh", "state")}
	n := Native{Cfg: config.Default(d, loose), Dirs: d, GOOS: "windows"}
	if err := n.ensureRoot(); err != nil {
		t.Fatal(err)
	}
	if err := n.ensureHome(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(loose, "fresh"), d.Data, n.Cfg.Server.ComposeDir, n.Root(), n.Home()} {
		testtool.RequirePrivate(t, p)
	}
}

// Note 8 of Task 10: what a Windows install says never names a systemd
// unit or journalctl; it points at the supervisor's logs.
func TestWindowsTextsNameNoSystemdUnit(t *testing.T) {
	n, tools, _ := winInstall(t)
	oldDB, oldPoll, oldAPI := dbWait, dbPoll, apiWait
	dbWait, dbPoll, apiWait = 50*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { dbWait, dbPoll, apiWait = oldDB, oldPoll, oldAPI })
	withAPIHealth(t, func(context.Context, string) (string, error) { return "", errors.New("connection refused") })
	tools.Fail = func(c toolCall) error { return errors.New("exit status 2: could not connect") }
	var texts []string
	for _, err := range []error{n.waitDB(context.Background()), n.waitAPI(context.Background())} {
		if err == nil {
			t.Fatal("a wait passed with nothing answering")
		}
		texts = append(texts, err.Error())
	}
	tools.Fail, tools.Out = nil, dbToolsOut("off")
	if err := n.CheckFsync(context.Background()); err == nil {
		t.Fatal("fsync off passed")
	} else {
		texts = append(texts, err.Error())
	}
	if !strings.Contains(texts[0], filepath.Join(n.LogDir(), "bs-db.log")) || !strings.Contains(texts[1], filepath.Join(n.LogDir(), "bs-api.log")) ||
		!strings.Contains(texts[2], n.SuperviseFile()) {
		t.Errorf("texts do not point at the logs and supervise.json:\n%s", strings.Join(texts, "\n"))
	}
	for _, s := range texts {
		if strings.Contains(s, ".service") || strings.Contains(s, "journalctl") || strings.Contains(s, "systemd") {
			t.Errorf("a Windows text names systemd: %s", s)
		}
	}
	if err := os.WriteFile(filepath.Join(n.Root(), "task.xml"), []byte("<Task/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.Ask = func(context.Context, string, string) (supervise.Status, error) { return supervise.Status{}, supervise.ErrNotRunning }
	states, err := n.UnitStates(context.Background())
	if err != nil || len(states) != 2 || states["db"] != "inactive" || states["hindsight"] != "inactive" {
		t.Errorf("UnitStates = %v %v, want db and hindsight inactive", states, err)
	}
}
