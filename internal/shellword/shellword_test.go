package shellword

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPOSIX(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "''",
		"my-proj_1.v2/a:b=c@d%e+f,g": "my-proj_1.v2/a:b=c@d%e+f,g",
		"a b":                        "'a b'",
		"R&D $(touch /tmp/pwned)":    "'R&D $(touch /tmp/pwned)'",
		"it's":                       `'it'\''s'`,
		"a`b`":                       "'a`b`'",
		`a\b`:                        `'a\b'`,
		"~":                          "'~'",
		"it\u2019s":                  "'it\u2019s'",
	} {
		if got := POSIX(in); got != want {
			t.Errorf("POSIX(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPowerShell(t *testing.T) {
	for in, want := range map[string]string{
		"":                        "''",
		`C:\Users\me\x`:           `C:\Users\me\x`,
		"R&D $(touch /tmp/pwned)": "'R&D $(touch /tmp/pwned)'",
		"it's":                    "'it''s'",
		"it\u2019s":               "'it\u2019\u2019s'",
		"a,b":                     "'a,b'",
		"@h":                      "'@h'",
		"--%":                     "'--%'",
	} {
		if got := PowerShell(in); got != want {
			t.Errorf("PowerShell(%q) = %s, want %s", in, got, want)
		}
	}
}

// Word is this system's shell: POSIX sh off Windows, PowerShell on it.
func TestWordFollowsTheSystem(t *testing.T) {
	want := POSIX("it's")
	if runtime.GOOS == "windows" {
		want = PowerShell("it's")
	}
	if got := Word("it's"); got != want {
		t.Errorf("Word = %s, want %s", got, want)
	}
}

// Run by sh, a POSIX word gives back the exact value, and the command
// substitution in it never runs. A PowerShell word pasted in sh runs
// nothing either (its quotes may be lost, never its quoting).
func TestWordsRunNothingInSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	pwned := filepath.Join(dir, "pwned")
	for _, v := range []string{
		"R&D $(touch " + pwned + ")",
		"Bob's notes; touch " + pwned,
		"it's `touch " + pwned + "`",
		"a'b\u2019c $(touch " + pwned + ")'",
		"'",
		"",
	} {
		out, err := exec.Command(sh, "-c", "printf '%s\\n' "+POSIX(v)).Output()
		if err != nil {
			t.Fatalf("%s: %v", POSIX(v), err)
		}
		if got := strings.TrimSuffix(string(out), "\n"); got != v {
			t.Errorf("POSIX %s gave %q, want %q", POSIX(v), got, v)
		}
		if _, err := exec.Command(sh, "-c", "printf '%s\\n' "+PowerShell(v)).Output(); err != nil {
			t.Fatalf("%s: %v", PowerShell(v), err)
		}
		if _, err := os.Stat(pwned); err == nil {
			t.Fatalf("%q ran a command", v)
		}
	}
}
