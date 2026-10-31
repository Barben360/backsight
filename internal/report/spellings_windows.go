//go:build windows

package report

import (
	"path/filepath"
	"strings"

	"github.com/Barben360/backsight/internal/fsutil"
)

// spellings are the other ways Windows and Git Bash write path p: its long
// and 8.3 names (C:\Users\RUNNER~1 is C:\Users\runneradmin), either case
// of its drive letter, and Git Bash's /c/Users/... form.
func spellings(p string) []string {
	var out []string
	forms := []string{p}
	if l, err := fsutil.LongPathName(p); err == nil {
		forms = append(forms, l)
	}
	if s, err := fsutil.ShortPathName(p); err == nil {
		forms = append(forms, s)
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
