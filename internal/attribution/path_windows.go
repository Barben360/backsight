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
// temporary directory (Git for Windows mounts it there). Anything else is
// returned as it is.
func hostPath(p string) string {
	if len(p) >= 2 && p[0] == '/' && ('a' <= p[1]|0x20 && p[1]|0x20 <= 'z') && (len(p) == 2 || p[2] == '/') {
		return strings.ToUpper(p[1:2]) + `:\` + filepath.FromSlash(strings.TrimPrefix(p[2:], "/"))
	}
	if p == "/tmp" || strings.HasPrefix(p, "/tmp/") {
		return filepath.Join(os.TempDir(), filepath.FromSlash(p[len("/tmp"):]))
	}
	return p
}

// driveRe matches a drive-letter path in a command (C:\x, C:/x), anchored
// like absRe.
var driveRe = regexp.MustCompile("^[A-Za-z]:[\\\\/][^\\s'\"`;|&<>(){}\\[\\],:*?]*")
