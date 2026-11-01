package doctor

import (
	"errors"
	"strings"
	"testing"
)

// Claude Code runs hooks in Git Bash when it finds one, else in
// PowerShell, where SessionStart gets no CLAUDE_ENV_FILE. The check is ok
// only when Git Bash is named (CLAUDE_CODE_GIT_BASH_PATH) or in a standard
// Git for Windows location; a git on PATH elsewhere (PortableGit) gives
// the command that names its bash.exe.
func TestGitBashCases(t *testing.T) {
	const (
		pf       = `C:\Program Files`
		local    = `C:\Users\jo\AppData\Local`
		portable = local + `\PortableGit`
	)
	for _, c := range []struct {
		name     string
		env      map[string]string
		files    []string
		git      string // git.exe found on PATH, "" none
		status   Status
		say, fix []string // substrings of the summary+detail, of the fix
		noFix    bool
	}{
		{name: "named and present", env: map[string]string{"CLAUDE_CODE_GIT_BASH_PATH": portable + `\bin\bash.exe`}, files: []string{portable + `\bin\bash.exe`},
			status: OK, say: []string{portable + `\bin\bash.exe`, "CLAUDE_CODE_GIT_BASH_PATH"}, noFix: true},
		{name: "named but missing", env: map[string]string{"CLAUDE_CODE_GIT_BASH_PATH": `D:\gone\bin\bash.exe`}, git: portable + `\cmd\git.exe`, files: []string{portable + `\bin\bash.exe`, portable + `\cmd\git.exe`},
			status: Warn, say: []string{`D:\gone\bin\bash.exe`, "does not exist", "PowerShell", "BACKSIGHT_PROJECT"},
			fix: []string{"[Environment]::SetEnvironmentVariable('CLAUDE_CODE_GIT_BASH_PATH', '" + portable + `\bin\bash.exe', 'User')`, "restart Claude Code"}},
		{name: "named the launcher", env: map[string]string{"CLAUDE_CODE_GIT_BASH_PATH": portable + `\git-bash.exe`}, git: portable + `\cmd\git.exe`,
			files:  []string{portable + `\git-bash.exe`, portable + `\cmd\git.exe`, portable + `\bin\bash.exe`},
			status: Warn, say: []string{portable + `\git-bash.exe`, "not bash.exe", "PowerShell"},
			fix: []string{"'" + portable + `\bin\bash.exe'`, "restart Claude Code"}},
		{name: "named in another case", env: map[string]string{"CLAUDE_CODE_GIT_BASH_PATH": portable + `\bin\BASH.EXE`}, files: []string{portable + `\bin\BASH.EXE`},
			status: OK, noFix: true},
		{name: "Program Files", files: []string{pf + `\Git\bin\bash.exe`}, status: OK, say: []string{pf + `\Git\bin\bash.exe`}, noFix: true},
		{name: "per-user Git for Windows", files: []string{local + `\Programs\Git\bin\bash.exe`}, status: OK, say: []string{local + `\Programs\Git\bin\bash.exe`}, noFix: true},
		{name: "only PortableGit on PATH", git: portable + `\cmd\git.exe`, files: []string{portable + `\cmd\git.exe`, portable + `\bin\bash.exe`},
			status: Warn, say: []string{"PowerShell", "BACKSIGHT_PROJECT", "BACKSIGHT_SESSION"},
			fix: []string{"[Environment]::SetEnvironmentVariable('CLAUDE_CODE_GIT_BASH_PATH', '" + portable + `\bin\bash.exe', 'User')`, "restart Claude Code"}},
		{name: "git in mingw64", git: portable + `\mingw64\bin\git.exe`, files: []string{portable + `\mingw64\bin\git.exe`, portable + `\bin\bash.exe`},
			status: Warn, fix: []string{"'" + portable + `\bin\bash.exe'`}},
		{name: "a quote in the path", git: `C:\Users\o'neil\Git\cmd\git.exe`, files: []string{`C:\Users\o'neil\Git\cmd\git.exe`, `C:\Users\o'neil\Git\bin\bash.exe`},
			status: Warn, fix: []string{`'C:\Users\o''neil\Git\bin\bash.exe'`}},
		{name: "nothing", status: Warn, say: []string{"PowerShell"}, fix: []string{"Git for Windows", "CLAUDE_CODE_GIT_BASH_PATH", "restart Claude Code"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"ProgramFiles": pf, "LOCALAPPDATA": local}
			for k, v := range c.env {
				env[k] = v
			}
			p := GitBashProbe{
				Getenv: func(k string) string { return env[k] },
				Exists: func(path string) bool {
					for _, f := range c.files {
						if strings.EqualFold(f, path) {
							return true
						}
					}
					return false
				},
				LookPath: func(name string) (string, error) {
					if name == "git" && c.git != "" {
						return c.git, nil
					}
					return "", errors.New("not found")
				},
			}
			got := checkGitBash(p)
			text := got.Summary + " " + got.Detail
			if got.ID != "git-bash" || got.Status != c.status {
				t.Fatalf("%s: %+v", c.status, got)
			}
			for _, s := range c.say {
				if !strings.Contains(text, s) {
					t.Errorf("summary/detail lack %q: %q", s, text)
				}
			}
			for _, s := range c.fix {
				if !strings.Contains(got.Fix, s) {
					t.Errorf("fix lacks %q: %q", s, got.Fix)
				}
			}
			if c.noFix && got.Fix != "" {
				t.Errorf("a fix for an ok check: %q", got.Fix)
			}
		})
	}
}
