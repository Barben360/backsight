package doctor

import (
	"strings"
)

// GitBashProbe is what the git-bash check reads (Windows; injected so
// tests describe any machine): the environment, whether a file exists,
// and the PATH lookup of a command.
type GitBashProbe struct {
	Getenv   func(string) string
	Exists   func(path string) bool
	LookPath func(name string) (string, error)
}

// gitBashImpact is what a Claude Code without Git Bash means for backsight.
const gitBashImpact = "Claude Code then runs backsight's hooks in PowerShell, where SessionStart gets no environment file: the commands Claude runs get no BACKSIGHT_PROJECT or BACKSIGHT_SESSION (backsight's CLI then names the project from the directory it runs in)"

// checkGitBash says whether Claude Code on Windows will find Git Bash
// (its Bash tool, and the shell of backsight's hooks). Claude Code's own
// detection is not fully documented, so it is judged conservatively: ok
// when CLAUDE_CODE_GIT_BASH_PATH names an existing file, or when Git for
// Windows is in one of its standard places (%ProgramFiles%\Git,
// %LOCALAPPDATA%\Programs\Git); any other Git (a PortableGit, a git on
// PATH elsewhere) counts as not found. The fix names the bash.exe beside
// the git on PATH when there is one.
func checkGitBash(p GitBashProbe) Check {
	c := Check{ID: "git-bash"}
	fix := func() string {
		if bash := bashBesideGit(p); bash != "" {
			return "in PowerShell: [Environment]::SetEnvironmentVariable('CLAUDE_CODE_GIT_BASH_PATH', '" + strings.ReplaceAll(bash, "'", "''") +
				"', 'User'), then restart Claude Code (from a new terminal)"
		}
		return "install Git for Windows (https://git-scm.com/downloads/win), or set the user environment variable CLAUDE_CODE_GIT_BASH_PATH to the bin\\bash.exe of your Git " +
			"(in PowerShell: [Environment]::SetEnvironmentVariable('CLAUDE_CODE_GIT_BASH_PATH', '<Git>\\bin\\bash.exe', 'User')), then restart Claude Code (from a new terminal)"
	}
	if named := p.Getenv("CLAUDE_CODE_GIT_BASH_PATH"); named != "" {
		if p.Exists(named) {
			c.Status, c.Summary = OK, "Claude Code finds Git Bash at "+named+" (CLAUDE_CODE_GIT_BASH_PATH)"
			return c
		}
		c.Status, c.Summary = Warn, "CLAUDE_CODE_GIT_BASH_PATH names "+named+", which does not exist"
		c.Detail, c.Fix = gitBashImpact, fix()
		return c
	}
	var roots, looked []string
	if pf := p.Getenv("ProgramFiles"); pf != "" {
		roots = append(roots, pf)
	}
	if la := p.Getenv("LOCALAPPDATA"); la != "" {
		roots = append(roots, winJoin(la, "Programs"))
	}
	for _, root := range roots {
		bash := winJoin(root, "Git", "bin", "bash.exe")
		if p.Exists(bash) {
			c.Status, c.Summary = OK, "Git Bash at "+bash+", where Claude Code looks for Git for Windows"
			return c
		}
		looked = append(looked, bash)
	}
	c.Status, c.Summary = Warn, "Claude Code may not find Git Bash"
	c.Detail = gitBashImpact + "; CLAUDE_CODE_GIT_BASH_PATH is not set and Git for Windows is not at " + strings.Join(looked, " or ")
	if bash := bashBesideGit(p); bash != "" {
		c.Detail += " (the git on PATH, beside " + bash + ", is not in a place Claude Code is known to look)"
	}
	c.Fix = fix()
	return c
}

// bashBesideGit is the bin\bash.exe of the Git whose git.exe is on PATH
// (in <Git>\cmd, <Git>\bin or <Git>\mingw64\bin), "" when there is none.
func bashBesideGit(p GitBashProbe) string {
	git, err := p.LookPath("git")
	if err != nil || git == "" {
		return ""
	}
	dir := winDir(git)
	root := winDir(dir)
	switch strings.ToLower(winBase(dir)) {
	case "bin":
		if strings.EqualFold(winBase(root), "mingw64") {
			root = winDir(root)
		}
	case "cmd":
	default:
		return ""
	}
	if bash := winJoin(root, "bin", "bash.exe"); p.Exists(bash) {
		return bash
	}
	return ""
}

// winJoin, winDir and winBase handle Windows paths whatever the OS doctor
// is tested on.
func winJoin(dir string, parts ...string) string {
	return strings.TrimRight(dir, `\/`) + `\` + strings.Join(parts, `\`)
}

func winDir(p string) string {
	p = strings.TrimRight(p, `\/`)
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[:i]
	}
	return ""
}

func winBase(p string) string {
	p = strings.TrimRight(p, `\/`)
	return p[strings.LastIndexAny(p, `\/`)+1:]
}
