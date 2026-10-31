//go:build windows

package cli

import (
	"encoding/base64"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// A resume command pasted in PowerShell gives backsight the exact
// arguments, whatever the path holds (Review Focus 4: spaces, &, a quote,
// non-ASCII).
func TestResumeCommandsRoundTripThroughPowerShell(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("no Windows PowerShell")
	}
	out := `C:\Users\Jé & Co\O'Brien $x;(y)\join.token`
	f := exposeFlags{private: "192.168.1.10", out: out, answers: []string{"cleartext_lan=yes"}}
	cmd := f.command()
	// backsight is a function printing each argument, base64 of its UTF-8
	// bytes, on its own line.
	script := "function backsight { foreach ($a in $args) { [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes([string]$a)) } }; " + cmd
	raw, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, raw)
	}
	var got []string
	for _, l := range strings.Fields(string(raw)) {
		b, err := base64.StdEncoding.DecodeString(l)
		if err != nil {
			t.Fatalf("%s: output %q", cmd, raw)
		}
		got = append(got, string(b))
	}
	want := []string{"server", "expose", "--private", "192.168.1.10", "--out", out, "--answer", "cleartext_lan=yes"}
	if !slices.Equal(got, want) {
		t.Errorf("%s\ngave %q\nwant %q", cmd, got, want)
	}
}
