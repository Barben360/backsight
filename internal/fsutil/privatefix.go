package fsutil

import "strings"

// PrivateFix is the command that makes path private again on Windows,
// MakePrivate's DACL: full access to the user (sid, from CurrentUserSID;
// "" names them by PowerShell's user name), SYSTEM and Administrators by SID (names are localized), the
// inherited entries cut and the other readers' explicit ones removed. A
// directory's entries are inherited by what it holds; a file's carry no
// inheritance flags. It is a PowerShell command: Git Bash would rewrite
// /inheritance:r as a path.
func PrivateFix(path string, dir bool, sid string, readers []string) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	perm := ":F"
	if dir {
		perm = ":(OI)(CI)F"
	}
	me := q("*" + sid + perm)
	if sid == "" {
		me = `"${env:USERNAME}` + perm + `"`
	}
	cmd := "in PowerShell: icacls " + q(path) + " /inheritance:r /grant:r " + me + " " + q("*S-1-5-18"+perm) + " " + q("*S-1-5-32-544"+perm)
	var remove []string
	for _, r := range readers {
		switch {
		case r == "Everyone": // a missing DACL
			continue
		case strings.HasPrefix(r, "S-1-"):
			remove = append(remove, q("*"+r))
		default:
			remove = append(remove, q(r))
		}
	}
	if len(remove) > 0 {
		cmd += " /remove:g " + strings.Join(remove, " ")
	}
	return cmd
}
