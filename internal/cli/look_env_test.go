package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/termstyle"
)

// cleanColorEnv makes the environment neutral for color: a plain xterm,
// outside Claude Code, no forcing variable.
func cleanColorEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{"TERM": "xterm", "COLORTERM": "", "WT_SESSION": "", "CLAUDECODE": "", "NO_COLOR": "", "BACKSIGHT_COLOR": ""} {
		t.Setenv(k, v)
	}
}

// fakeTerminal makes isTerminalFd answer yes for the given descriptors
// (every descriptor when none is given).
func fakeTerminal(t *testing.T, fds ...uintptr) {
	t.Helper()
	old, oldVT := isTerminalFd, enableVT
	t.Cleanup(func() { isTerminalFd, enableVT = old, oldVT })
	// A temp file is never a Windows console: the faked terminal is one.
	enableVT = func(io.Writer) (func(), bool) { return func() {}, true }
	isTerminalFd = func(fd uintptr) bool {
		if len(fds) == 0 {
			return true
		}
		for _, f := range fds {
			if f == fd {
				return true
			}
		}
		return false
	}
}

func outFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func fileText(t *testing.T, f *os.File) string {
	t.Helper()
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBufferIsNeverATerminal(t *testing.T) {
	cleanColorEnv(t)
	fakeTerminal(t)
	var out, errb bytes.Buffer
	code, e := executeContext(context.Background(), []string{"version"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if termstyle.LevelOf(e.stdout) != termstyle.None || termstyle.LevelOf(e.stderr) != termstyle.None {
		t.Errorf("a buffer got color: stdout %v, stderr %v", termstyle.LevelOf(e.stdout), termstyle.LevelOf(e.stderr))
	}
}

func TestTerminalIsTheWritersDescriptor(t *testing.T) {
	setBuild(t, "0.1.0-test", "abc1234")
	cleanColorEnv(t)
	f := outFile(t)
	fakeTerminal(t, f.Fd())
	var errb bytes.Buffer
	code, e := executeContext(context.Background(), []string{"version"}, f, &errb)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := termstyle.LevelOf(e.stdout); got != termstyle.Basic {
		t.Errorf("stdout level %v, want Basic", got)
	}
	if got := termstyle.LevelOf(e.stderr); got != termstyle.None {
		t.Errorf("stderr level %v, want None", got)
	}
	_, plain, _ := run(t, "version")
	// At the terminal the text is colored (phase 3), and only colored.
	if got := fileText(t, f); got == plain || termstyle.Strip(got) != plain {
		t.Errorf("at a terminal, got:\n%q\nwant the colored form of\n%q", got, plain)
	}
}

func TestJSONTurnsColorOff(t *testing.T) {
	setBuild(t, "0.1.0-test", "abc1234")
	cleanColorEnv(t)
	t.Setenv("BACKSIGHT_COLOR", "always")
	for _, flag := range []string{"--json", "--json=true"} {
		f := outFile(t)
		fakeTerminal(t, f.Fd())
		_, e := executeContext(context.Background(), []string{"version", flag, "--color=always"}, f, io.Discard)
		if got := termstyle.LevelOf(e.stdout); got != termstyle.None {
			t.Errorf("%s: stdout level %v, want None", flag, got)
		}
		_, plain, _ := run(t, "version", "--json")
		if got := fileText(t, f); got != plain {
			t.Errorf("%s: envelope changed:\n%s\nwant\n%s", flag, got, plain)
		}
	}
}

func TestPlainCommandIsNeverColored(t *testing.T) {
	cleanColorEnv(t)
	t.Setenv("BACKSIGHT_HOME", t.TempDir())
	t.Setenv("BACKSIGHT_STATE_DIR", t.TempDir())
	out, errf := outFile(t), outFile(t)
	fakeTerminal(t, out.Fd(), errf.Fd())
	_, e := executeContext(context.Background(), []string{"secret", "list", "--color=always"}, out, errf)
	if termstyle.LevelOf(e.stdout) != termstyle.None || termstyle.LevelOf(e.stderr) != termstyle.None {
		t.Errorf("a plain command got color: stdout %v, stderr %v", termstyle.LevelOf(e.stdout), termstyle.LevelOf(e.stderr))
	}
}

func TestClaudeCodeShellIsPlain(t *testing.T) {
	cleanColorEnv(t)
	t.Setenv("CLAUDECODE", "1")
	for args, want := range map[string]termstyle.Level{"version": termstyle.None, "version --color=always": termstyle.Basic} {
		f := outFile(t)
		fakeTerminal(t, f.Fd())
		_, e := executeContext(context.Background(), strings.Fields(args), f, io.Discard)
		if got := termstyle.LevelOf(e.stdout); got != want {
			t.Errorf("%s: level %v, want %v", args, got, want)
		}
	}
}

func TestColorFlagInvalid(t *testing.T) {
	code, _, errOut := run(t, "--color=rainbow", "version")
	if code != 2 || !strings.Contains(errOut, "want auto, always or never") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

func TestColorFlagHonouredOnParseError(t *testing.T) {
	cleanColorEnv(t)
	for mode, want := range map[string]termstyle.Level{"never": termstyle.None, "always": termstyle.Basic} {
		errf := outFile(t)
		fakeTerminal(t, errf.Fd())
		// After --bogus: pflag stops there, so only the pre-scan sees it.
		code, e := executeContext(context.Background(), []string{"version", "--bogus", "--color=" + mode}, io.Discard, errf)
		if code != 2 {
			t.Errorf("%s: exit %d", mode, code)
		}
		if got := termstyle.LevelOf(e.stderr); got != want {
			t.Errorf("%s: stderr level %v, want %v", mode, got, want)
		}
	}
}

func TestJSONEqualsTrueOnParseError(t *testing.T) {
	// After --bogus: pflag stops there, so only the pre-scan sees it.
	code, out, _ := run(t, "version", "--bogus", "--json=true")
	var env struct {
		OK bool `json:"ok"`
	}
	if code != 2 || json.Unmarshal([]byte(out), &env) != nil || env.OK {
		t.Errorf("exit %d, stdout %q", code, out)
	}
}

func TestNoSplashFlagParsed(t *testing.T) {
	code, e := executeContext(context.Background(), []string{"--no-splash", "version"}, io.Discard, io.Discard)
	if code != 0 || !e.noSplash {
		t.Errorf("exit %d, noSplash %v", code, e.noSplash)
	}
}

func TestPagerGetsTheRawStream(t *testing.T) {
	f := outFile(t)
	_, e := executeContext(context.Background(), []string{"version"}, f, io.Discard)
	cmd := pagerCmd(e, nil, "/bin/true", "x")
	if cmd.Stdout != io.Writer(f) {
		t.Errorf("the pager's stdout is %T, want the *os.File itself", cmd.Stdout)
	}
}

func TestColorFlagSeparateValueOnParseError(t *testing.T) {
	cleanColorEnv(t)
	errf := outFile(t)
	fakeTerminal(t, errf.Fd())
	// never, not always: auto at this faked terminal would already color.
	_, e := executeContext(context.Background(), []string{"version", "--bogus", "--color", "never"}, io.Discard, errf)
	if got := termstyle.LevelOf(e.stderr); got != termstyle.None {
		t.Errorf("stderr level %v, want None", got)
	}
}

func TestVTFailureTurnsTheStreamOff(t *testing.T) {
	cleanColorEnv(t)
	f := outFile(t)
	fakeTerminal(t, f.Fd())
	enableVT = func(io.Writer) (func(), bool) { return func() {}, false }
	_, e := executeContext(context.Background(), []string{"version", "--color=always"}, f, io.Discard)
	if got := termstyle.LevelOf(e.stdout); got != termstyle.None {
		t.Errorf("level %v with the console refusing VT, want None", got)
	}
}

func TestVTRestoredWhenTheCommandEnds(t *testing.T) {
	cleanColorEnv(t)
	for name, ctx := range map[string]context.Context{"done": context.Background(), "cancelled": cancelledCtx()} {
		f := outFile(t)
		fakeTerminal(t, f.Fd())
		restored := 0
		enableVT = func(io.Writer) (func(), bool) { return func() { restored++ }, true }
		executeContext(ctx, []string{"version", "--color=always"}, f, io.Discard)
		if restored != 1 {
			t.Errorf("%s: console mode restored %d times, want 1", name, restored)
		}
	}
}

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
