package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/doctor"
	"github.com/Barben360/backsight/internal/output"
	"github.com/Barben360/backsight/internal/setup"
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

// coloredGolden runs args again with color forced on: the output, with
// normalize applied and its escapes stripped, is the golden name's text,
// and a text golden (not a -json one) is colored somewhere.
func coloredGolden(t *testing.T, name string, normalize func(string) string, args ...string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	forceColor(t)
	defer t.Setenv("BACKSIGHT_COLOR", "")
	var out, errb bytes.Buffer
	if code := Execute(args, &out, &errb); code != output.OK {
		t.Fatalf("colored %v: exit %d: %s", args, code, errb.String())
	}
	got := normalize(out.String())
	if termstyle.Strip(got) != string(want) {
		t.Errorf("%s: the colored output strips to\n%s\nwant\n%s", name, termstyle.Strip(got), want)
	}
	isJSON := strings.Contains(name, "-json")
	if hasEsc := strings.Contains(got, "\x1b["); hasEsc == isJSON {
		t.Errorf("%s: colored output has escapes = %v", name, hasEsc)
	}
}

// stripsTo checks colored strips to plain and is colored.
func stripsTo(t *testing.T, what, colored, plain string) {
	t.Helper()
	if termstyle.Strip(colored) != plain {
		t.Errorf("%s strips to\n%q\nwant\n%q", what, termstyle.Strip(colored), plain)
	}
	if !strings.Contains(colored, "\x1b[") {
		t.Errorf("%s is not colored: %q", what, colored)
	}
}

func TestSetupTextColored(t *testing.T) {
	steps := []setup.Step{{Name: "config", Detail: "write config.toml"}, {Name: "a-long-step-name", Detail: "x"}}
	plan := setupPlanText(basic, steps)
	stripsTo(t, "setup plan", plan, setupPlanText(noColor, steps))
	if !strings.Contains(plan, "\x1b[1msetup will:\x1b[0m") {
		t.Errorf("the plan heading is not bold: %q", plan)
	}
	r := setupResult{Plan: true, Steps: steps, Preflight: []setup.Check{
		{ID: "docker", Status: setup.OK, Summary: "running"},
		{ID: "disk", Status: setup.Fail, Summary: "full", Fix: "free some space"},
	}}
	got := setupText(basic, r)
	stripsTo(t, "setup --plan", got, setupText(noColor, r))
	for _, want := range []string{"\x1b[32mok\x1b[0m", "\x1b[31mfail\x1b[0m", "\x1b[36mfree some space\x1b[0m"} {
		if !strings.Contains(got, want) {
			t.Errorf("setup --plan lacks %q: %q", want, got)
		}
	}
	done := setupResult{Steps: steps, TryFirst: "ask Claude", NextSteps: []string{"restart"},
		Preflight: []setup.Check{{ID: "disk", Status: setup.Fail, Summary: "full", Fix: "free"}}}
	stripsTo(t, "setup done", setupText(basic, done), setupText(noColor, done))
}

func TestQuestionTextColored(t *testing.T) {
	q := setup.Question{ID: "runtime", Prompt: "Which runtime?", Default: "docker", Recommended: "docker",
		Options: []setup.Option{{Value: "docker", Label: "containers", Cost: "2 GB"}, {Value: "native", Label: "no Docker", Caveat: "beta"}}, Free: true}
	got := askText(basic, q)
	stripsTo(t, "a setup question", got, askText(noColor, q))
	for _, want := range []string{"\x1b[1mWhich runtime?\x1b[0m", "\x1b[32m[recommended]\x1b[0m", "\x1b[33m⚠ beta\x1b[0m"} {
		if !strings.Contains(got, want) {
			t.Errorf("the question lacks %q: %q", want, got)
		}
	}
}

func TestConnectTextColored(t *testing.T) {
	r := connectResult{URL: "https://nas:8888", Bank: "b", ServerID: "srv1", HindsightVersion: "1.2.0", Plan: true,
		Warnings: []string{"old server"}, Notes: []string{"fyi"}, Steps: []step{{Name: "config", Status: "planned", Detail: "point at nas"}}}
	got := connectText(basic, r)
	stripsTo(t, "connect --plan", got, connectText(noColor, r))
	for _, want := range []string{"\x1b[33mwarning\x1b[0m", "\x1b[2mnote\x1b[0m", "\n\x1b[1mconnect will:\x1b[0m"} {
		if !strings.Contains(got, want) {
			t.Errorf("connect --plan lacks %q: %q", want, got)
		}
	}
	setupDone := setupResult{Steps: []setup.Step{{Name: "x"}}, TryFirst: "ask"}
	r2 := connectResult{Steps: r.Steps, Setup: &setupDone, Warnings: []string{"w"}, Notes: []string{"n"}, NextSteps: []string{"restart"}}
	stripsTo(t, "connect done", connectText(basic, r2), connectText(noColor, r2))
}

