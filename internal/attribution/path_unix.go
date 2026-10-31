//go:build !windows

package attribution

import "regexp"

// hostPath is p: a shell's paths are the OS's.
func hostPath(p string) string { return p }

// driveRe matches a drive-letter path in a command: none off Windows.
var driveRe *regexp.Regexp

// systemDir: none but the POSIX ones (userPath).
func systemDir(string) bool { return false }

// driveRedirect: no drive paths off Windows.
const driveRedirect = ""
