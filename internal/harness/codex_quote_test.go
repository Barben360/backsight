package harness

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"
)

// trickyPaths are shim paths a quoting must give back as they are.
var trickyPaths = []string{
	"/home/u/.local/share/backsight/bin/current",
	"/Users/u/Library/Application Support/backsight/bin/current",
	"/home/o'neil/bin/current",
	"/tmp/$HOME/`id`/bin/current",
	`/tmp/back\slash\\double/current`,
	"/tmp/50%/a&b|c;d/current",
	"/tmp/'quoted' \"double\"/*?[x]/current",
}

// Every shell Codex may run a hook command with ($SHELL -lc) reads QuoteSh's
// word back as the path: each one found here is run.
func TestQuoteShRoundTripsInEveryShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shells")
	}
	ran := 0
	for _, sh := range []string{"sh", "bash", "zsh", "fish"} {
		bin, err := exec.LookPath(sh)
		if err != nil {
			continue
		}
		ran++
		for _, p := range trickyPaths {
			out, err := exec.Command(bin, "-c", "printf '%s' "+QuoteSh(p)).Output()
			if err != nil || string(out) != p {
				t.Errorf("%s: %q read back as %q (%v)", sh, p, out, err)
			}
		}
	}
	if ran == 0 {
		t.Skip("no shell found")
	}
}

func TestQuoteShForm(t *testing.T) {
	for in, want := range map[string]string{
		"/a b/current": `'/a b/current'`,
		"/o'neil":      `'/o'\''neil'`,
		`/a\b`:         `'/a'\\'b'`,
	} {
		if got := QuoteSh(in); got != want {
			t.Errorf("QuoteSh(%q) = %s, want %s", in, got, want)
		}
	}
}

// cmd's quoting keeps spaces and cmd's operators inside double quotes, and
// refuses what cmd cannot read back.
func TestQuoteCmd(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\u\AppData\Local\backsight\bin\current.exe`: `"C:\Users\u\AppData\Local\backsight\bin\current.exe"`,
		`C:\Users\Jane Doe\a&b ^c (x)\bin\current.exe`:       `"C:\Users\Jane Doe\a&b ^c (x)\bin\current.exe"`,
		`C:\Users\o'neil\$x\bin\current.exe`:                 `"C:\Users\o'neil\$x\bin\current.exe"`,
	} {
		if got, err := QuoteCmd(in); err != nil || got != want {
			t.Errorf("QuoteCmd(%q) = %s, %v, want %s", in, got, err, want)
		}
	}
	for _, in := range []string{`C:\50%\current.exe`, `C:\%USERPROFILE%\current.exe`, `C:\a"b\current.exe`, "C:\\a\nb\\current.exe"} {
		if got, err := QuoteCmd(in); !errors.Is(err, ErrUnquotable) || got != "" {
			t.Errorf("QuoteCmd(%q) = %q, %v, want refused", in, got, err)
		}
	}
}
