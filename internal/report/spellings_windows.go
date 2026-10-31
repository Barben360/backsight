//go:build windows

package report

import (
	"path/filepath"
	"strings"

	"github.com/Barben360/backsight/internal/fsutil"
)

// spellings are the other ways Windows and Git Bash write path p: its long
// name, its 8.3 names from any ancestor down (C:\Users\RUNNER~1 is
// C:\Users\runneradmin), either case of its drive letter, and Git Bash's
// /c/Users/... form.
func spellings(p string) []string {
	var out []string
	forms := []string{p}
	long := p
	if l, err := fsutil.LongPathName(p); err == nil {
		long = l
		forms = append(forms, l)
	}
	// Any ancestor may come in its 8.3 form with the rest long: a TEMP of
	// C:\Users\RUNNER~1\AppData\Local\Temp holds long names below it.
	for a := long; filepath.Dir(a) != a; a = filepath.Dir(a) {
		if s, err := fsutil.ShortPathName(a); err == nil && s != a {
			forms = append(forms, s+long[len(a):])
		}
	}
	for _, f := range forms {
		out = append(out, f)
		if len(f) >= 3 && f[1] == ':' && (f[2] == '\\' || f[2] == '/') {
			d := f[:1]
			if up := strings.ToUpper(d); up != d {
				out = append(out, up+f[1:])
			} else {
				out = append(out, strings.ToLower(d)+f[1:])
			}
			out = append(out, "/"+strings.ToLower(d)+filepath.ToSlash(f[2:]))
		}
	}
	return out
}
