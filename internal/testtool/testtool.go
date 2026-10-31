// Package testtool makes the fake programs of the tests portable: no
// "#!/bin/sh" file, which Windows cannot run, but the test binary itself
// started again under another name.
//
// Write links (Windows: copies) the running test binary to dir/name
// (dir/name.exe on Windows) and puts the fake's behaviour in a sidecar
// dir/name.fake.json. Main, called first in a package's TestMain, looks for
// that sidecar next to its own executable: when it finds one, the process
// was started as the fake, behaves as it says and exits; otherwise it
// returns and the tests run.
//
// The sidecar, not an environment variable, tells the role: the code under
// test often runs a tool with a clean environment, and a variable set by
// the test would not reach it.
package testtool

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fake is what a fake program does: append its argv to dir/name.log (one
// JSON array per line), run Script, print Stdout, write Stderr, and exit
// Code.
//
// Script is a list of steps, one per line:
//
//	sleep <ms>               wait
//	write <path> <text>      write the text (\n and \t unescaped) to the file
//	append <path> <text>     the same, appending
//	env <KEY> [default]      print the variable and a newline (default when unset)
//	stdin                    copy stdin to stdout
//	stdinto <path>           copy stdin to the file
//	stdout <text>            print the text (\n and \t unescaped) and a newline
//	stderr <text>            the same on stderr
//	argv                     print each argument on a line of its own
//	argline                  print the arguments joined by spaces, then a newline
//	exit <code>              exit now with the code, skipping Stdout and Stderr
//	waitfile <path>          wait until the file exists (a process that runs until told to stop)
//	nofile <path> <code>     exit now with the code when the file does not exist
//	pid <path>               append this process's PID and a newline to the file
//
// A step may start with a condition on an argument, `<n>=<value>` or
// `<n>!=<value>` (n from 1; a missing argument is ""), and runs only when
// it holds. A fake needing more than this stays in a !windows file with a
// one-line reason.
type Fake struct {
	Stdout, Stderr string
	Code           int
	Script         string
	// NoLog keeps the fake from writing its log, for a test that compares
	// the directory before and after the fake ran.
	NoLog bool
}

// spec is Fake as the sidecar holds it, with where to log.
type spec struct {
	Fake
	Log string
}

// suffix is the sidecar's extension.
const suffix = ".fake.json"

// Write installs the fake as dir/name (name.exe on Windows) and returns
// that path.
func Write(t testing.TB, dir, name string, f Fake) string {
	t.Helper()
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		name += ".exe"
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	// A hard link where the OS and the volume allow (no copy of a 30 MB
	// binary per fake), else a copy. A test must not rewrite a fake in place.
	// Windows cannot delete a link to an image that is running (the test
	// binary itself): TempDir's cleanup fails with "Access is denied", so
	// there it is always a copy.
	linked := false
	if runtime.GOOS != "windows" {
		linked = os.Link(exe, path) == nil
	}
	if !linked {
		if err := copyFile(exe, path); err != nil {
			t.Fatal(err)
		}
	}
	base := strings.TrimSuffix(path, ".exe")
	raw, err := json.Marshal(spec{Fake: f, Log: base + ".log"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+suffix, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(base + ".log")
	return path
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Calls is the argv of each run of the fake at path, in order, without the
// program name.
func Calls(t testing.TB, path string) [][]string {
	t.Helper()
	raw, err := os.ReadFile(strings.TrimSuffix(path, ".exe") + ".log")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		return nil
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("%s: line %q: %v", path, line, err)
		}
		out = append(out, argv)
	}
	return out
}

// Main must be called first in TestMain. Started as a fake, the process
// behaves as the fake and exits; otherwise Main returns.
func Main() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(strings.TrimSuffix(exe, ".exe") + suffix)
	if err != nil {
		return
	}
	var s spec
	if err := json.Unmarshal(raw, &s); err != nil {
		fmt.Fprintln(os.Stderr, "testtool: bad sidecar:", err)
		os.Exit(127)
	}
	os.Exit(s.run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func (s spec) run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if line, err := json.Marshal(args); err == nil && !s.NoLog {
		if f, err := os.OpenFile(s.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	}
	for _, step := range strings.Split(s.Script, "\n") {
		step = strings.TrimSpace(step)
		if step == "" {
			continue
		}
		if code, done := s.step(step, args, stdin, stdout, stderr); done {
			return code
		}
	}
	_, _ = io.WriteString(stdout, s.Stdout)
	_, _ = io.WriteString(stderr, s.Stderr)
	return s.Code
}

// holds reports whether the condition word cond (`n=value`, `n!=value`)
// is true of args.
func holds(cond string, args []string) (ok, isCond bool) {
	for _, op := range []string{"!=", "="} {
		l, r, found := strings.Cut(cond, op)
		if !found {
			continue
		}
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			return false, false
		}
		got := ""
		if n <= len(args) {
			got = args[n-1]
		}
		return (got == r) == (op == "="), true
	}
	return false, false
}

func unescape(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\t`, "\t").Replace(s)
}

func (s spec) step(step string, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int, exit bool) {
	f := strings.Fields(step)
	if ok, isCond := holds(f[0], args); isCond {
		if !ok {
			return 0, false
		}
		step = strings.TrimSpace(strings.TrimPrefix(step, f[0]))
		f = f[1:]
	}
	rest := func(skip int) string { // the text after the first skip words
		t := step
		for range skip {
			w := strings.Fields(t)
			if len(w) == 0 {
				break
			}
			t = strings.TrimSpace(strings.TrimPrefix(t, w[0]))
		}
		return unescape(t)
	}
	switch f[0] {
	case "sleep":
		ms, _ := strconv.Atoi(f[1])
		time.Sleep(time.Duration(ms) * time.Millisecond)
	case "write", "append":
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if f[0] == "append" {
			flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		w, err := os.OpenFile(f[1], flag, 0o644)
		if err != nil {
			fmt.Fprintln(stderr, "testtool:", err)
			return 127, true
		}
		_, _ = io.WriteString(w, rest(2))
		_ = w.Close()
	case "env":
		v, ok := os.LookupEnv(f[1])
		if !ok && len(f) > 2 {
			v = f[2]
		}
		fmt.Fprintln(stdout, v)
	case "stdin":
		_, _ = io.Copy(stdout, stdin)
	case "stdinto":
		w, err := os.Create(f[1])
		if err != nil {
			fmt.Fprintln(stderr, "testtool:", err)
			return 127, true
		}
		_, _ = io.Copy(w, stdin)
		_ = w.Close()
	case "stdout":
		fmt.Fprintln(stdout, rest(1))
	case "stderr":
		fmt.Fprintln(stderr, rest(1))
	case "argline":
		fmt.Fprintln(stdout, strings.Join(args, " "))
	case "argv":
		for _, a := range args {
			fmt.Fprintln(stdout, a)
		}
	case "exit":
		n, _ := strconv.Atoi(f[1])
		return n, true
	case "waitfile":
		for {
			if _, err := os.Stat(f[1]); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	case "nofile":
		if _, err := os.Stat(f[1]); err != nil {
			n, _ := strconv.Atoi(f[2])
			return n, true
		}
	case "pid":
		w, err := os.OpenFile(f[1], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintln(stderr, "testtool:", err)
			return 127, true
		}
		fmt.Fprintln(w, os.Getpid())
		_ = w.Close()
	default:
		fmt.Fprintf(stderr, "testtool: unknown step %q\n", step)
		return 127, true
	}
	return 0, false
}

// RequireGit skips the test when git is not installed (a machine such as
// the Windows test VM has none), and fails it on CI, where a missing git
// would hide every test that needs it.
func RequireGit(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("git is not installed on CI:", err)
		}
		t.Skip("git is not installed")
	}
}

// SetHome points the home directory at dir for the test: HOME, which Go's
// os.UserHomeDir reads on Linux, and USERPROFILE, which it reads on
// Windows (a test that sets only HOME would run against the real profile).
func SetHome(t testing.TB, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", dir)
	}
}

// SkipWindows skips the test on Windows, naming what owns the behaviour it
// cannot run yet ("Task 3: mode bits"). Task 15 checks that none is left.
func SkipWindows(t testing.TB, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("windows: " + reason)
	}
}

// JSONPath escapes a file path for a JSON string written by hand: the
// backslashes of a Windows path would otherwise be taken as escapes.
func JSONPath(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }

// TOMLStr quotes a file path as a TOML basic string, with the backslashes
// of a Windows path escaped.
func TOMLStr(p string) string { return strconv.Quote(p) }

// OtherPID is the PID of a live process that is not this one (the test
// binary's parent): a lock owner that is alive and someone else. PID 1 is
// that on Linux only.
func OtherPID() int { return os.Getppid() }