func TestUninstallTextColored(t *testing.T) {
	r := uninstallResult{Steps: []undoAction{{Name: "hooks", Status: "done"}, {Name: "server", Status: "planned", Detail: "stop"}},
		Kept: []string{"backups"}, Warnings: []string{"a lock"}, NextSteps: []string{"restart Claude Code"}}
	got := uninstallText(basic, r)
	stripsTo(t, "uninstall", got, uninstallText(noColor, r))
	for _, want := range []string{"\x1b[32mdone\x1b[0m", "\x1b[33mplanned\x1b[0m", "\x1b[33mwarning:\x1b[0m"} {
		if !strings.Contains(got, want) {
			t.Errorf("uninstall lacks %q: %q", want, got)
		}
	}
}

func TestErrorPrefixRed(t *testing.T) {
	_, _ = colorRun(t, "stats", "--until", "2026-01-01") // strips to the plain run on both streams
	forceColor(t)
	var out, errb bytes.Buffer
	if code := Execute([]string{"stats", "--until", "2026-01-01"}, &out, &errb); code != output.Usage {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if want := "\x1b[31mbacksight:\x1b[0m --until needs --since\n"; errb.String() != want {
		t.Errorf("stderr = %q, want %q", errb.String(), want)
	}
}

func TestPlainCommandErrorPrefixStaysPlain(t *testing.T) {
	forceColor(t)
	var out, errb bytes.Buffer
	if code := Execute([]string{"secret", "set"}, &out, &errb); code != output.Usage {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(errb.String(), "backsight: ") || strings.Contains(out.String()+errb.String(), "\x1b") {
		t.Errorf("a plain command's error is colored: %q", errb.String())
	}
}

func TestBackfillProgressColored(t *testing.T) {
	var colored, plain bytes.Buffer
	ce := &env{stderr: termstyle.NewWriter(&colored, func() termstyle.Level { return termstyle.Basic })}
	pe := &env{stderr: &plain}
	for _, ev := range []backfillEvent{{Step: "session", Status: "done", Done: 1, Total: 2, Session: "s1"}, {Step: "wait", Done: 1, Total: 2, Detail: "the server"}} {
		ce.backfillProgress(ev)
		pe.backfillProgress(ev)
	}
	stripsTo(t, "backfill progress", colored.String(), plain.String())
	for _, want := range []string{"\x1b[32mdone\x1b[0m", "\x1b[33mwaiting\x1b[0m"} {
		if !strings.Contains(colored.String(), want) {
			t.Errorf("backfill progress lacks %q: %q", want, colored.String())
		}
	}
}

// tokenDoctor makes doctor report a check that quotes a connection token.
func tokenDoctor(t *testing.T, tok string) {
	t.Helper()
	old := doctorRun
	doctorRun = func(context.Context, doctor.Env) doctor.Report {
		return doctor.Report{Checks: []doctor.Check{
			{ID: "config", Status: doctor.OK, Summary: "read"},
			{ID: "connect", Status: doctor.Error, Summary: "the token " + tok + " was refused", Fix: "backsight connect " + tok},
		}, Errors: 1}
	}
	t.Cleanup(func() { doctorRun = old })
}

func TestStyledTokenIsMasked(t *testing.T) {
	isolate(t)
	tok := token.Prefix + "eyJrZXkiOiJzZWNyZXQifQ"
	tokenDoctor(t, tok)
	plain, colored := colorRun(t, "doctor")
	if !strings.Contains(colored, "\x1b[") {
		t.Fatalf("doctor is not colored: %q", colored)
	}
	if strings.Contains(colored, tok) || strings.Contains(plain, tok) {
		t.Errorf("the token reached the output:\n%q", colored)
	}
	if !strings.Contains(colored, "\x1b[36mbacksight connect "+token.Prefix+"[redacted]\x1b[0m") {
		t.Errorf("the painted fix is not masked whole: %q", colored)
	}
}

func TestJSONUnchangedWithForcedColor(t *testing.T) {
	isolate(t)
	tokenDoctor(t, "none")
	setBuild(t, "0.1.0-test", "abc1234")
	for _, args := range [][]string{{"doctor", "--json"}, {"version", "--json"}, {"stats", "--until", "2026-01-01", "--json"}} {
		cleanColorEnv(t)
		_, plain, plainErr := run(t, args...)
		forceColor(t)
		_, colored, coloredErr := run(t, args...)
		t.Setenv("BACKSIGHT_COLOR", "")
		if colored != plain || coloredErr != plainErr {
			t.Errorf("%v: --json moved with forced color:\n%q\n%q", args, colored, plain)
		}
		if strings.Contains(colored+coloredErr, "\x1b") {
			t.Errorf("%v: an escape in --json output", args)
		}
	}
}
