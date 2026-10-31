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
	c := checkNativeHome(f)
	if c.Status != Error || !strings.Contains(c.Summary, `BUILTIN\Users, NT AUTHORITY\Authenticated Users`) ||
		!strings.Contains(c.Fix, `icacls "C:\n\home" /inheritance:r`) {
		t.Errorf("readers: %+v", c)
	}
}
