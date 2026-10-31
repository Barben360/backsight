package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/config"
	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/server"
	"github.com/Barben360/backsight/internal/testtool"
)

// The golden of this file was recorded from 884c942, before the Windows
// native runtime (M7 block 2): a Linux native install must keep running
// exactly these commands through systemd and its tools.

// nativeRecorder wraps the fake user manager and the fake tools every
// command of a native install reaches, recording each argv whole.
type nativeRecorder struct {
	mu    sync.Mutex
	sys   server.SysRunner
	tools server.ToolRunner
	argv  [][]string
	off   bool
}

func (r *nativeRecorder) record(a []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.off {
		r.argv = append(r.argv, a)
	}
}

func (r *nativeRecorder) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.record(append([]string{name}, args...))
	if name == "systemctl" && len(args) > 1 && args[1] == "show" { // status asks the user manager for each unit's state
		return []byte(strings.Repeat("ActiveState=active\nSubState=running\n\n", len(args)-3)), nil
	}
	return r.sys.Run(ctx, name, args...)
}

type nativeRecorderTools struct{ *nativeRecorder }

func (r nativeRecorderTools) Run(ctx context.Context, env []string, stdin io.Reader, path string, args ...string) ([]byte, error) {
	r.record(append([]string{path}, args...))
	return r.tools.Run(ctx, env, stdin, path, args...)
}

// nativeRandom: names a run draws.
var nativeRandom = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(backsight-verify-|verify-|/tmp/backsight-)[0-9a-f]{12}`), "${1}<rand>"},
	{regexp.MustCompile(`(/staging/[a-z]+-)[0-9-]+/`), "${1}<rand>/"},
	{regexp.MustCompile(`(\.requirements-)[0-9]+`), "${1}<rand>"},
	{regexp.MustCompile(`(--port |-p )[0-9]{4,5}\b`), "${1}<pgport>"}, // a free port drawn at install
}

func nativeArgvText(home string, argv [][]string) string {
	var b strings.Builder
	for _, a := range argv {
		q := make([]string, len(a))
		for i, s := range a {
			if i > 0 && a[i-1] == "-p" && strings.Trim(s, "0123456789") == "" && s != "" {
				s = "<pgport>"
			}
			s = strings.ReplaceAll(s, home, "<home>")
			for _, r := range nativeRandom {
				s = r.re.ReplaceAllString(s, r.with)
			}
			q[i] = strconv.Quote(s)
		}
		b.WriteString(strings.Join(q, " ") + "\n")
	}
	return b.String()
}

// nativeInstallRun is a native install from scratch driven through every
// command that reaches systemd or a native tool, in order; it returns the
// argv they ran, each under its command.
func nativeInstallRun(t *testing.T) (argv, keys string) {
	t.Helper()
	home := isolate(t)
	withSys(t, "linux")
	withClaude(t)
	withClock(t, time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC))
	n, sys, port := nativeFresh(t, home)
	d := pathsOf(home)
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.ConfigFile(), []byte("[server]\nurl = \"http://127.0.0.1:"+strconv.Itoa(port)+"\"\nui_port = 19999\nruntime = \"native\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The venv tools: uv builds a venv for the upgrade.
	tools := &venvTools{nativeDBTools: sys.db, n: n}
	rec := &nativeRecorder{sys: sys, tools: tools}
	nativeSys, nativeTools = rec, nativeRecorderTools{rec}

	var ab, kb strings.Builder
	archive := filepath.Join(d.Data, "backups", "backsight-20261025T040000Z.tar.gz.age")
	step := func(args ...string) {
		t.Helper()
		rec.mu.Lock()
		start := len(rec.argv)
		rec.mu.Unlock()
		code, out, errOut := run(t, append(args, "--json")...)
		if code != output.OK {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
		title := "# " + strings.ReplaceAll(strings.ReplaceAll(strings.Join(args, " "), archive, "<archive>"), strconv.Itoa(port), "<port>") + "\n"
		rec.mu.Lock()
		got := slices.Clone(rec.argv[start:])
		rec.mu.Unlock()
		ab.WriteString(title + nativeArgvText(home, got))
		var env any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("%v: not one JSON envelope: %v\n%s", args, err, out)
		}
		set := map[string]bool{}
		jsonKeys("", env, set)
		names := make([]string, 0, len(set))
		for k := range set {
			names = append(names, k)
		}
		sort.Strings(names)
		kb.WriteString(title + strings.Join(names, "\n") + "\n")
	}
	step("server", "install")
	step("server", "start")
	step("server", "status")
	step("server", "stop")
	step("server", "start")
	step("backup", "create")
	step("backup", "verify", archive)
	withClock(t, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	step("restore", archive)

	// A server running nativeOld, so that the upgrade has a venv to build.
	rec.mu.Lock()
	rec.off = true
	rec.mu.Unlock()
	cfg, err := config.Load(d.ConfigFile(), d, home)
	if err != nil {
		t.Fatal(err)
	}
	un := runtimeFor(cfg).(server.Native)
	completeVenv(t, un, nativeOld)
	if err := os.RemoveAll(un.VenvDir(un.Version())); err != nil {
		t.Fatal(err)
	}
	sec, err := config.LoadSecrets(d.SecretsFile())
	if err != nil {
		t.Fatal(err)
	}
	old := un.WithVersion(nativeOld)
	files, err := renderOn(old, sec, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Write(files); err != nil {
		t.Fatal(err)
	}
	if err := old.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	rec.off = false
	rec.mu.Unlock()
	step("server", "upgrade", "--allow-unordered")
	step("uninstall", "--delete-data", "--answer", "confirm_bank="+config.DefaultBank)
	return ab.String(), kb.String()
}

// Every command a native install runs reaches systemd and the native
// tools with the argv it ran at 884c942.
func TestNativeCommandsRunTheSameArgv(t *testing.T) {
	testtool.SkipWindows(t, "Task 7: the Linux native argv is pinned on Linux; Windows runs another host")
	argv, keys := nativeInstallRun(t)
	cliGolden(t, "native-argv.golden", []byte(argv))
	cliGolden(t, "native-json-keys.golden", []byte(keys))
}

// doctor's checks on a Linux native install, by id and in order: the
// Windows checks of M7 block 2 add none to it (recorded at 884c942).
func TestDoctorChecksOfANativeInstallAreUnchanged(t *testing.T) {
	testtool.SkipWindows(t, "linux-only: Linux's native doctor list (Windows': TestDoctorWindowsChecks, TestDoctorOnWindowsReadsTheTaskAndTheSupervisor)")
	_, n, _, _, _ := nativeHost(t)
	nativeSys = &doctorSys{states: "ActiveState=active\nSubState=running\n\nActiveState=active\nSubState=running\n", linger: "yes"}
	lockedVenv(t, n, n.Version(), "3.2.0")
	if err := os.Chmod(n.Home(), 0o700); err != nil {
		t.Fatal(err)
	}
	_, out, _ := run(t, "doctor", "--json")
	var env struct {
		Data doctor.Report `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	ids := make([]string, len(env.Data.Checks))
	for i, c := range env.Data.Checks {
		ids[i] = c.ID
	}
	if !slices.Contains(ids, "native-unit") {
		t.Fatalf("the native checks did not run: %v", ids)
	}
	cliGolden(t, "doctor-checks-native.golden", []byte(strings.Join(ids, "\n")+"\n"))
}
