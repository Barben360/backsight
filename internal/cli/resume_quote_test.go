package cli

import (
	"runtime"
	"testing"
)

func TestPowerShellQuoting(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\me\AppData\Local\Temp\x\join.token`: `C:\Users\me\AppData\Local\Temp\x\join.token`,
		`C:\Users\RUNNER~1\x`:                         `C:\Users\RUNNER~1\x`,
		"u@h":                                         "u@h",
		"consent=yes":                                 "consent=yes",
		`C:\Users\O'Brien\x`:                          `'C:\Users\O''Brien\x'`,
		`C:\Users\Jé & Co\x`:                          `'C:\Users\Jé & Co\x'`,
		"a,b":                                         "'a,b'",
		"@h":                                          "'@h'",
		`~\x`:                                         `'~\x'`,
		"$env:HOME":                                   "'$env:HOME'",
		"a`b":                                         "'a`b'",
		"--%":                                         "'--%'",
		"it\u2019s":                                   "'it\u2019\u2019s'",
		"":                                            "''",
	} {
		if got := psWord(in); got != want {
			t.Errorf("psWord(%q) = %s, want %s", in, got, want)
		}
	}
	if got := psQuote("plain"); got != "'plain'" {
		t.Errorf("psQuote: %s", got)
	}
}

// Linux and macOS keep their POSIX quoting byte for byte.
func TestResumeQuotingIsPOSIXOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX quoting is for Linux and macOS")
	}
	for _, s := range []string{"/home/u/x", "/home/O'Brien/x", "a b", "u@h", "a,b", ""} {
		if resumeWord(s) != shellWord(s) || resumeQuote(s) != shellQuote(s) {
			t.Errorf("%q: %s %s, want %s %s", s, resumeWord(s), resumeQuote(s), shellWord(s), shellQuote(s))
		}
	}
}
