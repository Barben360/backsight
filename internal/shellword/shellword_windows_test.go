//go:build windows

package shellword

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
)

// Pasted in PowerShell, a word gives back the exact value, in double quotes
// (a quote inside, nothing that expands) or in single quotes, and runs
// nothing.
func TestWordsRoundTripThroughPowerShell(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("no Windows PowerShell")
	}
	for _, v := range []string{
		"Bob's notes",
		`C:\Users\O'Brien\x`,
		"it\u2019s & it's, @all ~me; (x) | y > z",
		`a'b\ c\x`,
		"m'1$(Get-Date)",
		"a'b`c",
		`a'b"c`,
		`C:\Users\Jé & Co\x`,
	} {
		// out prints the argument it gets, base64 of its UTF-8 bytes.
		script := "function out { [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$args[0])) }; out " + Word(v)
		raw, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", Word(v), err, raw)
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatalf("%s: output %q", Word(v), raw)
		}
		if string(b) != v {
			t.Errorf("%s gave %q, want %q", Word(v), b, v)
		}
	}
}
