package dashboard

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/backsight/internal/paths"
)

func testDirs(t *testing.T) paths.Dirs {
	t.Helper()
	root := t.TempDir()
	return paths.Dirs{
		Data:   filepath.Join(root, "data"),
		Config: filepath.Join(root, "config"),
		State:  filepath.Join(root, "state"),
		Bin:    filepath.Join(root, "bin"),
	}
}

// testNow is a clock truncated to the second: filesystems keep mtimes at
// 100 ns to 1 s.
func testNow() time.Time { return time.Now().UTC().Truncate(time.Second) }

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// plant writes raw as the runtime file, with mtime at.
func plant(t *testing.T, d paths.Dirs, raw string, at time.Time) {
	t.Helper()
	p := RuntimeFile(d)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

func plantRuntime(t *testing.T, d paths.Dirs, r Runtime, at time.Time) string {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	plant(t, d, string(raw), at)
	return string(raw)
}

func readRuntime(t *testing.T, d paths.Dirs) Runtime {
	t.Helper()
	raw, err := os.ReadFile(RuntimeFile(d))
	if err != nil {
		t.Fatal(err)
	}
	var r Runtime
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("%v: %q", err, raw)
	}
	return r
}

func fileText(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func wantRunning(t *testing.T, err error, port int) {
	t.Helper()
	var re *RunningError
	if !errors.As(err, &re) {
		t.Fatalf("want *RunningError{Port: %d}, got %v", port, err)
	}
	if re.Port != port {
		t.Errorf("RunningError port %d, want %d", re.Port, port)
	}
}

func TestRuntimePaths(t *testing.T) {
	d := testDirs(t)
	if got, want := RuntimeFile(d), filepath.Join(d.State, "dashboard", "dashboard.json"); got != want {
		t.Errorf("RuntimeFile %q, want %q", got, want)
	}
	if got, want := CodesDir(d), filepath.Join(d.State, "dashboard", "codes"); got != want {
		t.Errorf("CodesDir %q, want %q", got, want)
	}
}

func TestClaimWritesOnlyPidPortStart(t *testing.T) {
	d, now := testDirs(t), testNow()
	r := Runtime{PID: os.Getpid(), Port: 4242, StartedAt: now}
	if err := Claim(d, r, now); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(fileText(t, RuntimeFile(d))), &fields); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "pid,port,started_at" {
		t.Errorf("keys %q, want pid,port,started_at", got)
	}
	if got := readRuntime(t, d); got.PID != r.PID || got.Port != r.Port || !got.StartedAt.Equal(now) {
		t.Errorf("file holds %+v, want %+v", got, r)
	}
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	if fi, err := os.Stat(RuntimeFile(d)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("runtime file mode %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(filepath.Dir(RuntimeFile(d))); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dashboard dir mode %v (%v), want 0700", fi.Mode().Perm(), err)
	}
}

func TestClaimRefusesALiveDashboard(t *testing.T) {
	d, now := testDirs(t), testNow()
	raw := plantRuntime(t, d, Runtime{PID: os.Getpid(), Port: 4242, StartedAt: now}, now)
	err := Claim(d, Runtime{PID: os.Getpid(), Port: 5555, StartedAt: now}, now)
	wantRunning(t, err, 4242)
	if got := fileText(t, RuntimeFile(d)); got != raw {
		t.Errorf("file changed: %q, want %q", got, raw)
	}
	r, alive, err := Running(d, now)
	if err != nil || !alive || r.Port != 4242 {
		t.Errorf("Running: %+v, %v, %v; want port 4242, alive", r, alive, err)
	}
}

func TestClaimReplacesAStaleFile(t *testing.T) {
	d, now := testDirs(t), testNow()
	plantRuntime(t, d, Runtime{PID: deadPID(t), Port: 4242, StartedAt: now}, now)
	if _, alive, err := Running(d, now); err != nil || alive {
		t.Fatalf("Running on a dead pid: alive %v, %v", alive, err)
	}
	want := Runtime{PID: os.Getpid(), Port: 5555, StartedAt: now}
	if err := Claim(d, want, now); err != nil {
		t.Fatal(err)
	}
	if got := readRuntime(t, d); got.PID != want.PID || got.Port != 5555 {
		t.Errorf("file holds %+v, want %+v", got, want)
	}
}

func TestClaimReplacesAReusedPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("start times are read from /proc")
	}
	d, now := testDirs(t), testNow()
	plantRuntime(t, d, Runtime{PID: os.Getpid(), Port: 4242, StartedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}, now)
	if err := Claim(d, Runtime{PID: os.Getpid(), Port: 5555, StartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if got := readRuntime(t, d); got.Port != 5555 {
		t.Errorf("file holds %+v, want port 5555", got)
	}
}

func TestPartialFileCountsAsStarting(t *testing.T) {
	d, now := testDirs(t), testNow()
	plant(t, d, `{"pid":`, now.Add(-5*time.Second))
	r, alive, err := Running(d, now)
	if err != nil || !alive || r.Port != 0 {
		t.Errorf("Running: %+v, %v, %v; want starting (alive, port 0)", r, alive, err)
	}
	err = Claim(d, Runtime{PID: os.Getpid(), Port: 5555, StartedAt: now}, now)
	wantRunning(t, err, 0)
	if got := fileText(t, RuntimeFile(d)); got != `{"pid":` {
		t.Errorf("file changed: %q", got)
	}
}

func TestOldPartialFileIsStale(t *testing.T) {
	d, now := testDirs(t), testNow()
	plant(t, d, `{"pid":`, now.Add(-11*time.Second))
	if _, alive, err := Running(d, now); err != nil || alive {
		t.Errorf("Running: alive %v, %v; want stale", alive, err)
	}
	if err := Claim(d, Runtime{PID: os.Getpid(), Port: 5555, StartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if got := readRuntime(t, d); got.Port != 5555 {
		t.Errorf("file holds %+v, want port 5555", got)
	}
}

func TestRunningWithoutAFile(t *testing.T) {
	d, now := testDirs(t), testNow()
	if r, alive, err := Running(d, now); err != nil || alive || r != (Runtime{}) {
		t.Errorf("Running: %+v, %v, %v; want nothing", r, alive, err)
	}
}

// A file that parses but names no process ({} or pid 0) is stale: kill(2)
// would read pid 0 as this process's group.
func TestFileWithoutAPIDIsStale(t *testing.T) {
	d, now := testDirs(t), testNow()
	plant(t, d, `{}`, now)
	if _, alive, err := Running(d, now); err != nil || alive {
		t.Errorf("Running: alive %v, %v; want stale", alive, err)
	}
	if err := Claim(d, Runtime{PID: os.Getpid(), Port: 5555, StartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
}

func TestClaimDoesNotRemoveAFreshFile(t *testing.T) {
	d, now := testDirs(t), testNow()
	plantRuntime(t, d, Runtime{PID: deadPID(t), Port: 4242, StartedAt: now}, now)
	calls := 0
	beforeStaleRemove = func() {
		calls++
		plantRuntime(t, d, Runtime{PID: os.Getpid(), Port: 4343, StartedAt: now}, now)
	}
	defer func() { beforeStaleRemove = func() {} }()
	err := Claim(d, Runtime{PID: os.Getpid(), Port: 5555, StartedAt: now}, now)
	wantRunning(t, err, 4343)
	if calls != 1 {
		t.Errorf("beforeStaleRemove ran %d times, want 1", calls)
	}
	if got := readRuntime(t, d); got.Port != 4343 {
		t.Errorf("file holds %+v, want port 4343", got)
	}
}

// Another stale file planted in place of the removed one: the claim is
// retried once, not forever.
func TestClaimRetriesOnce(t *testing.T) {
	d, now := testDirs(t), testNow()
	dead := deadPID(t)
	plantRuntime(t, d, Runtime{PID: dead, Port: 4242, StartedAt: now}, now)
	calls := 0
	beforeStaleRemove = func() {
		calls++
		if err := os.Remove(RuntimeFile(d)); err != nil {
			t.Error(err)
		}
		plantRuntime(t, d, Runtime{PID: dead, Port: 4242 + calls, StartedAt: now}, now)
	}
	defer func() { beforeStaleRemove = func() {} }()
	err := Claim(d, Runtime{PID: os.Getpid(), Port: 5555, StartedAt: now}, now)
	var re *RunningError
	if err == nil || errors.As(err, &re) {
		t.Errorf("Claim: %v, want a plain error", err)
	}
	if calls != 1 {
		t.Errorf("beforeStaleRemove ran %d times, want 1", calls)
	}
	if got := readRuntime(t, d); got.PID != dead {
		t.Errorf("file holds %+v, want the stale one", got)
	}
}

func TestReleaseOnlyItsOwn(t *testing.T) {
	d, now := testDirs(t), testNow()
	if err := Claim(d, Runtime{PID: os.Getpid(), Port: 4242, StartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := Release(d, os.Getpid()+1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(RuntimeFile(d)); err != nil {
		t.Errorf("another pid's release removed the file: %v", err)
	}
	if err := Release(d, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(RuntimeFile(d)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("own release left the file: %v", err)
	}
	if err := Release(d, os.Getpid()); err != nil {
		t.Errorf("release without a file: %v", err)
	}
}

func TestRunningErrorMessages(t *testing.T) {
	if got := (&RunningError{}).Error(); got != "a dashboard is starting" {
		t.Errorf("starting: %q", got)
	}
	if got := (&RunningError{Port: 4242}).Error(); got != "a dashboard is already running on port 4242" {
		t.Errorf("running: %q", got)
	}
}
