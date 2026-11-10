package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/paths"
	"github.com/Barben360/backsight/internal/splash"
	"github.com/Barben360/backsight/internal/termstyle"
	"github.com/Barben360/backsight/internal/token"
)

// splashTerminal is an interactive UTF-8 terminal width columns wide (a
// failing width query when err is set), over a fresh BACKSIGHT_HOME.
func splashTerminal(t *testing.T, width int, err error) {
	t.Helper()
	cleanColorEnv(t)
	fakeTerminal(t)
	for k, v := range map[string]string{"BACKSIGHT_HOME": t.TempDir(), "BACKSIGHT_SPLASH": "", "LANG": "C.UTF-8", "LC_ALL": "", "LC_CTYPE": ""} {
		t.Setenv(k, v)
	}
	old, oldUTF8 := termWidth, consoleUTF8
	t.Cleanup(func() { termWidth, consoleUTF8 = old, oldUTF8 })
	termWidth = func(uintptr) (int, error) { return width, err }
	// The locale on Unix, the console code page on Windows: fixed here,
	// each tested in internal/splash.
	consoleUTF8 = func(func(string) string) bool { return true }
}

// runAt runs args with stdout on a file and returns stdout and the code.
func runAt(t *testing.T, args ...string) (string, int, *env) {
	t.Helper()
	out := outFile(t)
	code, e := executeContext(context.Background(), args, out, io.Discard)
	return fileText(t, out), code, e
}

const logoTop = "     ▄▄▀▀▀"

// splashCount is how many splashes out holds (the logo's dot is drawn once
// per splash).
func splashCount(out string) int { return strings.Count(termstyle.Strip(out), "●") }

func TestSplashBeforeTheFirstByte(t *testing.T) {
	setBuild(t, "0.1.34-m7", "abc1234")
	splashTerminal(t, 80, nil)
	plain, code, _ := runAt(t, "version", "--no-splash")
	if code != 0 || splashCount(plain) != 0 {
		t.Fatalf("--no-splash: exit %d, %d splashes", code, splashCount(plain))
	}
	got, code, _ := runAt(t, "version")
	s := termstyle.Strip(got)
	if code != 0 || splashCount(got) != 1 {
		t.Fatalf("exit %d, %d splashes in\n%s", code, splashCount(got), s)
	}
	head, ok := strings.CutSuffix(s, termstyle.Strip(plain))
	if !ok || !strings.HasPrefix(head, logoTop) || !strings.HasSuffix(head, "\n\n") {
		t.Fatalf("the splash is not the logo, then a blank line, then the output:\n%s", s)
	}
	for _, want := range []string{"backsight 0.1.34-m7 · Hindsight ", "Setup    not set up yet · backsight setup"} {
		if !strings.Contains(head, want) {
			t.Errorf("no %q in the splash:\n%s", want, head)
		}
	}
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("the splash is not colored")
	}
}

func TestSplashOnHelp(t *testing.T) {
	splashTerminal(t, 80, nil)
	for _, args := range [][]string{{"--help"}, {"help"}, {}, {"stats", "--help"}} {
		if got, _, _ := runAt(t, args...); splashCount(got) != 1 || !strings.HasPrefix(termstyle.Strip(got), logoTop) {
			t.Errorf("%q: %d splashes", args, splashCount(got))
		}
	}
}

func TestNoSplash(t *testing.T) {
	for _, c := range []struct {
		name  string
		width int
		err   error
		env   map[string]string
		args  []string
		code  int
	}{
		{name: "71 columns", width: 71, args: []string{"version"}},
		{name: "width unknown", width: 200, err: errors.New("not a tty"), args: []string{"version"}},
		{name: "--no-splash", width: 80, args: []string{"--no-splash", "version"}},
		{name: "BACKSIGHT_SPLASH=0", width: 80, env: map[string]string{"BACKSIGHT_SPLASH": "0"}, args: []string{"version"}},
		{name: "NO_COLOR", width: 80, env: map[string]string{"NO_COLOR": "1"}, args: []string{"version"}},
		{name: "CLAUDECODE", width: 80, env: map[string]string{"CLAUDECODE": "1"}, args: []string{"version"}},
		{name: "--color=never", width: 80, args: []string{"--color=never", "version"}},
		{name: "--json", width: 80, args: []string{"version", "--json", "--color=always"}},
		{name: "bare group", width: 80, args: []string{"server"}, code: 2},
		{name: "unknown flag", width: 80, args: []string{"version", "--bogus"}, code: 2},
		{name: "runtime failure", width: 80, args: []string{"server", "status"}, code: 10},
		{name: "guide", width: 80, args: []string{"guide"}},
		{name: "plain command", width: 80, args: []string{"secret", "list"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			splashTerminal(t, c.width, c.err)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			got, code, _ := runAt(t, c.args...)
			if code != c.code {
				t.Fatalf("exit %d, want %d", code, c.code)
			}
			if splashCount(got) != 0 || strings.Contains(termstyle.Strip(got), logoTop) {
				t.Fatalf("a splash:\n%s", got)
			}
		})
	}
}

