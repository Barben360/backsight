package testtool

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	Main()
	os.Exit(m.Run())
}

// runFake starts the fake with an empty environment, as the code under
// test starts its tools, and returns what it printed and how it exited.
func runFake(t *testing.T, path, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(path, args...)
	cmd.Env = []string{}
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return o.String(), e.String(), code
}

func TestFakeRecordsArgvAndAnswers(t *testing.T) {
	dir := t.TempDir()
	path := Write(t, dir, "tool", Fake{Stdout: "out\n", Stderr: "err\n", Code: 3})
	argv := []string{"a b", `say "hi"`, "x&y", "", "100%", "it's"}
	stdout, stderr, code := runFake(t, path, "", argv...)
	if stdout != "out\n" || stderr != "err\n" || code != 3 {
		t.Errorf("stdout %q stderr %q code %d", stdout, stderr, code)
	}
	runFake(t, path, "", "second")
	want := [][]string{argv, {"second"}}
	if got := Calls(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("calls %q, want %q", got, want)
	}
	if got := Calls(t, filepath.Join(dir, "never-run")); got != nil {
		t.Errorf("calls of a fake that never ran: %q", got)
	}
}

func TestFakeScriptSteps(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "made.txt")
	path := Write(t, dir, "tool", Fake{Stdout: "last\n", Script: strings.Join([]string{
		"write " + out + " line1\\nline2",
		"append " + out + " tail",
		"env TESTTOOL_PROBE",
		"env TESTTOOL_NOPE unset",
		"stdin",
		"stdout hello  world",
		"stderr warning",
		"1=--version stdout 2.1.310",
		"1!=--version stdout not-a-version",
		"argv",
		"sleep 30",
	}, "\n")})
	// runFake gives an empty environment: the probe is unset, whatever this
	// process has.
	t.Setenv("TESTTOOL_PROBE", "set")
	start := time.Now()
	stdout, stderr, code := runFake(t, path, "piped\n", "--version", "x y")
	if code != 0 || time.Since(start) < 30*time.Millisecond {
		t.Errorf("code %d after %v: sleep did not wait", code, time.Since(start))
	}
	if want := "\nunset\npiped\nhello  world\n2.1.310\n--version\nx y\nlast\n"; stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	if stderr != "warning\n" {
		t.Errorf("stderr %q", stderr)
	}
	if raw, _ := os.ReadFile(out); string(raw) != "line1\nline2tail" {
		t.Errorf("file %q", raw)
	}
	stdout, _, _ = runFake(t, path, "", "other")
	if !strings.Contains(stdout, "not-a-version\n") || strings.Contains(stdout, "2.1.310") {
		t.Errorf("conditions: %q", stdout)
	}

	// A variable the caller does give reaches the step.
	path = Write(t, dir, "envtool", Fake{Script: "env TESTTOOL_PROBE"})
	cmd := exec.Command(path)
	cmd.Env = []string{"TESTTOOL_PROBE=given"}
	if o, err := cmd.Output(); err != nil || string(o) != "given\n" {
		t.Errorf("env step: %q %v", o, err)
	}

	// exit stops the script and skips Stdout.
	path = Write(t, dir, "quits", Fake{Stdout: "never\n", Script: "stdout before\nexit 7\nstdout after"})
	stdout, _, code = runFake(t, path, "")
	if stdout != "before\n" || code != 7 {
		t.Errorf("exit step: stdout %q code %d", stdout, code)
	}
}

// The test binary is not a fake: Main returns when no sidecar sits beside it.
func TestMainReturnsForTheTestBinary(t *testing.T) {
	Main()
}

// A log that exists but is empty means no call, not an error.
func TestCallsOfAnEmptyLogIsNil(t *testing.T) {
	dir := t.TempDir()
	path := Write(t, dir, "tool", Fake{})
	if err := os.WriteFile(strings.TrimSuffix(path, ".exe")+".log", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Calls(t, path); got != nil {
		t.Errorf("calls %q", got)
	}
}

// waitfile runs until its file appears, nofile exits while its file is
// missing, pid records the fake's process: a long-running server that
// stops on demand, a readiness probe, a process to look at.
func TestFakeWaitsForAFileAndRecordsItsPID(t *testing.T) {
	dir := t.TempDir()
	stop, pids := filepath.Join(dir, "stop"), filepath.Join(dir, "pids")
	path := Write(t, dir, "server", Fake{Stdout: "stopped\n", Script: "pid " + pids + "\nwaitfile " + stop})
	cmd := exec.Command(path)
	var o bytes.Buffer
	cmd.Stdout = &o
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("exited before its file appeared: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.WriteFile(stop, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil || o.String() != "stopped\n" {
			t.Errorf("after the file: %v, stdout %q", err, o.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("still running 5 s after its file appeared")
	}
	if raw, _ := os.ReadFile(pids); strings.TrimSpace(string(raw)) != strconv.Itoa(cmd.Process.Pid) {
		t.Errorf("pid file %q, want %d", raw, cmd.Process.Pid)
	}

	ready := filepath.Join(dir, "ready")
	probe := Write(t, dir, "probe", Fake{Script: "nofile " + ready + " 2"})
	if _, _, code := runFake(t, probe, ""); code != 2 {
		t.Errorf("probe without its file: code %d, want 2", code)
	}
	if err := os.WriteFile(ready, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runFake(t, probe, ""); code != 0 {
		t.Errorf("probe with its file: code %d, want 0", code)
	}
}

// ignoresignals keeps the fake running through an interrupt: a server that
// does not stop when asked. (Windows' CTRL_BREAK is the supervisor's
// tests'.)
func TestFakeIgnoresSignals(t *testing.T) {
	SkipWindows(t, "linux-only: os.Process.Signal cannot interrupt on Windows")
	dir := t.TempDir()
	stop := filepath.Join(dir, "stop")
	path := Write(t, dir, "stubborn", Fake{Script: "ignoresignals\nwrite " + filepath.Join(dir, "up") + " x\nwaitfile " + stop})
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for start := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "up")); err == nil {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("the fake never started")
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case err := <-done:
		t.Fatalf("exited on an interrupt: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := os.WriteFile(stop, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("after its file: %v", err)
	}
}
