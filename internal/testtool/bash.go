package testtool

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Bash is the bash Claude Code's Bash tool runs: on Windows Git Bash,
// found from git's install directory, then where Git for Windows installs
// per machine, per user or portable: never bash from PATH, which can be
// WSL's in System32. It
// skips the test when there is none, and fails it on CI, where a missing
// bash would hide what it checks.
func Bash(t testing.TB) string {
	t.Helper()
	if p := findBash(runtime.GOOS, exec.LookPath, os.Getenv, exists); p != "" {
		return p
	}
	if os.Getenv("CI") != "" {
		t.Fatal("no bash on CI (Git Bash on Windows)")
	}
	t.Skip("Task 12: no bash here (Git for Windows is not installed)")
	return ""
}

func exists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// findBash looks for bash as Bash does, with the system's lookups given.
func findBash(goos string, look func(string) (string, error), getenv func(string) string, isFile func(string) bool) string {
	if goos != "windows" {
		p, _ := look("bash")
		return p
	}
	var roots []string
	// git.exe is in <root>\cmd, <root>\bin or <root>\mingw64\bin.
	if g, err := look("git"); err == nil {
		d := filepath.Dir(g)
		roots = append(roots, filepath.Dir(d), filepath.Dir(filepath.Dir(d)))
	}
	for _, base := range []string{getenv("ProgramFiles"), getenv("ProgramW6432"), getenv("ProgramFiles(x86)")} {
		if base != "" {
			roots = append(roots, filepath.Join(base, "Git"))
		}
	}
	if l := getenv("LOCALAPPDATA"); l != "" {
		roots = append(roots, filepath.Join(l, "Programs", "Git"), filepath.Join(l, "PortableGit"))
	}
	for _, r := range roots {
		for _, p := range []string{filepath.Join(r, "bin", "bash.exe"), filepath.Join(r, "usr", "bin", "bash.exe")} {
			if isFile(p) {
				return p
			}
		}
	}
	return ""
}
