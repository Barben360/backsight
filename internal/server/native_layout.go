package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Barben360/backsight/internal/server/nativelock"
)

// The native runtime's per-OS layout (M7 block 2): Linux's paths, and
// Windows' venv (Scripts\*.exe, Lib\site-packages) and copied Postgres
// (pg\<version>\) under the same Root(). Each follows Native.GOOS, so that
// a test on one OS renders the other's paths, with that OS's separator.

// goos is the OS n lays out and runs for: GOOS, else this one.
func (n Native) goos() string {
	if n.GOOS == "" {
		return runtime.GOOS
	}
	return n.GOOS
}

// join is filepath.Join, but with Windows' \ for the Windows layout on
// another host (a test rendering Windows' files on Linux).
func (n Native) join(elem ...string) string {
	if n.goos() == "windows" && runtime.GOOS != "windows" {
		return winJoin(elem...)
	}
	return filepath.Join(elem...)
}

// winJoin joins Windows path elements with \ on any host.
func winJoin(elem ...string) string {
	var parts []string
	for i, e := range elem {
		e = strings.ReplaceAll(e, "/", `\`)
		if i == 0 {
			e = strings.TrimRight(e, `\`)
		} else {
			e = strings.Trim(e, `\`)
		}
		if e != "" {
			parts = append(parts, e)
		}
	}
	return strings.Join(parts, `\`)
}

// exe is name as an executable's file name on n's OS.
func (n Native) exe(name string) string {
	if n.goos() == "windows" {
		return name + ".exe"
	}
	return name
}

// UV is the pinned uv (uv.exe on Windows).
func (n Native) UV() string {
	return n.join(n.Root(), "uv", nativelock.UVVersion, n.exe("uv"))
}

// installation is where pg0 extracted its Postgres; on Windows, the copy
// of it under Root() the server runs.
func (n Native) installation() string {
	if n.goos() == "windows" {
		return n.join(n.Root(), "pg", nativelock.PGInstallation)
	}
	return n.join(n.Home(), ".pg0", "installation", nativelock.PGInstallation)
}

// venvBin is the program name of version v's venv: bin/<name>, or
// Scripts\<name>.exe on Windows.
func (n Native) venvBin(v, name string) string {
	if n.goos() == "windows" {
		return n.join(n.VenvDir(v), "Scripts", n.exe(name))
	}
	return n.join(n.VenvDir(v), "bin", name)
}

// sitePackages is version v's site-packages.
func (n Native) sitePackages(v string) string {
	if n.goos() == "windows" {
		return n.join(n.VenvDir(v), "Lib", "site-packages")
	}
	return n.join(n.VenvDir(v), "lib", "python3.11", "site-packages")
}

// pgBin is the installation's program name.
func (n Native) pgBin(name string) string { return n.join(n.installation(), "bin", n.exe(name)) }

// windowsToolEnv are the settings of the user's environment a tool gets on
// Windows besides PATH, TEMP and TMP (0080): what a program needs to find
// the system, the user's profile and the shell.
var windowsToolEnv = [][]string{{"SYSTEMROOT", "SystemDrive"}, {"USERPROFILE", "LOCALAPPDATA", "APPDATA", "COMSPEC"}}

// toolEnvFor is the whole environment of a tool in dir: toolEnv() on
// Linux; on Windows the fixed list, PATH=<dir>;<SYSTEMROOT>\System32 (
// C:\Windows without SYSTEMROOT), TEMP and TMP under Root(), the proxy
// settings, each once (Windows' names ignore case), and uv's settings (no
// configuration file, managed Pythons only).
func (n Native) toolEnvFor(dir string) []string {
	if n.goos() != "windows" {
		return toolEnv()
	}
	var env []string
	add := func(keys []string) {
		for _, k := range keys {
			if v := os.Getenv(k); v != "" {
				env = append(env, k+"="+v)
			}
		}
	}
	add(windowsToolEnv[0])
	tmp := n.join(n.Root(), "tmp")
	env = append(env, "TEMP="+tmp, "TMP="+tmp)
	add(windowsToolEnv[1])
	sysRoot := os.Getenv("SYSTEMROOT")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	env = append(env, "PATH="+dir+";"+winJoin(sysRoot, "System32"))
	seen := map[string]bool{}
	for _, k := range passedThrough {
		if v := os.Getenv(k); v != "" && !seen[strings.ToUpper(k)] {
			seen[strings.ToUpper(k)] = true
			env = append(env, k+"="+v)
		}
	}
	return append(env, "UV_NO_CONFIG=1", "UV_PYTHON_PREFERENCE=only-managed")
}
