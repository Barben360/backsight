package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/backsight/internal/testtool"
)

// sourced is what bash gives back for key after sourcing file, as Claude
// Code's Bash tool (Git Bash on Windows) does with CLAUDE_ENV_FILE.
func sourced(t *testing.T, bash, file, key string) string {
	t.Helper()
	// The file's path goes in the environment: Git Bash reads C:/x, and
	// a backslash path typed into the script would be mangled.
	cmd := exec.Command(bash, "-c", `. "$ENVFILE"; printf %s "$`+key+`"`)
	cmd.Env = append(os.Environ(), "ENVFILE="+filepath.ToSlash(file))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash sourcing %s: %v\n%s", file, err, out)
	}
	return string(out)
}

// Each line is `export KEY='value'`, single-quoted the POSIX way, so that
// bash gives back any value exactly: quotes, $, backquotes, backslashes,
// spaces, & and non-ASCII. A Windows path is written with forward
// slashes, which Git Bash and the Windows programs it starts both read.
func TestEnvFileLinesSourceUnderBash(t *testing.T) {
	bash := testtool.Bash(t)
	values := map[string]string{
		"BS_PLAIN":  "billing",
		"BS_QUOTE":  `it's "quoted"`,
		"BS_SHELL":  "$HOME `id` $(id) \\n \\",
		"BS_SPACES": "Jé & Co; ls | wc",
		"BS_EMPTY":  "",
		"BS_LINES":  "first line\nsecond 'line'\n",
		"BS_DRIVE":  `C:/Users/Jé & Co/src`,
	}
	file := filepath.Join(t.TempDir(), "env")
	var b strings.Builder
	for k, v := range values {
		line := envFileLine("linux", k, v)
		if !strings.HasPrefix(line, "export "+k+"='") || !strings.HasSuffix(line, "'\n") {
			t.Errorf("%s: line %q is not export KEY='value'", k, line)
		}
		b.WriteString(line)
	}
	// The writer's own rule for this OS: a backslash drive path comes
	// back with forward slashes on Windows (Git Bash), unchanged elsewhere.
	native, nativeWant := `C:\Users\Jé & Co\src`, `C:\Users\Jé & Co\src`
	if runtime.GOOS == "windows" {
		nativeWant = `C:/Users/Jé & Co/src`
	}
	b.WriteString(envFileLine(runtime.GOOS, "BS_NATIVE", native))
	values["BS_NATIVE"] = nativeWant
	if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range values {
		if got := sourced(t, bash, file, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// Windows paths, in either spelling, come back with forward slashes;
	// elsewhere a backslash is part of the value.
	for in, want := range map[string]string{`C:\Users\Jé & Co\src\a'b`: `C:/Users/Jé & Co/src/a'b`, `d:/code/x`: `d:/code/x`} {
		if got := envFileLine("windows", "K", in); got != "export K="+shellQuote(want)+"\n" {
			t.Errorf("windows %q: %q", in, got)
		}
	}
	if got := envFileLine("linux", "K", `C:\x`); got != `export K='C:\x'`+"\n" {
		t.Errorf("linux: %q", got)
	}
	if got := envFileLine("windows", "K", `not\a path`); got != `export K='not\a path'`+"\n" {
		t.Errorf("a backslash that is no Windows path: %q", got)
	}
}

// SessionStart's own file sources under bash: the CLI fallback gets the
// session and project back.
func TestSessionStartEnvFileSourcesUnderBash(t *testing.T) {
	bash := testtool.Bash(t)
	home := hookHome(t)
	envFile := filepath.Join(t.TempDir(), "claude env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)
	fire(t, "session-start", map[string]any{"session_id": "s-1", "source": "startup", "cwd": filepath.Join(home, "billing")})
	if got := sourced(t, bash, envFile, "BACKSIGHT_SESSION"); got != "s-1" {
		t.Errorf("BACKSIGHT_SESSION = %q", got)
	}
	if got := sourced(t, bash, envFile, "BACKSIGHT_PROJECT"); got != "billing" {
		t.Errorf("BACKSIGHT_PROJECT = %q", got)
	}
}

// plainAssignment matches a line Claude Code (2.1.296+) copies into
// PowerShell commands: export NAME=value, the value made of unquoted
// letters, digits and _ @ % + = : , . / -, single-quoted text, and
// double-quoted text with $, ` and " escaped
// (https://code.claude.com/docs/en/hooks.md#persisted-variables-in-powershell-commands).
// One line of another kind and PowerShell commands get none of them.
var plainAssignment = regexp.MustCompile(`^export [A-Za-z_][A-Za-z0-9_]*=(?:[A-Za-z0-9_@%+=:,./-]+|'[^']*'|"(?:[^"$` + "`" + `\\]|\\[$` + "`" + `"\\])*")*\n$`)

// BACKSIGHT_PROJECT and BACKSIGHT_SESSION, for the project names
// SessionStart writes (a directory's or a repository's basename, a
// .backsight.toml name) and a session id, stay single-quoted plain
// assignments with no / or \ in the value: PowerShell commands on Windows
// get them (a value with / or \ is dropped there).
func TestEnvFileLinesStayPlainForPowerShell(t *testing.T) {
	for _, c := range []struct{ key, value string }{
		{"BACKSIGHT_PROJECT", "orchid"},
		{"BACKSIGHT_PROJECT", "billing-api_v2"},
		{"BACKSIGHT_PROJECT", "Jé & Co"},
		{"BACKSIGHT_PROJECT", "transverse"},
		{"BACKSIGHT_SESSION", "0f3c9a5e-7b1d-4c2a-9e8f-1a2b3c4d5e6f"},
	} {
		for _, goos := range []string{"windows", "linux"} {
			line := envFileLine(goos, c.key, c.value)
			if line != "export "+c.key+"='"+c.value+"'\n" || !plainAssignment.MatchString(line) || strings.ContainsAny(c.value, `/\`) {
				t.Errorf("%s %s=%q: %q is not a single-quoted plain assignment", goos, c.key, c.value, line)
			}
		}
	}
}
