package doctor

import (
	"strings"
	"testing"
)

// Where the OS keeps access in ACLs the mode bits say nothing: the check
// judges the readers, whatever HomeMode holds.
func TestNativeHomeJudgesTheACLReaders(t *testing.T) {
	f := NativeFacts{Home: `C:\n\home`, HomeMode: 0o755, HomeACL: true}
	if c := checkNativeHome(f); c.Status != OK || !strings.Contains(c.Summary, "private") {
		t.Errorf("no readers: %+v", c)
	}
	f.HomeMode = 0o700
	f.HomeReaders = []string{`BUILTIN\Users`, `NT AUTHORITY\Authenticated Users`}
	f.UserSID = "S-1-5-21-1-2-3-1001"
	c := checkNativeHome(f)
	fix := `in PowerShell: icacls 'C:\n\home' /inheritance:r /grant:r '*S-1-5-21-1-2-3-1001:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F'` +
		` /remove:g 'BUILTIN\Users' 'NT AUTHORITY\Authenticated Users'`
	if c.Status != Error || !strings.Contains(c.Summary, `BUILTIN\Users, NT AUTHORITY\Authenticated Users`) || c.Fix != fix {
		t.Errorf("readers: %+v\nwant fix %s", c, fix)
	}
}

// Fix round 1 (Task 11): the icacls fix is a PowerShell command (no cmd
// %USERNAME%; Git Bash would rewrite /inheritance:r), names the accounts
// by SID, grants a file without inheritance flags, quotes a path with a
// quote, and removes the other readers' explicit entries (a SID by its
// * form).
func TestICACLSFix(t *testing.T) {
	for _, c := range []struct {
		name, path string
		dir        bool
		sid        string
		readers    []string
		want       string
	}{
		{"file", `C:\b\config\secrets.toml`, false, "S-1-5-21-9-1001", []string{`BUILTIN\Users`},
			`in PowerShell: icacls 'C:\b\config\secrets.toml' /inheritance:r /grant:r '*S-1-5-21-9-1001:F' '*S-1-5-18:F' '*S-1-5-32-544:F' /remove:g 'BUILTIN\Users'`},
		{"directory, unresolved SID", `C:\Jo's\data`, true, "S-1-5-21-9-1001", []string{"S-1-5-21-9-1002"},
			`in PowerShell: icacls 'C:\Jo''s\data' /inheritance:r /grant:r '*S-1-5-21-9-1001:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' /remove:g '*S-1-5-21-9-1002'`},
		{"no DACL, no SID", `C:\b\data`, true, "", []string{"Everyone"},
			`in PowerShell: icacls 'C:\b\data' /inheritance:r /grant:r "${env:USERNAME}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F'`},
	} {
		if got := icaclsFix(c.path, c.dir, c.sid, c.readers); got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, c.want)
		}
		if strings.Contains(c.want, "%") {
			t.Errorf("%s: cmd's %%VAR%% syntax", c.name)
		}
	}
}