func TestNoSplashIntoAPipeEvenWithColorAlways(t *testing.T) {
	splashTerminal(t, 80, nil)
	fakeTerminal(t, ^uintptr(0)) // no real descriptor is a terminal
	got, _, e := runAt(t, "--color=always", "version")
	if termstyle.LevelOf(e.stdout) == termstyle.None {
		t.Fatalf("--color=always left stdout uncolored")
	}
	if splashCount(got) != 0 {
		t.Fatalf("a splash into a file:\n%s", got)
	}
}

// The splash state lives in the stdout writer: an env copied before any
// stdout byte (server ssh install builds one) and the original print one
// splash between them.
func TestSplashOnceForACopiedEnv(t *testing.T) {
	splashTerminal(t, 80, nil)
	out := outFile(t)
	// A runtime failure: nothing on stdout yet, so no splash so far.
	if code, _ := executeContext(context.Background(), []string{"server", "status"}, out, io.Discard); code != 10 {
		t.Fatalf("exit %d", code)
	}
	_, e := executeContext(context.Background(), []string{"server", "status"}, out, io.Discard)
	copied := *e
	_, _ = io.WriteString(copied.stdout, "from the copy\n")
	_, _ = io.WriteString(e.stdout, "from the original\n")
	if got := fileText(t, out); splashCount(got) != 1 || !strings.HasSuffix(got, "from the copy\nfrom the original\n") {
		t.Fatalf("%d splashes:\n%s", splashCount(got), got)
	}
}

// A connection token in a panel value is masked like any text output.
func TestSplashMasksATokenInThePanel(t *testing.T) {
	splashTerminal(t, 120, nil)
	home := os.Getenv("BACKSIGHT_HOME")
	d, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	host := token.Prefix + "abcdefghij"
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "schema_version = 1\n[server]\nmode = \"remote\"\nurl = \"https://" + host + ":8888\"\n"
	if err := os.WriteFile(d.ConfigFile(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, _ := runAt(t, "version")
	if splashCount(got) != 1 || strings.Contains(got, "abcdefghij") || !strings.Contains(termstyle.Strip(got), "client of ") {
		t.Fatalf("home %s: the token shows or no Setup line:\n%s", home, termstyle.Strip(got))
	}
}

func TestSplashPanicDropsOnlyTheSplash(t *testing.T) {
	splashTerminal(t, 80, nil)
	plain, _, _ := runAt(t, "version", "--no-splash")
	old := buildPanel
	t.Cleanup(func() { buildPanel = old })
	buildPanel = func(splash.Sources) ([]splash.Line, string) { panic("boom") }
	got, code, _ := runAt(t, "version")
	if code != 0 || got != plain {
		t.Fatalf("exit %d, output\n%q\nwant\n%q", code, got, plain)
	}
}

func TestSplashASCIIOffUTF8(t *testing.T) {
	splashTerminal(t, 80, nil)
	consoleUTF8 = func(func(string) string) bool { return false }
	got, _, _ := runAt(t, "version")
	head, _, _ := strings.Cut(termstyle.Strip(got), "\n\n")
	if !strings.HasPrefix(head, "     .--'") {
		t.Fatalf("not the ASCII logo:\n%s", head)
	}
	for i := 0; i < len(head); i++ {
		if head[i] >= 0x80 {
			t.Fatalf("byte %#x in the ASCII splash:\n%s", head[i], head)
		}
	}
}

func TestSplashCreatesNothing(t *testing.T) {
	splashTerminal(t, 80, nil)
	home := os.Getenv("BACKSIGHT_HOME") + "/fresh"
	t.Setenv("BACKSIGHT_HOME", home)
	if got, _, _ := runAt(t, "version"); splashCount(got) != 1 {
		t.Fatalf("no splash")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("version with its splash created %s (%v)", home, err)
	}
}
