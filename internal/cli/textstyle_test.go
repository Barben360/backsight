package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/termstyle"
	"github.com/Barben360/backsight/internal/token"
)

// noColor is the Style of a stream whose color is off.
var noColor termstyle.Style

// basic is the 16-color Style, whose escapes the tests spell out.
var basic = termstyle.StyleAt(termstyle.Basic)

// forceColor turns color on for buffers (BACKSIGHT_COLOR=always in a plain
// xterm outside Claude Code); a buffer is never a Windows console, so the
// console switch is faked. Args stay as they are: outputs that quote the
// command line do not move.
func forceColor(t *testing.T) {
	t.Helper()
	cleanColorEnv(t)
	t.Setenv("BACKSIGHT_COLOR", "always")
	old := enableVT
	t.Cleanup(func() { enableVT = old })
	enableVT = func(io.Writer) (func(), bool) { return func() {}, true }
}

// colorRun runs args once with color off and once with it forced on, and
// checks the colored run strips to the plain one on both streams. It
// returns the colored stdout.
func colorRun(t *testing.T, args ...string) (plainOut, coloredOut string) {
	t.Helper()
	cleanColorEnv(t)
	var po, pe bytes.Buffer
	pcode := Execute(args, &po, &pe)
	forceColor(t)
	var co, ce bytes.Buffer
	ccode := Execute(args, &co, &ce)
	t.Setenv("BACKSIGHT_COLOR", "")
	if pcode != ccode {
		t.Fatalf("%v: exit %d plain, %d colored", args, pcode, ccode)
	}
	if got := termstyle.Strip(co.String()); got != po.String() {
		t.Errorf("%v: colored stdout strips to\n%s\nwant\n%s", args, got, po.String())
	}
	if got := termstyle.Strip(ce.String()); got != pe.String() {
		t.Errorf("%v: colored stderr strips to\n%s\nwant\n%s", args, got, pe.String())
	}
	return po.String(), co.String()
}

func sampleReport() doctor.Report {
	return doctor.Report{Checks: []doctor.Check{
		{ID: "server", Status: doctor.OK, Summary: "answering"},
		{ID: "hooks", Status: doctor.Info, Summary: "wired", Detail: "3 hooks"},
		{ID: "disk", Status: doctor.Warn, Summary: "low", Fix: "backsight retain prune"},
		{ID: "bank", Status: doctor.Error, Summary: "missing", Detail: "no bank", Fix: "backsight setup"},
	}, Errors: 1, Warnings: 1}
}

func TestDoctorTextColored(t *testing.T) {
	r := sampleReport()
	plain, colored := doctorText(noColor, r), doctorText(basic, r)
	if termstyle.Strip(colored) != plain {
		t.Fatalf("colored doctor strips to\n%s\nwant\n%s", termstyle.Strip(colored), plain)
	}
	for _, want := range []string{"\x1b[32mok\x1b[0m", "\x1b[2minfo\x1b[0m", "\x1b[33mWARN\x1b[0m", "\x1b[31mERROR\x1b[0m",
		"\x1b[36mbacksight setup\x1b[0m", "\x1b[31m1 error(s)\x1b[0m", "\x1b[33m1 warning(s)\x1b[0m"} {
		if !strings.Contains(colored, want) {
			t.Errorf("colored doctor lacks %q:\n%q", want, colored)
		}
	}
	for _, word := range []string{"ok ", "info ", "WARN ", "ERROR "} {
		if !strings.Contains(termstyle.Strip(colored), word) {
			t.Errorf("the stripped text lost the status word %q", word)
		}
	}
	clean := doctor.Report{Checks: r.Checks[:1]}
	if c := doctorText(basic, clean); strings.Contains(c, "\x1b[31m0 error") || strings.Contains(c, "\x1b[33m0 warning") {
		t.Errorf("zero counts are painted as failures: %q", c)
	}
}

func FuzzDoctorTextMasked(f *testing.F) {
	f.Add("answering", "detail", "backsight setup", "abc")
	f.Add("token "+token.Prefix+"x", "", token.Prefix, "Z9_=-")
	f.Fuzz(func(t *testing.T, summary, detail, fix, tok string) {
		if strings.Contains(summary+detail+fix+tok, "\x1b") {
			return
		}
		r := doctor.Report{Checks: []doctor.Check{
			{ID: "a", Status: doctor.Warn, Summary: summary + " " + token.Prefix + tok, Detail: detail, Fix: fix},
			{ID: token.Prefix + tok, Status: doctor.Error, Summary: summary, Fix: token.Prefix + tok + " " + fix},
		}, Errors: 1, Warnings: 1}
		plain := maskTokenText(doctorText(noColor, r))
		colored := maskTokenText(doctorText(termstyle.StyleAt(termstyle.TrueColor), r))
		if got := termstyle.Strip(colored); got != plain {
			t.Errorf("masked colored text strips to\n%q\nwant\n%q", got, plain)
		}
	})
}

func TestStepLineColored(t *testing.T) {
	s := step{Name: "server", Status: "failed", Detail: "no answer"}
	if got, want := stepLine(basic, s), "\x1b[31mfailed\x1b[0m   server: no answer\n"; got != want {
		t.Errorf("stepLine = %q, want %q", got, want)
	}
	for _, s := range []step{{Name: "a", Status: "done"}, {Name: "b", Status: "planned", Detail: "x"}, {Name: "c", Status: "odd-status"}, {Name: "d", Status: "a-very-long-status"}} {
		if got, want := termstyle.Strip(stepLine(basic, s)), stepLine(noColor, s); got != want {
			t.Errorf("stepLine(%v) strips to %q, want %q", s, got, want)
		}
	}
}

func TestVersionColored(t *testing.T) {
	setBuild(t, "0.1.0-test", "abc1234")
	_, colored := colorRun(t, "version")
	if !strings.HasPrefix(colored, "\x1b[1;35mbacksight\x1b[0m 0.1.0-test") {
		t.Errorf("version does not start with the brand word: %q", colored)
	}
	if !strings.Contains(colored, "\x1b[36mbacksight version --licenses\x1b[0m") {
		t.Errorf("the licenses command is not painted: %q", colored)
	}
}

func TestUpdateCheckColored(t *testing.T) {
	res := updateCheckResult{Installed: "0.1.0", Source: "github"}
	if got, want := updateCheckText(basic, res, nil), "\x1b[1;35mbacksight\x1b[0m 0.1.0 is \x1b[32mup to date\x1b[0m (github)\n"; got != want {
		t.Errorf("up to date = %q, want %q", got, want)
	}
	res = updateCheckResult{Installed: "0.1.0", Source: "github", UpdateAvailable: true, Latest: "0.2.0",
		Versions: []versionEntry{{Version: "0.2.0", Prerelease: true, Notes: "## Changelog\n- a fix"}}}
	colored, plain := updateCheckText(basic, res, nil), updateCheckText(noColor, res, nil)
	if termstyle.Strip(colored) != plain {
		t.Errorf("update check strips to %q, want %q", termstyle.Strip(colored), plain)
	}
	if !strings.Contains(colored, "\x1b[33m0.2.0\x1b[0m") {
		t.Errorf("the newer version is not painted: %q", colored)
	}
}

func TestColorRunNeedsNoTerminal(t *testing.T) {
	// The harness itself: a colored run of a styled command differs.
	setBuild(t, "0.1.0-test", "abc1234")
	plain, colored := colorRun(t, "version")
	if plain == colored {
		t.Fatal("forced color left version plain")
	}
}
