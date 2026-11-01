//go:build windows

package attribution

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// hostPath translates the paths of Git Bash, the shell of Claude Code's
// Bash tool on Windows, to Windows ones: /c/x is C:\x and /tmp/x is in the
// temporary directory (Git for Windows mounts it there). A long-path
// prefix goes: \\?\C:\x is C:\x, \\?\UNC\s\x is \\s\x. Anything else is
// returned as it is.
func hostPath(p string) string {
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	if rest, ok := strings.CutPrefix(p, `\\?\`); ok && len(rest) >= 2 && rest[1] == ':' {
		return rest
	}
	if len(p) >= 2 && p[0] == '/' && ('a' <= p[1]|0x20 && p[1]|0x20 <= 'z') && (len(p) == 2 || p[2] == '/') {
		return strings.ToUpper(p[1:2]) + `:\` + filepath.FromSlash(strings.TrimPrefix(p[2:], "/"))
	}
	if p == "/tmp" || strings.HasPrefix(p, "/tmp/") {
		return filepath.Join(os.TempDir(), filepath.FromSlash(p[len("/tmp"):]))
	}
	return p
}

// systemDir: p is under a directory of the system's (Windows, the
// programs, their shared data), never a project.
func systemDir(p string) bool {
	for _, k := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
		v := os.Getenv(k)
		if v == "" {
			continue
		}
		root := filepath.Clean(v)
		if len(p) >= len(root) && strings.EqualFold(p[:len(root)], root) && (len(p) == len(root) || p[len(root)] == '\\') {
			return true
		}
	}
	return false
}

// driveRedirect: a shell redirect to a drive path writes.
const driveRedirect = `|>\s*[A-Za-z]:[\\/]`

// driveRe matches a drive-letter path in a command (C:\x, C:/x), anchored
// like absRe.
var driveRe = regexp.MustCompile("^[A-Za-z]:[\\\\/][^\\s'\"`;|&<>(){}\\[\\],:*?]*")
