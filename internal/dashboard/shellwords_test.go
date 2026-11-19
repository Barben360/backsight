package dashboard

import (
	"encoding/base64"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// shWords is the arguments sh reads from words, pasted after a command
// (each printed between < and >); false off Linux and macOS or without sh.
// Whatever follows a ; or a & the words let through runs as a command of
// its own, so a word split there shows as fewer or other arguments.
func shWords(t *testing.T, words string) ([]string, bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return nil, false
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		return nil, false
	}
	out, _ := exec.Command(sh, "-c", "printf '<%s>\\n' "+words).Output() // a split command may fail: its output is what counts
	var got []string
	for _, l := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if a, ok := strings.CutPrefix(l, "<"); ok {
			got = append(got, strings.TrimSuffix(a, ">"))
		}
	}
	return got, true
}

// powerShellWords is the arguments Windows PowerShell reads from words
// pasted after a command; false off Windows or without it.
func powerShellWords(t *testing.T, words string) ([]string, bool) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return nil, false
	}
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		return nil, false
	}
	// out prints each argument it gets as base64 of its UTF-8 bytes.
	script := "function out { foreach ($a in $args) { [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$a)) } }; out " + words
	raw, _ := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", script).Output()
	var got []string
	for _, l := range strings.Fields(string(raw)) {
		b, err := base64.StdEncoding.DecodeString(l)
		if err != nil {
			t.Fatalf("%s: output %q", words, raw)
		}
		got = append(got, string(b))
	}
	return got, true
}
